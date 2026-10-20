package api

import (
	"github.com/gin-gonic/gin"

	"context"
	"errors"
	"net/http"
	"os"

	"filemanageServer/internal/ai"
	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
)

// handleAIDescribe POST /api/v1/ai/describe {rootId,path}
// 读取文件（图像等，限大小），调用 AI 生成描述并入库（记录归属、日期）。
func (s *Server) handleAIDescribe(c *gin.Context) {
	var req pathReq
	if err := decode(c, &req); err != nil || req.Path == "" {
		writeErr(c, http.StatusBadRequest, errors.New("rootId + path required"))
		return
	}
	rel := fileops.CleanRel(req.Path)
	if !s.requirePerm(c, req.RootID, rel, func(d perm.Decision) bool { return d.View }, "view") {
		return
	}
	u := currentUser(c)
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	abs := sc.Abs
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(c, http.StatusNotFound, errors.New("file not found"))
		return
	}
	kind := fileops.KindOf(rel, false)
	limitMB := s.Cfg.AI.MaxFileMB
	if int(info.Size()) > limitMB*1<<20 {
		kind = "other" // 大文件不送入模型，仅按元数据描述
	}
	var data []byte
	if kind == "image" && info.Size() <= int64(limitMB)<<20 {
		data, err = os.ReadFile(abs)
		if err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), ai.Timeout)
	defer cancel()
	text, err := s.AI.Describe(ctx, kind, info.Name(), data, ai.MimeOf(kind, info.Name()))
	if err != nil {
		writeErr(c, http.StatusBadGateway, err)
		return
	}
	// AI 描述按物理键入库，检索/展示时再换算显示键
	desc := &store.AIDescription{
		UserID: u.ID, RootID: sc.PhysID, Path: sc.Phys,
		Name: info.Name(), Kind: kind, Text: text, Model: s.AI.Name(),
	}
	if err := s.Store.SaveAIDescription(desc); err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	// 0.2.11 响应面收敛：不再转储 store 行（userId 数字与物理键不外露），
	// 客户端视图字段同 SearchHit（rootId/path 为显示键）。
	writeJSON(c, http.StatusOK, aiDescView(desc, sc.VRoot, sc.Disp))
}

// aiDescView AI 描述的用户端口视图（0.2.11：不含数字 userId 与物理键——
// 归属由凭证隐含、路径由调用方传入显示键）。
func aiDescView(d *store.AIDescription, rid, disp string) map[string]any {
	return map[string]any{
		"id": d.ID, "rootId": rid, "path": disp, "name": d.Name, "kind": d.Kind,
		"text": d.Text, "model": d.Model, "createdAt": d.CreatedAt, "updatedAt": d.UpdatedAt,
	}
}

// handleAIGetDescription GET /api/v1/ai/description?rootId=&path=
func (s *Server) handleAIGetDescription(c *gin.Context) {
	u := currentUser(c)
	q := c.Request.URL.Query()
	physRoot := ""
	if rid := q.Get("rootId"); rid != "" {
		physRoot, _ = parseVRootID(rid)
	}
	descs, err := s.Store.ListAIDescriptions(u.ID, physRoot)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	rel := fileops.CleanRel(q.Get("path"))
	for _, d := range descs {
		if rid, disp, ok := s.displayOf(c, d.RootID, d.Path); ok && disp == rel {
			writeJSON(c, http.StatusOK, aiDescView(d, rid, disp))
			return
		}
	}
	writeErr(c, http.StatusNotFound, store.ErrNotFound)
}

// handleAISearch POST /api/v1/ai/search {query} —— 按文字描述查找文件。
func (s *Server) handleAISearch(c *gin.Context) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decode(c, &req); err != nil || req.Query == "" {
		writeErr(c, http.StatusBadRequest, errors.New("query required"))
		return
	}
	u := currentUser(c)
	hits, err := s.Store.SearchDescriptions(u.ID, req.Query, 50)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		// 命中换算为客户端显示键（越出授权范围的旧记录跳过）；
		// 0.2.11 经 aiDescView 出视图（数字 userId 不外露）。
		if rid, disp, ok := s.displayOf(c, h.RootID, h.Path); ok {
			v := aiDescView(&h.AIDescription, rid, disp)
			v["score"] = h.Score
			out = append(out, v)
		}
	}
	writeJSON(c, http.StatusOK, out)
}
