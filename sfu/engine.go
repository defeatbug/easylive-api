// Package sfu 是轻量级 SFU 的媒体面骨架。
//
// 设计约束（与开题报告一致）：
//   - 只转发 RTP：不转码、不混流；
//   - 单进程、单 UDP 端口（ICE UDP mux），防火墙只需开一个口；
//   - 每个参与者两条 PeerConnection：pub（客户端发起 offer）与 sub（服务端发起 offer），
//     职责分离可以彻底避免重复协商（glare），也便于两者独立重协商。
package sfu

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"easylive-api/config"
)

type Engine struct {
	api *webrtc.API
	cfg *config.Config
	mux ice.UDPMux
}

// NewEngine 构造 Pion API：单端口 UDP mux + 对外地址广播 + 默认拦截器（NACK / RTCP Report / TWCC）。
func NewEngine(cfg *config.Config) (*Engine, error) {
	var se webrtc.SettingEngine

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

	// 3) 服务端不需要 mDNS 候选
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	// 4) Pion 自身日志。默认（error）会把 "Could not determine PayloadType"、"Failed to set
	//    codec settings" 这类**静默丢包**警告吞掉，排查"订阅端黑屏"时必开。
	//    注意：debug 级别会有逐包日志，所以这里比应用日志低一档。
	se.LoggerFactory = newLoggerFactory(cfg.LogLevel)

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
	return &Engine{api: api, cfg: cfg, mux: mux}, nil
}

// Close 关闭 UDP mux，释放端口。
func (e *Engine) Close() error {
	if e.mux != nil {
		return e.mux.Close()
	}
	return nil
}

// Configuration 生成 ICE 配置：STUN 直连探测 + TURN 中继兜底（用于对称 NAT）。
func (e *Engine) Configuration(userID string) webrtc.Configuration {
	cfg := webrtc.Configuration{ICEServers: []webrtc.ICEServer{}}

	if len(e.cfg.StunURLs) > 0 {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{URLs: e.cfg.StunURLs})
	}
	if len(e.cfg.TurnURLs) > 0 && e.cfg.TurnSecret != "" {
		user, cred := e.turnCredential(userID, time.Hour)
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
			URLs:           e.cfg.TurnURLs,
			Username:       user,
			Credential:     cred,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return cfg
}

// turnCredential 实现 coturn 的 REST API 临时凭证：
// username = <过期时间戳>:<用户标识>，password = base64(HMAC-SHA1(secret, username))。
func (e *Engine) turnCredential(userID string, ttl time.Duration) (string, string) {
	expiry := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	username := expiry + ":" + userID
	mac := hmac.New(sha1.New, []byte(e.cfg.TurnSecret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
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
