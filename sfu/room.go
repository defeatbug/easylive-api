package sfu

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"easylive-api/mediarec"
	"easylive-api/protocol"
)

// 活跃说话人广播参数
const (
	activeSpeakerInterval = 300 * time.Millisecond // 采样周期
	activeSpeakerMax      = 3                      // 一次最多报几个人（超过这个数没有展示意义）
)

// Manager 管理所有房间。单节点内存态：会话与媒体状态不落外部存储（见 ADR D8）。
type Manager struct {
	engine          *Engine
	maxParticipants int
	resumeGrace     time.Duration
	// recorder 非 nil 时开启录制：每条轨道多写一份到磁盘（见 mediarec 包）。
	// 它对转发是"旁路"：写队列满就丢帧，绝不阻塞媒体。
	recorder *mediarec.Manager

	mu    sync.RWMutex
	rooms map[string]*Room
}

// EnableRecording 打开会议录制（传 nil 表示关闭）。
func (m *Manager) EnableRecording(rec *mediarec.Manager) {
	m.mu.Lock()
	m.recorder = rec
	m.mu.Unlock()
}

// Recording 返回当前录制器（可能为 nil）。
func (m *Manager) Recording() *mediarec.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recorder
}

// ---------- 运行时录制控制（主持人触发）----------

// ErrRecordingDisabled 表示服务端没有开启录制能力。
var ErrRecordingDisabled = fmt.Errorf("服务端未开启录制（RECORD_ENABLED=false）")

// StartRecording 开始录制一个房间。
//
// 关键点：**已经在发布的轨道也要挂上**——主持人是会中才点录制的，
// 那时候摄像头/麦克风早就在推流了，只给"之后发布的轨道"挂写入器会漏掉所有人的画面。
//
// 幂等性按房间判定：同一房间重复开始返回已有会话，不同房间各自独立。
func (m *Manager) StartRecording(roomID, startedBy string) (*mediarec.Session, error) {
	rec := m.Recording()
	if rec == nil {
		return nil, ErrRecordingDisabled
	}
	if rec.Recording(roomID) {
		return rec.CurrentSession(roomID), nil // 本房间已在录：幂等
	}
	sess, err := rec.StartSession(roomID, startedBy)
	if err != nil {
		return nil, err
	}
	if r, ok := m.Get(roomID); ok {
		r.attachRecorders()
	}
	return sess, nil
}

// StopRecording 结束**指定房间**的录制并返回会话（没有在录时返回 nil）。
//
// 只影响这个房间：过去它会把所有房间的写入器一起摘掉，
// 于是 A 房结束录制会连带把 B 房正在进行的录制停掉（多房间串台的另一半）。
func (m *Manager) StopRecording(roomID string) *mediarec.Session {
	// 先把该房间的写入器摘掉，再收会话：让每条轨道在清单落盘前完成收尾
	if r, ok := m.Get(roomID); ok {
		r.detachRecorders()
	}
	if rec := m.Recording(); rec != nil {
		return rec.StopSession(roomID)
	}
	return nil
}

// RecordingSession 返回指定房间的录制会话（没有则 nil）。
func (m *Manager) RecordingSession(roomID string) *mediarec.Session {
	if rec := m.Recording(); rec != nil {
		return rec.CurrentSession(roomID)
	}
	return nil
}

// attachRecorders 给房间里已经在发布的每条轨道挂上录制写入器。
func (r *Room) attachRecorders() {
	rec := r.manager.Recording()
	if rec == nil || !rec.Recording(r.ID) {
		return
	}
	r.mu.RLock()
	tracks := make([]*PublishedTrack, 0, len(r.tracks))
	for _, t := range r.tracks {
		tracks = append(tracks, t)
	}
	r.mu.RUnlock()
	for _, t := range tracks {
		if t.recording() {
			continue
		}
		codec := mediarec.Codec{
			Name: "VP8", ClockRate: 90000,
			PayloadType: uint8(t.remote.PayloadType()), SSRC: uint32(t.remote.SSRC()),
		}
		if t.info.Kind == protocol.KindAudio {
			codec = mediarec.Codec{
				Name: "opus", ClockRate: 48000,
				PayloadType: uint8(t.remote.PayloadType()), SSRC: uint32(t.remote.SSRC()),
			}
		}
		w, err := rec.Open(r.ID, t.info.PublisherID, t.info.Source, codec)
		if err != nil {
			log.Printf("sfu: 中途开始录制时挂载轨道失败 track=%s: %v", t.info.ID, err)
			continue
		}
		t.attachRecorder(w)
		// 主动索要一个关键帧：录制器要等第一个关键帧才开始写文件，
		// 不请求的话"开始录制"之后可能好几秒里文件都是 0 字节（真人实测踩到过）。
		// 这与"新订阅者上线要回 PLI"是同一个道理。
		t.RequestKeyframe()
		log.Printf("sfu: 录制已挂载 track=%s（中途开始）→ %s", t.info.ID, w.Path())
	}
}

// detachRecorders 摘掉并关闭房间里所有轨道的录制写入器。
func (r *Room) detachRecorders() {
	r.mu.RLock()
	tracks := make([]*PublishedTrack, 0, len(r.tracks))
	for _, t := range r.tracks {
		tracks = append(tracks, t)
	}
	r.mu.RUnlock()
	for _, t := range tracks {
		if w := t.detachRecorder(); w != nil {
			_ = w.Close()
		}
	}
}

// NewManager 创建房间管理器。resumeGrace 是断线后保留参与者身份的宽限期（0 表示立即离会）。
func NewManager(engine *Engine, maxParticipants int, resumeGrace time.Duration) *Manager {
	return &Manager{
		engine:          engine,
		maxParticipants: maxParticipants,
		resumeGrace:     resumeGrace,
		rooms:           make(map[string]*Room),
	}
}

