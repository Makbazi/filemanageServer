package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/transfer"
)

// pathReq 通用路径请求体。
type pathReq struct {
	RootID  string   `json:"rootId"`
	Path    string   `json:"path"`
	Paths   []string `json:"paths"`
	Dest    string   `json:"dest"`
	Name    string   `json:"name"`
	NewName string   `json:"newName"`
}

// handleRoots GET /api/v1/roots（0.5.0 jail 模型）。
// 登录用户：管理员分配给自己的目录（user_roots）+ ACL 分享给自己的目录；
// 匿名：仅"公开共享目录"清单（public-view ACL）；
// 降级模式：plain 配置根（0.3.x 行为）。物理配置根对用户端口主体不可见。
func (s *Server) handleRoots(c *gin.Context) {
	type rootView struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Jail string `json:"jail"` // 授权基座（相对物理根；""=整根）
	}
	out := make([]rootView, 0, 4)
	if !s.dbReady() {
		for i := range s.Cfg.Roots {
			r := &s.Cfg.Roots[i]
			out = append(out, rootView{ID: r.ID, Name: r.Name})
		}
		writeJSON(c, http.StatusOK, out)
		return
	}
	bases, err := s.basesMap(c)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	// 分配条目的别名优先展示
	alias := map[string]string{}
	if u := currentUser(c); u != nil {
		if urs, err := s.Store.ListUserRoots(u.ID); err == nil {
			for _, r := range urs {
				alias[vrootID(r.RootID, fileops.CleanRel(r.SubPath))] = r.Alias
			}
		}
	}
	// 输出顺序确定性：配置根按声明序，其后内部合成根（_users，0.7.0）按 id 排序
	ordered := make([]string, 0, len(s.Roots))
	seen := map[string]bool{}
	for i := range s.Cfg.Roots {
		ordered = append(ordered, s.Cfg.Roots[i].ID)
		seen[s.Cfg.Roots[i].ID] = true
	}
	var extra []string
	for id := range s.Roots {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	ordered = append(ordered, extra...)
	for _, rid := range ordered {
		r := s.Roots[rid]
		if r == nil {
			continue
		}
		for _, b := range bases[rid] {
			name := alias[vrootID(rid, b)]
			if name == "" {
				name = baseName(r, b)
			}
			out = append(out, rootView{ID: vrootID(rid, b), Name: name, Jail: b})
		}
	}
	writeJSON(c, http.StatusOK, out)
}

