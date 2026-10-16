package sfu

import (
	"testing"

	"github.com/pion/interceptor"

	"easylive-api/protocol"
	"github.com/pion/rtcp"
	"time"
)

// TestRequestKeyframeThrottle 验证"订阅端要求关键帧"的限流。
//
// 为什么这件事必须测：关键帧体积远大于普通帧，如果每个订阅端的 PLI 都无脑转发，
// N 个订阅者（或一个坏客户端刷 PLI）就能把发布端逼成"一直在编关键帧"——
// 为了救一个人把所有人的画质都拖下去。限流是这条链路的保险丝。
func TestRequestKeyframeThrottle(t *testing.T) {
	before, beforeSuppressed := FrameRequestStats()

	tr := &PublishedTrack{}
	// 没有 pubPC/remote 时 RequestKeyframe 会安全返回（不 panic），
	// 但限流状态与计数照常推进，正好用来单测限流语义。
	if tr.RequestKeyframeThrottled() != true {
		t.Fatal("第一次请求应当放行")
	}
	for i := 0; i < 5; i++ {
		if tr.RequestKeyframeThrottled() {
			t.Fatalf("限流窗口内的第 %d 次请求应当被压掉", i+2)
		}
	}
	if got := tr.lastPLINs.Load(); got == 0 {
		t.Fatal("限流时间戳应当被写入")
	}

	// 人为把时间戳拨回窗口之前，应当重新放行
	tr.lastPLINs.Store(time.Now().Add(-2 * keyframeInterval).UnixNano())
	if !tr.RequestKeyframeThrottled() {
		t.Fatal("超出限流窗口后应当重新放行")
	}

	after, afterSuppressed := FrameRequestStats()
	if after-before != 2 {
		t.Fatalf("转发计数应为 2，实际 %d", after-before)
	}
	if afterSuppressed-beforeSuppressed != 5 {
		t.Fatalf("压掉计数应为 5，实际 %d", afterSuppressed-beforeSuppressed)
	}
}

// TestRememberSSRC 验证 SSRC 查表的新增/删除（PLI 只带 SSRC，靠它找回轨道）。
func TestRememberSSRC(t *testing.T) {
	tr := &PublishedTrack{}
	rememberSSRC(12345, tr)
	v, ok := trackBySSRC.Load(uint32(12345))
	if !ok || v.(*PublishedTrack) != tr {
		t.Fatal("SSRC 应当能查到对应轨道")
	}
	forgetSSRC(12345)
	if _, ok := trackBySSRC.Load(uint32(12345)); ok {
		t.Fatal("忘记后不应再查到")
	}
	// SSRC 0 是"尚未收到包"的占位值，不能进表（否则会互相覆盖）
	rememberSSRC(0, tr)
	if _, ok := trackBySSRC.Load(uint32(0)); ok {
		t.Fatal("SSRC 0 不应登记")
	}
}

// TestKeyframeForwarderForwardsPLI 验证拦截器真的会把订阅端的 PLI 转发给发布者。
//
// 这条测试针对一个实际踩过的坑：最初只登记了"发布者远端 SSRC"，而订阅端 PLI 里带的
// 是**服务端发出去的流**的 SSRC，两者不同 → 查表落空 → PLI 被静默忽略（实测转发 0 条）。
// 所以这里显式用一个"出向"SSRC 做映射，并断言计数器确实动了。
func TestKeyframeForwarderForwardsPLI(t *testing.T) {
	// info 必须设置：转发成功时会打日志用它（生产里轨道一定带 info）
	tr := &PublishedTrack{info: &protocol.TrackInfo{ID: "test/track"}}
	const outSSRC = 4242424242
	rememberSSRC(outSSRC, tr)
	defer forgetSSRC(outSSRC)

	forwarded0, _ := FrameRequestStats()

	f := &keyframeForwarder{}
	base := interceptor.RTCPReaderFunc(func(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		return len(b), nil, nil
	})
	reader := f.BindRTCPReader(base)

	// 注意：包级 rtcp.Marshal 对 PLI 这类反馈包会直接 panic（只支持 SR/RR/SDES/BYE 等），
	// 必须用各包自己的 Marshal 方法。
	pli, err := (&rtcp.PictureLossIndication{SenderSSRC: 1, MediaSSRC: outSSRC}).Marshal()
	if err != nil {
		t.Fatalf("构造 PLI 失败: %v", err)
	}
	if _, _, err := reader.Read(pli, nil); err != nil {
		t.Fatalf("拦截器读取失败: %v", err)
	}

	forwarded1, _ := FrameRequestStats()
	if forwarded1-forwarded0 != 1 {
		t.Fatalf("PLI 应当被转发一次，实际 %d", forwarded1-forwarded0)
	}

	// 未知 SSRC 不应触发任何转发
	unknown, err2 := (&rtcp.PictureLossIndication{MediaSSRC: 999}).Marshal()
	if err2 != nil {
		t.Fatalf("构造 PLI 失败: %v", err2)
	}
	if _, _, err := reader.Read(unknown, nil); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	forwarded2, _ := FrameRequestStats()
	if forwarded2 != forwarded1 {
		t.Fatalf("未知 SSRC 不应转发，实际又多了 %d 次", forwarded2-forwarded1)
	}

	// 畸形 RTCP 不能 panic，也不能吞掉包（照常放行）
	if n, _, err := reader.Read([]byte{0xff, 0x00, 0x01}, nil); err != nil || n != 3 {
		t.Fatalf("畸形 RTCP 应照常放行，n=%d err=%v", n, err)
	}
}
