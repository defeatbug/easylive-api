package sfu

// TrackStatsRow 是一条发布轨道的运行期画像。
//
// 字段命名刻意与**浏览器 getStats() 的口径**对齐（packetsReceived / bytesReceived /
// framesDropped 一类），这样"客户端看到的"和"服务端看到的"能直接并排放进同一张表里，
// 而不是一边叫 packets、一边叫 packetsReceived，对不上只能靠人猜。
//
// Codec 是**实际协商到的**编码（如 video/VP8）：协商偏好、浏览器能力、SDP 结果三者
// 谁说了算，最终就看这一个字段。要动编码策略时，先看这里，别靠推测。
type TrackStatsRow struct {
	ID          string `json:"id"`
	PublisherID string `json:"publisherId"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Codec       string `json:"codec"`
	PayloadType uint8  `json:"payloadType"`
	Muted       bool   `json:"muted"`
	Recording   bool   `json:"recording"`
	// Packets/Bytes 是服务端**收到**的量（对应浏览器的 packetsReceived/bytesReceived）
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
	// Dropped 是"继续读但不转发"丢掉的数量（静音、被强制静音），用于量化静音省了多少带宽
	Dropped uint64 `json:"dropped"`
	// WriteFails 是向订阅者写入失败的次数。持续增长通常意味着某个订阅者的 SRTP 没就绪
	WriteFails uint64 `json:"writeFails"`
	// Subscribers 是当前绑定到这条轨道的订阅者数量。
	// 以前这个数字在服务端**不可见**——只订阅出去有没有人收，日志里看不出来。
	Subscribers int `json:"subscribers"`
	// PLIForwarded/PLISuppressed 是订阅端关键帧请求的转发与限流计数
	PLIForwarded  uint64 `json:"pliForwarded"`
	PLISuppressed uint64 `json:"pliSuppressed"`
	// Keyframes 是服务端主动向发布者索要关键帧的次数（新订阅者加入等）
	Keyframes uint64 `json:"keyframes"`
}

// TrackStats 返回该轨道的画像。codec/payloadType 取自发布者实际协商到的参数。
func (t *PublishedTrack) TrackStats() TrackStatsRow {
	row := TrackStatsRow{
		ID:            t.info.ID,
		PublisherID:   t.info.PublisherID,
		Kind:          t.info.Kind,
		Source:        t.info.Source,
		Muted:         t.muted.Load(),
		Recording:     t.rec.Load() != nil,
		Packets:       t.packets.Load(),
		Bytes:         t.bytes.Load(),
		Dropped:       t.dropped.Load(),
		WriteFails:    t.writeFails.Load(),
		Subscribers:   int(t.subscribers.Load()),
		PLIForwarded:  t.pliForwarded.Load(),
		PLISuppressed: t.pliSuppressed.Load(),
		Keyframes:     t.keyframes.Load(),
	}
	if t.remote != nil {
		c := t.remote.Codec()
		row.Codec = c.MimeType
		row.PayloadType = uint8(c.PayloadType)
	}
	return row
}
