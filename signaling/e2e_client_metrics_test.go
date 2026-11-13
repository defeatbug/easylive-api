package signaling_test

import (
	"math"
	"testing"
	"time"

	"easylive-api/protocol"
)

// TestE2EClientMetricsReported 验证 `client_metrics` 的真实链路：
// 客户端上报 → 信令分发 → Room.SetClientQuality → Room.Snapshot() 可见。
//
// 这条链路上任何一环断了，metric_samples 的 rtt_ms/loss_rate/in_kbps/out_kbps
// 都会静默写 0（报表页变成一条"看起来很健康"的平线），所以必须端到端断言一次。
// 注意单位：协议里 lossRate 是百分比，房间快照里是比例（服务端入口除以 100）。
func TestE2EClientMetricsReported(t *testing.T) {
	srv := startServerFull(t, 0, "", "", "test")

	p := dial(t, srv.url, "reporter")
	defer p.close()
	p.join("room-metrics", "Reporter")

	p.send(&protocol.Message{
		Type: protocol.TypeClientMetrics,
		Quality: &protocol.ClientQuality{
			RTTMs: 42, LossRate: 1.5, OutKbps: 800, InKbps: 1600,
		},
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, snap := range srv.manager.RoomSnapshots() {
			if snap.ID != "room-metrics" {
				continue
			}
			if snap.RTTMs > 0 {
				if snap.RTTMs != 42 {
					t.Fatalf("RTT 应为 42ms，实际 %v", snap.RTTMs)
				}
				if math.Abs(snap.LossRate-0.015) > 1e-9 {
					t.Fatalf("丢包率应转成比例 0.015，实际 %v", snap.LossRate)
				}
				if snap.OutKbps != 800 || snap.InKbps != 1600 {
					t.Fatalf("码率不正确: out=%v in=%v", snap.OutKbps, snap.InKbps)
				}
				// 非法上报不得改写已有样本，也不能让连接出错
				p.send(&protocol.Message{
					Type:    protocol.TypeClientMetrics,
					Quality: &protocol.ClientQuality{RTTMs: -1, LossRate: 500},
				})
				p.send(&protocol.Message{Type: protocol.TypePing, ID: "after-bad-metrics"})
				if pong := p.waitType(protocol.TypePong, 5*time.Second); pong.ID != "after-bad-metrics" {
					t.Fatalf("非法质量上报后连接应仍然正常（pong id=%q）", pong.ID)
				}
				for _, again := range srv.manager.RoomSnapshots() {
					if again.ID == "room-metrics" && again.RTTMs != 42 {
						t.Fatalf("非法上报不应改写已有样本，实际 RTT=%v", again.RTTMs)
					}
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("client_metrics 上报后房间快照里仍是 0（链路某处断了）")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
