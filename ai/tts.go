package ai

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/godeps/opus"
)

// TTSBackend 是语音合成后端（AI 问答助手"说"的部分）。
//
// 对标 STTBackend 的可插拔设计：piper（本地 Piper CLI，默认，零调用成本）
// 与未来的云端 API 共用这个接口。Synthesize 返回 int16 PCM，方便后续重采样与编码。
type TTSBackend interface {
	// Synthesize 把文本合成语音，返回采样率见 SampleRate() 的 mono int16 PCM。
	Synthesize(ctx context.Context, text string) ([]int16, error)
	// SampleRate 返回 Synthesize 输出 PCM 的采样率（Hz）。
	SampleRate() int
	Close() error
}

// piperSampleRate 是 Piper --output-raw 的输出采样率（Hz，mono）。
// 注意：这是 Piper 的默认值；若换用其他采样率的语音模型，需同步改这里。
const piperSampleRate = 22050

// piperLocal 是本地 Piper TTS 的实现：stdin 传文本 → CLI → stdout 读 raw PCM。
//
// 为什么用 CLI 而不是常驻 server：问答是低频事件（一次几秒），CLI 启动开销
// （约 0.3 秒）相对合成可忽略；省掉 server 进程的保活与端口管理。
// 与 whisperLocal 同一哲学。
type piperLocal struct {
	bin   string // piper 可执行文件路径
	model string // 语音模型 .onnx 路径
}

var _ TTSBackend = (*piperLocal)(nil)

// NewPiperLocal 创建本地 Piper 后端。bin/model 不存在时返回错误，
// 让调用方在启动/Ask 时明确拒绝，而不是等到合成时才炸（学 NewWhisperLocal）。
func NewPiperLocal(bin, model string) (TTSBackend, error) {
	if bin == "" {
		bin = "piper"
	}
	if _, err := exec.LookPath(bin); err != nil {
		if _, serr := os.Stat(bin); serr != nil {
			return nil, fmt.Errorf("找不到 piper 可执行文件 %q（PIPER_BIN）：%v", bin, err)
		}
	}
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("找不到 piper 语音模型 %q（PIPER_MODEL_PATH）：%v", model, err)
	}
	return &piperLocal{bin: bin, model: model}, nil
}

func (p *piperLocal) SampleRate() int { return piperSampleRate }

func (p *piperLocal) Close() error { return nil } // CLI 模式无常驻资源

// Synthesize 调 piper CLI 合成。空文本直接返回空（Piper 对空输入行为不稳定）。
func (p *piperLocal) Synthesize(ctx context.Context, text string) ([]int16, error) {
	if text == "" {
		return nil, nil
	}
	// 30 秒硬超时：正常几十个字的合成 < 3 秒，超时说明卡住了
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, p.bin,
		"--model", p.model,
		"--output-raw", // stdout 输出 raw int16 PCM（22050Hz mono）
	)
	cmd.Stdin = strings.NewReader(text)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("piper-local: 合成失败: %w（输出: %s）", err, truncate(string(raw), 200))
	}
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("piper-local: PCM 长度非偶数（%d 字节），输出损坏？", len(raw))
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(raw[2*i]) | int16(raw[2*i+1])<<8 // 小端
	}
	return pcm, nil
}

// ResampleTo48000 把 mono int16 PCM 线性插值重采样到 48kHz（WebRTC Opus 固定采样率）。
//
// 线性插值对语音足够：22050→48000 是上采样，无混叠风险；质量损失远小于
// Opus 有损编码本身。不用引入重采样库，保持零依赖。
func ResampleTo48000(in []int16, fromRate int) []int16 {
	if len(in) == 0 || fromRate == opRate {
		return in
	}
	outLen := int(int64(len(in)) * opRate / int64(fromRate))
	out := make([]int16, outLen)
	ratio := float64(fromRate) / float64(opRate)
	for i := range out {
		pos := float64(i) * ratio
		idx := int(pos)
		frac := pos - float64(idx)
		if idx+1 >= len(in) {
			out[i] = in[len(in)-1]
			continue
		}
		// 线性插值（int32 中间值防溢出）
		v := int32(in[idx]) + int32(float64(int32(in[idx+1])-int32(in[idx]))*frac)
		out[i] = int16(v)
	}
	return out
}

// opusFrameSamples 是 20ms @48kHz 的采样数（WebRTC 音频标准帧长）。
const opusFrameSamples = 960

// EncodeOpusFrames 把 48kHz mono int16 PCM 切成 20ms 帧并 Opus 编码。
// 返回每帧的 payload（调用方自行组 RTP 包头：序号/时间戳/SSRC）。
//
// 不足一帧的尾巴补零：语音末尾多 20ms 静音无感知，比"丢掉尾巴"更保真。
func EncodeOpusFrames(pcm []int16) ([][]byte, error) {
	enc, err := opus.NewEncoder(opRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus 编码器创建失败: %w", err)
	}
	frames := (len(pcm) + opusFrameSamples - 1) / opusFrameSamples
	out := make([][]byte, 0, frames)
	buf := make([]byte, 4000) // Opus 单帧上限远小于此
	tmp := make([]int16, opusFrameSamples)
	for i := 0; i < frames; i++ {
		start := i * opusFrameSamples
		end := start + opusFrameSamples
		if end > len(pcm) {
			copy(tmp, pcm[start:])
			for j := len(pcm) - start; j < opusFrameSamples; j++ {
				tmp[j] = 0
			}
		} else {
			copy(tmp, pcm[start:end])
		}
		n, err := enc.Encode(tmp, buf)
		if err != nil {
			return nil, fmt.Errorf("opus 编码第 %d 帧失败: %w", i, err)
		}
		frame := make([]byte, n)
		copy(frame, buf[:n])
		out = append(out, frame)
	}
	return out, nil
}