func (m *Manager) GetOrCreate(roomID string) *Room {
	m.mu.RLock()
	r, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if ok {
		return r
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[roomID]; ok { // double check
		return r
	}
	r = newRoom(roomID, m)
	m.rooms[roomID] = r
	log.Printf("sfu: 创建房间 room=%s（当前房间数 %d）", roomID, len(m.rooms))
	return r
}

func (m *Manager) Get(roomID string) (*Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rooms[roomID]
	return r, ok
}

// Engine 暴露 Pion 引擎给信令层（创建参与者时需要）。
func (m *Manager) Engine() *Engine { return m.engine }

// remove 由 Room 在最后一个参与者离开时调用。
func (m *Manager) remove(roomID string) {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return
	}
	// "房间已空"是在 Leave 的临界区里判定的，但那和这里删表之间还有窗口：
	// GetOrCreate 在删表前仍会把这个 Room 对象发给新入会者，于是可能有人在窗口里 Join 回来。
	// 旧实现直接删表 → 房间变成孤儿（人还在开会，Manager 却不认它：录制状态、快照、
	// 空房回收全部失联）。所以在 m.mu + r.mu 下**重新确认**一次 emptiness 再删。
	//
	// 锁序固定为 m.mu → r.mu：反方向（持 r.mu 再取 m.mu）已在 Room.State 里消除，
	// 其余 manager.* 调用要么在 r.mu 之外，要么只读无锁字段（maxParticipants/resumeGrace）。
	r.mu.Lock()
	empty := len(r.participants) == 0
	if empty {
		delete(m.rooms, roomID)
		close(r.speakerStop) // 回收房间时停掉采样协程，避免泄漏
	}
	r.mu.Unlock()
	n := len(m.rooms)
	m.mu.Unlock()
	if !empty {
		log.Printf("sfu: 空房回收放弃 room=%s（期间有人重新加入，房间保留）", roomID)
		return
	}
	log.Printf("sfu: 回收空房间 room=%s（当前房间数 %d）", roomID, n)

	// 房间没了就没人可录：**自动收尾这个房间**的会话。
	// 必须放在解锁之后——StopRecording 自己也要拿 m.mu，锁内调用会死锁。
	// 只收本房间：其他房间正在进行的录制不能因为这里回收空房间而被停掉。
	if rec := m.Recording(); rec != nil && rec.Recording(roomID) {
		if sess := rec.CurrentSession(roomID); sess != nil {
			m.StopRecording(roomID)
			log.Printf("sfu: 房间回收，录制会话已自动收尾 room=%s session=%s", roomID, sess.ID)
		}
	}
}

// RoomRecordingState 把**指定房间**的录制会话映射成给客户端的录制状态
// （信令层广播与入会快照都用它）。
//
// 为什么必须带 roomID：录制是房间级状态，A 房在录不等于 B 房在录。
// 旧实现取全局会话，于是 B 房的中途入会者会看到"录制中"红点（实际录的是 A 房）。
func (m *Manager) RoomRecordingState(roomID string) *protocol.RecordingState {
	rec := m.Recording()
	if rec == nil || !rec.Recording(roomID) {
		return &protocol.RecordingState{Active: false}
	}
	sess := rec.CurrentSession(roomID)
	if sess == nil {
		return &protocol.RecordingState{Active: false}
	}
	return &protocol.RecordingState{
		Active: true, SessionID: sess.ID,
		StartedAt: sess.StartedAt.UnixMilli(), StartedBy: sess.StartedBy, Files: len(sess.Files),
	}
}

// RecordingState 返回**全局**（任一房间正在录制时最近开始的那个）录制状态。
//
// 仅供 HTTP 的 /recordings 列表使用：它返回的是所有房间的会话，
// 没有一个"当前房间"可归属。信令层与入会快照必须用 RoomRecordingState(roomID)。
func (m *Manager) RecordingState() *protocol.RecordingState {
	rec := m.Recording()
	if rec == nil {
		return &protocol.RecordingState{Active: false}
	}
	var latest *mediarec.Session
	for _, id := range rec.RecordingRooms() {
		if s := rec.CurrentSession(id); s != nil {
			if latest == nil || s.StartedAt.After(latest.StartedAt) {
				latest = s
			}
		}
	}
	if latest == nil {
		return &protocol.RecordingState{Active: false}
	}
	return &protocol.RecordingState{
		Active: true, SessionID: latest.ID,
		StartedAt: latest.StartedAt.UnixMilli(), StartedBy: latest.StartedBy, Files: len(latest.Files),
	}
}

// SetClientQuality 记录一个参与者上报的链路质量；返回是否被接受。
//
// 校验与限频都放在这里（而不是信令层）：房间状态是唯一事实源，
// 任何入口（信令、将来的 HTTP/压测客户端）都该走同一套规则。
//
// 单位转换只做一次：协议里 lossRate 是**百分比 0–100**，内部存**比例 0–1**
// （与 metric_samples.loss_rate 一致），前端展示时再乘回 100。
func (r *Room) SetClientQuality(participantID string, q protocol.ClientQuality) bool {
	if q.RTTMs < 0 || q.RTTMs > clientQualityMaxRTTMs {
		return false
	}
	if q.LossRate < 0 || q.LossRate > 100 {
		return false
	}
	if q.OutKbps < 0 || q.OutKbps > clientQualityMaxKbps ||
		q.InKbps < 0 || q.InKbps > clientQualityMaxKbps {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, joined := r.participants[participantID]; !joined {
		return false // 未入会/已离会：上报直接丢弃
	}
	if r.quality == nil {
		r.quality = make(map[string]clientQuality)
	}
	if prev, ok := r.quality[participantID]; ok && now.Sub(prev.at) < clientQualityMinInterval {
		return false // 限频：同一参与者每秒最多一条
	}
	r.quality[participantID] = clientQuality{
		RTTMs: q.RTTMs, LossRate: q.LossRate / 100, OutKbps: q.OutKbps, InKbps: q.InKbps, at: now,
	}
	return true
}

// qualityAverageLocked 返回未过期样本的平均值；没有有效样本时 ok=false。
// 调用方必须持有 r.mu（读锁即可：这里只读，过期条目留给后续上报覆盖）。
func (r *Room) qualityAverageLocked(now time.Time) (rtt, loss, out, in float64, ok bool) {
	n := 0
	for _, q := range r.quality {
		if now.Sub(q.at) > clientQualityStale {
			continue
		}
		rtt += q.RTTMs
		loss += q.LossRate
		out += q.OutKbps
		in += q.InKbps
		n++
	}
	if n == 0 {
		return 0, 0, 0, 0, false
	}
	f := float64(n)
	return rtt / f, loss / f, out / f, in / f, true
}

// RoomSnapshot 是单个房间的轻量快照（供采样器与观测使用）。
type RoomSnapshot struct {
	ID           string
	Participants int
	Tracks       int
	Packets      uint64
	Bytes        uint64
	// 以下四项来自**客户端上报**（client_metrics），没有上报时为 0。
	// LossRate 是**比例 0–1**（与 metric_samples.loss_rate 同口径）。
	RTTMs    float64
	LossRate float64
	OutKbps  float64
	InKbps   float64
}

// RoomSnapshots 返回所有房间的轻量快照。
//
// 刻意做得比 DebugInfo 轻得多：**不调用 PeerConnection.GetStats()**
// （那是与连接数成正比的耗时操作），只读内存里的原子计数。
// 采样协程按秒级周期调用它，绝不能对转发造成可观测的影响。
func (m *Manager) RoomSnapshots() []RoomSnapshot {
	m.mu.RLock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.RUnlock()

	out := make([]RoomSnapshot, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, r.Snapshot())
	}
	return out
}

