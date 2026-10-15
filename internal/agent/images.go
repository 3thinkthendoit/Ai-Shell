package agent

// 用户附图：仅当前轮进入 LLM 请求，不落审计、不进会话历史。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"ai-shell/internal/llm"
)

const (
	maxImageBytes   = 4 << 20 // 4 MiB 解码后上限
	maxImagesPerAsk = 3
)

// ImageIn 是前端传来的一张图（mime + base64，可带 data: 前缀）。
type ImageIn struct {
	Mime string `json:"mime"`
	Data string `json:"data"`
}

// ParseImagesJSON 解析 Ask 的 imagesJSON；空串表示无图。
func ParseImagesJSON(raw string) ([]ImageIn, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil, nil
	}
	var imgs []ImageIn
	if err := json.Unmarshal([]byte(raw), &imgs); err != nil {
		return nil, fmt.Errorf("图片参数不是合法 JSON 数组: %w", err)
	}
	return NormalizeImages(imgs)
}

// NormalizeImages 校验张数、mime 白名单、魔数与体积，并剥掉 data URL 前缀。
func NormalizeImages(imgs []ImageIn) ([]ImageIn, error) {
	if len(imgs) == 0 {
		return nil, nil
	}
	if len(imgs) > maxImagesPerAsk {
		return nil, fmt.Errorf("一次最多附 %d 张图片", maxImagesPerAsk)
	}
	out := make([]ImageIn, 0, len(imgs))
	for i, img := range imgs {
		declared, b64, err := normalizeOneImage(img)
		if err != nil {
			return nil, fmt.Errorf("图片[%d]: %w", i, err)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
			if err != nil {
				return nil, fmt.Errorf("图片[%d]: base64 无效", i)
			}
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("图片[%d]: 内容为空", i)
		}
		if len(raw) > maxImageBytes {
			return nil, fmt.Errorf("图片[%d]: 超过 4MB 上限（解码后 %d 字节）", i, len(raw))
		}
		sniffed, ok := sniffImageMIME(raw)
		if !ok {
			return nil, fmt.Errorf("图片[%d]: 不是可识别的 PNG/JPEG/WebP/GIF", i)
		}
		if declared != "" && declared != sniffed {
			return nil, fmt.Errorf("图片[%d]: 声明类型 %s 与文件内容 %s 不符", i, declared, sniffed)
		}
		out = append(out, ImageIn{Mime: sniffed, Data: base64.StdEncoding.EncodeToString(raw)})
	}
	return out, nil
}

func normalizeOneImage(img ImageIn) (mime, b64 string, err error) {
	mime = strings.TrimSpace(strings.ToLower(img.Mime))
	data := strings.TrimSpace(img.Data)
	if data == "" {
		return "", "", fmt.Errorf("缺少数据")
	}
	if strings.HasPrefix(data, "data:") {
		rest := strings.TrimPrefix(data, "data:")
		semi := strings.Index(rest, ";")
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return "", "", fmt.Errorf("data URL 格式无效")
		}
		if mime == "" {
			if semi > 0 && semi < comma {
				mime = strings.ToLower(strings.TrimSpace(rest[:semi]))
			} else {
				mime = strings.ToLower(strings.TrimSpace(rest[:comma]))
			}
		}
		b64 = rest[comma+1:]
	} else {
		b64 = data
	}
	if mime != "" {
		canon, ok := canonicalizeImageMIME(mime)
		if !ok {
			return "", "", fmt.Errorf("仅接受 image/png、image/jpeg、image/webp、image/gif，收到 %q", mime)
		}
		mime = canon
	}
	b64 = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, b64)
	return mime, b64, nil
}

// canonicalizeImageMIME 只放行安全的位图类型；拒绝 svg 等可携带脚本的格式。
func canonicalizeImageMIME(mime string) (string, bool) {
	mime = strings.TrimSpace(strings.ToLower(mime))
	// 拒绝参数尾（image/png;foo=bar）与路径噪声。
	if i := strings.IndexAny(mime, ";, \t"); i >= 0 {
		mime = mime[:i]
	}
	switch mime {
	case "image/png":
		return "image/png", true
	case "image/jpeg", "image/jpg":
		return "image/jpeg", true
	case "image/webp":
		return "image/webp", true
	case "image/gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func sniffImageMIME(raw []byte) (string, bool) {
	switch {
	case len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png", true
	case len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff:
		return "image/jpeg", true
	case len(raw) >= 6 && (bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a"))):
		return "image/gif", true
	case len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp", true
	default:
		return "", false
	}
}

// UserMessageWithImages 构造当前用户轮消息：有图则 Parts 多模态，历史只存 Content 文本。
func UserMessageWithImages(text string, imgs []ImageIn) llm.Message {
	text = strings.TrimSpace(text)
	if len(imgs) == 0 {
		return llm.Message{Role: "user", Content: text}
	}
	parts := make([]llm.ContentPart, 0, 1+len(imgs))
	if text == "" {
		text = "（见附图）"
	}
	parts = append(parts, llm.ContentPart{Type: "text", Text: text})
	for _, img := range imgs {
		parts = append(parts, llm.ContentPart{
			Type: "image_url",
			ImageURL: &llm.ImageURL{
				URL: "data:" + img.Mime + ";base64," + img.Data,
			},
		})
	}
	return llm.Message{Role: "user", Content: text, Parts: parts}
}
