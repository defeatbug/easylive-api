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

// Transcribe 把一段 WAV 音频发给 Whisper，返回转写文本。
// 无有效语音时返回空字符串（不是错误）。调用方用带超时的 ctx 控制。
func Transcribe(ctx context.Context, apiKey, model string, wav []byte, language string) (string, error) {
	if len(wav) == 0 {
		return "", nil
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("whisper: 构造表单失败: %w", err)
	}
	if _, err := fw.Write(wav); err != nil {
		return "", fmt.Errorf("whisper: 写入音频失败: %w", err)
	}
	_ = w.WriteField("model", model)
	if language != "" {
		_ = w.WriteField("language", language)
	}
	_ = w.WriteField("response_format", "json")
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("whisper: 关闭表单失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, whisperURL, &body)
	if err != nil {
		return "", fmt.Errorf("whisper: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

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