// handleList GET /api/v1/files?rootId=&path=（需 View 权限；匿名可浏览，需求6）。
// 0.5.0：rootId 为虚拟根 id，返回条目路径相对该根（显示路径）。
func (s *Server) handleList(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if d := s.permsFor(c, rootID, rel); !d.View {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: view"))
		return
	}
	dirs, err := os.ReadDir(sc.Abs)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(c, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	u := currentUser(c)
	view, err := s.permsBatch(c, rootID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	entries := make([]FileEntry, 0, len(dirs))
	for _, d := range dirs {
		// 0.7.0 需求3：家目录内嵌回收站目录对用户不可见（scope 已拦截其内容访问）
		if sc.PhysID == store.HomeRootID && sc.Disp == "" && d.Name() == fileops.HomeTrashName {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		e := sc.entry(filepath.Join(sc.Abs, d.Name()), info)
		// 跳过无查看权限的子条目（被 public/用户 ACL 收紧项不得在父列表暴露，0.3.1）
		if !view(e.Path).View {
			continue
		}
		entries = append(entries, e)
		s.recordMeta(u, sc, e)
	}
	sortEntries(entries)
	writeJSON(c, http.StatusOK, map[string]any{
		"rootId": sc.VRoot, "path": sc.Disp, "entries": entries,
		"anonymous": u == nil,
	})
}

// handleStat GET /api/v1/stat?rootId=&path=（需 View 权限）。
func (s *Server) handleStat(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if d := s.permsFor(c, rootID, rel); !d.View {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: view"))
		return
	}
	info, err := os.Stat(sc.Abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	e := sc.entry(sc.Abs, info)
	// 附带内容哈希，供客户端做乐观并发校验（需求3）
	if !info.IsDir() && info.Size() <= 512<<20 {
		if h, herr := fileops.SHA256File(sc.Abs); herr == nil {
			e.Hash = h
		}
	}
	writeJSON(c, http.StatusOK, e)
}

// handleMkdir POST /api/v1/mkdir（需 Upload 权限）。
func (s *Server) handleMkdir(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	rel := fileops.CleanRel(req.Path)
	if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.Upload }, "upload") {
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	unlock := s.Locks.Lock(transfer.Key(sc.PhysID, sc.Phys))
	defer unlock()
	if err := os.MkdirAll(sc.Abs, 0o755); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleRename POST /api/v1/rename（需 Upload 权限）。
func (s *Server) handleRename(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil || req.NewName == "" || strings.ContainsAny(req.NewName, "/\\") {
		writeErr(c, http.StatusBadRequest, errors.New("path + newName required"))
		return
	}
	rel := fileops.CleanRel(req.Path)
	if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.Upload }, "upload") {
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	// 0.7.0：家目录内禁止把任何条目改名成保留回收站目录名（scope 不经过目标名）
	if sc.PhysID == store.HomeRootID && req.NewName == fileops.HomeTrashName {
		writeErr(c, http.StatusBadRequest, errors.New("reserved directory name"))
		return
	}
	target := filepath.Join(filepath.Dir(sc.Abs), req.NewName)
	unlock := s.Locks.Lock(transfer.Key(sc.PhysID, sc.Phys))
	defer unlock()
	// 0.1.0 需求37：走 MovePath 而非裸 os.Rename——rename 保留源 mode，
	// MovePath 收尾统一剥离执行位（同目录快路径就是 os.Rename）。
	if err := fileops.MovePath(sc.Abs, target); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	u := currentUser(c)
	dispTarget := fileops.RelOf(sc.BaseAbs, target)
	_ = s.Store.RenameFileMeta(u.ID, sc.PhysID, sc.Phys, sc.physOf(dispTarget))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "path": dispTarget})
}

// handleMove POST /api/v1/move（源需 Delete 权限，目标需 Upload 权限）。
func (s *Server) handleMove(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil || req.Dest == "" {
		writeErr(c, http.StatusBadRequest, errors.New("paths + dest required"))
		return
	}
	destRel := fileops.CleanRel(req.Dest)
	if !s.requirePerm(c, req.RootID, destRel, func(d perm.Decision) bool { return d.Upload }, "upload(dest)") {
		return
	}
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.Delete }, "delete(src)") {
			return
		}
	}
	destSc, err := s.scope(c, req.RootID, destRel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if err := os.MkdirAll(destSc.Abs, 0o755); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		srcSc, err := s.scope(c, req.RootID, rel)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		if fileops.IsSameOrChild(srcSc.Abs, destSc.Abs) {
			writeErr(c, http.StatusBadRequest, fmt.Errorf("cannot move %s into itself", p))
			return
		}
		target := filepath.Join(destSc.Abs, filepath.Base(srcSc.Abs))
		if srcSc.Abs == target {
			continue
		}
		unlock := s.Locks.Lock(transfer.Key(srcSc.PhysID, srcSc.Phys))
		err = fileops.MovePath(srcSc.Abs, target)
		unlock()
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleCopy POST /api/v1/copy（源需 View 权限，目标需 Upload 权限）。
func (s *Server) handleCopy(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil || req.Dest == "" {
		writeErr(c, http.StatusBadRequest, errors.New("paths + dest required"))
		return
	}
	destRel := fileops.CleanRel(req.Dest)
	if !s.requirePerm(c, req.RootID, destRel, func(d perm.Decision) bool { return d.Upload }, "upload(dest)") {
		return
	}
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.View }, "view(src)") {
			return
		}
	}
	destSc, err := s.scope(c, req.RootID, destRel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		srcSc, err := s.scope(c, req.RootID, rel)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		if fileops.IsSameOrChild(srcSc.Abs, destSc.Abs) {
			writeErr(c, http.StatusBadRequest, fmt.Errorf("cannot copy %s into itself", p))
			return
		}
		target := filepath.Join(destSc.Abs, filepath.Base(srcSc.Abs))
		info, err := os.Stat(srcSc.Abs)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		if info.IsDir() {
			err = fileops.CopyDir(srcSc.Abs, target)
		} else {
			err = fileops.CopyFile(srcSc.Abs, target)
		}
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleDelete POST /api/v1/delete（需 Delete 权限；移入回收站）。
func (s *Server) handleDelete(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil || len(req.Paths) == 0 {
		writeErr(c, http.StatusBadRequest, errors.New("rootId + paths required"))
		return
	}
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.Delete }, "delete") {
			return
		}
	}
	u := currentUser(c)
	items := make([]store.TrashItem, 0, len(req.Paths))
	for _, p := range req.Paths {
		rel := fileops.CleanRel(p)
		sc, err := s.scope(c, req.RootID, rel)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		// 授权基座本身（家目录/jail 根）不可删——0.7.0 修复：旧判定只防物理根，
		// jailed 用户可删掉自己的 jail 基座（含家目录）。
		if sc.Abs == sc.Root.Path || sc.Abs == sc.BaseAbs {
			writeErr(c, http.StatusBadRequest, errors.New("cannot delete assigned root/home itself"))
			return
		}
		info, err := os.Stat(sc.Abs)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		trashID := fileops.RandomHex(8)
		// 0.7.0 需求3：有家目录的用户回收站实体进 <home>/.fmtrash/，无家目录回退中央目录
		dst := s.trashBlobPath(u, trashID)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		unlock := s.Locks.Lock(transfer.Key(sc.PhysID, sc.Phys))
		err = fileops.MovePath(sc.Abs, dst)
		unlock()
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		// 回收站条目按物理键存储（restore 时经 scope 反查显示路径）
		item := store.TrashItem{
			TrashID: trashID, UserID: u.ID, RootID: sc.PhysID,
			RelPath: sc.Phys, Name: info.Name(), IsDir: info.IsDir(),
			Size: info.Size(), DeletedAt: time.Now(),
		}
		if err := s.Store.AddTrashItem(&item); err != nil {
			// 入库失败则把文件放回，保证一致性
			_ = fileops.MovePath(dst, sc.Abs)
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		_ = s.Store.DeleteFileMeta(u.ID, sc.PhysID, sc.Phys)
		items = append(items, item)
	}
	// 响应转显示键（客户端以 trashId 操作，rootId/path 仅展示）
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		rid, disp, ok := s.displayOf(c, it.RootID, it.RelPath)
		if !ok {
			rid, disp = it.RootID, it.RelPath
		}
		out = append(out, map[string]any{
			"trashId": it.TrashID, "rootId": rid, "relPath": disp, "name": it.Name,
			"isDir": it.IsDir, "size": it.Size, "deletedAt": it.DeletedAt,
		})
	}
	s.audit(c, store.AuditFileDelete, "", 0, req.RootID, req.Paths[0], fmt.Sprintf("%d items to trash", len(items)))
	writeJSON(c, http.StatusOK, out)
}

