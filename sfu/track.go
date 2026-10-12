package sfu

import (
	"log"
	"math"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"easylive-api/mediarec"
	"easylive-api/protocol"
)

// 音频电平相关常量。
//
// 阈值 0.02 约等于 -34 dBov：安静环境下正常说话在 0.03—0.3，
// 环境噪声一般低于 0.01，取这个量级可以做到"说话亮、呼吸不亮"。
const (
	audioLevelAlpha     = 0.3                    // 指数平滑系数：单包电平抖得厉害，直接用会让高亮疯狂闪烁
	audioLevelThreshold = 0.02                   // 判定"正在说话"的门槛
	audioLevelHold      = 600 * time.Millisecond // 超过这么久没收到包就认为已停止说话
)

// 从协商结果里找音频电平头扩展的 ID（RFC 6464）
var extmapAudioLevelRe = regexp.MustCompile(`a=extmap:(\d+)[^\r\n]*` + regexp.QuoteMeta(sdp.AudioLevelURI))

// audioLevelExtID 从发布者连接的远端 SDP 里解析音频电平扩展 ID；0 表示未协商。
//
// 为什么要从 SDP 里抠：Pion 的 TrackRemote 不直接暴露扩展 ID，
// 而每个发布者的 ID 都是各自协商的，必须在协商完成后按连接取。
func audioLevelExtID(pc *webrtc.PeerConnection) uint8 {
	if pc == nil {
		return 0
	}
	desc := pc.RemoteDescription()
	if desc == nil {
		return 0
	}
	m := extmapAudioLevelRe.FindStringSubmatch(desc.SDP)
	if len(m) < 2 {
		return 0
	}
	id, err := strconv.Atoi(m[1])
	if err != nil || id <= 0 || id > 14 {
		return 0
	}
	return uint8(id)
}

// PublishedTrack 是一条已发布的轨道：远端（发布者）→ 本地（分发给订阅者）。
//
// 转发模型：一条轨道对应**一个** TrackLocalStaticRTP，把它 AddTrack 到每个订阅者的
// 订阅者连接上，Pion 会把 WriteRTP 的包扇出给所有绑定（这是 Pion broadcast 示例的标准做法）。
// 因此转发路径上没有额外的每订阅者缓冲与拷贝，符合"只转发不转码不混流"的约束。
type PublishedTrack struct {
	info   *protocol.TrackInfo
	local  *webrtc.TrackLocalStaticRTP
	remote *webrtc.TrackRemote
	pubPC  *webrtc.PeerConnection // 用于回 PLI，向发布者索要关键帧

	// onEnd 在转发循环结束时回调（轨道被取消发布 / 发布者断开），
	// 由 Room 登记，用于从房间移除该轨道并广播 track_unpublished。
	onEnd func(*PublishedTrack)

	packets    atomic.Uint64
	bytes      atomic.Uint64
	writeFails atomic.Uint64
	dropped    atomic.Uint64 // 因静音/关闭而被丢弃的包数（评测时能量化"静音省了多少带宽"）

	// muted 用原子量而不是直接改 info.Muted：转发热路径会读它，
	// 直接读写 info 会与信令协程构成数据竞争。
	muted atomic.Bool

	// rec 是这条视频轨的录制写入器（未开启录制时为 nil）。
	// 它的 Write 是**非阻塞**的：队列满就丢帧，绝不能拖住转发。
	rec *mediarec.Writer

	// 音频电平（活跃说话人检测用）。float64 拆成位存进 atomic，避免给热路径加锁。
	audioLevelExtID uint8
	audioLevelBits  atomic.Uint64
	audioLevelAt    atomic.Int64 // 最后一次更新时间（UnixMilli）
}

func newPublishedTrack(publisherID, source string, remote *webrtc.TrackRemote, pubPC *webrtc.PeerConnection) (*PublishedTrack, error) {
	kind := protocol.KindAudio
	if remote.Kind() == webrtc.RTPCodecTypeVideo {
		kind = protocol.KindVideo
	}

	// 轨道 id 用「发布者/源/ssrc」保证唯一；streamID 由客户端决定（客户端用 streamID 声明摄像头/屏幕）
	id := publisherID + "/" + source + "/" + remote.StreamID() + "/" + strconv.FormatUint(uint64(remote.SSRC()), 10)

	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, id, remote.StreamID())
	if err != nil {
		return nil, err
	}

	return &PublishedTrack{
		info: &protocol.TrackInfo{
			ID:          id,
			PublisherID: publisherID,
			Kind:        kind,
			Source:      source,
		},
		local:           local,
		remote:          remote,
		pubPC:           pubPC,
		audioLevelExtID: audioLevelExtID(pubPC),
	}, nil
}

