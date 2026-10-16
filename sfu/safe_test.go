package sfu

import (
	"sync"
	"testing"
	"time"
)

// TestSafeGoIsolatesPanic 验证"一个协程 panic 不会带走进程"。
//
// 这是本项目最实际的可用性需求之一：6 个长驻协程（转发热路径、RTCP 排水、
// 说话人循环、关键帧看门狗、写协程）以前一个 recover 都没有。
// 实测踩过一次：pion/rtcp 对畸形输入 panic，而数据完全来自客户端——
// 几个字节的垃圾就能让所有人的会议一起下线。隔离后最坏只是这一路失效。
func TestSafeGoIsolatesPanic(t *testing.T) {
	before := GoroutinePanicCount()

	var wg sync.WaitGroup
	wg.Add(2)
	safeGo("test-panic", func() {
		defer wg.Done()
		panic("故意 panic：验证隔离")
	})
	safeGo("test-ok", func() {
		defer wg.Done()
		// 正常协程照常跑完
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("协程没有在预期时间内结束")
	}

	// panic 被计数（说明 recover 生效，进程还活着）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if GoroutinePanicCount() > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if GoroutinePanicCount() <= before {
		t.Fatal("panic 应当被计数（说明 recover 生效）")
	}
}

// TestSafeGoRunsDefers 验证 panic 时**函数自己的 defer 仍然执行**。
//
// 这一点很关键：如果隔离把 defer 也跳过了，锁和资源就会泄漏，
// 那么"隔离一个 panic"会演变成"慢慢拖垮进程"，比直接崩溃更糟。
func TestSafeGoRunsDefers(t *testing.T) {
	released := make(chan struct{})
	safeGo("test-defer", func() {
		defer close(released)
		panic("验证 defer 仍执行")
	})
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("panic 时函数自己的 defer 必须照常执行")
	}
}
