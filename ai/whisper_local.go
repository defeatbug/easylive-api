package ai

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// whisperLocal 是本地 whisper.cpp 的实现：每个分片写临时 WAV，调 CLI 转写。
//
// 为什么用 CLI 而不是常驻 server：分片每 30~60 秒才来一次，CLI 启动开销（约 0.5 秒）
// 相对转写本身可忽略；省掉了 server 进程的保活、健康检查与端口管理。真到高并发再换 server 模式。
type whisperLocal struct {
	bin   string // whisper-cli 可执行文件路径
	model string // 模型文件路径（ggml-*.bin）
}

// NewWhisperLocal 创建本地 Whisper 后端。bin/model 不存在时返回错误，
// 让 Manager.Start 直接拒绝并给出明确原因，而不是等到转写时才炸。
func NewWhisperLocal(bin, model string) (STTBackend, error) {
	if bin == "" {
		bin = "whisper-cli"
	}
	if _, err := exec.LookPath(bin); err != nil {
		// 允许绝对路径
		if _, serr := os.Stat(bin); serr != nil {
			return nil, fmt.Errorf("找不到 whisper 可执行文件 %q（WHISPER_BIN）：%v", bin, err)
		}
	}
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("找不到 whisper 模型文件 %q（WHISPER_MODEL_PATH）：%v", model, err)
	}
	return &whisperLocal{bin: bin, model: model}, nil
}

// Transcribe 写临时 WAV → 调 whisper-cli → 读回 txt 结果。
func (w *whisperLocal) Transcribe(ctx context.Context, wav []byte, language string) (string, error) {
	if len(wav) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "ai-stt-*")
	if err != nil {
		return "", fmt.Errorf("whisper-local: 建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)

	wavPath := filepath.Join(dir, "chunk.wav")
	if err := os.WriteFile(wavPath, wav, 0o600); err != nil {
		return "", fmt.Errorf("whisper-local: 写 WAV 失败: %w", err)
	}

	// --output-txt 会在同目录生成 chunk.wav.txt
	args := []string{
		"-m", w.model,
		"-f", wavPath,
		"--output-txt",
		"--no-timestamps",
		"-t", "4", // 4 线程：转写 30 秒音频约 5~10 秒（small 模型，现代 CPU）
	}
	if language != "" {
		args = append(args, "-l", language)
	}
	// 60 秒硬超时：转写 30 秒音频正常 < 20 秒，超时说明卡住了
	tctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, w.bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper-local: 转写失败: %w（输出: %s）", err, truncate(string(out), 300))
	}
	txt, err := os.ReadFile(wavPath + ".txt")
	if err != nil {
		return "", fmt.Errorf("whisper-local: 读结果失败: %w", err)
	}
	return strings.TrimSpace(string(txt)), nil
}