// observeAudioLevel 从 RTP 包头扩展里取电平并做指数平滑。
// 只在转发热路径上调用，因此不分配内存、不加锁。
func (t *PublishedTrack) observeAudioLevel(pkt *rtp.Packet) {
	if t.audioLevelExtID == 0 || !pkt.Header.Extension {
		return
	}
	raw := pkt.Header.GetExtension(t.audioLevelExtID)
	if len(raw) == 0 {
		return
	}
	// RFC 6464：1 字节，最高位 V 表示"该帧包含人声"，低 7 位是 -dBov
	dbov := float64(raw[0] & 0x7f)
	level := math.Pow(10, -dbov/20)
	prev := math.Float64frombits(t.audioLevelBits.Load())
	next := prev*(1-audioLevelAlpha) + level*audioLevelAlpha
	t.audioLevelBits.Store(math.Float64bits(next))
	t.audioLevelAt.Store(time.Now().UnixMilli())
}

// AudioLevel 返回平滑后的音频电平（0—1）；停止说话一段时间后自动归零。
func (t *PublishedTrack) AudioLevel() float64 {
	if t.info.Kind != protocol.KindAudio {
		return 0
	}
	at := t.audioLevelAt.Load()
	if at == 0 || time.Since(time.UnixMilli(at)) > audioLevelHold {
		return 0
	}
	return math.Float64frombits(t.audioLevelBits.Load())
}

// SetMuted 设置静音状态。静音后转发热路径**不再写包**：
// SFU 不解析媒体，无法"消除"已编码的声音，但可以选择不转发——
// 这才是真正让对方听不见的做法，同时也省下带宽（评测时可以直接量化）。
func (t *PublishedTrack) SetMuted(muted bool) {
	prev := t.muted.Swap(muted)
	if prev && !muted && t.info.Kind == protocol.KindVideo {
		// 恢复转发后订阅端需要关键帧，否则会一直花屏到下一个自然关键帧
		t.RequestKeyframe()
	}
}

func (t *PublishedTrack) Muted() bool { return t.muted.Load() }

// DropCount 返回因静音丢弃的包数。
func (t *PublishedTrack) DropCount() uint64 { return t.dropped.Load() }

func (t *PublishedTrack) Info() *protocol.TrackInfo {
	info := *t.info
	info.Muted = t.muted.Load() // 以原子量为准，避免与转发热路径竞争
	return &info
}

func (t *PublishedTrack) Local() *webrtc.TrackLocalStaticRTP { return t.local }

func (t *PublishedTrack) Stats() (packets, bytes uint64) {
	return t.packets.Load(), t.bytes.Load()
}

// RequestKeyframe 向发布者请求关键帧（PLI）。
// 新订阅者加入时必须调用，否则要等发布者下一个自然关键帧才可能出画面（表现为黑屏）。
func (t *PublishedTrack) RequestKeyframe() {
	if t.pubPC == nil || t.remote == nil {
		return
	}
	ssrc := uint32(t.remote.SSRC())
	if ssrc == 0 {
		// 未声明 SSRC 的轨道在收到第一个包之前拿不到 SSRC，此时 PLI 无法定位流
		log.Printf("sfu: 跳过关键帧请求（SSRC 未知）track=%s", t.info.ID)
		return
	}
	pli := &rtcp.PictureLossIndication{MediaSSRC: ssrc}
	if err := t.pubPC.WriteRTCP([]rtcp.Packet{pli}); err != nil {
		log.Printf("sfu: 请求关键帧失败 track=%s: %v", t.info.ID, err)
		return
	}
	log.Printf("sfu: 已请求关键帧(PLI) track=%s ssrc=%d", t.info.ID, ssrc)
}

// NudgeKeyframe 在轨道迟迟没有数据时反复索要关键帧。
//
// 为什么需要：屏幕共享画面静止时，浏览器**不会编码新帧**，服务端自然收不到包
// （现象就是订阅端一直黑屏、服务端 packets=0）。这时必须由订阅动作触发发布端
// 重新编一个关键帧。单次 PLI 可能因为丢包或编码器节流而无效，所以重试几次。
func (t *PublishedTrack) NudgeKeyframe() {
	if t.remote == nil {
		return
	}
	go func() {
		for i := 0; i < 4; i++ {
			if t.packets.Load() > 0 {
				return // 已经有数据在流，不需要再催
			}
			t.RequestKeyframe()
			time.Sleep(1200 * time.Millisecond)
		}
	}()
}

