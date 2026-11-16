package ai

import (
	"context"
	"sync"
	"testing"

	"github.com/godeps/opus"
	"github.com/pion/rtp"
)

// stubSTT 是不依赖外部服务的转写后端，用于测试 Stop 的生命周期。
type stubSTT struct{}

func (stubSTT) Transcribe(context.Context, []byte, string) (string, error) { return "", nil }

// opusSilencePayload 造一个合法的 Opus 包（20ms 静音），让转写循环真的跑起来。
func opusSilencePayload(t *testing.T) []byte {
	t.Helper()
	enc, err := opus.NewEncoder(opRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatalf("创建 opus 编码器失败: %v", err)
	}
	buf := make([]byte, 2000)
	n, err := enc.Encode(make([]int16, 960), buf)
	if err != nil {
		t.Fatalf("编码静音帧失败: %v", err)
	}
	return buf[:n]
}

// TestTranscriberStopIdempotent 钉住一个会打死整个进程的缺陷：
//
// Stop 会在两条路径上被调用——房间停止 AI 总结（manager → session → 每条轨道），
// 以及轨道本身下线（session 收到轨道事件再 Stop 一次）。旧实现直接 close(t.done)，
// 第二次调用就是 `panic: close of closed channel`；而它发生在信令/媒体回调里，
// 没有 recover 的话整个服务进程直接退出（不是只坏一条轨道）。
func TestTranscriberStopIdempotent(t *testing.T) {
	tr := NewTranscriber("p-1", 30, stubSTT{}, "zh", func(Segment) {})
	// 塞一个包，确保后台循环真的在跑（Stop 要等它收尾）
	tr.Write(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: opusSilencePayload(t)})

	tr.Stop()
	tr.Stop() // 旧实现：这里 panic
	tr.Stop() // 再多一次也不能出问题
}

// TestTranscriberStopConcurrent 覆盖"两条路径同时 Stop"的竞态：
// 用 -race 跑，且 4 个 goroutine 同时 Stop 时必须都不 panic、不重复等待。
func TestTranscriberStopConcurrent(t *testing.T) {
	for i := 0; i < 20; i++ {
		tr := NewTranscriber("p-c", 30, stubSTT{}, "zh", func(Segment) {})
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tr.Stop()
			}()
		}
		wg.Wait()
	}
}
