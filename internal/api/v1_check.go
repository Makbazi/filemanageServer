package api

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/fileops"
)

// handleCheck POST /api/v1/check（0.2.1，加性端点，需求清单第 2 条：目录
// 重传的"相同文件校验"预检）。auth 组（登录 + requireDB + maintenanceGate）。
//
// body {rootId, files:[{path, size, hash?}]}（≤1000，客户端 500/批——同
// mkdirs 骨架）。逐路径 permsBatch Upload + scope jail 校验（任一不过整请求
// 4xx 零判定，客户端可安全整体重试）。响应 {results:[{path,state}]}，state：
//
//	missing  远端不存在 → 客户端按新文件上传（baseHash "-"）
//	differ   存在且确定内容不同（size 不符，或存哈希与请求哈希不符）
//	same     存在、size 相符、存哈希与请求哈希一致且 meta 新鲜（size/mtime
//	         秒级与磁盘一致——防绕库直改磁盘后 meta 过期误判）
//	unknown  无法廉价断定（请求未带 hash / 无存哈希 / meta 过期）——
//	         客户端二选一：本地已算 hash 的复查（pass2）后仍 unknown，按
//	         覆盖语义上传（服务端覆盖前存版本，需求3 兜底）
//
// 存哈希来源：上传完成/整传/回滚时 recordMeta 写入的 file_metas.content_hash
// （0.2.1 起真正落库；例行刷行按 size/mtime 保留，见 PutFileMeta）。
// hash 比对大小写不敏感（客户端与服务端同为小写 hex，防御式 EqualFold）。
func (s *Server) handleCheck(c *gin.Context) {
	var req struct {
		RootID string `json:"rootId"`
		Files  []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
			Hash string `json:"hash"`
		} `json:"files"`
	}
	if err := decode(c, &req); err != nil || req.RootID == "" || len(req.Files) == 0 || len(req.Files) > maxMkdirPaths {
		writeErr(c, http.StatusBadRequest, errors.New("rootId + files[1..1000] required"))
		return
	}
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	dec, err := s.permsBatch(c, req.RootID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	type result struct {
		Path  string `json:"path"`
		State string `json:"state"`
	}
	results := make([]result, 0, len(req.Files))
	for _, f := range req.Files {
		rel := fileops.CleanRel(f.Path)
		if !dec(rel).Upload {
			writeErr(c, http.StatusForbidden, errors.New("permission denied: upload(check) on "+rel))
			return
		}
		sc, err := s.scope(c, req.RootID, rel)
		if err != nil {
			writeErr(c, statusOf(err), err) // jail 逃逸/保留名/未知根 → 4xx 整请求
			return
		}
		results = append(results, result{Path: f.Path, State: s.checkOne(u.ID, sc, f.Size, f.Hash)})
	}
	writeJSON(c, http.StatusOK, map[string]any{"results": results})
}

// checkOne 单条判定（详见 handleCheck 注释的状态语义）。
func (s *Server) checkOne(userID int64, sc *scopeInfo, wantSize int64, wantHash string) string {
	const (
		stMissing = "missing"
		stSame    = "same"
		stDiffer  = "differ"
		stUnknown = "unknown"
	)
	info, err := os.Stat(sc.Abs)
	if err != nil {
		return stMissing // 不存在或不可读：按缺失处理（上传路径自会再校验）
	}
	if info.IsDir() || info.Size() != wantSize {
		return stDiffer // 目录占位/大小不符 = 确定不同（弹窗按"同名冲突"处置）
	}
	if wantHash == "" {
		return stUnknown // pass1：size 相符但客户端尚未提供内容哈希
	}
	meta, merr := s.Store.GetFileMeta(userID, sc.PhysID, sc.Phys)
	if merr != nil || meta == nil || strings.TrimSpace(meta.ContentHash) == "" {
		return stUnknown // 服务端无存哈希（历史上传未记录）
	}
	if !strings.EqualFold(meta.ContentHash, wantHash) {
		return stDiffer
	}
	// meta 新鲜性：哈希相同还必须 size/mtime（秒）与磁盘一致——覆盖"绕过上传
	// 通道原地改文件"的情形（mtime 变了而 size 未变），宁可 unknown 不误判 same。
	if meta.Size != wantSize || meta.Mtime.Unix() != info.ModTime().Unix() {
		return stUnknown
	}
	return stSame
}
