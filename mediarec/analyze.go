package mediarec

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pion/rtp"
)

// Stats 是一次离线重放分析的统计结果。它的用途是**把"录制为什么坏"变成数字**，
// 而不是靠猜：帧拼装、时间戳推进、容器合规各自都有对应的字段。
type Stats struct {
	Packets    int
	PacketsPT  map[uint8]int // 按 PayloadType 计数（能看到是否混入 RTX/padding）
	SeqGaps    int
	OutOfOrder int
	Duplicates int

	MarkerPackets       int
	Frames              int // 按 marker 收齐的帧数
	PacketsPerFrameHist map[int]int

	TSFirst, TSLast uint32
	TSDeltas        map[uint32]int // 帧时间戳差分布（最常见的几个说明推进方式）
	TSBackwards     int

	PayloadMin, PayloadMax, PayloadAvg int

	// 帧首内容校验（VP8）：关键帧必须带 9d 01 2a 起始码；非关键帧首字节最低位必须是 1
	KeyFrames      int
	KeyHeaderBad   int
	InterHeaderBad int

	// 帧明细（前若干帧，用于人工核对拼装是否正确）
	FrameDetail []string

	// 写出结果
	Written         int
	CorruptDropped  uint64
	OrphanFragments uint64
	Clamped         uint64

	// 音频（Ogg）侧
	OggPages            int
	OggGranuleBackwards int
	OggEmptyPayload     int
	AudioTSDeltas       map[uint32]int
	AudioTSBackwards    int
}

func (s Stats) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "包=%d 帧=%d 写出=%d 缺包丢帧=%d 孤残片丢弃=%d\n",
		s.Packets, s.Frames, s.Written, s.CorruptDropped, s.OrphanFragments)
	fmt.Fprintf(&b, "序列号：间隙=%d 乱序=%d 重复=%d   marker 包=%d\n", s.SeqGaps, s.OutOfOrder, s.Duplicates, s.MarkerPackets)
	fmt.Fprintf(&b, "载荷字节：最小=%d 最大=%d 平均=%d\n", s.PayloadMin, s.PayloadMax, s.PayloadAvg)
	fmt.Fprintf(&b, "每帧包数分布(包数:帧数)=%s\n", histString(s.PacketsPerFrameHist, 6))
	fmt.Fprintf(&b, "时间戳：首=%d 末=%d 倒退=%d 差分布=%s\n", s.TSFirst, s.TSLast, s.TSBackwards, histString32(s.TSDeltas, 6))
	fmt.Fprintf(&b, "帧首校验：关键帧=%d 关键帧起始码错误=%d 非关键帧首字节错误=%d\n", s.KeyFrames, s.KeyHeaderBad, s.InterHeaderBad)
	if s.OggPages > 0 || len(s.AudioTSDeltas) > 0 {
		fmt.Fprintf(&b, "音频：Ogg 页=%d 颗粒度倒退=%d 空载荷包=%d 时间戳倒退=%d 差分布=%s\n",
			s.OggPages, s.OggGranuleBackwards, s.OggEmptyPayload, s.AudioTSBackwards, histString32(s.AudioTSDeltas, 6))
	}
	return b.String()
}

func histString(m map[int]int, top int) string {
	return topHist(m, top, func(k int) string { return fmt.Sprint(k) })
}
func histString32(m map[uint32]int, top int) string {
	return topHist(m, top, func(k uint32) string { return fmt.Sprint(k) })
}