// Snapshot 返回房间的一次性轻量快照（人数/轨道/累计流量 + 客户端上报质量的平均值）。
//
// 只拿一次读锁：采样器每 Interval 调一次，若分几次加锁会得到"人数是 A 时刻、
// 质量是 B 时刻"的拼接结果（数值上无害，但排查时会对不上）。
func (r *Room) Snapshot() RoomSnapshot {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()

	snap := RoomSnapshot{
		ID:           r.ID,
		Participants: len(r.participants),
		Tracks:       len(r.tracks),
	}
	for _, t := range r.tracks {
		p, b := t.Stats()
		snap.Packets += p
		snap.Bytes += b
	}
	if rtt, loss, out, in, ok := r.qualityAverageLocked(now); ok {
		snap.RTTMs, snap.LossRate, snap.OutKbps, snap.InKbps = rtt, loss, out, in
	}
	return snap
}

// Stats 供 /healthz 与评测脚本观测。
// RoomIDs 返回当前所有房间 ID 的快照（按 ID 排序）。
// 用于"把所有房间的指标曲线合并成一条全局曲线"。
func (m *Manager) RoomIDs() []string {
	m.mu.RLock()
	ids := make([]string, 0, len(m.rooms))
	for id := range m.rooms {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Stats 供 /healthz 与评测脚本观测。
//
// 锁序约定：**先快照 m.rooms，释放 m.mu 之后再逐房间取 r.mu**。
// 以前是持有 m.mu.RLock 时调用 r.counts()（m.mu → r.mu），
// 而 Room.State 是持有 r.mu.RLock 时再取 m.mu（r.mu → m.mu）——两个方向同时出现
// 就是经典的 ABBA 死锁；RWMutex 的写者优先会让它更容易触发。只保留一个方向即可。
func (m *Manager) Stats() (rooms, participants, tracks int) {
	m.mu.RLock()
	roomsSnapshot := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		roomsSnapshot = append(roomsSnapshot, r)
	}
	m.mu.RUnlock()

	for _, r := range roomsSnapshot {
		p, t := r.counts()
		participants += p
		tracks += t
	}
	return len(roomsSnapshot), participants, tracks
}

// ---------- Room ----------

// Room 是一个会议室：参与者 + 已发布轨道 + 订阅关系（转发矩阵）。
type Room struct {
	ID        string
	CreatedAt time.Time
	manager   *Manager

	mu            sync.RWMutex
	participants  map[string]*Participant
	tracks        map[string]*PublishedTrack
	subscriptions map[string]map[string]bool // participantID -> trackID 集合
	// grace 保存"已断线、等待恢复"的参与者的超时计时器（见 Detach/Resume）
	grace map[string]*time.Timer
	// speakerStop 关闭时停止活跃说话人采样协程（房间被回收时调用）
	speakerStop chan struct{}
	// hands 记录举手状态（ID → 是否举手）。进房间快照，新入会者也能看到谁举了手。
	hands map[string]bool
	// whiteboard 是房间当前的白板内容（按提交顺序的已校验操作）。
	// 存"操作序列"而不是位图：数据小、可撤销、也能让中途进来的人完整还原画面。
	whiteboard []protocol.WhiteboardOp
	// whiteboardSeq 是白板操作的房间内递增序号（客户端不能自定顺序）
	whiteboardSeq uint64
	// trackHooks 是轨道事件钩子（AI 转写用）：added=true 发布，false 下线。
	// 用独立锁：回调里可能再调回 Room 的方法，不能复用 r.mu（会死锁）。
	hookMu     sync.RWMutex
	trackHooks []func(track *PublishedTrack, added bool)
	// aiSummaryProvider 由 ai 包设置：返回当前 AI 总结状态（nil 表示未启用）。
	// Room 本身不依赖 ai 包（避免循环依赖），只通过这个函数指针取状态进快照。
	aiSummaryProvider func() *protocol.AISummaryState
	// quality 是各参与者最近一次上报的链路质量（client_metrics）。
	// 采样器在写 metric_samples 时取"未过期样本的平均值"——服务端自己测不到
	// 到浏览器的 RTT/丢包（它不解码媒体），所以这几列的唯一来源就是这里。
	quality map[string]clientQuality
}

// OnTrackEvent 注册轨道事件钩子（AI 转写用）。回调在轨道发布/下线后被调用，
// 不要在回调里做耗时操作。返回注销函数，不再需要时调用以避免钩子堆积。
func (r *Room) OnTrackEvent(fn func(track *PublishedTrack, added bool)) (unregister func()) {
	r.hookMu.Lock()
	r.trackHooks = append(r.trackHooks, fn)
	idx := len(r.trackHooks) - 1
	r.hookMu.Unlock()
	return func() {
		r.hookMu.Lock()
		defer r.hookMu.Unlock()
		if idx < len(r.trackHooks) && r.trackHooks[idx] != nil {
			// 与闭包比较函数值不可行（Go 不允许比较 func），用索引占位注销
			r.trackHooks[idx] = nil
		}
	}
}

func (r *Room) emitTrackEvent(track *PublishedTrack, added bool) {
	r.hookMu.RLock()
	hooks := make([]func(*PublishedTrack, bool), len(r.trackHooks))
	copy(hooks, r.trackHooks)
	r.hookMu.RUnlock()
	for _, fn := range hooks {
		if fn != nil {
			fn(track, added)
		}
	}
}

// SetAISummaryProvider 设置/清除 AI 总结状态提供者（ai 包在会话起停时调用）。
func (r *Room) SetAISummaryProvider(fn func() *protocol.AISummaryState) {
	r.mu.Lock()
	r.aiSummaryProvider = fn
	r.mu.Unlock()
}

// AudioTracks 返回当前所有音频轨道的快照。
func (r *Room) AudioTracks() []*PublishedTrack {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*PublishedTrack
	for _, t := range r.tracks {
		if t.info.Kind == protocol.KindAudio {
			out = append(out, t)
		}
	}
	return out
}

// ParticipantName 返回参与者的显示名，找不到返回 ID 本身。
func (r *Room) ParticipantName(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.participants[id]; ok && p != nil {
		if n := p.Info().Name; n != "" {
			return n
		}
	}
	return id
}

// clientQuality 是一条参与者上报的质量样本（内部表示，单位已归一到落库口径）。
type clientQuality struct {
	RTTMs    float64   // 毫秒
	LossRate float64   // **比例 0–1**（协议里是百分比，入口处已除以 100）
	OutKbps  float64   // 千比特/秒
	InKbps   float64   // 千比特/秒
	at       time.Time // 上报时刻，用于过期判断
}

const (
	// clientQualityStale 是质量样本的有效期：客户端每 10s 上报一次，
	// 超过 3 个周期没更新就视为过期（人走了/断线），避免用旧值画出一条"看起来正常"的平线。
	clientQualityStale = 30 * time.Second
	// clientQualityMinInterval 是同一参与者两次上报的最小间隔（限频）。
	// 正常情况下 10s 一条；这里只防"被改过的客户端每包刷一条"把房间锁与库写打爆。
	clientQualityMinInterval = time.Second
	// 数值上限：超出视为异常数据直接丢弃（而不是截断后当真值落库）。
	clientQualityMaxRTTMs = 60_000
	clientQualityMaxKbps  = 1_000_000
)

func newRoom(id string, m *Manager) *Room {
	r := &Room{
		ID:            id,
		CreatedAt:     time.Now(),
		manager:       m,
		participants:  make(map[string]*Participant),
		tracks:        make(map[string]*PublishedTrack),
		subscriptions: make(map[string]map[string]bool),
		grace:         make(map[string]*time.Timer),
		speakerStop:   make(chan struct{}),
		hands:         make(map[string]bool),
		quality:       make(map[string]clientQuality),
	}
	safeGo("speaker-loop/"+r.ID, r.speakerLoop)
	return r
}

// speakerLoop 周期性计算"正在说话的人"并广播。
//
// 三个设计要点：
//  1. 判定完全基于 RTP 包头扩展里的音频电平（RFC 6464），**不解码音频**，
//     与"媒体面只转发不转码"的约束一致；
//  2. 300ms 采样 + 每条轨道指数平滑：单包电平抖动极大，直接广播会让高亮疯狂闪烁；
//  3. **只在集合变化时广播**（与 LiveKit 的 ActiveSpeakersChanged 同语义），
//     否则 20 人房间里每秒会刷出几十条无意义消息。
func (r *Room) speakerLoop() {
	ticker := time.NewTicker(activeSpeakerInterval)
	defer ticker.Stop()

	var last string
	for {
		select {
		case <-r.speakerStop:
			return
		case <-ticker.C:
			speakers := r.detectSpeakers()
			key := speakerKey(speakers)
			if key == last {
				continue
			}
			last = key
			r.Broadcast(&protocol.Message{Type: protocol.TypeActiveSpeakers, Speakers: speakers}, "")
		}
	}
}

// detectSpeakers 返回当前超过门槛、按电平降序的前 N 位说话人。
func (r *Room) detectSpeakers() []protocol.SpeakerInfo {
	r.mu.RLock()
	participants := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		participants = append(participants, p)
	}
	r.mu.RUnlock()

	out := make([]protocol.SpeakerInfo, 0, len(participants))
	for _, p := range participants {
		if level := p.AudioLevel(); level >= audioLevelThreshold {
			out = append(out, protocol.SpeakerInfo{ParticipantID: p.ID, Level: level})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Level > out[j].Level })
	if len(out) > activeSpeakerMax {
		out = out[:activeSpeakerMax]
	}
	return out
}

