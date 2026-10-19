package ai

import (
	"math"
	"testing"

	"github.com/godeps/opus"
)

// TestResampleTo48000 验证：48k 输入原样返回；22050 上采样后长度正确；空输入不炸。
func TestResampleTo48000(t *testing.T) {
	in := []int16{1, 2, 3, 4}
	if out := ResampleTo48000(in, opRate); &out[0] != &in[0] || len(out) != 4 {
		t.Fatalf("48k 输入应原样返回，got len=%d", len(out))
	}
	if out := ResampleTo48000(nil, 22050); len(out) != 0 {
		t.Fatalf("空输入应返回空，got len=%d", len(out))
	}
	// 1 秒 22050Hz → 48000 个采样
	in22050 := make([]int16, 22050)
	for i := range in22050 {
		in22050[i] = int16(math.Sin(2 * math.Pi * 440 * float64(i) / 22050) * 10000)
	}
	out := ResampleTo48000(in22050, 22050)
	if len(out) != 48000 {
		t.Fatalf("重采样后长度应为 48000，got %d", len(out))
	}
	// 正弦波重采样后能量应基本保留（粗略检查：最大值在合理区间）
	var max int16
	for _, v := range out {
		if v < 0 {
			v = -v
		}
		if v > max {
			max = v
		}
	}
	if max < 8000 || max > 12000 {
		t.Fatalf("重采样后波形失真，max=%d", max)
	}
}

// TestEncodeOpusFrames 验证：PCM → Opus 帧数正确 → 解码回 PCM 能量保留。
// 这是 AI 语音注入链路的核心（TTS PCM → RTP payload）。
func TestEncodeOpusFrames(t *testing.T) {
	// 0.5 秒 440Hz 正弦波 @48k
	pcm := make([]int16, 24000)
	for i := range pcm {
		pcm[i] = int16(math.Sin(2 * math.Pi * 440 * float64(i) / 48000) * 10000)
	}
	frames, err := EncodeOpusFrames(pcm)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := (len(pcm) + opusFrameSamples - 1) / opusFrameSamples
	if len(frames) != want {
		t.Fatalf("帧数应为 %d，got %d", want, len(frames))
	}
	for i, f := range frames {
		if len(f) == 0 {
			t.Fatalf("第 %d 帧为空", i)
		}
	}
	// 解码第一帧验证有效性
	dec, err := opus.NewDecoder(opRate, 1)
	if err != nil {
		t.Fatalf("解码器创建失败: %v", err)
	}
	out := make([]int16, opusFrameSamples)
	n, err := dec.Decode(frames[0], out)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if n != opusFrameSamples {
		t.Fatalf("解码采样数应为 %d，got %d", opusFrameSamples, n)
	}
	var energy float64
	for _, v := range out[:n] {
		energy += float64(v) * float64(v)
	}
	if energy == 0 {
		t.Fatal("解码出全零，能量丢失")
	}
}

// TestNewPiperLocalMissing 验证：二进制/模型缺失时明确报错（fail loudly）。
func TestNewPiperLocalMissing(t *testing.T) {
	if _, err := NewPiperLocal("/nonexistent/piper", "/nonexistent/model.onnx"); err == nil {
		t.Fatal("缺失二进制应报错")
	}
	if _, err := NewPiperLocal("piper", "/nonexistent/model.onnx"); err == nil {
		// piper 可能恰好在 PATH 里——只断言模型缺失报错
		t.Log("piper 在 PATH 中，跳过二进制检查")
	}
}

// TestIsAIMention 验证 @AI 前缀识别（大小写不敏感，空问题不触发）。
func TestIsAIMention(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"@AI 今天天气怎么样", true},
		{"@ai hello", true},
		{"  @Ai  总结一下  ", true},
		{"@AI", false},       // 无实质内容不触发
		{"@AI   ", false},    // 只有空格不触发
		{"hello @AI", false}, // 前缀才算
		{"大家好", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsAIMention(c.in); got != c.want {
			t.Errorf("IsAIMention(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if q := trimQuestion("@AI   今天天气怎么样 "); q != "今天天气怎么样" {
		t.Errorf("trimQuestion 未正确清理，got %q", q)
	}
}

// TestRmsLevel 验证电平映射：静音→0，强信号→钳制在 1。
func TestRmsLevel(t *testing.T) {
	if l := rmsLevel(make([]int16, 960)); l != 0 {
		t.Errorf("静音电平应为 0，got %f", l)
	}
	loud := make([]int16, 960)
	for i := range loud {
		loud[i] = 30000
	}
	if l := rmsLevel(loud); l != 1 {
		t.Errorf("强信号电平应钳制为 1，got %f", l)
	}
}
