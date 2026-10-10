// cmd/loadtest 是合成压测入口：在一台机器上模拟 N 路参与者，采集链路与服务器指标。
//
// 用法示例（本地）：
//
//	go run ./cmd/loadtest -ws ws://127.0.0.1:8080/ws -room bench -lanes 4 -duration 20s
//
// 输出目录（默认 results/<时间戳>-<tag>/）：
//
//	lanes.csv     每路每秒一行（RTT/抖动/丢包/码率/单向延迟）
//	server.csv    服务端每秒一行（在线人数/轨道/goroutine/堆内存/CPU）
//	summary.json  汇总（脚本与图表的数据源）
//	summary.md    人读的汇总表（可直接贴进论文草稿）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"easylive-api/loadtest"
)

func main() {
	var (
		wsURL     = flag.String("ws", "ws://127.0.0.1:8080/ws", "信令地址")
		room      = flag.String("room", "bench-room", "房间号")
		lanes     = flag.Int("lanes", 4, "并发路数")
		duration  = flag.Duration("duration", 20*time.Second, "压测时长（不含建连时间）")
		videoKbps = flag.Int("video-kbps", 1500, "每路视频目标码率 kbps（0 表示不发布视频）")
		audioKbps = flag.Int("audio-kbps", 64, "每路音频目标码率 kbps（0 表示不发布音频）")
		fps       = flag.Int("fps", 30, "视频帧率")
		subscribe = flag.Bool("subscribe", true, "是否订阅其他人的轨道（false 只发不收）")
		outDir    = flag.String("out", "results", "结果根目录")
		tag       = flag.String("tag", "", "本次运行标签（用于目录名，如 20lanes-720p）")
		prefix    = flag.String("identity", "load", "身份前缀")
		stagger   = flag.Duration("stagger", 150*time.Millisecond, "每路之间的启动间隔（避免惊群）")
		debug     = flag.Bool("debug", false, "打印协商细节（扩展 ID / payload type / extmap）")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runID := time.Now().Format("20060102-150405")
	if *tag != "" {
		runID += "-" + *tag
	}
	dir := filepath.Join(*outDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("创建结果目录失败: %v", err)
	}

	// ICE 配置与令牌只取一次，所有路共用：否则 N 路会打 N 次 /ice（还可能各签一份凭证）
	ice, err := loadtest.FetchICE(ctx, *wsURL, *prefix)
	if err != nil {
		log.Printf("loadtest: 拉取 /ice 失败（将只用默认 STUN）: %v", err)
	}
	token := loadtest.FetchToken(ctx, *wsURL, *room, *prefix+"-00")
	if token != "" {
		log.Printf("loadtest: 已获取访问令牌")
	}

	opts := loadtest.Options{
		WSURL: *wsURL, RoomID: *room, Identity: *prefix, Token: token,
		VideoKbps: *videoKbps, AudioKbps: *audioKbps, VideoFPS: *fps,
		Subscribe: *subscribe, ICE: ice, Debug: *debug,
	}

	collector := &loadtest.Collector{}
	srvStop := make(chan struct{})
	var srvWG sync.WaitGroup

	// 服务端资源采样：与媒体指标同步进行，便于把"路数 → CPU/内存"对上
	srvWG.Add(1)
	go func() {
		defer srvWG.Done()
		poller := &loadtest.HealthPoller{WSURL: *wsURL}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-srvStop:
				return
			case <-ticker.C:
				s, perr := poller.Poll(ctx)
				if perr != nil {
					continue
				}
				collector.AddServer(s)
			}
		}
	}()

	startedAt := time.Now()
	log.Printf("loadtest: 开始 room=%s lanes=%d duration=%s video=%dkbps/路 audio=%dkbps/路 subscribe=%v",
		*room, *lanes, *duration, *videoKbps, *audioKbps, *subscribe)

	var wg sync.WaitGroup
	for i := 0; i < *lanes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			loadtest.RunLane(ctx, opts, idx, collector.Add)
		}(i)
		// 错峰启动：真实会议里也不是所有人同时进房
		time.Sleep(*stagger)
	}

	// 跑满时长（或外部中断）
	select {
	case <-time.After(*duration):
	case <-ctx.Done():
		log.Printf("loadtest: 收到中断，提前结束")
	}
	stop() // 通知所有 lane 退出（它们会发 leave 并关闭连接）
	wg.Wait()

	close(srvStop)
	srvWG.Wait()
	endedAt := time.Now()

	// ---- 落盘 ----
	if err := collector.WriteLanesCSV(filepath.Join(dir, "lanes.csv")); err != nil {
		log.Printf("loadtest: 写 lanes.csv 失败: %v", err)
	}
	if err := collector.WriteServerCSV(filepath.Join(dir, "server.csv")); err != nil {
		log.Printf("loadtest: 写 server.csv 失败: %v", err)
	}
	summary := collector.BuildSummary(opts, *lanes, startedAt, endedAt)
	if err := loadtest.WriteJSON(filepath.Join(dir, "summary.json"), summary); err != nil {
		log.Printf("loadtest: 写 summary.json 失败: %v", err)
	}
	md := summary.Markdown()
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(md), 0o644); err != nil {
		log.Printf("loadtest: 写 summary.md 失败: %v", err)
	}

	fmt.Println()
	fmt.Println(md)
	fmt.Printf("\n结果目录: %s\n", dir)
}
