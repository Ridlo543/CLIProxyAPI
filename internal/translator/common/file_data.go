package common

import (
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
)

// NormalizeOpenAIFileData returns the MIME type and raw base64 payload for OpenAI file content.
func NormalizeOpenAIFileData(filename, fallbackMIMEType, fileData string) (mimeType, data string, ok bool) {
	if fileData == "" {
		return "", "", false
	}

	if fallbackMIMEType == "" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
		fallbackMIMEType = misc.MimeTypes[ext]
	}
	const dataURLPrefix = "data:"
	if len(fileData) < len(dataURLPrefix) || !strings.EqualFold(fileData[:len(dataURLPrefix)], dataURLPrefix) {
		if fallbackMIMEType == "" {
			return "", "", false
		}
		return fallbackMIMEType, fileData, true
	}

	metadata, payload, found := strings.Cut(fileData[len(dataURLPrefix):], ",")
	if !found || payload == "" {
		return "", "", false
	}
	fields := strings.Split(metadata, ";")
	mimeType = strings.TrimSpace(fields[0])
	if mimeType == "" {
		return "", "", false
	}
	for _, field := range fields[1:] {
		if strings.EqualFold(strings.TrimSpace(field), "base64") {
			return mimeType, payload, true
		}
	}
	return "", "", false
}

// ExtractMultimodalImagePayload extracts mimeType and base64 data from diverse image part representations:
// - OpenAI: {"type":"image_url", "image_url":{"url":"data:..."}} or {"type":"image_url", "image_url":"data:..."} or {"url":"data:..."}
// - Responses: {"type":"input_image", "image_url":"data:..."}
// - Anthropic: {"type":"image", "source":{"type":"base64", "media_type":"image/png", "data":"..."}}
// - Direct/OMP: {"type":"image", "data":"...", "mimeType":"image/png"} or {"mime_type":"image/png"}
func ExtractMultimodalImagePayload(part ojson.Result) (mimeType, data string, ok bool) {
	if !part.Exists() || !part.IsObject() {
		return "", "", false
	}

	// Case 1: Anthropic source block: {"source": {"type": "base64", "media_type": "...", "data": "..."}}
	if source := part.Get("source"); source.Exists() && strings.EqualFold(source.Get("type").String(), "base64") {
		d := strings.TrimSpace(source.Get("data").String())
		m := strings.TrimSpace(source.Get("media_type").String())
		if d != "" {
			if m == "" {
				m = "image/png"
			}
			return m, d, true
		}
	}

	// Case 2: Direct data & mimeType fields: {"data": "...", "mimeType": "..."} or {"mime_type": "..."}
	if d := strings.TrimSpace(part.Get("data").String()); d != "" {
		if strings.HasPrefix(strings.ToLower(d), "data:") {
			return NormalizeOpenAIFileData("", "", d)
		}
		m := strings.TrimSpace(part.Get("mimeType").String())
		if m == "" {
			m = strings.TrimSpace(part.Get("mime_type").String())
		}
		if m == "" {
			m = "image/png"
		}
		return m, d, true
	}

	// Case 3: URL fields (image_url.url object, image_url string, url string)
	url := strings.TrimSpace(part.Get("image_url.url").String())
	if url == "" {
		url = strings.TrimSpace(part.Get("image_url").String())
	}
	if url == "" {
		url = strings.TrimSpace(part.Get("url").String())
	}
	if url != "" {
		return NormalizeOpenAIFileData("", "", url)
	}

	// Case 4: File object
	if fileData := part.Get("file_data").String(); fileData != "" {
		filename := part.Get("filename").String()
		return NormalizeOpenAIFileData(filename, "", fileData)
	}
	if fileData := part.Get("file.file_data").String(); fileData != "" {
		filename := part.Get("file.filename").String()
		return NormalizeOpenAIFileData(filename, "", fileData)
	}

	return "", "", false
}
