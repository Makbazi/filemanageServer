package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/transfer"
)

// serveContent 统一文件下发：支持 Range/条件请求（http.ServeContent），
// 配置了下载限速时经令牌桶节流（需求18）。
func (s *Server) serveContent(c *gin.Context, abs, name string, inline bool) {
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(c, http.StatusNotFound, errors.New("file not found"))
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	defer f.Close()
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	c.Writer.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disp, name))
	if inline {
		c.Writer.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(name)))
	}
	lim := transfer.NewLimiter(s.Cfg.RateLimit.DownloadKBps)
	content := transfer.NewRateReadSeeker(c.Request.Context(), f, lim)
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), content)
}

// handleDownload GET /api/v1/download?rootId=&path=。
// 正常模式：需登录 + Download 权限（需求6，匿名 401 与 0.2.x 一致）；
// 降级模式（0.3.0 无元数据库）：匿名按只读决策放开下载（用户决策，见 degradedView）。
func (s *Server) handleDownload(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if !s.degradedView(c, rootID, rel, func(d perm.Decision) bool { return d.Download }, "download") {
		return
	}
	_, abs, err := s.resolve(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.serveContent(c, abs, filepath.Base(abs), false)
}

// handlePreview GET /api/v1/preview?rootId=&path=（需登录 + Download 权限）。
// inline 下发，支持 Range，供客户端预览图片/视频/音频/文档（需求11）。
func (s *Server) handlePreview(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if !s.degradedView(c, rootID, rel, func(d perm.Decision) bool { return d.Download }, "download(preview)") {
		return
	}
	_, abs, err := s.resolve(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.serveContent(c, abs, filepath.Base(abs), true)
}

// handleUpload POST /api/v1/upload?rootId=&path=（兼容旧客户端的整文件上传）。
// body 为原始字节流；限速读取、持路径锁、覆盖前自动存版本、tmp+rename 原子写。
func (s *Server) handleUpload(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	target := c.Request.URL.Query().Get("path")
	if target == "" {
		writeErr(c, http.StatusBadRequest, errors.New("path required"))
		return
	}
	if strings.HasSuffix(target, "/") || filepath.Base(target) == "." {
		writeErr(c, http.StatusBadRequest, errors.New("path must be full file path"))
		return
	}
	rel := fileops.CleanRel(target)
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	if d := s.permsFor(c, rootID, rel); !d.Upload {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: upload"))
		return
	}
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(sc.Abs), 0o755); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	lim := transfer.NewLimiter(s.Cfg.RateLimit.UploadKBps)
	body := transfer.NewRateReader(c.Request.Context(), c.Request.Body, lim)

	unlock := s.Locks.Lock(transfer.Key(sc.PhysID, sc.Phys))
	defer unlock()
	// 覆盖已有文件前保存版本（需求13；版本索引按物理键存储）
	if err := s.Versions.Snapshot(u.ID, sc.PhysID, sc.Phys, sc.Abs); err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	// 整文件大小硬上限（maxUploadBytes，0=不限；超限 413，不落地半成品，0.3.1）
	n, err := fileops.WriteFileAtomicLimit(sc.Abs, body, s.Cfg.MaxUploadBytes)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	e := FileEntry{Name: filepath.Base(sc.Abs), Path: sc.Disp, IsDir: false,
		Size: n, Mtime: time.Now(), Kind: fileops.KindOf(sc.Disp, false)}
	s.recordMeta(u, sc, e)
	writeJSON(c, http.StatusOK, e)
}

// ---------- 分片上传 / 断点续传（需求3） ----------

// uploadSessionView 会话对外视图（rootId/path 已由调用方换算为客户端显示键，0.5.0）。
func uploadSessionView(sess *store.UploadSession, resumed bool, dispRoot, dispPath string) map[string]any {
	return map[string]any{
		"sessionId":      sess.ID,
		"rootId":         dispRoot,
		"path":           dispPath,
		"size":           sess.Size,
		"chunkSize":      sess.ChunkSize,
		"chunkCount":     transfer.ChunkCount(sess),
		"uploadedChunks": sess.UploadedChunks,
		"status":         sess.Status,
		"resumed":        resumed,
		"createdAt":      sess.CreatedAt,
		"updatedAt":      sess.UpdatedAt,
	}
}

// handleUploadSessionCreate POST /api/v1/upload/session
// body: {rootId, path, size, chunkSize?, contentHash?, baseHash?}
// baseHash：创建时目标文件现有内容 SHA-256（可从 GET /stat 获取）；
// 新文件传 "-" 表示"断言目标不存在"；不传则跳过并发校验。
// 同路径已有其他用户的活动会话 → 409（多客户端修改相同文件保护）。
func (s *Server) handleUploadSessionCreate(c *gin.Context) {
	var req struct {
		RootID      string `json:"rootId"`
		Path        string `json:"path"`
		Size        int64  `json:"size"`
		ChunkSize   int64  `json:"chunkSize"`
		ContentHash string `json:"contentHash"`
		BaseHash    string `json:"baseHash"`
	}
	if err := decode(c, &req); err != nil || req.Path == "" || req.Size < 0 {
		writeErr(c, http.StatusBadRequest, errors.New("rootId + path + size required"))
		return
	}
	rel := fileops.CleanRel(req.Path)
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	if d := s.permsFor(c, req.RootID, rel); !d.Upload {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: upload"))
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if s.Cfg.MaxUploadBytes > 0 && req.Size > s.Cfg.MaxUploadBytes {
		writeErr(c, http.StatusRequestEntityTooLarge,
			fmt.Errorf("size %d exceeds maxUploadBytes %d", req.Size, s.Cfg.MaxUploadBytes))
		return
	}
	// 会话按物理键入库（多实例共享 + 完成时反查授权）
	sess, resumed, err := s.Uploads.CreateOrResume(u.ID, sc.PhysID, sc.Phys, req.Size, req.ChunkSize, req.ContentHash, req.BaseHash)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	code := http.StatusOK
	if !resumed {
		code = http.StatusCreated
	}
	writeJSON(c, code, s.sessView(c, sess, resumed))
}

// sessView 会话视图：存储的物理键反查为客户端显示键。
func (s *Server) sessView(c *gin.Context, sess *store.UploadSession, resumed bool) map[string]any {
	dispRoot, dispPath := sess.RootID, sess.Path
	if rid, disp, ok := s.displayOf(c, sess.RootID, sess.Path); ok {
		dispRoot, dispPath = rid, disp
	}
	return uploadSessionView(sess, resumed, dispRoot, dispPath)
}

// sessionOwned 取会话并校验属主（管理员可代管）。
func (s *Server) sessionOwned(c *gin.Context, id string) *store.UploadSession {
	if id == "" {
		writeErr(c, http.StatusBadRequest, errors.New("sessionId required"))
		return nil
	}
	sess, err := s.Uploads.Store.GetUploadSession(id)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return nil
	}
	u := currentUser(c)
	if u == nil || (sess.UserID != u.ID && u.Role != store.RoleAdmin) {
		writeErr(c, http.StatusForbidden, transfer.ErrNotOwner)
		return nil
	}
	return sess
}

// handleUploadSessionStatus GET /api/v1/upload/session?id=（断点续传查询）。
func (s *Server) handleUploadSessionStatus(c *gin.Context) {
	sess := s.sessionOwned(c, c.Request.URL.Query().Get("id"))
	if sess == nil {
		return
	}
	writeJSON(c, http.StatusOK, s.sessView(c, sess, true))
}

// handleUploadChunk PUT /api/v1/upload/chunk?sessionId=&index=，body 为分片原始字节。
func (s *Server) handleUploadChunk(c *gin.Context) {
	sess := s.sessionOwned(c, c.Request.URL.Query().Get("sessionId"))
	if sess == nil {
		return
	}
	if sess.Status != store.UploadStatusUploading {
		writeErr(c, http.StatusBadRequest, transfer.ErrBadSession)
		return
	}
	index, err := strconv.ParseInt(c.Request.URL.Query().Get("index"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad chunk index"))
		return
	}
	lim := transfer.NewLimiter(s.Cfg.RateLimit.UploadKBps)
	body := transfer.NewRateReader(c.Request.Context(), c.Request.Body, lim)
	if err := s.Uploads.WriteChunk(sess, index, body); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "index": index})
}