// searchReq 扩展搜索条件（需求12：名称/类型/大小/修改时间）。
type searchReq struct {
	RootID    string   `json:"rootId"`
	Query     string   `json:"query"`
	Kinds     []string `json:"kinds"`
	SizeMin   *int64   `json:"sizeMin"`
	SizeMax   *int64   `json:"sizeMax"`
	MtimeFrom string   `json:"mtimeFrom"` // RFC3339
	MtimeTo   string   `json:"mtimeTo"`   // RFC3339
	MaxDepth  int      `json:"maxDepth"`
}

// handleSearch POST /api/v1/search（需 View 权限）。
func (s *Server) handleSearch(c *gin.Context) {
	var req searchReq
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Query == "" && len(req.Kinds) == 0 && req.SizeMin == nil && req.SizeMax == nil && req.MtimeFrom == "" && req.MtimeTo == "" {
		writeErr(c, http.StatusBadRequest, errors.New("at least one search condition required"))
		return
	}
	if !s.degradedView(c, req.RootID, "", func(d perm.Decision) bool { return d.View }, "view") {
		return
	}
	sc, err := s.scope(c, req.RootID, "")
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	view, err := s.permsBatch(c, req.RootID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	var mFrom, mTo time.Time
	if req.MtimeFrom != "" {
		if mFrom, err = time.Parse(time.RFC3339, req.MtimeFrom); err != nil {
			writeErr(c, http.StatusBadRequest, errors.New("mtimeFrom must be RFC3339"))
			return
		}
	}
	if req.MtimeTo != "" {
		if mTo, err = time.Parse(time.RFC3339, req.MtimeTo); err != nil {
			writeErr(c, http.StatusBadRequest, errors.New("mtimeTo must be RFC3339"))
			return
		}
	}
	lq := strings.ToLower(req.Query)
	kindSet := map[string]bool{}
	for _, k := range req.Kinds {
		kindSet[strings.ToLower(k)] = true
	}
	maxDepth := req.MaxDepth
	if maxDepth <= 0 || maxDepth > 16 {
		maxDepth = 8
	}
	entries := make([]FileEntry, 0, 64)
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(entries) >= 200 || depth > maxDepth {
			return
		}
		ds, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range ds {
			if strings.HasPrefix(d.Name(), ".") {
				continue
			}
			p := filepath.Join(dir, d.Name())
			info, err := d.Info()
			if err != nil {
				continue
			}
			rel := fileops.RelOf(sc.BaseAbs, p) // 显示路径（相对命中的授权基座）
			// 跳过无查看权限的子条目（目录不递归、文件不出现在结果，0.3.1 起文件同样过滤）
			if !view(rel).View {
				continue
			}
			if matchSearch(d.Name(), rel, info, lq, kindSet, req.SizeMin, req.SizeMax, mFrom, mTo) {
				entries = append(entries, sc.entry(p, info))
			}
			if d.IsDir() {
				walk(p, depth+1)
			}
		}
	}
	walk(sc.Abs, 0)
	sortEntries(entries)
	writeJSON(c, http.StatusOK, entries)
}

func matchSearch(name, rel string, info fs.FileInfo, lq string, kinds map[string]bool,
	sizeMin, sizeMax *int64, mFrom, mTo time.Time) bool {
	if lq != "" && !strings.Contains(strings.ToLower(name), lq) &&
		!strings.Contains(strings.ToLower(rel), lq) {
		return false
	}
	if len(kinds) > 0 && !kinds[fileops.KindOf(rel, info.IsDir())] {
		return false
	}
	if !info.IsDir() {
		if sizeMin != nil && info.Size() < *sizeMin {
			return false
		}
		if sizeMax != nil && info.Size() > *sizeMax {
			return false
		}
	}
	if !mFrom.IsZero() && info.ModTime().Before(mFrom) {
		return false
	}
	if !mTo.IsZero() && info.ModTime().After(mTo) {
		return false
	}
	return true
}