func topHist[K comparable](m map[K]int, top int, fmtKey func(K) string) string {
	type kv struct {
		k K
		n int
	}
	items := make([]kv, 0, len(m))
	for k, n := range m {
		items = append(items, kv{k, n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return fmtKey(items[i].k) < fmtKey(items[j].k)
	})
	parts := make([]string, 0, top)
	for i, it := range items {
		if i >= top {
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", fmtKey(it.k), it.n))
	}
	return strings.Join(parts, " ")
}

// AnalyzeDump 读取原始 RTP 转储，做统计并**用同一套写入器**重放成 IVF/Ogg。
//
// 关键设计：重放用的是生产代码里的 IVFWriter/oggWriter，所以离线修复一旦生效，
// 线上就生效——不存在"分析器和线上实现不一致"的问题。
func AnalyzeDump(dumpPath, outPath string) (Stats, error) {
	h, packets, _, err := ReadRawDump(dumpPath)
	if err != nil {
		return Stats{}, err
	}
	s := Stats{
		Packets:             len(packets),
		PacketsPT:           map[uint8]int{},
		PacketsPerFrameHist: map[int]int{},
		TSDeltas:            map[uint32]int{},
		AudioTSDeltas:       map[uint32]int{},
		PayloadMin:          -1,
	}

	f, err := os.Create(outPath)
	if err != nil {
		return s, err
	}
	defer f.Close()

	if strings.EqualFold(h.Codec, "opus") {
		s, err := analyzeAudio(s, h, packets, f)
		_ = f.Close()
		return s, err
	}
	s, err = analyzeVideo(s, h, packets, f)
	_ = f.Close() // 先落盘，再读回校验
	if err == nil {
		frames, keys, badKey, verr := validateIVF(outPath)
		if verr != nil {
			return s, verr
		}
		s.Written = frames
		s.KeyFrames = keys
		s.KeyHeaderBad = badKey
	}
	return s, err
}

// validateIVF 读回写出的 IVF 逐帧校验：关键帧必须带 9d 01 2a 起始码。
//
// 为什么校验**磁盘上的文件**而不是在内存里再走一遍：之前的诊断循环自己按 marker
// 定帧，于是"写入器已修好"却仍报出旧结论——诊断必须看真实产物。
func validateIVF(path string) (frames, keyFrames, badKey int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(raw) < 32 || string(raw[0:4]) != "DKIF" {
		return 0, 0, 0, fmt.Errorf("不是合法 IVF 文件")
	}
	off := 32
	for off+12 <= len(raw) {
		n := int(binary.LittleEndian.Uint32(raw[off : off+4]))
		off += 12
		if n <= 0 || off+n > len(raw) {
			break
		}
		frame := raw[off : off+n]
		off += n
		frames++
		if len(frame) >= 6 && frame[0]&0x01 == 0 { // 关键帧
			keyFrames++
			if frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
				badKey++
			}
		}
	}
	return frames, keyFrames, badKey, nil
}

func analyzeVideo(s Stats, h rawDumpHeader, packets []*rtp.Packet, f *os.File) (Stats, error) {
	ivf := &IVFWriter{w: f}

	var (
		lastSeq  uint16
		haveSeq  bool
		lastTS   uint32
		haveTS   bool
		perFrame int
	)
	for _, pkt := range packets {
		s.PacketsPT[pkt.PayloadType]++

		if haveSeq {
			switch {
			case pkt.SequenceNumber == lastSeq+1:
			case seqGreater(pkt.SequenceNumber, lastSeq):
				s.SeqGaps++
			case pkt.SequenceNumber == lastSeq:
				s.Duplicates++
			default:
				s.OutOfOrder++
			}
		}
		lastSeq, haveSeq = pkt.SequenceNumber, true

		if haveTS && pkt.Timestamp != lastTS {
			delta := pkt.Timestamp - lastTS
			if pkt.Timestamp < lastTS {
				s.TSBackwards++
			} else {
				s.TSDeltas[delta]++
			}
			lastTS = pkt.Timestamp
		} else if !haveTS {
			s.TSFirst, lastTS, haveTS = pkt.Timestamp, pkt.Timestamp, true
		}
		s.TSLast = pkt.Timestamp

		n := len(pkt.Payload)
		if s.PayloadMin < 0 || n < s.PayloadMin {
			s.PayloadMin = n
		}
		if n > s.PayloadMax {
			s.PayloadMax = n
		}
		s.PayloadAvg += n

		perFrame++
		if pkt.Marker {
			s.MarkerPackets++
			s.PacketsPerFrameHist[perFrame]++
			s.Frames++
			perFrame = 0
		}

		_ = ivf.WritePacket(pkt) // 用生产实现重放
	}
	s.PayloadAvg = avg(s.PayloadAvg, s.Packets)

	s.Written = int(ivf.Frames())
	s.CorruptDropped = ivf.CorruptFrames()
	s.OrphanFragments = ivf.OrphanFragments()
	return s, ivf.Close()
}

func analyzeAudio(s Stats, h rawDumpHeader, packets []*rtp.Packet, f *os.File) (Stats, error) {
	o := newOggWriter(f, 2, 12345)
	var lastTS uint32
	haveTS := false
	_ = h
	for _, pkt := range packets {
		s.PacketsPT[pkt.PayloadType]++
		n := len(pkt.Payload)
		if s.PayloadMin < 0 || n < s.PayloadMin {
			s.PayloadMin = n
		}
		if n > s.PayloadMax {
			s.PayloadMax = n
		}
		s.PayloadAvg += n
		if n == 0 {
			s.OggEmptyPayload++
			continue
		}
		if haveTS {
			if pkt.Timestamp < lastTS {
				s.AudioTSBackwards++
			} else {
				s.AudioTSDeltas[pkt.Timestamp-lastTS]++
			}
		}
		lastTS, haveTS = pkt.Timestamp, true
		o.WritePacket(pkt.Payload, pkt.Timestamp)
		s.Frames++
	}
	s.PayloadAvg = avg(s.PayloadAvg, s.Packets)
	s.Written = s.Frames
	s.OggPages = int(o.pageSeq)
	return s, o.Close()
}

func seqGreater(a, b uint16) bool { return int16(a-b) > 0 }

func avg(sum, n int) int {
	if n == 0 {
		return 0
	}
	return sum / n
}