// attachRecorder 挂上录制写入器（由 participant 在 OnTrack 时调用）。
func (t *PublishedTrack) attachRecorder(w *mediarec.Writer) { t.rec = w }

// Pump 从发布者读取 RTP 并转发给所有订阅者，直到发布者停止。
// 这是整个媒体面唯一的"热路径"，也是评测 CPU / 带宽的观测点。
func (t *PublishedTrack) Pump() {
	defer func() {
		if t.rec != nil {
			_ = t.rec.Close() // flush + 回填 IVF 帧数
		}
		log.Printf("sfu: 轨道转发结束 track=%s packets=%d bytes=%d", t.info.ID, t.packets.Load(), t.bytes.Load())
		// 关键：必须通知房间回收这条轨道，否则客户端会永远留着一个"最后一帧"的瓦片，
		// 重新共享时还会多出第二个瓦片。
		if t.onEnd != nil {
			t.onEnd(t)
		}
	}()

	for {
		pkt, _, err := t.remote.ReadRTP()
		if err != nil {
			return // 发布者取消发布、连接关闭或出错
		}
		t.packets.Add(1)
		t.bytes.Add(uint64(len(pkt.Payload)))

		// 录制：在**转发之前**落盘，录到的是"发布者实际发出的内容"。
		// 它挂在热路径上，所以 Write 必须是非阻塞的（队列满则丢帧并计数）。
		if t.rec != nil {
			t.rec.Write(pkt)
		}

		// 活跃说话人：顺手从包头扩展取电平，不额外读一遍包、不解码
		if t.info.Kind == protocol.KindAudio {
			t.observeAudioLevel(pkt)
		}

		// 静音（自己静音或被主持人强制静音）：**继续读但不转发**。
		// 继续读是必须的——不读会让 Pion 的接收缓冲堆积，恢复时反而更糟；
		// 不转发则同时达到两个目的：对方听不见，且节省下行带宽。
		if t.muted.Load() {
			t.dropped.Add(1)
			continue
		}

		// 转发前剥掉发布者带来的 RTP 头扩展。
		//
		// 为什么必须剥：**头扩展的 ID 是每条连接各自协商的**（同一个 urn 在发布者那边是 4、
		// 到订阅者这边可能是 2）。Pion 的 TrackLocalStaticRTP 会重写 SSRC 与 PayloadType，
		// 但**不会重写扩展 ID**；而 Chrome 在 BUNDLE 下是按 mid 扩展把包分配到 m-line 的，
		// 收到 mid 对不上自己协商结果的包会**整包静默丢弃**。
		//
		// 症状极具迷惑性：订阅端传输层字节数一直涨（≈媒体速率），inbound-rtp 计数与
		// framesDecoded 却始终为 0——"媒体到了但没被算作任何轨道"。真实浏览器订阅会中招，
		// 而只按 SSRC 收包的合成客户端不会，所以早期测试全绿也发现不了。
		//
		// 服务端自己需要的信息（RFC 6464 音量）已经在上面读完，剥离不影响活跃说话人。
		// 代价：订阅端拿不到 TWCC/abs-send-time，其接收侧带宽估计会退化——本项目服务端
		// 不做拥塞控制，这个代价可以接受；要恢复则需要按订阅端重映射扩展 ID。
		pkt.Header.Extension = false
		pkt.Header.ExtensionProfile = 0
		pkt.Header.Extensions = nil

		// TrackLocalStaticRTP 会把包写给所有绑定的订阅者连接；没有订阅者时直接丢弃。
		if err := t.local.WriteRTP(pkt); err != nil {
			// 注意：写入失败通常只是某个订阅者的 SRTP 尚未就绪（Pion 在未就绪时静默丢包），
			// 绝不能因此退出循环——那会让这条轨道对所有人永久失效。
			if n := t.writeFails.Add(1); n <= 3 {
				log.Printf("sfu: 转发写入失败（忽略并继续）track=%s 第 %d 次: %v", t.info.ID, n, err)
			}
			continue
		}
	}
}

// drainReceiverRTCP 持续读取接收端 RTCP。
//
// Pion 的拦截器链路是"有人 Read 才流动"：不读的话，发布端回来的 RR/SR/NACK/PLI
// 会一直堆在缓冲里，RTT 等远程统计也不会有值。
func drainReceiverRTCP(r *webrtc.RTPReceiver) {
	if r == nil {
		return
	}
	buf := make([]byte, 1500)
	for {
		if _, _, err := r.Read(buf); err != nil {
			return // simulcast（同一 receiver 多轨）时会直接报错，此处忽略
		}
	}
}

// drainRTCP 必须持续读取发送者的 RTCP，否则 Pion 内部管道会阻塞。
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}
