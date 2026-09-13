package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	filemanagedb "filemanageDb"
)

// AIProvider 是预留的 AI 能力接口：对文件/图像/视频生成文字描述。
// 目前提供 openai 兼容协议（云厂商或本地网关）与 ollama 两种实现，
// 后续可扩展 embedding 向量检索（filemanageDb 已预留 Embedding 字段）。
type AIProvider interface {
	Name() string
	// Describe 根据文件内容/元数据生成中文描述。data 可为空（如视频或超大文件仅按名描述）。
	Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error)
}

func NewAIProvider(cfg AICfg) AIProvider {
	switch cfg.Provider {
	case "openai":
		return &openaiProvider{cfg: cfg}
	case "ollama":
		return &ollamaProvider{cfg: cfg}
	default:
		return &noneProvider{}
	}
}

type noneProvider struct{}

func (noneProvider) Name() string { return "none" }
func (noneProvider) Describe(context.Context, string, string, []byte, string) (string, error) {
	return "", errors.New("AI 未配置：请在服务端 config 的 ai 字段中设置 provider=openai|ollama")
}

// ---------- prompt ----------

func promptFor(kind, name string) string {
	switch kind {
	case "image":
		return fmt.Sprintf("请观察这张图片（文件名：%s），用一句中文描述它的内容、主体与场景。只输出描述本身。", name)
	case "video":
		return fmt.Sprintf("这是一个视频文件（文件名：%s，含首帧图像）。请用一句中文描述其可能内容。只输出描述本身。", name)
	case "audio":
		return fmt.Sprintf("这是一个音频文件（文件名：%s）。请用一句中文说明它可能的内容类型。只输出描述本身。", name)
	default:
		return fmt.Sprintf("这是一个 %s 类型的文件（文件名：%s）。请用一句中文根据文件名推断其内容。只输出描述本身。", kind, name)
	}
}

func mimeOf(kind, name string) string {
	switch kind {
	case "image":
		switch extOf(name) {
		case "png":
			return "image/png"
		case "gif":
			return "image/gif"
		case "webp":
			return "image/webp"
		case "bmp":
			return "image/bmp"
		case "svg":
			return "image/svg+xml"
		default:
			return "image/jpeg"
		}
	case "video":
		return "video/" + defaultIfEmpty(extOf(name), "mp4")
	case "audio":
		return "audio/" + defaultIfEmpty(extOf(name), "mp3")
	}
	return "application/octet-stream"
}

func extOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	return ext[1:]
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------- openai-compatible ----------

type openaiProvider struct{ cfg AICfg }

func (p *openaiProvider) Name() string { return "openai:" + p.cfg.Model }

func (p *openaiProvider) Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	// 多模态：图像以 data URL 形式附带；其余仅按名称/类型文字描述
	userContent := any(promptFor(kind, name))
	if len(data) > 0 && kind == "image" {
		userContent = []any{
			map[string]any{"type": "text", "text": promptFor(kind, name)},
			map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data)),
				},
			},
		}
	}
	payload := map[string]any{
		"model": p.cfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": "你是文件管理器中的智能归档助手，用简洁中文描述文件。"},
			{"role": "user", "content": userContent},
		},
		"max_tokens": 200,
	}
	return p.post(ctx, base, payload)
}

func (p *openaiProvider) post(ctx context.Context, base string, payload map[string]any) (string, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai request: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai error %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("ai bad response: %s", string(b))
	}
	return trimText(out.Choices[0].Message.Content), nil
}

// ---------- ollama ----------

type ollamaProvider struct{ cfg AICfg }

func (p *ollamaProvider) Name() string { return "ollama:" + p.cfg.Model }

func (p *ollamaProvider) Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	msg := map[string]any{
		"role":    "user",
		"content": promptFor(kind, name),
	}
	if len(data) > 0 && kind == "image" {
		msg["images"] = []string{base64.StdEncoding.EncodeToString(data)}
	}
	payload := map[string]any{
		"model":    p.cfg.Model,
		"messages": []any{msg},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("ollama bad response: %s", string(b))
	}
	return trimText(out.Message.Content), nil
}

func trimText(s string) string {
	return string(bytes.TrimSpace([]byte(s)))
}

// ---------- AI handlers ----------

// handleAIDescribe POST /api/v1/ai/describe {rootId,path}
// 读取文件（图像等，限大小），调用 AI 生成描述并入库（记录归属、日期）。
func (s *server) handleAIDescribe(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("rootId + path required"))
		return
	}
	u := currentUser(r)
	root, abs, err := s.resolve(r, req.RootID, req.Path)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, errors.New("file not found"))
		return
	}
	kind := kindOf(req.Path, false)
	limitMB := s.cfg.AI.MaxFileMB
	if int(info.Size()) > limitMB*1<<20 {
		// 大文件（如视频）不送入模型，仅按元数据描述
		kind = "other"
	}
	var data []byte
	if kind == "image" && info.Size() <= int64(limitMB)<<20 {
		data, err = os.ReadFile(abs)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	text, err := s.ai.Describe(ctx, kind, info.Name(), data, mimeOf(kind, info.Name()))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	rel := relOf(root, abs)
	desc := &filemanagedb.AIDescription{
		UserID: u.ID, RootID: req.RootID, Path: rel, Name: info.Name(),
		Kind: kind, Text: text, Model: s.ai.Name(),
	}
	if err := s.store.SaveAIDescription(desc); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, desc)
}

// handleAIGetDescription GET /api/v1/ai/description?rootId=&path=
func (s *server) handleAIGetDescription(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	descs, err := s.store.ListAIDescriptions(u.ID, q.Get("rootId"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rel := cleanRel(q.Get("path"))
	for _, d := range descs {
		if d.Path == rel {
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	writeErr(w, http.StatusNotFound, filemanagedb.ErrNotFound)
}

// handleAISearch POST /api/v1/ai/search {query} —— 根据用户文字描述查找文件（预留）。
func (s *server) handleAISearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decode(r, &req); err != nil || req.Query == "" {
		writeErr(w, http.StatusBadRequest, errors.New("query required"))
		return
	}
	u := currentUser(r)
	hits, err := s.store.SearchDescriptions(u.ID, req.Query, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if hits == nil {
		hits = []*filemanagedb.SearchHit{}
	}
	writeJSON(w, http.StatusOK, hits)
}