// speakerKey 把说话人集合压成一个可比较的字符串（电平取两位小数，避免抖动导致误判变化）。
func speakerKey(list []protocol.SpeakerInfo) string {
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(list))
	for _, s := range list {
		parts = append(parts, fmt.Sprintf("%s=%d", s.ParticipantID, int(s.Level*100)))
	}
	return strings.Join(parts, ",")
}

func (r *Room) maxParticipants() int { return r.manager.maxParticipants }

func (r *Room) counts() (participants, tracks int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.participants), len(r.tracks)
}

// Join 把参与者加入房间，并向房间内其他人广播 participant_joined。
func (r *Room) Join(p *Participant) error {
	r.mu.Lock()
	if len(r.participants) >= r.maxParticipants() {
		r.mu.Unlock()
		// 提示里点明"含重连中的参与者"：断线宽限期内的身份仍然占位，
		// 不写清楚的话，用户看到"房间已满"但界面上只有几个人，会以为人数统计错了。
		return fmt.Errorf("房间已满（上限 %d，含重连中的参与者）", r.maxParticipants())
	}
	r.participants[p.ID] = p
	p.Confirm(r)
	others := r.othersLocked(p.ID)
	r.mu.Unlock()

	for _, o := range others {
		o.NotifyTo(&protocol.Message{
			Type:        protocol.TypeParticipantJoin,
			Participant: p.Info(),
		})
	}
	log.Printf("sfu: 参与者入会 room=%s participant=%s(%s) 当前 %d 人", r.ID, p.ID, p.Name, len(others)+1)
	return nil
}

// Detach 表示参与者的信令连接已断开，但不立即视为离会：
// 参与者被标记为"重连中"，**轨道、订阅关系与媒体连接全部保留**，其他人只看到状态变化。
// 若宽限期内带着 resumeToken 重连成功，整个过程对房间内其他人无感（这正是 M4 的核心指标）。
// 返回 true 表示已进入宽限期；返回 false 表示宽限期为 0、已按立即离会处理。
func (r *Room) Detach(participantID string) bool {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	grace := r.manager.resumeGrace
	if grace <= 0 {
		r.mu.Unlock()
		r.Leave(participantID)
		return false
	}
	if t, exists := r.grace[participantID]; exists {
		t.Stop() // 反复抖动：重置计时，不叠加
	}
	p.setState(protocol.StateReconnecting)
	info := p.Info()
	others := r.othersLocked(participantID)
	r.grace[participantID] = time.AfterFunc(grace, func() { r.expireDetached(participantID) })
	r.mu.Unlock()

	for _, o := range others {
		o.NotifyTo(&protocol.Message{
			Type:          protocol.TypeParticipantState,
			ParticipantID: participantID,
			State:         protocol.StateReconnecting,
		})
	}
	log.Printf("sfu: 参与者断线，进入 %s 恢复宽限期 room=%s participant=%s(%s) state=%s",
		grace, r.ID, info.ID, info.Name, info.State)
	return true
}

