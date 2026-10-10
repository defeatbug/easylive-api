// Package sysinfo 读取进程自身的资源用量（目前只有 CPU 时间）。
//
// 为什么放在这里而不是直接用 /proc：本项目的部署目标是 Linux，
// 但开发机是 macOS，而 syscall.Getrusage 在两者上都可用（POSIX），
// 于是同一份代码在本地跑冒烟测试、在服务器上跑正式评测都成立。
package sysinfo

import "syscall"

// CPUSeconds 返回进程累计消耗的 CPU 时间（用户态 + 内核态，单位秒）。
//
// 采样方用它算 CPU 占用率：两个时刻的差值 ÷ 墙上时间。
// 单看一个瞬时值没有意义——CPU 时间只会单调增长。
func CPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return timevalSeconds(ru.Utime) + timevalSeconds(ru.Stime)
}

// timevalSeconds 把 syscall.Timeval 转成秒。
// 单独抽出来是因为 Darwin 与 Linux 的 Timeval 字段名虽然一致，
// 但类型宽度不同（int32/int64），用 Sec/Usec 取值对两边都成立。
func timevalSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}
