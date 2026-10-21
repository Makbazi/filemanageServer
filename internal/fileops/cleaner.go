package fileops

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
)

// Cleaner 后台清理器：
//   - 回收站超过保留期（默认 30 天）自动清理（需求4）
//   - 过期登录会话清理（需求22）
//   - 已结束/超期的分片上传会话与暂存目录清理（需求3）
type Cleaner struct {
	Store      store.Store
	TrashDir   string
	UsersDir   string        // <dataDir>/Users（家目录内嵌回收站清理需要）
	Retention  time.Duration // 回收站保留时长
	StagingDir string        // 分片上传暂存目录
	Interval   time.Duration // 巡检周期，默认 1h
}

// Run 阻塞运行直到 ctx 取消（应在 goroutine 中调用）。
func (c *Cleaner) Run(ctx context.Context) {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	// 启动后先立即执行一次，再按周期巡检
	c.runOnce()
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.runOnce()
		}
	}
}

func (c *Cleaner) runOnce() {
	if store.Degraded(c.Store) {
		// 降级模式：无元数据库可清，仅扫孤儿暂存目录（无会话记录，全部按孤儿判定）
		c.sweepStaging(nil)
		return
	}
	// 0.2.7：清理失败是真实故障（下一轮虽会重试，但持续失败须让运维看到），
	// 走 logx.Warn——默认等级 warn 下可见；成功计数行仍按生命周期 INFO 桥接。
	if n, err := c.cleanTrash(); err != nil {
		logx.Warn("cleaner: trash: %v", err)
	} else if n > 0 {
		log.Printf("cleaner: 已自动清理 %d 条超过保留期的回收站条目", n)
	}
	if err := c.Store.DeleteExpiredSessions(time.Now()); err != nil {
		logx.Warn("cleaner: sessions: %v", err)
	}
	// 审计日志保留 90 天
	if err := c.Store.DeleteAuditBefore(time.Now().Add(-90 * 24 * time.Hour)); err != nil {
		logx.Warn("cleaner: audit: %v", err)
	}
	if n, err := c.cleanUploadSessions(); err != nil {
		logx.Warn("cleaner: uploads: %v", err)
	} else if n > 0 {
		log.Printf("cleaner: 已清理 %d 个失效上传会话", n)
	}
}

// cleanTrash 清理删除时间早于保留期的回收站条目（实体+记录）。
func (c *Cleaner) cleanTrash() (int, error) {
	before := time.Now().Add(-c.Retention)
	items, err := c.Store.ListTrashBefore(before)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		c.removeTrashBlob(it)
		if err := c.Store.RemoveTrashItem(it.UserID, it.TrashID); err != nil {
			logx.Warn("cleaner: remove trash record %s: %v", it.TrashID, err)
			continue
		}
		n++
	}
	return n, nil
}

// removeTrashBlob 清理回收站实体的全部候选位置（0.2.11，新→旧逐一 RemoveAll，
// 存在才删、天然幂等）：并级 <家目录名>_trash、迁移中间态（新名内 .fmtrash）、
// 旧布局名内 .fmtrash、中央遗留路径。家目录名以分配行 SubPath 为准（缺失时按
// user_<账户>_<id> 现算；账户名不可安全作目录名时退回旧名直拼）。
func (c *Cleaner) removeTrashBlob(it *store.TrashItem) {
	if c.UsersDir != "" {
		if u, err := c.Store.GetUserByID(it.UserID); err == nil {
			home := HomeDirName(u.Username, u.ID)
			if sub := c.homeSub(it.UserID); sub != "" {
				home = sub
			}
			for _, p := range TrashBlobCandidates(c.UsersDir, home, &u.Username, it.TrashID) {
				_ = os.RemoveAll(p)
			}
			if UsernameAsDir(u.Username) != nil {
				// 不安全账户名遗留（迁移跳过的用户）：实体只可能在旧名目录内。
				_ = os.RemoveAll(HomeTrashPath(c.UsersDir, u.Username, it.TrashID))
			}
		}
	}
	_ = os.RemoveAll(TrashPath(c.TrashDir, it.UserID, it.TrashID))
}

// homeSub 用户家目录分配的 SubPath（无分配返回 ""）。
func (c *Cleaner) homeSub(userID int64) string {
	urs, err := c.Store.ListUserRoots(userID)
	if err != nil {
		return ""
	}
	for _, r := range urs {
		if r.RootID == store.HomeRootID {
			return CleanRel(r.SubPath)
		}
	}
	return ""
}

// cleanUploadSessions 清理已结束的上传会话（保留 24h 供查询）与
// 超过 7 天仍在 uploading 的僵尸会话，并移除对应暂存目录。
func (c *Cleaner) cleanUploadSessions() (int, error) {
	if c.StagingDir == "" {
		return 0, nil
	}
	now := time.Now()
	sessions, err := c.Store.ListUploadSessions(0)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		expired := false
		switch s.Status {
		case store.UploadStatusUploading:
			// 0.2.1：僵尸判定改按最后活动时间（UpdatedAt，每片写入即刷新）——
			// 旧口径按 CreatedAt+7d 会把跨越 7 天的合法慢续传连坐清除。
			// 7d 无任何活动 = 确属废弃会话（其 staging 碎片一并回收）。
			expired = now.Sub(s.UpdatedAt) > 7*24*time.Hour
		default:
			expired = now.Sub(s.UpdatedAt) > 24*time.Hour
		}
		if !expired {
			continue
		}
		_ = os.RemoveAll(filepath.Join(c.StagingDir, s.ID))
		_ = c.Store.DeleteUploadSession(s.ID)
		n++
	}
	// 孤儿暂存目录（有目录无记录）
	known := map[string]bool{}
	for _, s := range sessions {
		known[s.ID] = true
	}
	return n + c.sweepStaging(known), nil
}

// sweepStaging 移除 mtime 超过 24h 且无对应会话记录的暂存子目录，返回清理数。
// known 为 nil 表示无任何有效记录（降级模式）。
func (c *Cleaner) sweepStaging(known map[string]bool) int {
	if c.StagingDir == "" {
		return 0
	}
	entries, err := os.ReadDir(c.StagingDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || (known != nil && known[e.Name()]) {
			continue
		}
		info, err := e.Info()
		if err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(c.StagingDir, e.Name()))
			n++
		}
	}
	return n
}