// Resume 用新的信令连接恢复一个处于宽限期内的参与者，并返回房间快照。
// 只有"确实处于重连中"的参与者才允许恢复：否则两个连接可能同时操作同一参与者状态。
func (r *Room) Resume(participantID string, n Notifier, resetMedia bool) (*Participant, bool) {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if !ok || p.State() != protocol.StateReconnecting {
		r.mu.Unlock()
		return nil, false
	}
	if t, exists := r.grace[participantID]; exists {
		t.Stop()
		delete(r.grace, participantID)
	}
	p.SetNotifier(n)
	p.setState(protocol.StateActive)

	// 媒体面已死的重连：清掉旧轨道（保留身份），客户端随后用新连接重新发布。
	stale := make([]*PublishedTrack, 0, 4)
	if resetMedia {
		for id, t := range r.tracks {
			if t.info.PublisherID == participantID {
				delete(r.tracks, id)
				stale = append(stale, t)
			}
		}
		for _, subs := range r.subscriptions {
			for _, t := range stale {
				delete(subs, t.info.ID)
			}
		}
	}
	others := r.othersLocked(participantID)
	r.mu.Unlock()

	if resetMedia {
		p.ResetMedia()
		for _, t := range stale {
			log.Printf("sfu: 重连清理旧轨道 room=%s track=%s（客户端将重新发布）", r.ID, t.info.ID)
			for _, o := range others {
				o.NotifyTo(&protocol.Message{Type: protocol.TypeTrackUnpublished, TrackID: t.info.ID})
			}
			// 同 finishLeave：这条路径绕过了 removeTrack，必须显式通知钩子，
			// 否则 AI 转写器会一直挂在这条已死的轨道上（表现为总结里还在等旧音频）。
			r.emitTrackEvent(t, false)
		}
	}

	for _, o := range others {
		o.NotifyTo(&protocol.Message{
			Type:          protocol.TypeParticipantState,
			ParticipantID: participantID,
			State:         protocol.StateActive,
		})
	}
	log.Printf("sfu: 参与者断线恢复成功 room=%s participant=%s resetMedia=%v", r.ID, participantID, resetMedia)
	return p, true
}

// expireDetached 由宽限期计时器触发：此时才真正把参与者移出房间。
//
// 必须与 Resume 互斥，而且**判定状态的那一刻就要在锁内**：旧实现先取锁确认
// "参与者还在"，释放锁后再调 Leave；这中间的窗口里客户端可能刚好带着 resumeToken
// 重连成功（Resume 把状态改回 active、停掉计时器），随后 Leave 仍然把这个人踢出房间——
// 这就是"重连成功却被踢"的 TOCTOU。现在把"检查 state == reconnecting + 摘除"放进同一个临界区。
func (r *Room) expireDetached(participantID string) {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if !ok || p.State() != protocol.StateReconnecting {
		// 已经离会，或已经在窗口内恢复成功：这是一次过期事件，直接丢弃。
		r.mu.Unlock()
		return
	}
	log.Printf("sfu: 恢复宽限期已过，参与者离会 room=%s participant=%s", r.ID, participantID)
	removed, others, empty := r.removeParticipantLocked(participantID)
	r.mu.Unlock()
	r.finishLeave(participantID, p, removed, others, empty)
}

// Leave 让参与者离开：清理其轨道与订阅关系，并广播事件。
func (r *Room) Leave(participantID string) {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if !ok {
		r.mu.Unlock()
		return
	}
	removed, others, empty := r.removeParticipantLocked(participantID)
	r.mu.Unlock()
	r.finishLeave(participantID, p, removed, others, empty)
}

// removeParticipantLocked 在**已持有 r.mu 写锁**时把参与者及其轨道/订阅从房间摘掉，
// 只做内存操作，不做任何可能阻塞或再取锁的事（p.Close / 广播都在 finishLeave 里）。
// 返回该参与者已下线的轨道（指针，收尾时要靠它触发轨道事件钩子）、
// 其余参与者、以及"房间是否已空"。
func (r *Room) removeParticipantLocked(participantID string) (removed []*PublishedTrack, others []*Participant, empty bool) {
	if t, exists := r.grace[participantID]; exists {
		t.Stop()
		delete(r.grace, participantID)
	}
	delete(r.participants, participantID)
	delete(r.subscriptions, participantID)
	delete(r.hands, participantID)
	// 离会后不能再让他的旧质量样本参与平均（否则人走了曲线还挂着他的 RTT）
	delete(r.quality, participantID)

	// 该参与者发布的轨道全部下线
	removed = make([]*PublishedTrack, 0, len(r.tracks))
	for id, t := range r.tracks {
		if t.info.PublisherID == participantID {
			delete(r.tracks, id)
			removed = append(removed, t)
		}
	}
	// 其他人对该参与者的订阅关系一并清理
	for _, subs := range r.subscriptions {
		for _, t := range removed {
			delete(subs, t.info.ID)
		}
	}
	others = r.othersLocked(participantID)
	empty = len(r.participants) == 0
	return removed, others, empty
}

// finishLeave 做离会后的收尾：关连接、广播事件、触发轨道事件钩子、必要时回收空房间。
// 必须在释放 r.mu 之后调用——p.Close() 会关 PeerConnection（可能阻塞），
// 广播与钩子回调都要回到别的组件，都不该持有房间写锁。
func (r *Room) finishLeave(participantID string, p *Participant, removed []*PublishedTrack, others []*Participant, empty bool) {
	p.Close()

	for _, o := range others {
		o.NotifyTo(&protocol.Message{Type: protocol.TypeParticipantLeave, ParticipantID: participantID})
	}
	for _, t := range removed {
		log.Printf("sfu: 轨道下线 room=%s track=%s 原因=参与者离会", r.ID, t.info.ID)
		for _, o := range others {
			o.NotifyTo(&protocol.Message{Type: protocol.TypeTrackUnpublished, TrackID: t.info.ID})
		}
		// 必须显式触发轨道下线钩子（AI 转写器靠它 detach）：
		// 这条路径不走 removeTrack，而 p.Close() 之后 Pump 结束时 removeTrack 会发现
		// 轨道已从 map 里删掉而提前返回——不在这里补发，转写器就永远不会被摘掉。
		r.emitTrackEvent(t, false)
	}
	log.Printf("sfu: 参与者离会 room=%s participant=%s 剩余 %d 人", r.ID, participantID, len(others))

	if empty {
		r.manager.remove(r.ID)
	}
}

