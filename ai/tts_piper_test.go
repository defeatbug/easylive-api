package ai

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/godeps/opus"
)

// TestPiperLocalBackend 用真实 Piper 验证"合成 → 重采样 → Opus 编码"全链路。
// 需要环境变量：PIPER_BIN、PIPER_MODEL_PATH。未设置时跳过（CI 无模型）。
func TestPiperLocalBackend(t *testing.T) {
	bin := os.Getenv("PIPER_BIN")
	model := os.Getenv("PIPER_MODEL_PATH")
	if bin == "" || model == "" {
		t.Skip("缺 PIPER_BIN / PIPER_MODEL_PATH，跳过")
	}
	be, err := NewPiperLocal(bin, model)
	if err != nil {
		t.Fatalf("NewPiperLocal: %v", err)
	}
	defer be.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pcm, err := be.Synthesize(ctx, "你好，我是会议 AI 助手")
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	if len(pcm) < be.SampleRate() { // 至少 1 秒音频
		t.Fatalf("合成音频过短：%d 采样", len(pcm))
	}
	t.Logf("合成 %d 采样 @%dHz", len(pcm), be.SampleRate())

	pcm48 := ResampleTo48000(pcm, be.SampleRate())
	if len(pcm48) < opRate {
		t.Fatalf("重采样后过短：%d", len(pcm48))
	}
	frames, err := EncodeOpusFrames(pcm48)
	if err != nil {
		t.Fatalf("Opus 编码失败: %v", err)
	}
	t.Logf("编码 %d 帧（20ms/帧）", len(frames))

	// 解码首帧验证：能量非零
	dec, err := opus.NewDecoder(opRate, 1)
	if err != nil {
		t.Fatalf("解码器创建失败: %v", err)
	}
	out := make([]int16, opusFrameSamples)
	n, err := dec.Decode(frames[0], out)
	if err != nil || n == 0 {
		t.Fatalf("解码失败: n=%d err=%v", n, err)
	}
	var energy float64
	for _, v := range out[:n] {
		energy += float64(v) * float64(v)
	}
	if energy == 0 {
		t.Fatal("解码全零，能量丢失")
	}
}
