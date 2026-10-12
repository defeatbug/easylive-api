package mediarec

import (
	"encoding/binary"
	"io"
)

// oggWriter 写一个最小的 Ogg Opus 流（RFC 7845 的必需部分）。
//
// 为什么自己做而不是引第三方库：Opus over RTP 的每个包就是一个 Opus 包，
// 需要的只是「OpusHead + OpusTags + 数据页」三样东西，代码不到 200 行；
// 换来的是产物能被 ffmpeg / VLC / 浏览器直接播放，且服务端零依赖。
type oggWriter struct {
	w      io.Writer
	serial uint32

	// 一个页里最多攒这么多包再落页（约 1 秒 @20ms），减少页头开销
	buf     [][]byte
	granule uint64

	preSkip   uint16
	channels  uint8
	firstTS   uint32
	haveFirst bool
	sentHead  bool

	pageSeq uint32
	err     error
}

const (
	oggPreSkipDefault = 312 // 常见编码器的预跳过样本数（48kHz）
	opusFrameSamples  = 960 // 20ms @48kHz
	oggMaxPerPage     = 50  // 约 1 秒
)

// newOggWriter 写好 OpusHead 与 OpusTags（它们各占一个页）。
func newOggWriter(w io.Writer, channels uint8, serial uint32) *oggWriter {
	if channels == 0 {
		channels = 2
	}
	o := &oggWriter{w: w, serial: serial, channels: channels, preSkip: oggPreSkipDefault}

	head := make([]byte, 0, 19)
	head = append(head, "OpusHead"...)
	head = append(head, 1) // version
	head = append(head, channels)
	var tmp [8]byte
	binary.LittleEndian.PutUint16(tmp[0:2], o.preSkip)
	binary.LittleEndian.PutUint32(tmp[2:6], 48000) // 原始采样率（仅供参考）
	binary.LittleEndian.PutUint16(tmp[6:8], 0)     // 输出增益
	head = append(head, tmp[:]...)
	head = append(head, 0)     // 声道映射表（0 = 单流）
	o.writePage(head, 0, 0x02) // BOS

	tags := append([]byte("OpusTags"), []byte("easylive-api")...)
	tags = append(tags, make([]byte, 4)...) // 0 条注释
	o.writePage(tags, 0, 0)
	o.sentHead = true
	return o
}

// WritePacket 收集一个 Opus 包；攒够一页或超时（Flush）时落页。
func (o *oggWriter) WritePacket(pkt []byte, ts uint32) {
	if o.err != nil {
		return
	}
	if !o.haveFirst {
		o.firstTS, o.haveFirst = ts, true
	}
	// granule 是"到这一包结束为止的总样本数"，用 RTP 时间戳差直接换算（48kHz 时钟）
	o.granule = uint64(ts-o.firstTS) + uint64(o.preSkip) + opusFrameSamples
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	o.buf = append(o.buf, cp)
	if len(o.buf) >= oggMaxPerPage {
		o.Flush()
	}
}

// Flush 把已攒的包写成一个数据页。
func (o *oggWriter) Flush() {
	if len(o.buf) == 0 || o.err != nil {
		return
	}
	o.writePageMulti(o.buf, o.granule, 0)
	o.buf = o.buf[:0]
}

// Close 落最后一页并标记流结束（EOS）。
func (o *oggWriter) Close() error {
	if o.err != nil {
		return o.err
	}
	if len(o.buf) > 0 {
		o.writePageMulti(o.buf, o.granule, 0x04)
		o.buf = o.buf[:0]
	} else {
		// 没有待写数据时，发一个**不含任何段**的 EOS 页。
		// 曾经这里写成 writePage(nil,...)，那会产生"一个长度为 0 的包"——
		// 空包不是合法 Opus 包，ffmpeg 会直接报
		// "Packet processing failed: Invalid data found"，整条流 demux 失败。
		o.writePageMulti(nil, o.granule, 0x04)
	}
	return o.err
}

func (o *oggWriter) writePage(payload []byte, granule uint64, headerType byte) {
	o.writePageMulti([][]byte{payload}, granule, headerType)
}

// writePageMulti 按 Ogg 规范组装一页：27 字节页头 + 段表 + 数据，最后回填 CRC32。
func (o *oggWriter) writePageMulti(packets [][]byte, granule uint64, headerType byte) {
	if o.err != nil {
		return
	}
	// 段表：每 255 字节一段，包的结束用 <255 的段长标记
	segs := make([]byte, 0, len(packets)+2)
	total := 0
	for _, p := range packets {
		n := len(p)
		total += n
		for n >= 255 {
			segs = append(segs, 255)
			n -= 255
		}
		segs = append(segs, byte(n))
	}
	if len(segs) > 255 {
		// 极端情况：一页段数超限。截断处理（丢弃本页），不让录制崩掉。
		return
	}

	page := make([]byte, 0, 27+len(segs)+total)
	hdr := make([]byte, 27)
	copy(hdr[0:4], "OggS")
	hdr[4] = 0          // version
	hdr[5] = headerType // 0x02=BOS, 0x04=EOS
	binary.LittleEndian.PutUint64(hdr[6:14], granule)
	binary.LittleEndian.PutUint32(hdr[14:18], o.serial)
	binary.LittleEndian.PutUint32(hdr[18:22], o.pageSeq)
	// hdr[22:26] 是 CRC，稍后回填
	hdr[26] = byte(len(segs))
	page = append(page, hdr...)
	page = append(page, segs...)
	for _, p := range packets {
		page = append(page, p...)
	}
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
	if _, err := o.w.Write(page); err != nil {
		o.err = err
		return
	}
	o.pageSeq++
}

// oggCRC 是 Ogg 使用的 CRC32：多项式 0x04c11db7，**不反射**、初值 0、无最终异或。
// 与常见的 IEEE CRC32 不同，用错了播放器会直接拒绝整个流。
var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
