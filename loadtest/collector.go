package loadtest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServerSample 是一次服务端健康快照（/healthz）。
type ServerSample struct {
	At           time.Time `json:"at"`
	UptimeSec    int       `json:"uptimeSec"`
	Rooms        int       `json:"rooms"`
	Participants int       `json:"participants"`
	Tracks       int       `json:"tracks"`
	WSConns      int       `json:"wsConns"`
	Goroutines   int       `json:"goroutines"`
	HeapAllocMB  float64   `json:"heapAllocMB"`
	CPUSeconds   float64   `json:"cpuSeconds"`
	// CPUPercent 由相邻两次采样的 cpuSeconds 差值算出（累计值单看没有意义）
	CPUPercent float64 `json:"cpuPercent"`
}

// HealthPoller 轮询 /healthz 并把 cpuSeconds 换算成占用率。
type HealthPoller struct {
	WSURL string

	prevAt  time.Time
	prevCPU float64
}

// Poll 取一次服务端快照。
func (p *HealthPoller) Poll(ctx context.Context) (ServerSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, HTTPBase(p.WSURL)+"/healthz", nil)
	if err != nil {
		return ServerSample{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ServerSample{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var s ServerSample
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return ServerSample{}, err
	}
	s.At = time.Now()

	if !p.prevAt.IsZero() {
		dt := s.At.Sub(p.prevAt).Seconds()
		if dt > 0 && s.CPUSeconds >= p.prevCPU {
			s.CPUPercent = (s.CPUSeconds - p.prevCPU) / dt * 100
		}
	}
	p.prevAt, p.prevCPU = s.At, s.CPUSeconds
	return s, nil
}

// Collector 汇总所有 lane 的样本与服务端快照。
type Collector struct {
	mu      sync.Mutex
	samples []Sample
	server  []ServerSample
}

func (c *Collector) Add(s Sample) {
	c.mu.Lock()
	c.samples = append(c.samples, s)
	c.mu.Unlock()
}

func (c *Collector) AddServer(s ServerSample) {
	c.mu.Lock()
	c.server = append(c.server, s)
	c.mu.Unlock()
}

func (c *Collector) lanes() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Sample{}, c.samples...)
}

func (c *Collector) servers() []ServerSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ServerSample{}, c.server...)
}

// ---------- CSV ----------