// onTrackPublished 由 Participant 的 OnTrack 回调触发：登记轨道并广播。
//
// 注意：这里**也要通知发布者自己**——发布者需要知道自己轨道的服务端 trackId，
// 才能在结束屏幕共享时发 unpublish 把它撤下来。
func (r *Room) onTrackPublished(publisher *Participant, track *PublishedTrack) {
	track.onEnd = r.onTrackEnded

	r.mu.Lock()
	r.tracks[track.info.ID] = track
	targets := r.allLocked()
	r.mu.Unlock()

	log.Printf("sfu: 轨道发布 room=%s track=%s kind=%s source=%s", r.ID, track.info.ID, track.info.Kind, track.info.Source)
	for _, p := range targets {
		p.NotifyTo(&protocol.Message{Type: protocol.TypeTrackPublished, Track: track.Info()})
	}
	r.emitTrackEvent(track, true)
}

// Unpublish 停止发布某条轨道（发布者主动请求）。
func (r *Room) Unpublish(participantID, trackID string) error {
	r.mu.RLock()
	track, ok := r.tracks[trackID]
	r.mu.RUnlock()
	if !ok {
		return nil // 已经下线，幂等返回
	}
	if track.info.PublisherID != participantID {
		return fmt.Errorf("无权取消他人的轨道: %s", trackID)
	}
	r.removeTrack(trackID, "发布者取消发布")
	return nil
}

// onTrackEnded 由转发热路径在结束时回调（发布者断开、轨道流结束等）。
func (r *Room) onTrackEnded(track *PublishedTrack) {
	r.removeTrack(track.info.ID, "转发结束")
}

// removeTrack 是轨道下线的唯一出口：从房间移除、清理订阅关系、广播 track_unpublished。
// 幂等——重复调用不会产生重复广播。
func (r *Room) removeTrack(trackID, reason string) {
	r.mu.Lock()
	track, ok := r.tracks[trackID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.tracks, trackID)
	for _, subs := range r.subscriptions {
		delete(subs, trackID)
	}
	targets := r.allLocked()
	r.mu.Unlock()

	log.Printf("sfu: 轨道下线 room=%s track=%s publisher=%s kind=%s source=%s 原因=%s",
		r.ID, trackID, track.info.PublisherID, track.info.Kind, track.info.Source, reason)
	for _, p := range targets {
		p.NotifyTo(&protocol.Message{Type: protocol.TypeTrackUnpublished, TrackID: trackID})
	}
	r.emitTrackEvent(track, false)
}

// Subscribe 让 subscriberID 订阅 trackID，返回需要发给客户端的 offer SDP。
// 这是"按需转发"的开关：只有订阅了才会把包写给该参与者（未订阅时 TrackLocalStaticRTP 没有绑定，包直接丢弃）。
func (r *Room) Subscribe(subscriberID, trackID string) (string, error) {
	r.mu.RLock()
	sub, ok := r.participants[subscriberID]
	if !ok {
		r.mu.RUnlock()
		return "", fmt.Errorf("参与者不存在")
	}
	track, ok := r.tracks[trackID]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("轨道不存在: %s", trackID)
	}

	offer, err := sub.Subscribe(track)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	if r.subscriptions[subscriberID] == nil {
		r.subscriptions[subscriberID] = make(map[string]bool)
	}
	r.subscriptions[subscriberID][trackID] = true
	r.mu.Unlock()

	// 空 offer = 参与者已经订阅过（幂等命中），不重复协商也不打"建立订阅"的日志。
	if offer == "" {
		return "", nil
	}
	log.Printf("sfu: 建立订阅 room=%s subscriber=%s track=%s", r.ID, subscriberID, trackID)
	return offer, nil
}

func (r *Room) Unsubscribe(subscriberID, trackID string) (string, error) {
	r.mu.RLock()
	sub, ok := r.participants[subscriberID]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("参与者不存在")
	}
	// 只移除单条轨道的发送器并重协商；offer 为空表示无需通知客户端
	offer, err := sub.Unsubscribe(trackID)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if subs := r.subscriptions[subscriberID]; subs != nil {
		delete(subs, trackID)
	}
	r.mu.Unlock()
	return offer, nil
}

// SetTrackMuted 只更新状态并广播；真正的静音由发布端停发（或发静音帧）实现，
// SFU 不解析媒体内容，因此这里不做任何转码层面的处理。
func (r *Room) SetTrackMuted(trackID string, muted bool) error {
	r.mu.Lock()
	track, ok := r.tracks[trackID]
	if ok {
		track.SetMuted(muted)
	}
	targets := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		targets = append(targets, p)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("轨道不存在: %s", trackID)
	}

	for _, p := range targets {
		p.NotifyTo(&protocol.Message{Type: protocol.TypeTrackMuted, TrackID: trackID, Muted: &muted})
	}
	return nil
}

// requestKeyframesFor 向某参与者已订阅的所有轨道请求关键帧。
//
// 用于"订阅端传输建立完成"的时刻：订阅动作发出的 PLI 产出的关键帧，可能落在
// 订阅端 SRTP 尚未就绪的窗口里而被 Pion 静默丢弃（srtpWriterFuture 在未就绪时返回 (0,nil)）。
// 因此需要在连接真正 established 后，再向发布端要一次关键帧，确保这一帧能被投递。
func (r *Room) requestKeyframesFor(participantID string) {
	r.mu.RLock()
	set := r.subscriptions[participantID]
	tracks := make([]*PublishedTrack, 0, len(set))
	for id := range set {
		if t, ok := r.tracks[id]; ok {
			tracks = append(tracks, t)
		}
	}
	r.mu.RUnlock()

	for _, t := range tracks {
		t.RequestKeyframe()
	}
}

