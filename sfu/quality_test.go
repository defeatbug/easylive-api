package sfu

import (
	"math"
	"testing"
	"time"

	"easylive-api/protocol"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestClientQualityReport 钉住客户端质量上报的入口语义：
//   - 落库口径是**比例 0–1**（协议里是百分比，入口转换一次）；
//   - 同一参与者 1s 内重复上报被限频；
//   - 未入会者、越界数值一律丢弃（观测数据不能污染历史曲线）；
//   - 多路取平均值，过期样本不参与；
//   - 离会者的样本立即移除（人走了曲线不该还挂着他的 RTT）。
func TestClientQualityReport(t *testing.T) {
	m := testManager()
	r := newTestRoom("room-q", m)

	p1 := NewParticipant("p-1", "A", nil, nil)
	p1.Confirm(r)
	r.participants[p1.ID] = p1

	// 1) 正常上报：百分比 → 比例
	if ok := r.SetClientQuality("p-1", protocol.ClientQuality{
		RTTMs: 40, LossRate: 2.5, OutKbps: 800, InKbps: 1600,
	}); !ok {
		t.Fatal("有效的质量上报应被接受")
	}
	snap := r.Snapshot()
	if !near(snap.RTTMs, 40) || !near(snap.LossRate, 0.025) ||
		!near(snap.OutKbps, 800) || !near(snap.InKbps, 1600) {
		t.Fatalf("快照应带回上报值（丢包转成比例），实际 %+v", snap)
	}

	// 2) 限频：1s 内第二条必须被拒
	if r.SetClientQuality("p-1", protocol.ClientQuality{RTTMs: 999, LossRate: 9}) {
		t.Fatal("1 秒内的重复上报应被限频拒绝")
	}

	// 3) 未入会者上报被拒（不得凭空造出质量样本）
	if r.SetClientQuality("ghost", protocol.ClientQuality{RTTMs: 10, InKbps: 100}) {
		t.Fatal("未入会的参与者上报应被拒绝")
	}

	// 4) 越界数值被拒
	bad := []protocol.ClientQuality{
		{RTTMs: -1},
		{RTTMs: clientQualityMaxRTTMs + 1},
		{LossRate: -0.1},
		{LossRate: 100.1},
		{OutKbps: -1},
		{InKbps: clientQualityMaxKbps + 1},
	}
	for i, q := range bad {
		// 绕过限频：把 at 拨回窗口之外，确保被拒原因确实是"越界"而不是限频
		r.mu.Lock()
		if prev, ok := r.quality["p-1"]; ok {
			prev.at = time.Now().Add(-2 * time.Second)
			r.quality["p-1"] = prev
		}
		r.mu.Unlock()
		if r.SetClientQuality("p-1", q) {
			t.Fatalf("第 %d 组越界数据不应被接受: %+v", i+1, q)
		}
	}

	// 5) 两路平均（先绕过 p-1 的限频窗口）
	r.mu.Lock()
	prev := r.quality["p-1"]
	prev = clientQuality{RTTMs: 40, LossRate: 0.025, OutKbps: 800, InKbps: 1600, at: time.Now().Add(-2 * time.Second)}
	r.quality["p-1"] = prev
	r.mu.Unlock()

	p2 := NewParticipant("p-2", "B", nil, nil)
	p2.Confirm(r)
	r.participants[p2.ID] = p2
	if !r.SetClientQuality("p-2", protocol.ClientQuality{RTTMs: 60, LossRate: 0.5, OutKbps: 1200, InKbps: 2400}) {
		t.Fatal("第二路上报应被接受")
	}
	snap = r.Snapshot()
	if !near(snap.RTTMs, 50) || !near(snap.LossRate, 0.015) ||
		!near(snap.OutKbps, 1000) || !near(snap.InKbps, 2000) {
		t.Fatalf("两路应取平均值，实际 %+v", snap)
	}

	// 6) 过期样本（> clientQualityStale）不参与平均
	r.mu.Lock()
	stale := r.quality["p-2"]
	stale.at = time.Now().Add(-(clientQualityStale + time.Second))
	r.quality["p-2"] = stale
	r.mu.Unlock()
	snap = r.Snapshot()
	if !near(snap.RTTMs, 40) || !near(snap.InKbps, 1600) {
		t.Fatalf("过期样本不应参与平均，实际 %+v", snap)
	}

	// 7) 离会后样本立即清理
	r.Leave("p-1")
	r.mu.RLock()
	_, stillThere := r.quality["p-1"]
	r.mu.RUnlock()
	if stillThere {
		t.Fatal("参与者离会后其质量样本应被删除")
	}
}
