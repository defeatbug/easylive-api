// cmd/dbcheck 用一次**真实往返**证明数据层（Ent + sqlc）确实生效。
//
// 为什么需要这个命令：数据层在"数据库不可用"时会被整体跳过（这是刻意的设计——
// 数据库不参与媒体热路径，连不上就只影响业务接口）。代价是：只启动服务的人
// 看不到它做过任何事，很容易以为"根本没接 Ent/sqlc"。
//
// 这个命令把两层的读写各跑一遍，把结果、用到的 sqlc 生成函数名、以及表里
// 真实的计数打印出来——"有没有生效"变成一个可以自己验证的事实。
//
// 用法：
//
//	make db-check
//	# 或
//	DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/sfu_dev?sslmode=disable' go run ./cmd/dbcheck
//
// 默认会在结束时清掉自己造的测试数据；加 -keep 可保留以便手工翻表。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"easylive-api/config"
	"easylive-api/store"
)

func main() {
	keep := flag.Bool("keep", false, "保留测试数据（默认跑完就清掉）")
	dir := flag.String("migrations", "", "迁移目录（默认取 MIGRATIONS_DIR 或 migrations）")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.Load()
	dsn := cfg.PostgresDSN
	if v := os.Getenv("DATABASE_URL"); v != "" {
		dsn = v
	}
	if *dir != "" {
		cfg.MigrationsDir = *dir
	}

	fmt.Println("== 数据层自检（Ent + sqlc）==")
	fmt.Printf("DSN: %s\n", maskDSN(dsn))

	fmt.Println("\n[1/4] 连接数据库")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Printf("❌ 连接失败: %v\n", err)
		if hint := cfg.ComposeHostHint(); hint != "" {
			fmt.Printf("\n提示：%s\n", hint)
		}
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()
	fmt.Println("✅ 连接成功（同一个 pgxpool 同时供 Ent 与 sqlc 使用）")

	fmt.Println("\n[2/4] 执行迁移（migrations/*.sql，幂等 + advisory lock）")
	if err := st.Migrate(ctx, cfg.MigrationsDir); err != nil {
		fmt.Printf("❌ 迁移失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ 迁移完成（服务启动时也会自动执行，见 AUTO_MIGRATE）")

	roomID := fmt.Sprintf("dbcheck-%d", time.Now().Unix())
	meetingID := roomID + "-m1"

	fmt.Println("\n[3/4] Ent 路径：房间与会议记录")
	if err := st.EnsureRoom(ctx, roomID, "dbcheck"); err != nil {
		fail("EnsureRoom", err)
	}
	if err := st.EnsureRoom(ctx, roomID, "dbcheck"); err != nil { // 幂等：再来一次不应报唯一键冲突
		fail("EnsureRoom(重复)", err)
	}
	fmt.Println("  ✅ UpsertRoom（OnConflict 幂等，重复调用不报错）")
	if err := st.RecordMeetingStart(ctx, roomID, meetingID); err != nil {
		fail("RecordMeetingStart", err)
	}
	if err := st.RecordMeetingEnd(ctx, meetingID, 7, 11); err != nil {
		fail("RecordMeetingEnd", err)
	}
	fmt.Println("  ✅ StartMeeting / EndMeeting（含峰值人数 7、峰值轨道 11）")
	stats, err := st.RoomMeetingStats(ctx, roomID)
	if err != nil {
		fail("RoomMeetingStats", err)
	}
	fmt.Printf("  ✅ RoomMeetingStats（Ent 聚合）: 会议数=%d 峰值人数=%d 峰值轨道=%d 总时长=%ds\n",
		stats.Meetings, stats.PeakParticipants, stats.PeakTracks, stats.TotalDurationSec)

	fmt.Println("\n[4/4] sqlc 路径：指标样本与聚合")
	for i := 1; i <= 3; i++ {
		if err := st.RecordMetricSample(ctx, store.MetricSample{
			RoomID: roomID, Participants: i, Tracks: i * 2,
			Packets: int64(i) * 100, Bytes: int64(i) * 1000,
			RTTMs: 12, LossRate: 0.5, OutKbps: 640, InKbps: 1280,
		}); err != nil {
			fail("RecordMetricSample", err)
		}
	}
	fmt.Println("  ✅ sqlc: InsertMetricSample ×3")
	m, err := st.RoomMetrics(ctx, roomID, time.Now().Add(-time.Hour))
	if err != nil {
		fail("RoomMetrics", err)
	}
	fmt.Printf("  ✅ sqlc: RoomMetricsWindow（窗口聚合）: 样本=%d 均人数=%.2f 峰值轨道=%d 总字节=%d 平均RTT=%.2fms\n",
		m.SampleCount, m.AvgParticipants, m.PeakTracks, m.TotalBytes, m.AvgRTTMs)
	buckets, err := st.SampleBuckets(ctx, roomID, time.Hour, time.Now().Add(-time.Hour))
	if err != nil {
		fail("SampleBuckets", err)
	}
	fmt.Printf("  ✅ sqlc: ListSampleBuckets（时间桶，非 TimescaleDB）: %d 个桶\n", len(buckets))
	global, err := st.GlobalMetrics(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		fail("GlobalMetrics", err)
	}
	fmt.Printf("  ✅ sqlc: GlobalMetricsWindow（全局）: 样本=%d 峰值人数=%d 峰值轨道=%d 总字节=%d\n",
		global.SampleCount, global.PeakParticipants, global.PeakTracks, global.TotalBytes)
	pruned, err := st.PruneMetrics(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		fail("PruneMetrics", err)
	}
	fmt.Printf("  ✅ sqlc: DeleteSamplesBefore（保留策略，本次清理 24h 前的 %d 条）\n", pruned)

	if *keep {
		fmt.Printf("\n（-keep）测试数据保留，清理方式：\n  DELETE FROM rooms WHERE id = '%s';\n  DELETE FROM metric_samples WHERE room_id = '%s';\n", roomID, roomID)
	} else {
		if _, err := st.Pool.Exec(ctx, "DELETE FROM rooms WHERE id = $1", roomID); err != nil {
			log.Printf("清理房间失败: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, "DELETE FROM metric_samples WHERE room_id = $1", roomID); err != nil {
			log.Printf("清理指标失败: %v", err)
		}
		fmt.Println("\n🧹 已清理本次测试数据（-keep 可保留）")
	}
	fmt.Println("\n== 结论：Ent 与 sqlc 两层都在这条 DSN 上真实读写了 ==")
}

func fail(what string, err error) {
	fmt.Printf("❌ %s 失败: %v\n", what, err)
	os.Exit(1)
}

// maskDSN 隐藏密码，避免自检输出把口令打进日志/截图。
func maskDSN(dsn string) string {
	out := []rune(dsn)
	for i := 0; i < len(out); i++ {
		if out[i] == ':' && i+1 < len(out) && out[i+1] == '/' && i+2 < len(out) && out[i+2] == '/' {
			// 形如 scheme://user:pass@host —— 回退到 '@' 之前把口令替换掉
			at := -1
			for j := i; j < len(out); j++ {
				if out[j] == '@' {
					at = j
					break
				}
			}
			if at > 0 {
				for j := i + 3; j < at; j++ {
					out[j] = '*'
				}
			}
			break
		}
	}
	return string(out)
}
