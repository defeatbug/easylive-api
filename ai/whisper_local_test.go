package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestWhisperLocalBackend 用真实音频验证本地转写链路。
// 需要环境变量：WHISPER_BIN、WHISPER_MODEL_PATH、WHISPER_TEST_WAV。
// 未设置时跳过（CI 无模型）。
func TestWhisperLocalBackend(t *testing.T) {
	bin := os.Getenv("WHISPER_BIN")
	model := os.Getenv("WHISPER_MODEL_PATH")
	wavPath := os.Getenv("WHISPER_TEST_WAV")
	if bin == "" || model == "" || wavPath == "" {
		t.Skip("缺 WHISPER_BIN / WHISPER_MODEL_PATH / WHISPER_TEST_WAV，跳过")
	}
	be, err := NewWhisperLocal(bin, model)
	if err != nil {
		t.Fatalf("NewWhisperLocal: %v", err)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("读测试音频: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	text, err := be.Transcribe(ctx, wav, "en")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("转写结果: %s", text)
	if text == "" {
		t.Fatal("转写结果为空")
	}
}