// Broadcast 向房间内所有参与者推送事件（exceptID 非空时跳过该参与者）。
// 聊天、静音状态这类"广播给对方即可"的事件走这里，避免信令层直接操作参与者集合。
func (r *Room) Broadcast(msg *protocol.Message, exceptID string) {
	r.mu.RLock()
	targets := make([]*Participant, 0, len(r.participants))
	for id, p := range r.participants {
		if id == exceptID {
			continue
		}
		targets = append(targets, p)
	}
	r.mu.RUnlock()

	for _, p := range targets {
		p.NotifyTo(msg)
	}
}

// Track 查询轨道（供信令层做权限校验等）。
func (r *Room) Track(trackID string) (*PublishedTrack, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tracks[trackID]
	return t, ok
}

// State 生成房间快照：新成员入会或断线重连时下发。
func (r *Room) State() *protocol.RoomState {
	// 录制状态要在**取 r.mu 之前**读：RoomRecordingState 内部会拿 Manager.mu，
	// 而 Manager 侧的部分方法（如 Stats）也会拿 r.mu——两个方向同时存在就是 ABBA 死锁。
	// 这里统一成"r.mu 内不再取 Manager.mu"。
	recording := r.manager.RoomRecordingState(r.ID)

	r.mu.RLock()
	defer r.mu.RUnlock()

	state := &protocol.RoomState{
		RoomID:       r.ID,
		Participants: make([]*protocol.PeerInfo, 0, len(r.participants)),
		Tracks:       make([]*protocol.TrackInfo, 0, len(r.tracks)),
		// 带上录制状态：中途入会的人也要能立刻看到"录制中"红点，
		// 而不是等到下一次广播（否则"有人在录我"这件事对新来者不可见）
		Recording: recording,
	}
	for _, p := range r.participants {
		state.Participants = append(state.Participants, p.Info())
	}
	for _, t := range r.tracks {
		state.Tracks = append(state.Tracks, t.Info())
	}
	state.Whiteboard = append([]protocol.WhiteboardOp(nil), r.whiteboard...)
	for id := range r.hands {
		state.Hands = append(state.Hands, id)
	}
	// 带上 AI 总结状态：中途入会的人能立刻看到当前纪要，而不是等下一次更新
	if r.aiSummaryProvider != nil {
		state.AISummary = r.aiSummaryProvider()
	}
	return state
}

// ParticipantByID 按参与者 ID 查人（主持人管理需要先确认目标存在）。
func (r *Room) ParticipantByID(participantID string) (*Participant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.participants[participantID]
	return p, ok
}

// ForceMute 由主持人发起：把某参与者的**音频轨**全部静音（服务端停止转发），并广播状态。
//
// 为什么服务端能做"真静音"：SFU 虽然不解析媒体，但可以选择不转发——
// 对房间里其他人来说这就是彻底听不见了。返回被处理的轨道数。
// 同时给目标客户端下发 force_mute，让它的 UI 如实显示"已被主持人静音"。
func (r *Room) ForceMute(participantID string, muted bool) int {
	r.mu.Lock()
	target, ok := r.participants[participantID]
	if !ok {
		r.mu.Unlock()
		return 0
	}
	affected := make([]*PublishedTrack, 0, 2)
	for _, t := range r.tracks {
		if t.info.PublisherID == participantID && t.info.Kind == protocol.KindAudio {
			t.SetMuted(muted)
			affected = append(affected, t)
		}
	}
	targets := r.allLocked()
	r.mu.Unlock()

	for _, t := range affected {
		info := t.Info()
		for _, p := range targets {
			p.NotifyTo(&protocol.Message{Type: protocol.TypeTrackMuted, TrackID: info.ID, Muted: &muted})
		}
	}
	// 单独通知被静音者本人：它需要据此更新麦克风按钮状态
	target.NotifyTo(&protocol.Message{Type: protocol.TypeForceMute, Muted: &muted, TargetID: participantID})
	log.Printf("sfu: 主持人强制静音 room=%s participant=%s muted=%v 影响轨道 %d 条", r.ID, participantID, muted, len(affected))
	return len(affected)
}

// ---------- 数据通道（SCTP 数据面）----------

// OnReaction 把表情中继给房间内**其他**参与者。
//
// 服务端不保存表情：它是瞬时事件，丢了无所谓，属于数据面语义。
// 之所以走数据通道而不是信令，是因为表情可能很频繁，塞进信令会挤占重连快照、心跳这些关键消息。
func (r *Room) OnReaction(fromID, emoji, text, kind string) {
	// 发送者名字同样由服务端填：弹幕要显示"谁说的"，
	// 让客户端自报名字等于允许任何人冒充别人发言。
	fromName := ""
	r.mu.RLock()
	if p := r.participants[fromID]; p != nil {
		fromName = p.Name
	}
	r.mu.RUnlock()
	payload, err := json.Marshal(&protocol.DataMessage{
		Type:     protocol.DataReaction,
		Emoji:    emoji,
		Text:     text,
		Kind:     kind,
		FromID:   fromID, // 身份由服务端填，客户端无法伪造
		FromName: fromName,
	})
	if err != nil {
		return
	}
	r.mu.RLock()
	targets := r.othersLocked(fromID)
	r.mu.RUnlock()

	delivered := 0
	for _, p := range targets {
		if p.SendData(payload) {
			delivered++
		}
	}
	log.Printf("sfu: 表情中继 room=%s from=%s emoji=%s 送达 %d/%d", r.ID, fromID, emoji, delivered, len(targets))
}

// SetHand 更新举手状态并广播。
//
// 与表情相反，举手是**状态**：必须可靠、断线恢复后仍在、新入会者要能看到，
// 所以它落在房间状态里（进 RoomState.Hands）并通过信令广播。
// AddWhiteboardOp 把一条**已校验**的操作记入房间并返回带序号的副本。
//
// 上限处理：到达 MaxWhiteboardOps 时拒绝新操作（而不是丢弃最旧的）——
// 丢弃最旧会让画面莫名少一块，比"告诉用户白板满了"更糟。
func (r *Room) AddWhiteboardOp(op protocol.WhiteboardOp, by string) (protocol.WhiteboardOp, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch op.Kind {
	case protocol.WhiteboardUndo:
		// 撤销只撤自己的最后一笔：从后往前找 by 的最新一笔 stroke。
		// 不校验归属的话，A 点撤销会擦掉 B 刚画的笔画。
		idx := -1
		for i := len(r.whiteboard) - 1; i >= 0; i-- {
			if r.whiteboard[i].By == by {
				idx = i
				break
			}
		}
		if idx < 0 {
			return op, false // 自己没有可撤的笔画：不广播空操作
		}
		removed := r.whiteboard[idx]
		r.whiteboard = append(r.whiteboard[:idx], r.whiteboard[idx+1:]...)
		// 撤销也要带序号广播，并带上被撤销那一笔的序号，
		// 客户端据此精确删除对应笔画（而不是盲目弹掉最后一笔）
		r.whiteboardSeq++
		return protocol.WhiteboardOp{Kind: protocol.WhiteboardUndo, By: by, Seq: r.whiteboardSeq, UndoSeq: removed.Seq}, true
	case protocol.WhiteboardClear:
		if len(r.whiteboard) == 0 {
			return op, false
		}
		r.whiteboard = r.whiteboard[:0]
		r.whiteboardSeq++
		return protocol.WhiteboardOp{Kind: protocol.WhiteboardClear, By: by, Seq: r.whiteboardSeq}, true
	default:
		if len(r.whiteboard) >= protocol.MaxWhiteboardOps {
			return op, false
		}
		op.By, op.Seq = by, r.whiteboardSeq+1
		r.whiteboardSeq++
		r.whiteboard = append(r.whiteboard, op)
		return op, true
	}
}

