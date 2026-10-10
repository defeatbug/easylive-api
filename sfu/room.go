package sfu

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

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

	mu    sync.RWMutex
	rooms map[string]*Room
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
	if r, ok := m.rooms[roomID]; ok {
		close(r.speakerStop) // 回收房间时停掉采样协程，避免泄漏
	}
	delete(m.rooms, roomID)
	n := len(m.rooms)
	m.mu.Unlock()
	log.Printf("sfu: 回收空房间 room=%s（当前房间数 %d）", roomID, n)
}

// Stats 供 /healthz 与评测脚本观测。
func (m *Manager) Stats() (rooms, participants, tracks int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rooms {
		p, t := r.counts()
		participants += p
		tracks += t
	}
	return len(m.rooms), participants, tracks
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
}

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
	}
	go r.speakerLoop()
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
		return fmt.Errorf("房间已满（上限 %d）", r.maxParticipants())
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
	stale := make([]string, 0, 4)
	if resetMedia {
		for id, t := range r.tracks {
			if t.info.PublisherID == participantID {
				delete(r.tracks, id)
				stale = append(stale, id)
			}
		}
		for _, subs := range r.subscriptions {
			for _, id := range stale {
				delete(subs, id)
			}
		}
	}
	others := r.othersLocked(participantID)
	r.mu.Unlock()

	if resetMedia {
		p.ResetMedia()
		for _, id := range stale {
			log.Printf("sfu: 重连清理旧轨道 room=%s track=%s（客户端将重新发布）", r.ID, id)
			for _, o := range others {
				o.NotifyTo(&protocol.Message{Type: protocol.TypeTrackUnpublished, TrackID: id})
			}
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
func (r *Room) expireDetached(participantID string) {
	r.mu.Lock()
	_, ok := r.participants[participantID]
	r.mu.Unlock()
	if !ok {
		return
	}
	log.Printf("sfu: 恢复宽限期已过，参与者离会 room=%s participant=%s", r.ID, participantID)
	r.Leave(participantID)
}

// Leave 让参与者离开：清理其轨道与订阅关系，并广播事件。
func (r *Room) Leave(participantID string) {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if !ok {
		r.mu.Unlock()
		return
	}
	if t, exists := r.grace[participantID]; exists {
		t.Stop()
		delete(r.grace, participantID)
	}
	delete(r.participants, participantID)
	delete(r.subscriptions, participantID)
	delete(r.hands, participantID)

	// 该参与者发布的轨道全部下线
	removed := make([]string, 0, len(r.tracks))
	for id, t := range r.tracks {
		if t.info.PublisherID == participantID {
			delete(r.tracks, id)
			removed = append(removed, id)
		}
	}
	// 其他人对该参与者的订阅关系一并清理
	for _, subs := range r.subscriptions {
		for _, id := range removed {
			delete(subs, id)
		}
	}
	others := r.othersLocked(participantID)
	empty := len(r.participants) == 0
	r.mu.Unlock()

	p.Close()

	for _, o := range others {
		o.NotifyTo(&protocol.Message{Type: protocol.TypeParticipantLeave, ParticipantID: participantID})
	}
	for _, id := range removed {
		log.Printf("sfu: 轨道下线 room=%s track=%s 原因=参与者离会", r.ID, id)
		for _, o := range others {
			o.NotifyTo(&protocol.Message{Type: protocol.TypeTrackUnpublished, TrackID: id})
		}
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

	log.Printf("sfu: 建立订阅 room=%s subscriber=%s track=%s", r.ID, subscriberID, trackID)
	return offer, nil
}

func (r *Room) Unsubscribe(subscriberID, trackID string) error {
	r.mu.RLock()
	sub, ok := r.participants[subscriberID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("参与者不存在")
	}
	if err := sub.Unsubscribe(trackID); err != nil {
		return err
	}
	r.mu.Lock()
	if subs := r.subscriptions[subscriberID]; subs != nil {
		delete(subs, trackID)
	}
	r.mu.Unlock()
	return nil
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
	r.mu.RLock()
	defer r.mu.RUnlock()

	state := &protocol.RoomState{
		RoomID:       r.ID,
		Participants: make([]*protocol.PeerInfo, 0, len(r.participants)),
		Tracks:       make([]*protocol.TrackInfo, 0, len(r.tracks)),
	}
	for _, p := range r.participants {
		state.Participants = append(state.Participants, p.Info())
	}
	for _, t := range r.tracks {
		state.Tracks = append(state.Tracks, t.Info())
	}
	for id := range r.hands {
		state.Hands = append(state.Hands, id)
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
func (r *Room) OnReaction(fromID, emoji string) {
	payload, err := json.Marshal(&protocol.DataMessage{
		Type:   protocol.DataReaction,
		Emoji:  emoji,
		FromID: fromID, // 身份由服务端填，客户端无法伪造
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
}

// DebugInfo 输出房间内所有参与者的转发与连接状态。
func (r *Room) DebugInfo() *RoomDebug {
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
	r.mu.RUnlock()

	debug := &RoomDebug{RoomID: r.ID, Participants: make([]*ParticipantDebug, 0, len(participants))}
	for _, p := range participants {
		debug.Participants = append(debug.Participants, p.DebugInfo(subs[p.ID]))
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
