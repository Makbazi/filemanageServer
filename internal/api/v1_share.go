package api

import (
	"github.com/gin-gonic/gin"

	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
)

// 临时分享链接（需求7）：有效期自定义、密码默认随机生成、可管理（改密码/改有效期/免密/禁用/删除）。
// 匿名访问流程：GET /s/{token} 查看信息 → 有密码则 POST /s/{token}/verify 换取
// 短期 shareToken（0.2.3：认证图随机凭证 kind=share、绝对 2h、Ref 绑定链接 token）
// → 携带 ?st= 或 Bearer 访问 files/download。

const shareTokenTTL = 2 * time.Hour

// linkView 链接对外视图（永不含密码哈希）。
// 需求14：不再输出物理 rootId/path——分享访问者（含登录用户）只能通过链接
// 本身访问，不得知悉文件的物理位置。管理端口需要物理键时用 linkViewFull。
func linkView(l *store.ShareLink) map[string]any {
	return map[string]any{
		"id": l.ID, "token": l.Token, "url": "/s/" + l.Token,
		"isDir":       l.IsDir,
		"hasPassword": l.HasPassword, "expiresAt": l.ExpiresAt,
		"disabled": l.Disabled, "createdBy": l.CreatedBy, "createdAt": l.CreatedAt,
		"expired": l.ExpiresAt != nil && l.ExpiresAt.Before(time.Now()),
	}
}

// linkViewFull 管理端口视图：含物理键（仅 AdminHandler 引擎可达）。
func linkViewFull(l *store.ShareLink) map[string]any {
	v := linkView(l)
	v["rootId"] = l.RootID
	v["path"] = l.Path
	return v
}

// handleShareCreate POST /api/v1/share/links
// body: {rootId, path, expiresAt?(RFC3339), expiresInHours?, password?, noPassword?, autoPassword?}
func (s *Server) handleShareCreate(c *gin.Context) {
	var req struct {
		RootID         string  `json:"rootId"`
		Path           string  `json:"path"`
		ExpiresAt      string  `json:"expiresAt"`
		ExpiresInHours float64 `json:"expiresInHours"`
		Password       string  `json:"password"`
		NoPassword     bool    `json:"noPassword"`
		AutoPassword   bool    `json:"autoPassword"`
	}
	if err := decode(c, &req); err != nil || req.Path == "" {
		writeErr(c, http.StatusBadRequest, errors.New("rootId + path required"))
		return
	}
	rel := fileops.CleanRel(req.Path)
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	if d := s.permsFor(c, req.RootID, rel); !d.Share {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: share"))
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	info, err := os.Stat(sc.Abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	// 过期时间：expiresAt(RFC3339) 优先，其次 expiresInHours
	var expires *time.Time
	if req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, req.ExpiresAt)
		if terr != nil {
			writeErr(c, http.StatusBadRequest, errors.New("expiresAt must be RFC3339"))
			return
		}
		expires = &t
	} else if req.ExpiresInHours > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInHours * float64(time.Hour)))
		expires = &t
	}
	// 密码策略：noPassword → 免密；显式 password → 用之；否则随机生成（默认，需求7）
	plain := ""
	hash := ""
	switch {
	case req.NoPassword:
	case req.Password != "":
		plain = req.Password
	default:
		_ = req.AutoPassword // 默认即自动生成（六位字母+数字）
		plain, err = auth.RandomPassword(6)
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	if plain != "" {
		if hash, err = bcryptHash(plain); err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	// 链接按物理键存储：匿名 /s/ 访问与管理台直查物理根，不依赖创建者会话
	link := &store.ShareLink{
		Token: fileops.RandomHex(12), RootID: sc.PhysID, Path: sc.Phys,
		IsDir: info.IsDir(), PasswordHash: hash, ExpiresAt: expires, CreatedBy: u.ID,
	}
	if err := s.Store.CreateShareLink(link); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	out := linkView(link)
	out["password"] = plain // 明文密码仅创建时返回一次
	s.audit(c, store.AuditShareCreate, "", 0, req.RootID, rel, "link "+link.Token)
	writeJSON(c, http.StatusCreated, out)
}

// handleShareList GET /api/v1/share/links → 我创建的链接。
func (s *Server) handleShareList(c *gin.Context) {
	u := currentUser(c)
	links, err := s.Store.ListShareLinks(u.ID)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, l := range links {
		v := linkView(l)
		if rid, disp, ok := s.displayOf(c, l.RootID, l.Path); ok { // 显示键换算
			v["rootId"], v["path"] = rid, disp
		}
		out = append(out, v)
	}
	writeJSON(c, http.StatusOK, out)
}

// shareByTokenOwner 取链接并校验属主（管理员可代管）。
func (s *Server) shareByTokenOwner(c *gin.Context) *store.ShareLink {
	token := c.Param("token")
	link, err := s.Store.GetShareLinkByToken(token)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return nil
	}
	u := currentUser(c)
	if u == nil || (link.CreatedBy != u.ID && u.Role != store.RoleAdmin) {
		writeErr(c, http.StatusForbidden, errors.New("not the owner of this link"))
		return nil
	}
	return link
}

