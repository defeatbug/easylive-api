// 屏幕共享场景的回归测试（对应真实故障：一个用户共享屏幕后，另一个用户看不到）。
//
// 复现的关键点是「静止画面」：
//  1. 发布端只写一帧就不再写（真实浏览器在画面不变时不会继续编码新帧）；
//  2. Pion 要等到第一个 RTP 包才触发 OnTrack，所以轨道在服务端是"有轨道、无数据"；
//  3. 订阅端订阅时，服务端必须主动发 PLI 索要关键帧，发布端才可能重新出帧；
//  4. 否则服务端 packets 永远是 0，订阅端永远黑屏。
//
// 本测试同时验证：SFU 确实发出了 PLI（用拦截器在客户端侧计数），以及收到 PLI 后补帧能被转发。
package signaling_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"easylive-api/protocol"
)

// pliCounter 统计客户端收到的 PLI。
type pliCounter struct {
	interceptor.NoOp
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

// NewInterceptor 实现 interceptor.Factory：返回自身，让所有流共用同一个计数器。
func (i *pliCounter) NewInterceptor(string) (interceptor.Interceptor, error) { return i, nil }

func (i *pliCounter) hit() {
	i.mu.Lock()
	i.count++
	i.mu.Unlock()
	select {
	case i.ch <- struct{}{}:
	default:
	}
}

func (i *pliCounter) Count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.count
}

func (i *pliCounter) Wait(d time.Duration) bool {
	select {
	case <-i.ch:
		return true
	case <-time.After(d):
		return false
	}
}

// BindRTCPReader 在收包路径上挂钩，解析出 PLI。
func (i *pliCounter) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := reader.Read(b, a)
		if err == nil && n > 0 {
			if pkts, perr := rtcp.Unmarshal(b[:n]); perr == nil {
				for _, p := range pkts {
					if _, ok := p.(*rtcp.PictureLossIndication); ok {
						i.hit()
					}
				}
			}
		}
		return n, attr, err
	})
}

// newPionAPIWithPLICounter 构造带 PLI 计数器的 Pion API。
func newPionAPIWithPLICounter(counter *pliCounter) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	ir.Add(counter)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), nil
}

// waitPCConnected 轮询等待 PeerConnection 进入 connected 状态。
func waitPCConnected(pc *webrtc.PeerConnection, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}

func vp8Frame(size int) []byte {
	f := make([]byte, size)
	for i := range f {
		f[i] = byte(i % 251)
	}
	return f
}