// handleUploadComplete POST /api/v1/upload/complete {sessionId}
// 校验分片齐全与哈希 → 持锁做 BaseHash 并发校验 → 旧内容存版本 → 原子替换。
func (s *Server) handleUploadComplete(c *gin.Context) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := decode(c, &req); err != nil || req.SessionID == "" {
		writeErr(c, http.StatusBadRequest, errors.New("sessionId required"))
		return
	}
	sess := s.sessionOwned(c, req.SessionID)
	if sess == nil {
		return
	}
	if sess.Status != store.UploadStatusUploading {
		writeErr(c, http.StatusBadRequest, transfer.ErrBadSession)
		return
	}
	u := currentUser(c)
	// sess 存的是物理键，scopePhys 反查授权基座（越权/回收的分配在此拒绝）
	sc, err := s.scopePhys(c, sess.RootID, sess.Path)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	beforeReplace := func() error {
		return s.Versions.Snapshot(u.ID, sc.PhysID, sc.Phys, sc.Abs)
	}
	hash, size, err := s.Uploads.Complete(sess, sc.Abs, beforeReplace)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	e := FileEntry{Name: filepath.Base(sc.Abs), Path: sc.Disp, IsDir: false,
		Size: size, Mtime: time.Now(), Kind: fileops.KindOf(sc.Disp, false), Hash: hash}
	s.recordMeta(u, sc, e)
	writeJSON(c, http.StatusOK, e)
}

// handleUploadSessionAbort DELETE /api/v1/upload/session?id=
func (s *Server) handleUploadSessionAbort(c *gin.Context) {
	sess := s.sessionOwned(c, c.Request.URL.Query().Get("id"))
	if sess == nil {
		return
	}
	if err := s.Uploads.Abort(sess); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}
