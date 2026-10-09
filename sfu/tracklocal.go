package sfu

import (
	"log"

	"github.com/pion/webrtc/v4"
)

// bindLoggingTrackLocal 包一层 TrackLocal，把"绑定失败"这个静默故障变成显式日志。
//
// 为什么需要它：Pion 的 TrackLocalStaticRTP.Bind 在编码器不匹配时返回 ErrUnsupportedCodec，
// 但 RTPSender 仍会持有该 track；此时 writeRTP 因为 bindings 为空而**直接返回 nil**，
// 于是表现为「sender 上有轨道、服务端也不报错、却一个包都发不出去」——极难定位。
//
// 这个包装只多一次 Bind 调用开销（每个订阅者一次），换来的是可观测性。
type bindLoggingTrackLocal struct {
	webrtc.TrackLocal
	label string
}

func wrapTrackLocal(t webrtc.TrackLocal, label string) webrtc.TrackLocal {
	return &bindLoggingTrackLocal{TrackLocal: t, label: label}
}

func (b *bindLoggingTrackLocal) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	params, err := b.TrackLocal.Bind(ctx)
	if err != nil {
		pts := make([]string, 0, len(ctx.CodecParameters()))
		for _, c := range ctx.CodecParameters() {
			pts = append(pts, c.MimeType)
		}
		log.Printf("sfu: ⚠️ 轨道绑定失败，订阅端将收不到任何包 track=%s mid=%s ssrc=%d err=%v 本端协商到的编码=%v",
			b.label, ctx.ID(), ctx.SSRC(), err, pts)
		return params, err
	}
	log.Printf("sfu: 轨道已绑定 track=%s mid=%s ssrc=%d pt=%d codec=%s",
		b.label, ctx.ID(), ctx.SSRC(), params.PayloadType, params.MimeType)
	return params, nil
}
