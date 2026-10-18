package ai

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godeps/opus"
	"github.com/pion/rtp"
)

const (
	opRate      = 48000 // WebRTC Opus 固定 48kHz
	whisperRate = 16000 // Whisper 最优输入采样率
	// maxFrameSamples 是单次 Decode 的 PCM 缓冲（120ms @48k，覆盖所有合法 Opus 帧长）
	maxFrameSamples = 5760
	// silenceThreshold 是静音判定门限（int16 RMS）：正常说话通常 >1000，环境噪声 <200
	silenceThreshold = 400
	// silencePacketsToCut 是连续静音包数达到多少就切分片（约 4 秒，按 20ms/包估算）
	silencePacketsToCut = 200
	// minChunkSeconds 是送 Whisper 的最小分片时长：太短的语音没有上下文，转写质量差还浪费一次调用
	minChunkSeconds = 3
)

// Segment 是一段转写结果。
type Segment struct {
	Speaker   string
	StartedAt time.Time
	Text      string
}

// Transcriber 消费一条音频轨道的 Opus RTP 包，解码成 PCM，按"静音切分 / 时长上限"
// 分片后调 Whisper 转写。实现 sfu.PacketSink——Write 必须非阻塞。
type Transcriber struct {
	speaker   string
	onSegment func(Segment)

	ch   chan []byte // Opus payload 队列；满则丢包（计数），绝不卡住转发热路径
	done chan struct{}
	wg   sync.WaitGroup

	dropped    atomic.Uint64
	chunkSeconds int
	stt          STTBackend
	lang         string
}

// NewTranscriber 创建一条轨道的转写器并启动后台协程。
func NewTranscriber(speaker string, chunkSeconds int, stt STTBackend, lang string, onSegment func(Segment)) *Transcriber {
	t := &Transcriber{
		speaker:      speaker,
		onSegment:    onSegment,
		ch:           make(chan []byte, 512),
		done:         make(chan struct{}),
		chunkSeconds: chunkSeconds,
		stt:          stt,
		lang:         lang,
	}
	t.wg.Add(1)
	go t.loop()
	return t
}

// Write 实现 sfu.PacketSink：只拷贝 payload 入队，队列满直接丢弃并计数。
func (t *Transcriber) Write(pkt *rtp.Packet) {
	if pkt == nil || len(pkt.Payload) == 0 {
		return
	}
	cp := make([]byte, len(pkt.Payload))
	copy(cp, pkt.Payload)
	select {
	case t.ch <- cp:
	default:
		t.dropped.Add(1)
	}
}

// Stop 停止转写器：把缓冲里剩下的音频做最后一次转写，然后返回。
func (t *Transcriber) Stop() {
	close(t.done)
	t.wg.Wait()
	if d := t.dropped.Load(); d > 0 {
		log.Printf("ai: 转写器 %s 丢包 %d（队列满，转发不受影响）", t.speaker, d)
	}
}

func (t *Transcriber) loop() {
	defer t.wg.Done()
	dec, err := opus.NewDecoder(opRate, 1)
	if err != nil {
		log.Printf("ai: 创建 Opus 解码器失败 %s: %v", t.speaker, err)
		return
	}

	var pcm48 []int16 // 当前分片累积的 48kHz PCM
	var chunkStart time.Time
	var silencePackets int

	flush := func() {
		start := chunkStart
		chunkStart = time.Time{}
		silencePackets = 0
		if len(pcm48) < opRate*minChunkSeconds {
			pcm48 = nil
			return
		}
		wav := encodeWAV(downsample16k(pcm48))
		pcm48 = nil
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		text, err := t.stt.Transcribe(ctx, wav, t.lang)
		cancel()
		if err != nil {
			log.Printf("ai: whisper 转写失败 %s: %v", t.speaker, err)
			return
		}
		if text == "" {
			return
		}
		t.onSegment(Segment{Speaker: t.speaker, StartedAt: start, Text: text})
	}

	pcm := make([]int16, maxFrameSamples)
	for {
		select {
		case <-t.done:
			flush()
			return
		case payload := <-t.ch:
			if chunkStart.IsZero() {
				chunkStart = time.Now()
			}
			n, err := dec.Decode(payload, pcm)
			if err != nil || n <= 0 {
				continue // 坏包跳过；不做 PLC，丢一包对转写无影响
			}
			frame := pcm[:n]
			if rms(frame) < silenceThreshold {
				silencePackets++
			} else {
				silencePackets = 0
			}
			pcm48 = append(pcm48, frame...)
			durSec := len(pcm48) / opRate
			// 切分条件：达到分片时长上限，或"说了几句话后静音 4 秒"，或兜底 120 秒
			if durSec >= t.chunkSeconds || (silencePackets >= silencePacketsToCut && durSec >= minChunkSeconds) || durSec >= 120 {
				flush()
			}
		}
	}
}

// rms 计算一帧 PCM 的均方根（能量），用于静音检测。
func rms(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum int64
	for _, v := range pcm {
		sum += int64(v) * int64(v)
	}
	return math.Sqrt(float64(sum) / float64(len(pcm)))
}

// downsample16k 把 48kHz 单声道 PCM 抽稀到 16kHz（每 3 个取 1 个；语音转写够用）。
func downsample16k(pcm []int16) []int16 {
	out := make([]int16, 0, len(pcm)/3)
	for i := 0; i < len(pcm); i += 3 {
		out = append(out, pcm[i])
	}
	return out
}

// encodeWAV 编码 16-bit 单声道 16kHz WAV（Whisper 可直接消费）。
func encodeWAV(pcm []int16) []byte {
	var buf bytes.Buffer
	dataLen := len(pcm) * 2
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(whisperRate))
	binary.Write(&buf, binary.LittleEndian, uint32(whisperRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))             // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))            // bits per sample
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	binary.Write(&buf, binary.LittleEndian, pcm)
	return buf.Bytes()
}
