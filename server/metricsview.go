package server

import (
	"context"
	"sort"
	"time"

	"easylive-api/store"
)

// HistoryPoint 是曲线上的一个点（在 store.SampleBucket 之上补了"速率"）。
//
// 为什么必须补速率：metric_samples 里存的是**累计**字节数（内存计数的快照），
// 直接画成曲线会单调上升、看着像"流量一直在涨"，那是误导。速率才是要展示的东西。
type HistoryPoint struct {
	store.SampleBucket
	Kbps float64 `json:"kbps"`
}

// mergeBuckets 把多个房间的时间桶合并成一条全局曲线。
//
// 口径说明（写进论文要交代清楚）：
//   - participants / tracks：跨房间**求和**，语义明确（"此刻共有多少人在会"）；
//   - kbps：**按房间先算速率、再跨房间求和**。速率可加，累计值不可加——
//     曾经先求和再求差，结果出现 6.6MB→4.4MB→6.6MB 的震荡：
//     各房间生命周期不同，某个桶里少一个房间，累计和的差就会跳。
//   - rtt / loss：各房间均值再取均值；房间样本数差异大时会有偏差，更准需要加权（后续工作）。
//   - 注意 rtt_ms / loss_rate 这两列**当前从未被采样**（采样器只写累计包数/字节），
//     所以恒为 0。报表页据此显式标注"未采样"，而不是画一条看着像真的平线。
func mergeBuckets(ctx context.Context, st *store.Store, roomIDs []string, bucket time.Duration, since time.Time) ([]HistoryPoint, error) {
	type acc struct {
		at           time.Time
		participants float64
		tracks       float64
		rttSum       float64
		lossSum      float64
		rttRooms     int
		lossRooms    int
		totalBytes   int64
		kbps         float64
	}
	merged := map[int64]*acc{}

	for _, roomID := range roomIDs {
		rows, err := st.SampleBuckets(ctx, roomID, bucket, since)
		if err != nil {
			return nil, err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Bucket.Before(rows[j].Bucket) })

		for i, r := range rows {
			a := merged[r.Bucket.Unix()]
			if a == nil {
				a = &acc{at: r.Bucket}
				merged[r.Bucket.Unix()] = a
			}
			a.participants += r.AvgParticipants
			a.tracks += r.AvgTracks
			a.totalBytes += r.TotalBytes
			a.rttSum += r.AvgRTTMs
			a.rttRooms++
			a.lossSum += r.AvgLossRate
			a.lossRooms++

			if i > 0 {
				// 除以**实际**时间差，而不是名义桶宽：只含有样本的桶会被保留，
				// 于是相邻两行之间可能跨了若干个空桶。用桶宽会凭空放大速率
				// （实测表现为 3505/0 交替），用真实时间差才如实。
				// 负增量按 0 处理：房间关闭会让累计值回退，宁可少画一段也不画出负速率。
				dt := r.Bucket.Sub(rows[i-1].Bucket).Seconds()
				if delta := r.TotalBytes - rows[i-1].TotalBytes; delta > 0 && dt > 0 {
					a.kbps += float64(delta) * 8 / dt / 1000
				}
			}
		}
	}

	out := make([]HistoryPoint, 0, len(merged))
	for _, a := range merged {
		row := HistoryPoint{
			SampleBucket: store.SampleBucket{
				Bucket:          a.at,
				AvgParticipants: a.participants,
				AvgTracks:       a.tracks,
				TotalBytes:      a.totalBytes,
			},
			Kbps: a.kbps,
		}
		if a.rttRooms > 0 {
			row.AvgRTTMs = a.rttSum / float64(a.rttRooms)
		}
		if a.lossRooms > 0 {
			row.AvgLossRate = a.lossSum / float64(a.lossRooms)
		}
		out = append(out, row)
	}
	// 时间升序：图表直接按下标连线，不需要前端再排一次
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket.Before(out[j].Bucket) })
	return out, nil
}
