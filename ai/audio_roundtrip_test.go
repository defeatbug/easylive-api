package ai

import (
	"math"
	"testing"

	"github.com/godeps/opus"
)

// TestOpusRoundtrip 验证：PCM → Opus 编码 → 解码 → PCM，链路通且能量基本保留。
func TestOpusRoundtrip(t *testing.T) {
	enc, err := opus.NewEncoder(opRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	dec, err := opus.NewDecoder(opRate, 1)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	// 20ms 440Hz 正弦波
	frame := make([]int16, 960)
	for i := range frame {
		frame[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/opRate))
	}
	opusBuf := make([]byte, 2000)
	n, err := enc.Encode(frame, opusBuf)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pcm := make([]int16, maxFrameSamples)
	dn, err := dec.Decode(opusBuf[:n], pcm)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dn != 960 {
		t.Fatalf("decoded %d samples, want 960", dn)
	}
	if r := rms(pcm[:dn]); r < 3000 {
		t.Fatalf("decoded RMS too low: %f (input ~7071)", r)
	}
	// WAV 编码 sanity
	wav := encodeWAV(downsample16k(pcm[:dn]))
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" {
		t.Fatalf("bad wav header")
	}
	// 16kHz 下 20ms = 320 采样 = 640 字节 + 44 头
	if len(wav) != 44+320*2 {
		t.Fatalf("wav size %d, want %d", len(wav), 44+320*2)
	}
	t.Logf("roundtrip OK: decoded=%d samples wav=%d bytes", dn, len(wav))
}
