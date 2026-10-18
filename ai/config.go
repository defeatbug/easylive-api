// Package ai 实现 AI 会议总结：订阅房间音频 → Whisper 转写 → DeepSeek 生成 markdown 纪要。
//
// 链路：sfu.PublishedTrack 的音频包（PacketSink tap）→ Opus 解码（godeps/opus，
// WASM 版 libopus，无需 cgo，与 CGO_ENABLED=0 的构建约束兼容）→ 按静音/时长分片 →
// Whisper API 转写 → 转写片段累积 → 定时调 DeepSeek 生成 markdown → 信令推送给客户端。
package ai

import (
	"os"
	"strconv"
	"time"
)

// Config 是 AI 总结的配置，全部来自环境变量。
type Config struct {
	// OpenAIAPIKey 供 Whisper 转写用（OPENAI_API_KEY）
	OpenAIAPIKey string
	// DeepSeekAPIKey 供纪要生成用（DEEPSEEK_API_KEY）
	DeepSeekAPIKey string
	// DeepSeekModel 默认 deepseek-chat（DEEPSEEK_MODEL）
	DeepSeekModel string
	// WhisperModel 默认 whisper-1（WHISPER_MODEL）
	WhisperModel string
	// WhisperLang 转写语言，默认 zh（WHISPER_LANG）
	WhisperLang string
	// SummaryInterval 纪要刷新周期，默认 60s（AI_SUMMARY_INTERVAL 秒）
	SummaryInterval time.Duration
	// ChunkSeconds 单个转写分片的最大音频时长（秒），默认 30（AI_CHUNK_SECONDS）
	ChunkSeconds int
}

// Ready 当且仅当两个 API Key 都配好。
func (c Config) Ready() bool { return c.OpenAIAPIKey != "" && c.DeepSeekAPIKey != "" }

// ConfigFromEnv 从环境变量读取配置。Key 缺失不报错——Ready() 为 false 时
// 启动会话会明确拒绝，日志里也会说明缺了哪个。
func ConfigFromEnv() Config {
	c := Config{
		OpenAIAPIKey:   os.Getenv("OPENAI_API_KEY"),
		DeepSeekAPIKey: os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:  envOr("DEEPSEEK_MODEL", "deepseek-chat"),
		WhisperModel:   envOr("WHISPER_MODEL", "whisper-1"),
		WhisperLang:    envOr("WHISPER_LANG", "zh"),
		SummaryInterval: 60 * time.Second,
		ChunkSeconds:    30,
	}
	if v := os.Getenv("AI_SUMMARY_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 15 {
			c.SummaryInterval = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("AI_CHUNK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 && n <= 120 {
			c.ChunkSeconds = n
		}
	}
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
