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
	// frameStart / frameParts：帧内**按序列号装配**的部件表。
	//
	// 为什么不能"按到达顺序追加"：真实链路上会重复（15 秒里 7 个）和乱序（2 个）到达，
	// 顺序追加会把重复包算两次、把乱序包放到错误位置，整帧因此损坏——离线分析显示
	// 6 个关键帧里有 4 个起始码被拼坏，这正是画面花屏与解码报错的来源。
	frameStart      uint16
	frameParts      map[uint16][]byte
	corrupt         uint64 // 因缺包被丢弃的整帧数（可观测）
	duplicates      uint64 // 帧内重复包数（可观测）
	orphanFragments uint64 // 起始分片已丢、只剩中间残片而被丢弃的包数（可观测）
	outOfOrder      uint64 // 时间戳倒退而被丢弃的整帧数（可观测）
	lastPTS         uint32
	havePTS         bool
	// baseTS 是**第一个真正写出的帧**的 RTP 时间戳；写入的 PTS 一律减去它，从 0 起算。
	//
	// 为什么必须归零：RTP 时间戳是随机的 32 位值（实测约 1.09e9），IVF 时间基是 1/90000，
	// 于是 PTS 约 3.6 小时；再转封装成 WebM/MP4（毫秒时间基）会**溢出**，
	// 表现为合流后时长为 9223372036854 这种垃圾值。归零只改起点、不改帧间隔，
	// 所以节奏与总时长都不受影响。
	baseTS   uint32
	haveBase bool
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
	// IVF 头的 rate/scale 决定每个时间刻度多长：timebase = scale/rate 秒。
	// 我们的 PTS 直接用 RTP 的 90kHz 时间戳，所以必须写成 rate=90000、scale=1（timebase=1/90000）。
	//
	// 曾经写反（rate=1、scale=90000），后果很隐蔽：ffprobe 报 r_frame_rate=1/90000，
	// 单轨转封装成 WebM 后时长会变成 720000 秒这种垃圾值（8 帧 × 90000 秒）；
	// 而**合流**时会重新计时，看起来反而正常。是靠实测转封装链路才抓出来的。
	binary.LittleEndian.PutUint32(hdr[16:20], 90000) // rate
	binary.LittleEndian.PutUint32(hdr[20:24], 1)     // scale
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
	// **帧的起点由描述符的 S 位决定，不是 marker**。
	//
	// 离线分析真实抓包后发现：浏览器把大帧分片成多个 RTP 包，而**分片丢失后，
	// 剩下的中间分片仍然带 marker 位**。只按 marker 定帧，就会把"帧中间的残片"
	// 当成完整帧写出去（表现为 0xaa a3 b6... 这种垃圾帧与 "Invalid profile 5"）。
	// S=1 表示"这是分区的开始"=一帧的起点；S=0 说明它属于前面某一帧。
	startFrame := func() {
		v.frameTS = pkt.Timestamp
		v.frameStart = pkt.SequenceNumber
		v.inFrame = true
		if v.frameParts == nil {
			v.frameParts = make(map[uint16][]byte, 8)
		}
		for k := range v.frameParts {
			delete(v.frameParts, k)
		}
	}
	switch {
	case !v.inFrame:
		if v.depay.S != 1 {
			// 起始分片没收到，这只是一帧的中间残片：不能当成帧
			v.orphanFragments++
			return nil
		}
		startFrame()
	case v.depay.S == 1:
		// 上一帧还没等到 marker，新帧的起始包就到了 → 上一帧残缺，丢弃重开
		v.corrupt++
		startFrame()
	}
	if _, dup := v.frameParts[pkt.SequenceNumber]; dup {
		v.duplicates++ // 重复包只保留一份（真实链路上 15 秒就有 7 个）
	} else {
		part := make([]byte, len(payload))
		copy(part, payload)
		v.frameParts[pkt.SequenceNumber] = part
	}
	if !pkt.Marker {
		return nil // 这一帧还没收完
	}
	v.inFrame = false

	// 按序列号顺序装配整帧；[帧起始序号..marker 序号] 区间内缺任何一个包，整帧丢弃。
	// 之前的实现是"按到达顺序追加"，重复包会被算两次、乱序包会落到错误位置，
	// 离线分析显示 6 个关键帧里有 4 个起始码被拼坏——这就是画面花屏/解码报错的来源。
	v.frameBuf = v.frameBuf[:0]
	complete := true
	for seq := v.frameStart; ; seq++ {
		part, ok := v.frameParts[seq]
		if !ok {
			complete = false
			break
		}
		v.frameBuf = append(v.frameBuf, part...)
		if seq == pkt.SequenceNumber {
			break
		}
	}
	if !complete {
		v.corrupt++ // 缺包：宁可少一帧，也不给播放器喂残缺数据
		return nil
	}

	// 时间戳倒退的整帧丢弃。
	// 真实链路上会有帧起始包乱序到达（15 秒里 2 次），若照写会让 PTS 倒退，
	// ffmpeg 报 "non monotonically increasing dts" 并可能丢帧。
	// 这类帧内容虽完整，但保持时间线单调更重要——代价约 0.7%。
	// 必须**严格**递增：相等也会被 ffmpeg 判为 non monotonic dts 并告警
	if v.havePTS && v.frameTS <= v.lastPTS {
		v.outOfOrder++
		return nil
	}
	v.lastPTS, v.havePTS = v.frameTS, true

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

	// 注意：这里曾尝试把"倒退/跳变"的时间戳钳制成单调递增，但实测 393 帧里触发了 391 次，
	// 说明触发条件本身就没被理解清楚（比原问题更糟）。已撤下，改由后续专门的排查解决：
	// 需要先把一条轨道的原始 RTP 落盘做离线分析，弄清浏览器侧时间戳的真实推进方式。
	// 当前行为：原样写入 RTP 的 90kHz 时间戳。
	ts := v.frameTS
	if !v.haveBase {
		v.baseTS, v.haveBase = ts, true
	}
	rec := make([]byte, 12)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(v.frameBuf)))
	binary.LittleEndian.PutUint64(rec[4:12], uint64(ts-v.baseTS))
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

// CorruptFrames 返回因丢包被丢弃的残缺帧数（可观测，而不是悄悄丢）。
func (v *IVFWriter) CorruptFrames() uint64 { return v.corrupt }

// DuplicatePackets 返回帧内被去重的重复包数（真实链路一定会出现）。
func (v *IVFWriter) DuplicatePackets() uint64 { return v.duplicates }

// OrphanFragments 返回"起始分片丢失、只剩中间残片"而被丢弃的包数。
func (v *IVFWriter) OrphanFragments() uint64 { return v.orphanFragments }

// OutOfOrderFrames 返回因时间戳倒退被丢弃的整帧数。
func (v *IVFWriter) OutOfOrderFrames() uint64 { return v.outOfOrder }

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
