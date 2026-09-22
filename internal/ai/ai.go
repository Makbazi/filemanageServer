// Package ai 提供 AI 能力抽象：对文件/图像生成中文描述。
// Provider 接口 + none/openai/ollama 三个实现（接口化设计，可扩展）。
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"filemanageServer/internal/config"
)

// Provider 是 AI 能力接口。Describe 的 data 可为空（如视频或超大文件仅按名描述）。
type Provider interface {
	Name() string
	Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error)
}

// NewProvider 按配置构造 Provider。
func NewProvider(cfg config.AICfg) Provider {
	switch cfg.Provider {
	case "openai":
		return &openaiProvider{cfg: cfg}
	case "ollama":
		return &ollamaProvider{cfg: cfg}
	default:
		return noneProvider{}
	}
}

type noneProvider struct{}

func (noneProvider) Name() string { return "none" }
func (noneProvider) Describe(context.Context, string, string, []byte, string) (string, error) {
	return "", errors.New("AI 未配置：请在服务端 config 的 ai 字段中设置 provider=openai|ollama")
}

// ---------- prompt / mime 工具 ----------

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

// MimeOf 按类型/文件名推断 MIME。
func MimeOf(kind, name string) string {
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

func trimText(s string) string {
	return string(bytes.TrimSpace([]byte(s)))
}

// ---------- openai 兼容协议 ----------

type openaiProvider struct{ cfg config.AICfg }

func (p *openaiProvider) Name() string { return "openai:" + p.cfg.Model }

func (p *openaiProvider) Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
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

type ollamaProvider struct{ cfg config.AICfg }

func (p *ollamaProvider) Name() string { return "ollama:" + p.cfg.Model }

func (p *ollamaProvider) Describe(ctx context.Context, kind, name string, data []byte, mime string) (string, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	msg := map[string]any{"role": "user", "content": promptFor(kind, name)}
	if len(data) > 0 && kind == "image" {
		msg["images"] = []string{base64.StdEncoding.EncodeToString(data)}
	}
	payload := map[string]any{"model": p.cfg.Model, "messages": []any{msg}, "stream": false}
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

// Timeout 是 AI 调用的统一超时。
const Timeout = 120 * time.Second