func (c *Collector) WriteLanesCSV(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	defer w.Flush()
	header := []string{
		"at", "lane", "identity", "participantId", "connected", "error",
		"rttMs", "jitterMs", "lossRate", "packetsLost", "inKbps", "outKbps",
		"latencyP50Ms", "latencyP95Ms", "latencyMaxMs", "latencySamples",
		"publishTracks", "subscribeTracks",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, s := range c.lanes() {
		row := []string{
			s.At.Format(time.RFC3339Nano), strconv.Itoa(s.Lane), s.Identity, s.ParticipantID,
			strconv.FormatBool(s.Connected), s.Error,
			f2(s.RTTMs), f2(s.JitterMs), f2(s.LossRate), strconv.FormatInt(s.PacketsLost, 10),
			f2(s.InKbps), f2(s.OutKbps),
			f2(s.LatencyP50), f2(s.LatencyP95), f2(s.LatencyMax), strconv.Itoa(s.LatencySamples),
			strconv.Itoa(s.PublishTracks), strconv.Itoa(s.SubscribeTracks),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) WriteServerCSV(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"at", "uptimeSec", "rooms", "participants", "tracks", "wsConns",
		"goroutines", "heapAllocMB", "cpuSeconds", "cpuPercent",
	}); err != nil {
		return err
	}
	for _, s := range c.servers() {
		row := []string{
			s.At.Format(time.RFC3339Nano), strconv.Itoa(s.UptimeSec), strconv.Itoa(s.Rooms),
			strconv.Itoa(s.Participants), strconv.Itoa(s.Tracks), strconv.Itoa(s.WSConns),
			strconv.Itoa(s.Goroutines), f2(s.HeapAllocMB), f4(s.CPUSeconds), f2(s.CPUPercent),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 汇总 ----------

// Stat 是一组数值的分布摘要。用 P50/P95 而不是平均值：
// 实时媒体的体验由尾部决定（平均 30ms 但 P95 到 300ms 就是会卡）。
type Stat struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
	N   int     `json:"n"`
}

func statOf(values []float64) Stat {
	if len(values) == 0 {
		return Stat{}
	}
	s := append([]float64{}, values...)
	sort.Float64s(s)
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	return Stat{
		P50: percentile(s, 0.50),
		P95: percentile(s, 0.95),
		Avg: sum / float64(len(s)),
		Max: s[len(s)-1],
		N:   len(s),
	}
}

// LaneSummary 是单路的汇总。
type LaneSummary struct {
	Lane            int     `json:"lane"`
	Identity        string  `json:"identity"`
	ParticipantID   string  `json:"participantId"`
	Samples         int     `json:"samples"`
	ConnectedRatio  float64 `json:"connectedRatio"`
	Errors          int     `json:"errors"`
	PublishTracks   int     `json:"publishTracks"`
	SubscribeTracks int     `json:"subscribeTracks"`

	RTTMs    Stat `json:"rttMs"`
	JitterMs Stat `json:"jitterMs"`
	LossRate Stat `json:"lossRate"`
	InKbps   Stat `json:"inKbps"`
	OutKbps  Stat `json:"outKbps"`
	Latency  Stat `json:"latencyMs"`
}

// ServerSummary 是服务端资源侧的汇总。
type ServerSummary struct {
	Samples          int     `json:"samples"`
	PeakParticipants int     `json:"peakParticipants"`
	PeakTracks       int     `json:"peakTracks"`
	PeakWSConns      int     `json:"peakWsConns"`
	PeakGoroutines   int     `json:"peakGoroutines"`
	PeakHeapAllocMB  float64 `json:"peakHeapAllocMB"`
	CPUPercentAvg    float64 `json:"cpuPercentAvg"`
	CPUPercentMax    float64 `json:"cpuPercentMax"`
}

// Summary 是一次运行的完整汇总，会被写成 JSON 与 Markdown 两种形态。
type Summary struct {
	Room        string         `json:"room"`
	Lanes       int            `json:"lanes"`
	DurationSec float64        `json:"durationSec"`
	StartedAt   time.Time      `json:"startedAt"`
	EndedAt     time.Time      `json:"endedAt"`
	Targets     map[string]any `json:"targets"`

	PerLane []LaneSummary `json:"perLane"`
	// Overall 是所有 lane 的样本混在一起算的分布（"系统整体"视角）
	Overall struct {
		RTTMs    Stat `json:"rttMs"`
		JitterMs Stat `json:"jitterMs"`
		LossRate Stat `json:"lossRate"`
		InKbps   Stat `json:"inKbps"`
		OutKbps  Stat `json:"outKbps"`
		Latency  Stat `json:"latencyMs"`
	} `json:"overall"`
	Server ServerSummary `json:"server"`
}

// BuildSummary 把样本折叠成汇总。
func (c *Collector) BuildSummary(o Options, lanes int, startedAt, endedAt time.Time) Summary {
	sum := Summary{
		Room:        o.RoomID,
		Lanes:       lanes,
		DurationSec: endedAt.Sub(startedAt).Seconds(),
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		Targets: map[string]any{
			"videoKbpsPerLane": o.VideoKbps,
			"audioKbpsPerLane": o.AudioKbps,
			"videoFps":         o.VideoFPS,
			"subscribe":        o.Subscribe,
		},
	}

	byLane := map[int][]Sample{}
	var allRTT, allJitter, allLoss, allIn, allOut, allLat []float64
	for _, s := range c.lanes() {
		byLane[s.Lane] = append(byLane[s.Lane], s)
		if s.Connected {
			allRTT = append(allRTT, s.RTTMs)
			allJitter = append(allJitter, s.JitterMs)
			allLoss = append(allLoss, s.LossRate)
		}
		allIn = append(allIn, s.InKbps)
		allOut = append(allOut, s.OutKbps)
		if s.LatencySamples > 0 {
			allLat = append(allLat, s.LatencyP50)
		}
	}

	idxs := make([]int, 0, len(byLane))
	for k := range byLane {
		idxs = append(idxs, k)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		ss := byLane[idx]
		ls := LaneSummary{Lane: idx, Samples: len(ss)}
		if len(ss) > 0 {
			ls.Identity = ss[0].Identity
			ls.ParticipantID = ss[0].ParticipantID
			ls.PublishTracks = ss[0].PublishTracks
		}
		var rtt, jit, loss, in, out, lat []float64
		connected := 0
		for _, s := range ss {
			if s.Connected {
				connected++
				rtt = append(rtt, s.RTTMs)
				jit = append(jit, s.JitterMs)
				loss = append(loss, s.LossRate)
			}
			if s.Error != "" {
				ls.Errors++
			}
			in = append(in, s.InKbps)
			out = append(out, s.OutKbps)
			if s.LatencySamples > 0 {
				lat = append(lat, s.LatencyP50)
			}
			if s.SubscribeTracks > ls.SubscribeTracks {
				ls.SubscribeTracks = s.SubscribeTracks
			}
		}
		if len(ss) > 0 {
			ls.ConnectedRatio = float64(connected) / float64(len(ss))
		}
		ls.RTTMs = statOf(rtt)
		ls.JitterMs = statOf(jit)
		ls.LossRate = statOf(loss)
		ls.InKbps = statOf(in)
		ls.OutKbps = statOf(out)
		ls.Latency = statOf(lat)
		sum.PerLane = append(sum.PerLane, ls)
	}

	sum.Overall.RTTMs = statOf(allRTT)
	sum.Overall.JitterMs = statOf(allJitter)
	sum.Overall.LossRate = statOf(allLoss)
	sum.Overall.InKbps = statOf(allIn)
	sum.Overall.OutKbps = statOf(allOut)
	sum.Overall.Latency = statOf(allLat)

	srv := c.servers()
	sum.Server.Samples = len(srv)
	var cpu []float64
	for _, s := range srv {
		if s.Participants > sum.Server.PeakParticipants {
			sum.Server.PeakParticipants = s.Participants
		}
		if s.Tracks > sum.Server.PeakTracks {
			sum.Server.PeakTracks = s.Tracks
		}
		if s.WSConns > sum.Server.PeakWSConns {
			sum.Server.PeakWSConns = s.WSConns
		}
		if s.Goroutines > sum.Server.PeakGoroutines {
			sum.Server.PeakGoroutines = s.Goroutines
		}
		if s.HeapAllocMB > sum.Server.PeakHeapAllocMB {
			sum.Server.PeakHeapAllocMB = s.HeapAllocMB
		}
		if s.CPUPercent > 0 {
			cpu = append(cpu, s.CPUPercent)
		}
	}
	if len(cpu) > 0 {
		st := statOf(cpu)
		sum.Server.CPUPercentAvg = st.Avg
		sum.Server.CPUPercentMax = st.Max
	}
	return sum
}

// WriteJSON 落盘汇总（给脚本/图表用，也是"可复现"的凭据）。
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Markdown 生成人读的汇总表（可直接贴进论文的实验章节草稿）。
func (s Summary) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 压测结果：%s\n\n", s.Room)
	fmt.Fprintf(&b, "- 路数：%d，时长：%.1fs（%s → %s）\n",
		s.Lanes, s.DurationSec, s.StartedAt.Format(time.RFC3339), s.EndedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- 每路目标：视频 %v kbps / 音频 %v kbps / %v fps，订阅：%v\n\n",
		s.Targets["videoKbpsPerLane"], s.Targets["audioKbpsPerLane"], s.Targets["videoFps"], s.Targets["subscribe"])

	b.WriteString("## 整体（所有采样点混算）\n\n")
	b.WriteString("| 指标 | P50 | P95 | 平均 | 最大 | 样本数 |\n|---|---|---|---|---|---|\n")
	row := func(name string, st Stat) {
		fmt.Fprintf(&b, "| %s | %.1f | %.1f | %.1f | %.1f | %d |\n", name, st.P50, st.P95, st.Avg, st.Max, st.N)
	}
	row("RTT (ms)", s.Overall.RTTMs)
	row("抖动 (ms)", s.Overall.JitterMs)
	row("丢包率 (%)", s.Overall.LossRate)
	row("下行码率 (kbps/路)", s.Overall.InKbps)
	row("上行码率 (kbps/路)", s.Overall.OutKbps)
	row("单向延迟 P50 (ms)", s.Overall.Latency)

	b.WriteString("\n## 服务端资源\n\n")
	b.WriteString("| 指标 | 值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 峰值在线人数 | %d |\n", s.Server.PeakParticipants)
	fmt.Fprintf(&b, "| 峰值轨道数 | %d |\n", s.Server.PeakTracks)
	fmt.Fprintf(&b, "| 峰值 WS 连接 | %d |\n", s.Server.PeakWSConns)
	fmt.Fprintf(&b, "| 峰值 goroutine 数 | %d |\n", s.Server.PeakGoroutines)
	fmt.Fprintf(&b, "| 峰值堆内存 (MB) | %.1f |\n", s.Server.PeakHeapAllocMB)
	fmt.Fprintf(&b, "| CPU 占用（平均 / 峰值，%%） | %.1f / %.1f |\n", s.Server.CPUPercentAvg, s.Server.CPUPercentMax)

	b.WriteString("\n## 分路\n\n")
	b.WriteString("| 路 | 身份 | 采样 | 连接率 | RTT P95 | 丢包 P95 | 延迟 P50 | 上行 P95 | 下行 P95 | 错误 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, l := range s.PerLane {
		fmt.Fprintf(&b, "| %d | %s | %d | %.0f%% | %.1f | %.2f | %.1f | %.0f | %.0f | %d |\n",
			l.Lane, l.Identity, l.Samples, l.ConnectedRatio*100,
			l.RTTMs.P95, l.LossRate.P95, l.Latency.P50, l.OutKbps.P95, l.InKbps.P95, l.Errors)
	}

	b.WriteString("\n> 说明：本表来自合成压测客户端（只收发 RTP，不编码/解码），因此反映的是**服务端转发与信令开销**；\n")
	b.WriteString("> 主观画质与端侧解码耗时必须用真实浏览器另测。\n")
	return b.String()
}

func f2(v float64) string { return strconv.FormatFloat(round2(v), 'f', -1, 64) }
func f4(v float64) string { return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64) }
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// HTTPBase 把 ws URL 转成对应的 HTTP 基地址（导出给 CLI 用）。
func HTTPBase(wsURL string) string { return httpBase(wsURL) }
