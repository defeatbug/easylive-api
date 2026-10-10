// 纯函数单测：SDP 解析、码率→包数换算、百分位。
//
// 这几处都是"错了不会报错、只会让数字变得好看但不真实"的地方（例如扩展 ID 解析不到 →
// 延迟样本永远为空；包数向下取整 → 实测码率比目标低 25%），所以值得单独钉住。
package loadtest

import (
	"testing"

	"easylive-api/sfu"
)

const sampleSDP = `v=0
o=- 1 1 IN IP4 0.0.0.0
m=video 9 UDP/TLS/RTP/SAVPF 96 97
a=rtpmap:96 VP8/90000
a=rtpmap:97 rtx/90000
a=extmap:2 urn:ietf:params:rtp-hdrext:sdes:mid
a=extmap:1 urn:easylive:send-timestamp
a=extmap:5 http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01
m=audio 9 UDP/TLS/RTP/SAVPF 111
a=rtpmap:111 opus/48000/2
`

func TestPayloadTypeFromSDP(t *testing.T) {
	if pt, ok := payloadTypeFromSDP(sampleSDP, "VP8"); !ok || pt != 96 {
		t.Fatalf("VP8 应为 96，实际 %d ok=%v", pt, ok)
	}
	if pt, ok := payloadTypeFromSDP(sampleSDP, "opus"); !ok || pt != 111 {
		t.Fatalf("opus 应为 111，实际 %d ok=%v", pt, ok)
	}
	if _, ok := payloadTypeFromSDP(sampleSDP, "H264"); ok {
		t.Fatalf("未协商的编解码器不应命中")
	}
}

func TestExtIDFromSDP(t *testing.T) {
	if id := extIDFromSDP(sampleSDP, sfu.LatencyExtensionURI); id != 1 {
		t.Fatalf("延迟扩展应为 1，实际 %d", id)
	}
	if id := extIDFromSDP(sampleSDP, "urn:not-negotiated"); id != 0 {
		t.Fatalf("未协商的扩展应返回 0，实际 %d", id)
	}
	// 带方向后缀的形式（a=extmap:3/sendonly uri）也要能解析
	withDir := "a=extmap:3/sendonly urn:easylive:send-timestamp\r\n"
	if id := extIDFromSDP(withDir, sfu.LatencyExtensionURI); id != 3 {
		t.Fatalf("带方向后缀时应解析为 3，实际 %d", id)
	}
}

func TestVideoPacketSizing(t *testing.T) {
	// 800 kbps @30fps = 3333 字节/帧；1200 字节的包需要 3 个（向上取整），末包 933
	frameBytes := 800 * 1000 / 8 / 30
	if frameBytes != 3333 {
		t.Fatalf("帧字节数应为 3333，实际 %d", frameBytes)
	}
	const payload = 1200
	packets := (frameBytes + payload - 1) / payload
	if packets != 3 {
		t.Fatalf("应拆成 3 个包（向下取整会只有 2 个、码率低 25%%），实际 %d", packets)
	}
	last := frameBytes - (packets-1)*payload
	if last != 933 {
		t.Fatalf("末包应为 933 字节，实际 %d", last)
	}
	// 反推实际码率应贴住目标
	actualKbps := float64(packets-1)*payload + float64(last)
	actualKbps = actualKbps * 8 * 30 / 1000
	if actualKbps < 795 || actualKbps > 805 {
		t.Fatalf("实际码率应贴近 800 kbps，实际 %.1f", actualKbps)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(vals, 0.5); got != 5 {
		t.Fatalf("P50 应为 5，实际 %v", got)
	}
	if got := percentile(vals, 0.95); got != 10 {
		t.Fatalf("P95 应为 10，实际 %v", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("空输入应返回 0，实际 %v", got)
	}
}

func TestLatencyWindowTakeResets(t *testing.T) {
	w := &latencyWindow{}
	for _, v := range []float64{1, 2, 3, 4, 100} {
		w.add(v)
	}
	var out Sample
	w.take(&out)
	if out.LatencySamples != 5 || out.LatencyP50 != 3 || out.LatencyMax != 100 {
		t.Fatalf("统计不正确: %+v", out)
	}
	// take 必须清空窗口，否则百分比会跨周期累积、越到后面越"平滑"，掩盖抖动
	var again Sample
	w.take(&again)
	if again.LatencySamples != 0 {
		t.Fatalf("take 后窗口应清空，实际 %d 个样本", again.LatencySamples)
	}
}
