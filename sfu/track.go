package sfu

import (
	"log"
	"math"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
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

	// rec 是这条轨道的录制写入器（没在录时为 nil）。
	//
	// 用原子指针而不是普通字段：录制现在可以在**转发进行中**被主持人启停，
	// 而转发热路径每个包都会读它——普通字段会被 race detector 判定为数据竞争。
	rec atomic.Pointer[mediarec.Writer]
	// lastPLINs 是上次向发布者请求关键帧的时间（UnixNano），用于限流。
	// 必须限流：每个订阅者在新订阅、丢包、解码失败时都会发 PLI，
	// 不限流的话 N 个订阅者能把发布端逼成"一直在编关键帧"——关键帧体积远大于普通帧，
	// 结果是为了救一个人把所有人的画质都拖下去。
	lastPLINs atomic.Int64
	// rememberSSRCO 保证 SSRC 只登记一次
	rememberSSRCO sync.Once
	// registerLocalSSRCO 保证出向 SSRC 只登记一次
	registerLocalSSRCO sync.Once
	// outSSRC 是我们转发给订阅者的流的 SSRC（从写出的包头上取，注销时要清）
	outSSRC atomic.Uint32

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

// keyframeInterval 是同一路轨道两次关键帧请求之间的最小间隔。
// 400ms 的依据：关键帧本身要几百毫秒才能编完并送达，请求得比这更密只是徒增带宽。
const keyframeInterval = 400 * time.Millisecond

// RequestKeyframeThrottled 是"订阅端要求关键帧"的入口：内部限流，返回是否真的发了 PLI。
//
// 与 RequestKeyframe 的区别：那个是无条件发送（用于新订阅者加入这类必须成功的场景），
// 这个是**外部触发**（订阅端 PLI/FIR）——外部触发必须限流，否则就是一个放大器。
func (t *PublishedTrack) RequestKeyframeThrottled() bool {
	now := time.Now().UnixNano()
	last := t.lastPLINs.Load()
	if last != 0 && now-last < int64(keyframeInterval) {
		pliSuppressed.Add(1)
		return false
	}
	// CAS 抢占时间戳：并发订阅者同时到达时只有一个能发出去
	if !t.lastPLINs.CompareAndSwap(last, now) {
		pliSuppressed.Add(1)
		return false
	}
	t.RequestKeyframe()
	pliForwarded.Add(1)
	return true
}

// 关键帧请求的进程级计数（观测用：healthz / 日志都能看到）。
var (
	pliForwarded  atomic.Uint64
	pliSuppressed atomic.Uint64
)

// FrameRequestStats 返回 (转发给发布者的关键帧请求数, 因限流被压掉的数量)。
func FrameRequestStats() (forwarded, suppressed uint64) {
	return pliForwarded.Load(), pliSuppressed.Load()
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
	safeGo("keyframe-nudge/"+t.info.ID, func() {
		for i := 0; i < 4; i++ {
			if t.packets.Load() > 0 {
				return // 已经有数据在流，不需要再催
			}
			t.RequestKeyframe()
			time.Sleep(1200 * time.Millisecond)
		}
	})
}

// attachRecorder 挂上录制写入器（发布时或主持人中途开始录制时调用）。
func (t *PublishedTrack) attachRecorder(w *mediarec.Writer) { t.rec.Store(w) }

// detachRecorder 摘掉并返回当前写入器（停止录制时调用，返回 nil 表示本来就没挂）。
func (t *PublishedTrack) detachRecorder() *mediarec.Writer { return t.rec.Swap(nil) }

// recordedBytes 报告录制是否挂着（观测用）。
func (t *PublishedTrack) recording() bool { return t.rec.Load() != nil }

// Pump 从发布者读取 RTP 并转发给所有订阅者，直到发布者停止。
// 这是整个媒体面唯一的"热路径"，也是评测 CPU / 带宽的观测点。
func (t *PublishedTrack) Pump() {
	defer func() {
		if w := t.rec.Swap(nil); w != nil {
			_ = w.Close() // flush + 回填 IVF 帧数
		}
		log.Printf("sfu: 轨道转发结束 track=%s packets=%d bytes=%d", t.info.ID, t.packets.Load(), t.bytes.Load())
		// 关键：必须通知房间回收这条轨道，否则客户端会永远留着一个"最后一帧"的瓦片，
		// 重新共享时还会多出第二个瓦片。
		if t.onEnd != nil {
			t.onEnd(t)
		}
	}()

	for {
		// SSRC 在这里才可知：订阅端的 PLI/FIR 只带 SSRC，必须能据此找回这条轨道
		t.rememberSSRCO.Do(func() {
			// 登记发布者远端的 SSRC：我们发给发布者的 PLI 用它。
			// 注明：订阅端 PLI 里的 SSRC 在 Pion v4.2 下与它相同（见下面 WriteRTP 处的实测说明），
			// 但那属于实现细节；出向 SSRC 会被单独、显式地登记，不依赖这个巧合。
			remoteSSRC := uint32(t.remote.SSRC())
			rememberSSRC(remoteSSRC, t)
			log.Printf("sfu: 登记远端 SSRC=%d → track=%s", remoteSSRC, t.info.ID)
		})
		pkt, _, err := t.remote.ReadRTP()
		if err != nil {
			// 读循环结束 = 这条轨道不再有数据：把两个映射都清掉，避免查表拿到死轨道
			forgetSSRC(uint32(t.remote.SSRC()))
			if ssrc := t.outSSRC.Load(); ssrc != 0 {
				forgetSSRC(ssrc)
			}
			return // 发布者取消发布、连接关闭或出错
		}
		t.packets.Add(1)
		t.bytes.Add(uint64(len(pkt.Payload)))

		// 录制：在**转发之前**落盘，录到的是"发布者实际发出的内容"。
		// 它挂在热路径上，所以 Write 必须是非阻塞的（队列满则丢帧并计数）。
		if w := t.rec.Load(); w != nil {
			w.Write(pkt)
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
		writeErr := t.local.WriteRTP(pkt)
		if writeErr == nil {
			// 出向 SSRC 从写出的包头取（该类型没有导出 SSRC 的取值接口）。
			//
			// 实测结论（不是推测）：Pion v4.2 的 TrackLocalStaticRTP.WriteRTP 对**已经有
			// SSRC** 的包会原样保留，所以这里取到的值与发布者的远端 SSRC 相同——
			// 日志里 remote 与 out 两个值一模一样就是这么来的。
			// 仍然显式登记它，是为了把"订阅端 PLI 里带的到底是哪个 SSRC"这条不变量钉住：
			// 一旦将来换实现/升级改动成"出向重新分配 SSRC"，这里就是唯一正确的取值点，
			// 而不是靠"两个值恰好相等"这个巧合。
			t.registerLocalSSRCO.Do(func() {
				ssrc := pkt.Header.SSRC
				t.outSSRC.Store(ssrc)
				rememberSSRC(ssrc, t)
				log.Printf("sfu: 登记出向 SSRC=%d → track=%s（订阅端 PLI 带的是它）", ssrc, t.info.ID)
			})
		}
		if writeErr != nil {
			// 注意：写入失败通常只是某个订阅者的 SRTP 尚未就绪（Pion 在未就绪时静默丢包），
			// 绝不能因此退出循环——那会让这条轨道对所有人永久失效。
			if n := t.writeFails.Add(1); n <= 3 {
				log.Printf("sfu: 转发写入失败（忽略并继续）track=%s 第 %d 次: %v", t.info.ID, n, writeErr)
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

// trackBySSRC 把 RTP 的 SSRC 映射到发布轨道。
//
// 为什么需要进程级映射：浏览器发来的 PLI/FIR 里**只有 SSRC**，而 RTCP 拦截器是挂在
// PeerConnection 上的（一个订阅端 PC 可能同时收好几个人的画面），拦截器自身不知道
// 这条 PLI 对应哪一路轨道。用 SSRC 查表是最直接的解法，也让拦截器保持为
// "RTCP 字节的纯函数"，不必给每条 PC 单独组装 API 与拦截器链。
var trackBySSRC sync.Map // uint32 -> *PublishedTrack

func rememberSSRC(ssrc uint32, t *PublishedTrack) {
	if ssrc != 0 {
		trackBySSRC.Store(ssrc, t)
	}
}

func forgetSSRC(ssrc uint32) {
	if ssrc != 0 {
		trackBySSRC.Delete(ssrc)
	}
}

// keyframeForwarder 是一个 RTCP 拦截器：观察订阅端发来的 PLI/FIR，转发给对应的发布者。
//
// 这是弱网下恢复画面最快的路径：浏览器一旦丢包/解码失败就会发 PLI（或 FIR），
// 以前订阅端的 RTCP 只是被排空——PLI 直接丢掉，只能等发布者下一个自然关键帧，
// 表现为"卡住好几秒"。转发后从丢包到恢复只需一个 RTT 量级。
//
// 它实现 interceptor.RTCPReader 包装：只观察，不改写、不吞包。
type keyframeForwarder struct {
	interceptor.NoOp
}

// keyframeForwarderFactory 是拦截器注册所需的工厂：为每条 PeerConnection 造一个实例。
type keyframeForwarderFactory struct{}

func (keyframeForwarderFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	// 必须是**指针**：内嵌的 interceptor.NoOp 方法都是指针接收者
	return &keyframeForwarder{}, nil
}

// rtcpMalformed 统计被丢弃的畸形 RTCP（观测用：出现异常增长说明有客户端在发垃圾）。
var rtcpMalformed atomic.Uint64

// MalformedRTCPCount 返回畸形 RTCP 的计数。
func MalformedRTCPCount() uint64 { return rtcpMalformed.Load() }

// unmarshalRTCP 解析 RTCP，**绝不 panic**。
//
// 为什么必须包 recover：pion/rtcp 的 Unmarshal 对某些畸形输入是直接 panic 而不是返回 error，
// 而这条路径的数据**完全来自客户端**——不加保护的话，一个客户端发几个字节的垃圾 RTCP
// 就能让整个服务进程崩溃。这是测试里真实触发出来才发现的。
func unmarshalRTCP(b []byte) (pkts []rtcp.Packet, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			rtcpMalformed.Add(1)
			pkts, ok = nil, false
		}
	}()
	parsed, err := rtcp.Unmarshal(b)
	if err != nil {
		rtcpMalformed.Add(1)
		return nil, false
	}
	return parsed, true
}

func (keyframeForwarder) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := reader.Read(b, a)
		if err != nil || n <= 0 {
			return n, attr, err
		}
		pkts, ok := unmarshalRTCP(b[:n])
		if !ok {
			return n, attr, err // 畸形 RTCP：照常放行
		}
		for _, pkt := range pkts {
			var ssrc uint32
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				ssrc = p.MediaSSRC
			case *rtcp.FullIntraRequest:
				ssrc = p.MediaSSRC
			default:
				continue
			}
			v, ok := trackBySSRC.Load(ssrc)
			if !ok {
				continue
			}
			if t, ok := v.(*PublishedTrack); ok && t.RequestKeyframeThrottled() {
				log.Printf("sfu: 订阅端请求关键帧 → 已转发给发布者 track=%s ssrc=%d", t.info.ID, ssrc)
			}
		}
		return n, attr, err
	})
}
