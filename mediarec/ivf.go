// Package mediarec 把 SFU 转发的 RTP **原样落盘**，实现"转发即录制、零转码"。
//
// 设计取向（与整个项目一致）：
//   - 不解码、不转码：录制只是把已经在转发的包多写一份到磁盘，CPU 成本接近零；
//   - **绝不阻塞转发**：每条轨道一个后台写协程 + 有界队列，队列满就丢帧并计数
//     （宁可少录几帧，也不能让参会者的画面卡住——这是本项目的铁律）；
//   - 不自造私有格式：视频写标准 **IVF**（VP8 的原生封装，ffmpeg/VLC/浏览器都能直接播），
//     音频下一轮写 Ogg Opus。
package mediarec

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Codec 是录制一条轨道所需的编解码参数（来自 SDP 协商与 RTP 头）。
type Codec struct {
	Name        string // "VP8" / "opus"
	ClockRate   uint32 // 视频 90000，Opus 48000
	PayloadType uint8
	SSRC        uint32
}

// IVFWriter 写 VP8 的 IVF 封装。
//
// 格式极简：32 字节文件头 + 每帧「4 字节长度 + 8 字节时间戳 + 载荷」。
// 时间基取 1/90000，于是 RTP 的 90kHz 时间戳可以**原样**写进去，不需要重采样。
type IVFWriter struct {
	w        io.Writer
	frames   uint32
	width    uint16
	height   uint16
	wroteHdr bool

	// 关键帧之前的包不能写：文件从中间帧开始的话，播放器解不出任何画面。
	seenKeyframe bool
	// inFrame 表示"当前正在累积一帧"（由 marker 位翻转为 false）
	inFrame bool
	// VP8 分片重组：一帧可能被拆成多个 RTP 包，靠 marker 位判断结束。
	depay    codecs.VP8Packet
	frameBuf []byte
	frameTS  uint32
}

func (v *IVFWriter) header(w io.Writer) error {
	hdr := make([]byte, 32)
	copy(hdr[0:4], "DKIF")
	binary.LittleEndian.PutUint16(hdr[4:6], 0)  // version
	binary.LittleEndian.PutUint16(hdr[6:8], 32) // header length
	copy(hdr[8:12], "VP80")                     // fourcc
	binary.LittleEndian.PutUint16(hdr[12:14], v.width)
	binary.LittleEndian.PutUint16(hdr[14:16], v.height)
	binary.LittleEndian.PutUint32(hdr[16:20], 1)     // timebase numerator
	binary.LittleEndian.PutUint32(hdr[20:24], 90000) // timebase denominator（RTP 时钟）
	binary.LittleEndian.PutUint32(hdr[24:28], 0)     // frame count（先占位）
	binary.LittleEndian.PutUint32(hdr[28:32], 0)     // unused
	_, err := w.Write(hdr)
	return err
}

// WritePacket 接收一个 RTP 包并（在完整帧就绪时）写出一帧。
func (v *IVFWriter) WritePacket(pkt *rtp.Packet) error {
	payload, err := v.depay.Unmarshal(pkt.Payload)
	if err != nil {
		return nil // 分片/畸形包：跳过而不是中断录制
	}
	// 帧边界用 RTP 的 marker 位判断，而不是 VP8 descriptor 的 S 位：
	// S 只表示"分区的开始"，多分区编码一帧会有多个 S（会把一帧拆错），
	// 而 marker 位就是 RTP 层的"这一帧到此结束"，语义唯一、可靠。
	if !v.inFrame {
		v.frameBuf = v.frameBuf[:0]
		v.frameTS = pkt.Timestamp
		v.inFrame = true
	}
	v.frameBuf = append(v.frameBuf, payload...)
	if !pkt.Marker {
		return nil // 这一帧还没收完
	}
	v.inFrame = false

	isKey := len(v.frameBuf) >= 10 && v.frameBuf[0]&0x01 == 0
	if !v.seenKeyframe {
		if !isKey {
			return nil // 关键帧之前的数据一律不写，否则文件开头无法解码
		}
		v.seenKeyframe = true
		if w, h, ok := vp8Dimensions(v.frameBuf); ok {
			v.width, v.height = w, h
		}
		if !v.wroteHdr {
			if err := v.header(v.w); err != nil {
				return err
			}
			v.wroteHdr = true
		}
	}

	rec := make([]byte, 12)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(v.frameBuf)))
	binary.LittleEndian.PutUint64(rec[4:12], uint64(v.frameTS))
	if _, err := v.w.Write(rec); err != nil {
		return err
	}
	if _, err := v.w.Write(v.frameBuf); err != nil {
		return err
	}
	v.frames++
	return nil
}

// Close 回填帧数（IVF 头部第 24 字节起的 frame count）。
//
// 需要能随机写回；调用方传进来的若是只写流，这里会静默跳过（播放器通常仍能读）。
func (v *IVFWriter) Close() error {
	if !v.wroteHdr {
		return nil
	}
	sw, ok := v.w.(io.WriteSeeker)
	if !ok {
		return nil
	}
	if _, err := sw.Seek(24, io.SeekStart); err != nil {
		return err
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v.frames)
	_, err := sw.Write(buf[:])
	return err
}

// Frames 返回已写出的帧数（测试与日志用）。
func (v *IVFWriter) Frames() uint32 { return v.frames }

// Dimensions 返回解析出的分辨率（0,0 表示还没拿到关键帧）。
func (v *IVFWriter) Dimensions() (uint16, uint16) { return v.width, v.height }

// vp8Dimensions 从 VP8 关键帧的未压缩数据里取宽高。
//
// 位置是 VP8 规范固定的：3 字节帧标签 + 3 字节起始码(0x9d 0x01 0x2a) 之后，
// 依次是 2 字节宽、2 字节高（各取低 14 位）。
func vp8Dimensions(frame []byte) (uint16, uint16, bool) {
	if len(frame) < 10 || frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return 0, 0, false
	}
	w := uint16(frame[6]) | uint16(frame[7])<<8
	h := uint16(frame[8]) | uint16(frame[9])<<8
	return w & 0x3fff, h & 0x3fff, true
}

// 便于日志与错误信息
func (c Codec) String() string {
	return fmt.Sprintf("%s(pt=%d,ssrc=%d,%dHz)", c.Name, c.PayloadType, c.SSRC, c.ClockRate)
}