// handleShareUpdate PUT /api/v1/share/links/{token}
// body: {password?, noPassword?, expiresAt?(RFC3339|null=永久), disabled?}
// expiresAt 字段缺省=不修改；null=改为永久；字符串=更新有效期。
func (s *Server) handleShareUpdate(c *gin.Context) {
	link := s.shareByTokenOwner(c)
	if link == nil {
		return
	}
	var req struct {
		Password   string          `json:"password"`
		NoPassword bool            `json:"noPassword"`
		ExpiresAt  json.RawMessage `json:"expiresAt"`
		Disabled   *bool           `json:"disabled"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if req.NoPassword {
		link.PasswordHash = ""
	} else if req.Password != "" {
		h, err := bcryptHash(req.Password)
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		link.PasswordHash = h
	}
	if len(req.ExpiresAt) > 0 {
		if string(req.ExpiresAt) == "null" {
			link.ExpiresAt = nil
		} else {
			var ts string
			if err := json.Unmarshal(req.ExpiresAt, &ts); err != nil {
				writeErr(c, http.StatusBadRequest, errors.New("expiresAt must be RFC3339 string or null"))
				return
			}
			t, terr := time.Parse(time.RFC3339, ts)
			if terr != nil {
				writeErr(c, http.StatusBadRequest, errors.New("expiresAt must be RFC3339"))
				return
			}
			link.ExpiresAt = &t
		}
	}
	if req.Disabled != nil {
		link.Disabled = *req.Disabled
	}
	link.HasPassword = link.PasswordHash != ""
	if err := s.Store.UpdateShareLink(link); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, linkView(link))
}

// handleShareDelete DELETE /api/v1/share/links/{token}
func (s *Server) handleShareDelete(c *gin.Context) {
	link := s.shareByTokenOwner(c)
	if link == nil {
		return
	}
	if err := s.Store.DeleteShareLink(link.ID); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.audit(c, store.AuditShareDelete, "", 0, link.RootID, link.Path, "link "+link.Token)
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 匿名访问 ----------

// validLink 校验链接有效性（存在/未禁用/未过期/目标根在/创建者仍具 Share 权限）。
// 创建者的 Share 权限被回收（ACL 收紧/分配解除/用户删除）后链接立即失效。
func (s *Server) validLink(c *gin.Context, token string) *store.ShareLink {
	link, err := s.Store.GetShareLinkByToken(token)
	if err != nil {
		writeErr(c, http.StatusNotFound, errors.New("share link not found"))
		return nil
	}
	if link.Disabled {
		writeErr(c, http.StatusGone, errors.New("share link disabled"))
		return nil
	}
	if link.ExpiresAt != nil && link.ExpiresAt.Before(time.Now()) {
		writeErr(c, http.StatusGone, errors.New("share link expired"))
		return nil
	}
	if s.Roots[link.RootID] == nil {
		writeErr(c, http.StatusGone, errors.New("share link target root unavailable"))
		return nil
	}
	// 创建者失去 share（ACL 收紧），或失去链接目标所在授权基座（分配回收）→ 立即断链。
	// admin 创建者不受 jail 范围约束（其链接指向物理路径，管理台维护）。
	creator, err := s.Store.GetUserByID(link.CreatedBy)
	if err != nil || !s.Perms.For(creator, link.RootID, link.Path).Share {
		writeErr(c, http.StatusGone, errors.New("share link revoked (creator lost share permission)"))
		return nil
	}
	if creator.Role != store.RoleAdmin && !s.pathInUserScope(creator, link.RootID, link.Path) {
		writeErr(c, http.StatusGone, errors.New("share link revoked (creator lost access to target)"))
		return nil
	}
	return link
}

// shareUnlocked 判断请求是否已通过密码校验（免密链接直接放行；
// 有密码链接需携带有效 shareToken：?st= 查询参数或 Bearer 头）。
func (s *Server) shareUnlocked(c *gin.Context, link *store.ShareLink) bool {
	if !link.HasPassword {
		return true
	}
	st := c.Request.URL.Query().Get("st")
	if st == "" {
		if h := c.Request.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			st = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if st == "" {
		return false
	}
	// 0.2.3：shareToken 为认证图中的随机凭证（kind=share，绝对 2h，Ref 绑定链接
	// token——同一凭证换链接即失效）；非 share 条目/登录凭证走这里一律 false。
	e, err := s.Auth.Lookup(c.Request.Context(), st)
	return err == nil && e.Kind == store.AuthKindShare && e.Ref == link.Token
}

func clientIP(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// handleShareInfo GET /s/{token} → 链接基本信息（匿名可访问）。
func (s *Server) handleShareInfo(c *gin.Context) {
	link := s.validLink(c, c.Param("token"))
	if link == nil {
		return
	}
	v := linkView(link)
	v["needPassword"] = link.HasPassword && !s.shareUnlocked(c, link)
	v["name"] = filepath.Base(link.Path)
	writeJSON(c, http.StatusOK, v)
}

// handleShareVerify POST /s/{token}/verify {password} → {shareToken}
func (s *Server) handleShareVerify(c *gin.Context) {
	token := c.Param("token")
	link := s.validLink(c, token)
	if link == nil {
		return
	}
	ip := clientIP(c)
	if s.guard.Blocked(token, ip) {
		writeErr(c, http.StatusTooManyRequests, errors.New("too many attempts, try later"))
		return
	}
	if !link.HasPassword {
		st, err := s.issueShareToken(c, token)
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		writeJSON(c, http.StatusOK, map[string]any{"shareToken": st})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("password required"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(link.PasswordHash), []byte(req.Password)); err != nil {
		s.guard.Fail(token, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("bad password"))
		return
	}
	s.guard.Reset(token, ip)
	st, err := s.issueShareToken(c, token)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"shareToken": st})
}

// issueShareToken 签发分享解锁凭证（0.2.3 去 JWT：认证图 kind=share 条目，
// 随机凭证 + Ref 绑定链接 token；绝对 2h，无滑动）。异常路径无存储时退为
// 内存条目（本实例有效、重启失效——访客重输口令即恢复，语义不劣于旧降级）。
func (s *Server) issueShareToken(c *gin.Context, linkToken string) (string, error) {
	return s.Auth.Issue(c.Request.Context(), auth.Spec{
		Kind: store.AuthKindShare, UserID: store.AuthShareUserID, Ref: linkToken,
		TTL: shareTokenTTL, Persist: s.dbReady(),
	})
}

// linkScope 返回链接目标的根配置与磁盘绝对路径。
func (s *Server) linkScope(link *store.ShareLink) (*config.RootCfg, string, error) {
	root := s.Roots[link.RootID]
	if root == nil {
		return nil, "", fmt.Errorf("unknown root %q", link.RootID)
	}
	abs, err := fileops.JoinRoot(root.Path, link.Path)
	if err != nil {
		return nil, "", err
	}
	return root, abs, nil
}

// subAbs 校验子路径不逃逸链接目录，返回磁盘绝对路径与相对链接目录的路径。
func subAbs(scopeAbs, sub string) (string, string, error) {
	sub, err := fileops.SafeRel(sub)
	if err != nil {
		return "", "", err
	}
	abs := filepath.Join(scopeAbs, filepath.FromSlash(sub))
	if abs != scopeAbs && !strings.HasPrefix(abs, scopeAbs+string(filepath.Separator)) {
		return "", "", errors.New("path escapes share scope")
	}
	if err := fileops.EnsureNoEscape(scopeAbs, abs); err != nil { // 符号链接加固
		return "", "", err
	}
	return abs, sub, nil
}

// handleShareFiles GET /s/{token}/files?path=&st=（匿名，凭链接授权）。
func (s *Server) handleShareFiles(c *gin.Context) {
	link := s.validLink(c, c.Param("token"))
	if link == nil {
		return
	}
	if !s.shareUnlocked(c, link) {
		writeErr(c, http.StatusUnauthorized, errors.New("password required"))
		return
	}
	_, scopeAbs, err := s.linkScope(link)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if !link.IsDir {
		info, err := os.Stat(scopeAbs)
		if err != nil {
			writeErr(c, http.StatusNotFound, err)
			return
		}
		e := FileEntry{Name: info.Name(), Path: "", IsDir: false,
			Size: info.Size(), Mtime: info.ModTime(), Kind: fileops.KindOf(info.Name(), false)}
		// 需求14：不输出物理 rootId——访问者只能在链接目录内活动。
		writeJSON(c, http.StatusOK, map[string]any{"path": "", "entries": []FileEntry{e}, "scope": filepath.Base(link.Path)})
		return
	}
	abs, sub, err := subAbs(scopeAbs, c.Request.URL.Query().Get("path"))
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	dirs, err := os.ReadDir(abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	entries := make([]FileEntry, 0, len(dirs))
	for _, d := range dirs {
		info, err := d.Info()
		if err != nil {
			continue
		}
		rel := sub
		if rel == "" {
			rel = d.Name()
		} else {
			rel = rel + "/" + d.Name()
		}
		entries = append(entries, FileEntry{
			Name: info.Name(), Path: rel, IsDir: info.IsDir(),
			Size: info.Size(), Mtime: info.ModTime(),
			Kind: fileops.KindOf(rel, info.IsDir()),
		})
	}
	sortEntries(entries)
	writeJSON(c, http.StatusOK, map[string]any{"path": sub, "entries": entries, "scope": filepath.Base(link.Path)})
}

// handleShareDownload GET /s/{token}/download?path=&st=&inline=1（匿名，凭链接授权）。
func (s *Server) handleShareDownload(c *gin.Context) {
	link := s.validLink(c, c.Param("token"))
	if link == nil {
		return
	}
	if !s.shareUnlocked(c, link) {
		writeErr(c, http.StatusUnauthorized, errors.New("password required"))
		return
	}
	_, scopeAbs, err := s.linkScope(link)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	var abs string
	if link.IsDir {
		abs, _, err = subAbs(scopeAbs, c.Request.URL.Query().Get("path"))
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	} else {
		abs = scopeAbs
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(c, http.StatusNotFound, errors.New("file not found"))
		return
	}
	inline := c.Request.URL.Query().Get("inline") == "1"
	s.serveContent(c, abs, filepath.Base(abs), inline)
}
