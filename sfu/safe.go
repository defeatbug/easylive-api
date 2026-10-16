package sfu

import (
	"log"
	"runtime/debug"
	"sync/atomic"
)

// goroutinePanics 统计被隔离的 panic（观测用）。
var goroutinePanics atomic.Uint64

// GoroutinePanicCount 返回被隔离的 panic 次数。
//
// 这个数字**不该长期大于 0**：它表示有一路连接或轨道出了 bug。但它大于 0
// 与"整个服务挂掉"是两回事——这正是隔离的意义。
func GoroutinePanicCount() uint64 { return goroutinePanics.Load() }

// safeGo 启动一个长驻 goroutine，并保证它 panic 时**不会带走整个进程**。
//
// 为什么必须有：SFU 里每条连接、每条轨道都有若干长驻协程——RTCP 排水、转发热路径、
// 说话人循环、关键帧看门狗。它们中的任何一个 panic 都会终止整个进程，
// 把**所有人的会议**一起打掉。实测踩过一次：pion/rtcp 对畸形输入会 panic，
// 而那段数据完全来自客户端——几个字节的垃圾就能让服务全线不可用。
//
// 隔离之后，最坏情况只是这一路连接/轨道失效（客户端会自动重连并恢复媒体），
// 其他人的会议不受影响。注意 fn 内部自己的 defer 仍然会正常执行
// （panic 在栈展开时就会跑完它们），所以资源和锁不会因此泄漏。
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				goroutinePanics.Add(1)
				log.Printf("sfu: panic 已隔离 component=%s err=%v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// SafeGo 是 safeGo 的导出形式，供 signaling 包复用（写协程同样需要隔离）。
func SafeGo(name string, fn func()) { safeGo(name, fn) }