func TestE2EScreenShareKeyframe(t *testing.T) {
	url := startServer(t)

	pub := dial(t, url, "screen-pub")
	defer pub.close()
	sub := dial(t, url, "screen-sub")
	defer sub.close()

	pub.join("room-screen", "Pub")
	sub.join("room-screen", "Sub")

	// ---- 发布端：VP8 视频轨 + PLI 计数器 ----
	pli := &pliCounter{ch: make(chan struct{}, 1)}
	api, err := newPionAPIWithPLICounter(pli)
	if err != nil {
		t.Fatalf("构造 Pion API 失败: %v", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建发布者连接失败: %v", err)
	}
	pub.pubPC = pc

	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	track, err := webrtc.NewTrackLocalStaticSample(capability, "screen", "screen")
	if err != nil {
		t.Fatalf("创建本地视频轨失败: %v", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	// 关键：必须读发送端 RTCP，否则收到的 PLI 不会流经拦截器（Pion 是"有人读才派发"）
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, rerr := sender.Read(buf); rerr != nil {
				return
			}
		}
	}()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		pub.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer 失败: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription 失败: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	pub.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: "screen"})

	answer := pub.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("发布端设置 answer 失败: %v", err)
	}
	pub.flushCandidates()

	// ---- 等 DTLS 连上再写首帧 ----
	// 真实浏览器也是在连接建立后才发媒体；如果在 connecting 阶段就写，包会被 SRTP 丢掉，
	// 而这个测试只写一帧，丢了就再也不会有第二个包（这正好解释了"偶发无轨道"）。
	if !waitPCConnected(pc, 15*time.Second) {
		t.Fatalf("发布端连接在 15s 内未建立，当前状态: %s", pc.ConnectionState())
	}

	// ---- 关键：只写一帧，模拟"屏幕画面静止" ----
	if err := track.WriteSample(media.Sample{Data: vp8Frame(1200), Duration: 33 * time.Millisecond}); err != nil {
		t.Fatalf("写入首帧失败: %v", err)
	}

	// 模拟真实编码器对 PLI 的响应：每收到一次 PLI 就编一帧（静止画面只有被索要时才出帧）。
	// 这里按计数轮询，避免与断言 1 的 pli.Wait 争抢同一个 channel。
	go func() {
		seen := 0
		for {
			if pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
				return
			}
			if c := pli.Count(); c > seen {
				seen = c
				if err := track.WriteSample(media.Sample{Data: vp8Frame(2400), Duration: 33 * time.Millisecond}); err != nil {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	trackMsg := sub.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if trackMsg.Track == nil || trackMsg.Track.Source != "screen" {
		t.Fatalf("期望收到 source=screen 的 track_published，实际: %+v", trackMsg.Track)
	}

	// ---- 订阅端 ----
	subPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建订阅者连接失败: %v", err)
	}
	sub.subPC = subPC

	recv := make(chan int, 1)
	subPC.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		count := 0
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
			count++
			if count == 1 {
				select {
				case recv <- count:
				default:
				}
			}
		}
	})
	subPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		sub.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetSub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	sub.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: trackMsg.Track.ID})
	srvOffer := sub.waitType(protocol.TypeSubscribeOffer, 20*time.Second)
	if err := subPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: srvOffer.SDP}); err != nil {
		t.Fatalf("订阅端设置 offer 失败: %v", err)
	}
	subAnswer, err := subPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("订阅端 CreateAnswer 失败: %v", err)
	}
	if err := subPC.SetLocalDescription(subAnswer); err != nil {
		t.Fatalf("订阅端 SetLocalDescription 失败: %v", err)
	}
	<-webrtc.GatheringCompletePromise(subPC)
	sub.send(&protocol.Message{Type: protocol.TypeAnswer, Target: protocol.TargetSub, SDP: subPC.LocalDescription().SDP})
	sub.flushCandidates()

	// ---- 断言 1：服务端主动索要了关键帧（否则静止画面永远黑屏）----
	if !pli.Wait(15 * time.Second) {
		t.Fatalf("SFU 没有发出 PLI：静止画面的屏幕共享将永远停在 packets=0（订阅端黑屏）")
	}
	t.Logf("✅ 断言 1 通过：SFU 已发出 PLI（累计 %d 次）", pli.Count())

	// ---- 断言 2：订阅端最终能收到帧 ----
	// 关键帧只会在 PLI 触发后产生；订阅时发出的那帧可能落在 SRTP 未就绪窗口被吞掉，
	// 服务端在订阅连接 established 后会再索要一次，新一帧应能正常投递。
	select {
	case n := <-recv:
		t.Logf("✅ 断言 2 通过：订阅端收到 %d 个 RTP 包", n)
	case <-time.After(20 * time.Second):
		dumpDebugStats(t, url)
		t.Fatalf("订阅端 20s 内未收到任何 RTP 包（关键帧请求/转发链路仍有问题，PLI 累计 %d 次）", pli.Count())
	}
}

// dumpDebugStats 在失败时打印服务端快照，用于区分"没收到 / 没转发 / 没发出"。
func dumpDebugStats(t *testing.T, wsURL string) {
	t.Helper()
	base := strings.TrimSuffix(strings.TrimPrefix(wsURL, "ws://"), "/ws")
	resp, err := http.Get("http://" + base + "/debug/stats")
	if err != nil {
		t.Logf("（获取 /debug/stats 失败: %v）", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var pretty any
	if err := json.Unmarshal(body, &pretty); err == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Printf("\n===== /debug/stats =====\n%s\n========================\n", out)
		return
	}
	fmt.Printf("\n===== /debug/stats (raw) =====\n%s\n", body)
}
