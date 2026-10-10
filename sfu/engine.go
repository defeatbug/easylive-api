// Package sfu 是轻量级 SFU 的媒体面骨架。
//
// 设计约束（与开题报告一致）：
//   - 只转发 RTP：不转码、不混流；
//   - 单进程、单 UDP 端口（ICE UDP mux），防火墙只需开一个口；
//   - 每个参与者两条 PeerConnection：pub（客户端发起 offer）与 sub（服务端发起 offer），
//     职责分离可以彻底避免重复协商（glare），也便于两者独立重协商。
package sfu

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"easylive-api/config"
	"easylive-api/turn"
)

// LatencyExtensionURI 是压测专用的 RTP 头扩展（携带发送时刻，毫秒）。
// 服务端不解析它，只是随包转发——所以它不会进入媒体处理逻辑，也不影响转发性能。
const LatencyExtensionURI = "urn:easylive:send-timestamp"

type Engine struct {
	api *webrtc.API
	cfg *config.Config
	mux ice.UDPMux
	// tcpMux / tcpListener 是 ICE-TCP 兜底路径（UDP 被封锁的网络）
	tcpMux      ice.TCPMux
	tcpListener net.Listener
}

// NewEngine 构造 Pion API：单端口 UDP mux + 对外地址广播 + 默认拦截器（NACK / RTCP Report / TWCC）。
func NewEngine(cfg *config.Config) (*Engine, error) {
	var se webrtc.SettingEngine

	// 0) 先装日志工厂。**顺序有讲究**：后面的 ICE-TCP mux 要用它建 logger，
	//    如果放到最后赋值，`se.LoggerFactory.NewLogger(...)` 会对 nil 接口调用方法而 panic——
	//    而且只在 ICETCPPort>0 时触发（默认就是 7882），等于每次生产启动都会崩。
	//    Pion 默认（error）会把 "Could not determine PayloadType" 这类**静默丢包**警告吞掉，
	//    排查"订阅端黑屏"时必开；debug 级别会有逐包日志，所以这里比应用日志低一档。
	se.LoggerFactory = newLoggerFactory(cfg.LogLevel)

	// 1) 单端口 UDP mux：所有 PeerConnection 共用一个 UDP 端口。
	mux, err := ice.NewMultiUDPMuxFromPort(cfg.MediaUDPPort,
		ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4, ice.NetworkTypeUDP6),
	)
	if err != nil {
		return nil, fmt.Errorf("创建单端口 UDP mux（端口 %d）失败: %w", cfg.MediaUDPPort, err)
	}
	se.SetICEUDPMux(mux)

	// 2) 对外广播的候选地址。容器里枚举到的是 Docker 内网 IP（172.x），浏览器连不通，
	//    所以调试与部署时必须显式指定（宿主机 127.0.0.1 / 局域网 IP / 公网 IP）。
	if cfg.AdvertiseIP != "" {
		se.SetNAT1To1IPs([]string{cfg.AdvertiseIP}, webrtc.ICECandidateTypeHost)
		log.Printf("sfu: 广播 ICE 候选地址 %s（udp/%d）", cfg.AdvertiseIP, cfg.MediaUDPPort)
	} else {
		log.Printf("sfu: 未设置 SFU_ADVERTISE_IP，将自动枚举本机网卡地址（容器内通常连不通，调试请显式设置）")
	}
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)

	// 3) ICE-TCP 兜底：企业内网、部分校园网会封 UDP，此时只有 TCP 候选能连通。
	//    与 UDP 一样做成单端口 mux，安全组只需额外开一个 TCP 端口。
	//    （此前 ICETCPPort 只在配置里存在、代码里从未使用——这类"文档写了但没实现"
	//      的不一致会在公网验收时变成"明明开了端口却连不上"，必须对齐。）
	var tcpMux ice.TCPMux
	var tcpListener net.Listener
	if cfg.ICETCPPort > 0 {
		ln, lerr := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ICETCPPort))
		if lerr != nil {
			return nil, fmt.Errorf("监听 ICE TCP 端口 %d 失败: %w", cfg.ICETCPPort, lerr)
		}
		tcpListener = ln
		tcpMux = ice.NewTCPMuxDefault(ice.TCPMuxParams{
			Listener:        ln,
			Logger:          se.LoggerFactory.NewLogger("ice-tcp"),
			WriteBufferSize: 4 * 1024 * 1024,
		})
		se.SetICETCPMux(tcpMux)
		log.Printf("sfu: ICE-TCP 已启用，监听 tcp/%d（UDP 被封锁时的兜底路径）", cfg.ICETCPPort)
	}

	// 4) 服务端不需要 mDNS 候选
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	// 5) 默认编解码器与拦截器：NACK（丢包重传）、RTCP Report、TWCC（拥塞反馈）
	m := &webrtc.MediaEngine{}
	// 注册 RFC 6464 音频电平头扩展（urn:ietf:params:rtp-hdrext:ssrc-audio-level）。
	// 浏览器默认会在 offer 里带它；服务端这里注册后 answer 才会确认，
	// 之后每个音频包都携带电平值——活跃说话人检测就是解析它，不需要解码音频。
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI},
		webrtc.RTPCodecTypeAudio,
	); err != nil {
		return nil, fmt.Errorf("注册音频电平头扩展失败: %w", err)
	}

	// 注册压测用的发送时间戳头扩展：服务端**只透传、不解析**。
	// 存在的意义是让合成压测客户端能测单向延迟（同一台机器上时钟一致，
	// 收到时间 - 发送时间 = 传输 + 服务端转发的耗时）。
	// 必须由服务端也注册，否则 answer 里不会有 extmap，两端就协商不起来。
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: LatencyExtensionURI},
		webrtc.RTPCodecTypeVideo,
	); err != nil {
		return nil, fmt.Errorf("注册延迟测量头扩展失败: %w", err)
	}

	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("注册默认编解码器失败: %w", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("注册默认拦截器失败: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	)
	return &Engine{api: api, cfg: cfg, mux: mux, tcpMux: tcpMux, tcpListener: tcpListener}, nil
}

// Close 关闭 UDP/TCP mux，释放端口。
func (e *Engine) Close() error {
	if e.mux != nil {
		_ = e.mux.Close()
	}
	if e.tcpMux != nil {
		_ = e.tcpMux.Close()
	} else if e.tcpListener != nil {
		_ = e.tcpListener.Close()
	}
	return nil
}

// Configuration 生成 ICE 配置：STUN 直连探测 + TURN 中继兜底（用于对称 NAT）。
// 凭证生成与下发给浏览器的是同一套实现（turn 包），避免"服务端能用、客户端不能用"的偏差。
func (e *Engine) Configuration(userID string) webrtc.Configuration {
	servers := turn.Servers(e.cfg.StunURLs, e.cfg.TurnURLs, e.cfg.TurnSecret, userID, e.cfg.TurnCredTTL)
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		item := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" {
			item.Username = s.Username
			item.Credential = s.Credential
			item.CredentialType = webrtc.ICECredentialTypePassword
		}
		out = append(out, item)
	}
	return webrtc.Configuration{ICEServers: out}
}

// NewPeerConnection 用统一配置创建一条 PeerConnection。
func (e *Engine) NewPeerConnection(userID string) (*webrtc.PeerConnection, error) {
	return e.api.NewPeerConnection(e.Configuration(userID))
}

// newLoggerFactory 把应用的日志级别映射到 Pion 的日志工厂（低一档，避免逐包刷屏）。
func newLoggerFactory(appLevel string) logging.LoggerFactory {
	f := logging.NewDefaultLoggerFactory()
	f.Writer = os.Stdout
	switch strings.ToLower(appLevel) {
	case "trace":
		f.DefaultLogLevel = logging.LogLevelDebug
	case "debug":
		f.DefaultLogLevel = logging.LogLevelInfo
	case "info":
		f.DefaultLogLevel = logging.LogLevelWarn
	default:
		f.DefaultLogLevel = logging.LogLevelError
	}
	return f
}
