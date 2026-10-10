// 连库集成测试：默认跳过，需要真实 PostgreSQL。
//
// 为什么默认跳过：本项目的媒体面与会话状态都在内存里，数据库只存元数据，
// 所以"没有数据库"是一种合法的开发状态；不能让 go test 因此失败。
// 在有 Postgres 的机器（或部署服务器）上这样跑：
//
//	DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/sfu_dev?sslmode=disable' \
//	  go test ./store/ -run Integration -v
//
// 该测试会先执行 migrations/*.sql（幂等），再走一遍房间与会议记录的完整生命周期。
package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// applyMigrations 直接复用生产代码里的迁移执行器。
//
// 这里刻意不再自己拼 SQL：迁移文件是多条语句的，pgx 默认的扩展协议
// （预编译语句）不允许一个查询里有多条命令，必须走简单协议——
// 让测试跑生产同一条路径，才能验证这件事真的做对了。
func applyMigrations(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	if err := st.Migrate(ctx, "../migrations"); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
}

func TestStoreIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 DATABASE_URL，跳过连库测试（本机没有 Postgres 属正常）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	applyMigrations(t, ctx, st)

	roomID := "itest-room-" + time.Now().Format("150405.000")
	meetingID := roomID + "-m1"

	// ---- 房间 upsert 幂等：第二次应更新标题而不是报唯一键冲突 ----
	if _, err := st.UpsertRoom(ctx, roomID, "初始标题", "itest"); err != nil {
		t.Fatalf("首次 upsert 房间失败: %v", err)
	}
	first, err := st.GetRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("查询房间失败: %v", err)
	}
	createdAt := first.CreatedAt

	time.Sleep(10 * time.Millisecond)
	if _, err := st.UpsertRoom(ctx, roomID, "改过的标题", "itest"); err != nil {
		t.Fatalf("二次 upsert（冲突路径）失败: %v", err)
	}
	updated, err := st.GetRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("再次查询房间失败: %v", err)
	}
	if updated.Title != "改过的标题" {
		t.Fatalf("标题应被更新，实际 %q", updated.Title)
	}
	if !updated.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at 是 Immutable 的，冲突时不应被覆盖：%v → %v", createdAt, updated.CreatedAt)
	}

	// ---- 会议记录生命周期 ----
	if _, err := st.StartMeeting(ctx, roomID, meetingID); err != nil {
		t.Fatalf("创建会议记录失败: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // 让 duration_sec 至少为 1
	ended, err := st.EndMeeting(ctx, meetingID, 5, 7)
	if err != nil {
		t.Fatalf("收尾会议记录失败: %v", err)
	}
	if ended.DurationSec < 1 {
		t.Fatalf("时长应至少 1 秒，实际 %d", ended.DurationSec)
	}
	if ended.PeakParticipants != 5 || ended.PeakTracks != 7 {
		t.Fatalf("峰值未写入: %+v", ended)
	}
	if ended.EndedAt == nil {
		t.Fatalf("结束时间未写入")
	}

	list, err := st.ListMeetings(ctx, roomID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("应查到 1 条会议记录，实际 %d 条（err=%v）", len(list), err)
	}

	stats, err := st.RoomMeetingStats(ctx, roomID)
	if err != nil {
		t.Fatalf("聚合统计失败: %v", err)
	}
	if stats.Meetings != 1 || stats.PeakTracks != 7 || stats.PeakParticipants != 5 {
		t.Fatalf("聚合结果不正确: %+v", stats)
	}

	// ---- 关闭房间：应把未结束的会议一并收尾 ----
	if _, err := st.StartMeeting(ctx, roomID, meetingID+"-m2"); err != nil {
		t.Fatalf("创建第二条会议记录失败: %v", err)
	}
	closed, err := st.CloseRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("关闭房间失败: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Fatalf("closed_at 未写入")
	}
	n, err := st.EndOpenMeetings(ctx, roomID)
	if err != nil {
		t.Fatalf("收尾未结束会议失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("CloseRoom 应已收尾全部未结束会议，这里应剩 0 条，实际 %d", n)
	}
}

// TestStoreMetricsIntegration 覆盖 sqlc 那一半：写入 + 窗口聚合 + 时间桶 + 保留策略。
//
// 与上面的用例分开：它验证的是"指标时序由 sqlc 负责"这条边界，
// 失败时能一眼看出是 数据层 的哪一半出问题。
func TestStoreMetricsIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 DATABASE_URL，跳过连库测试（本机没有 Postgres 属正常）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	applyMigrations(t, ctx, st)

	roomID := "itest-metrics-" + time.Now().Format("150405.000")

	// 写 3 条样本：人数/轨道递增，RTT 与丢包固定，便于断言均值
	for i := 1; i <= 3; i++ {
		if err := st.RecordMetricSample(ctx, MetricSample{
			RoomID: roomID, Participants: i, Tracks: i * 2,
			Packets: int64(i) * 100, Bytes: int64(i) * 1000,
			RTTMs: 40, LossRate: 1.5, OutKbps: 500, InKbps: 1500,
		}); err != nil {
			t.Fatalf("写入第 %d 条样本失败: %v", i, err)
		}
	}

	m, err := st.RoomMetrics(ctx, roomID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("聚合房间指标失败: %v", err)
	}
	if m.SampleCount != 3 {
		t.Fatalf("样本数应为 3，实际 %d", m.SampleCount)
	}
	if m.PeakParticipants != 3 || m.PeakTracks != 6 {
		t.Fatalf("峰值应分别为 3 / 6，实际 %d / %d", m.PeakParticipants, m.PeakTracks)
	}
	if m.AvgParticipants < 1.9 || m.AvgParticipants > 2.1 {
		t.Fatalf("平均人数应约为 2，实际 %.2f", m.AvgParticipants)
	}
	if m.TotalBytes != 6000 {
		t.Fatalf("累计流量应为 6000，实际 %d", m.TotalBytes)
	}

	// 时间桶：桶宽取 1 小时，3 条样本（刚才写入的）应落在同一个桶里
	buckets, err := st.SampleBuckets(ctx, roomID, time.Hour, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("聚合时间桶失败: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("1 小时桶应只有 1 个，实际 %d", len(buckets))
	}
	if buckets[0].Bucket.IsZero() {
		t.Fatalf("时间桶应带有效时间戳")
	}
	if buckets[0].TotalBytes != 6000 {
		t.Fatalf("桶内累计流量应为 6000，实际 %d", buckets[0].TotalBytes)
	}

	// 保留策略：删掉"未来时间点之前"的全部样本，应删掉刚写入的 3 条
	deleted, err := st.PruneMetrics(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("清理过期样本失败: %v", err)
	}
	if deleted < 3 {
		t.Fatalf("应至少删除 3 条，实际 %d", deleted)
	}
}
