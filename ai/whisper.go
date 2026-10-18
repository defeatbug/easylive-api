package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// whisperURL 是 OpenAI 音频转写接口。
const whisperURL = "https://api.openai.com/v1/audio/transcriptions"

// STTBackend 是语音转写的后端接口：输入一段 WAV，返回转写文本。
// 无有效语音时返回空字符串（不是错误）。
type STTBackend interface {
	Transcribe(ctx context.Context, wav []byte, language string) (string, error)
}

// whisperAPI 是 OpenAI Whisper 云端接口的实现。
type whisperAPI struct {
	apiKey string
	model  string
}

// NewWhisperAPI 创建云端 Whisper 后端。
func NewWhisperAPI(apiKey, model string) STTBackend {
	return &whisperAPI{apiKey: apiKey, model: model}
}

// Transcribe 把一段 WAV 音频发给 Whisper，返回转写文本。
// 调用方用带超时的 ctx 控制。
func (w *whisperAPI) Transcribe(ctx context.Context, wav []byte, language string) (string, error) {
	if len(wav) == 0 {
		return "", nil
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("whisper: 构造表单失败: %w", err)
	}
	if _, err := fw.Write(wav); err != nil {
		return "", fmt.Errorf("whisper: 写入音频失败: %w", err)
	}
	_ = mw.WriteField("model", w.model)
	if language != "" {
		_ = mw.WriteField("language", language)
	}
	_ = mw.WriteField("response_format", "json")
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("whisper: 关闭表单失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, whisperURL, &body)
	if err != nil {
		return "", fmt.Errorf("whisper: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("whisper: 解析响应失败: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