// WhiteboardState 返回房间当前白板内容（快照）。
func (r *Room) WhiteboardState() []protocol.WhiteboardOp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]protocol.WhiteboardOp(nil), r.whiteboard...)
}

func (r *Room) SetHand(participantID string, raised bool) {
	r.mu.Lock()
	if raised {
		r.hands[participantID] = true
	} else {
		delete(r.hands, participantID)
	}
	targets := r.allLocked()
	r.mu.Unlock()

	for _, p := range targets {
		p.NotifyTo(&protocol.Message{Type: protocol.TypeHandState, ParticipantID: participantID, Raised: &raised})
	}
	log.Printf("sfu: 举手状态变更 room=%s participant=%s raised=%v", r.ID, participantID, raised)
}

// Stats 返回房间级指标，供评测脚本采集。
// AllTrackStats 返回所有房间的轨道画像（每行带 roomId），供 /metrics/tracks 使用。
func (m *Manager) AllTrackStats() []map[string]any {
	type roomRef struct {
		id string
		r  *Room
	}
	m.mu.RLock()
	rooms := make([]roomRef, 0, len(m.rooms))
	for id, r := range m.rooms {
		rooms = append(rooms, roomRef{id: id, r: r})
	}
	m.mu.RUnlock()
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].id < rooms[j].id })

	out := make([]map[string]any, 0, len(rooms))
	for _, rr := range rooms {
		for _, row := range rr.r.TrackStats() {
			out = append(out, map[string]any{
				"roomId": rr.id,
				"id":     row.ID, "publisherId": row.PublisherID, "kind": row.Kind, "source": row.Source,
				"codec": row.Codec, "payloadType": row.PayloadType, "muted": row.Muted,
				"recording": row.Recording, "packets": row.Packets, "bytes": row.Bytes,
				"dropped": row.Dropped, "writeFails": row.WriteFails, "subscribers": row.Subscribers,
				"pliForwarded": row.PLIForwarded, "pliSuppressed": row.PLISuppressed, "keyframes": row.Keyframes,
			})
		}
	}
	return out
}

// TrackStats 返回本房间所有发布轨道的画像（按轨道 ID 排序，便于逐次对比）。
func (r *Room) TrackStats() []TrackStatsRow {
	r.mu.RLock()
	tracks := make([]*PublishedTrack, 0, len(r.tracks))
	for _, t := range r.tracks {
		tracks = append(tracks, t)
	}
	r.mu.RUnlock()
	rows := make([]TrackStatsRow, 0, len(tracks))
	for _, t := range tracks {
		rows = append(rows, t.TrackStats())
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func (r *Room) Stats() (participants, tracks int, packets, bytes uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	participants = len(r.participants)
	tracks = len(r.tracks)
	for _, t := range r.tracks {
		p, b := t.Stats()
		packets += p
		bytes += b
	}
	return
}

// allLocked 返回房间内所有参与者（调用方需持有读锁）。
func (r *Room) allLocked() []*Participant {
	out := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p)
	}
	return out
}

func (r *Room) othersLocked(selfID string) []*Participant {
	out := make([]*Participant, 0, len(r.participants))
	for id, p := range r.participants {
		if id != selfID {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 可观测性 ----------

type RoomDebug struct {
	RoomID       string              `json:"roomId"`
	Participants []*ParticipantDebug `json:"participants"`
	// Quality 是房间内**客户端上报**质量的平均值（见 Room.SetClientQuality）；
	// 没有任何有效上报时为 nil（区分"没人报"与"报了 0"）。
	// 排查"画面卡但服务端指标正常"时，这里是判断"链路质量到底如何"的第一手数据。
	Quality *QualityDebug `json:"quality,omitempty"`
}

// QualityDebug 是质量快照的 JSON 形态；LossRate 是**比例 0–1**（与落库口径一致）。
type QualityDebug struct {
	RTTMs    float64 `json:"rttMs"`
	LossRate float64 `json:"lossRate"`
	OutKbps  float64 `json:"outKbps"`
	InKbps   float64 `json:"inKbps"`
}

// DebugInfo 输出房间内所有参与者的转发与连接状态。
func (r *Room) DebugInfo() *RoomDebug {
	now := time.Now()
	r.mu.RLock()
	participants := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		participants = append(participants, p)
	}
	subs := make(map[string][]string, len(r.subscriptions))
	for pid, set := range r.subscriptions {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		subs[pid] = ids
	}
	// 与参与者列表在同一次读锁里取质量：避免"人在这份快照里、质量在另一份里"的错位
	rtt, loss, out, in, hasQuality := r.qualityAverageLocked(now)
	r.mu.RUnlock()

	debug := &RoomDebug{RoomID: r.ID, Participants: make([]*ParticipantDebug, 0, len(participants))}
	for _, p := range participants {
		debug.Participants = append(debug.Participants, p.DebugInfo(subs[p.ID]))
	}
	if hasQuality {
		debug.Quality = &QualityDebug{RTTMs: rtt, LossRate: loss, OutKbps: out, InKbps: in}
	}
	return debug
}

// DebugInfo 输出所有房间的调试快照（/debug/stats 用）。
func (m *Manager) DebugInfo() []*RoomDebug {
	m.mu.RLock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.RUnlock()

	out := make([]*RoomDebug, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, r.DebugInfo())
	}
	return out
}
