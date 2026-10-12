// rtpfix 是录制缺陷的**离线调试工具**：读取 .rtp 原始转储，
// 打印统计（帧拼装、时间戳推进、丢包模式），并用与线上相同的写入器重放成 IVF/Ogg。
//
// 用法：
//
//	rtpfix <dump.rtp> <out.ivf|out.ogg>
//
// 然后直接 ffmpeg 校验产物：ffmpeg -v error -i out.ivf -f null -
// 这样每次假设验证只需几秒，不必重启服务、开浏览器、等宽限期。
package main

import (
	"fmt"
	"os"

	"easylive-api/mediarec"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法: rtpfix <dump.rtp> <out.ivf|out.ogg>")
		os.Exit(2)
	}
	dumpPath, outPath := os.Args[1], os.Args[2]
	h, packets, times, err := mediarec.ReadRawDump(dumpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取转储失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("转储: %s\n  编码=%s 时钟=%d pt=%d ssrc=%d 房间=%s 参与者=%s 来源=%s(%s)\n",
		dumpPath, h.Codec, h.ClockRate, h.PayloadType, h.SSRC, h.Room, h.Participant, h.Source, h.Kind)
	fmt.Printf("  包数=%d 时长≈%s\n", len(packets), times[len(times)-1].Round(100*1e6))

	stats, err := mediarec.AnalyzeDump(dumpPath, outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "分析失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("输出: %s\n%s", outPath, stats.String())
	for _, d := range stats.FrameDetail {
		fmt.Println("  " + d)
	}
}
