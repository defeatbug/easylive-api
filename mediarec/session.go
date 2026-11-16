package mediarec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Session 是一次录制：一个房间、从开始到停止这一段时间。
//
// 为什么要"会话"而不是一个全局开关：录制必须能被主持人**在会中随时启停**，
// 而且停下来之后要能告诉用户"录了什么、在哪、多大、多长"。
// 清单落到磁盘（manifest.json），所以服务重启后列表依然在——
// 内存里的状态只用于"当前是否正在录"。
type Session struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"roomId"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	StartedBy string     `json:"startedBy"`
	Files     []FileInfo `json:"files"`

	// stopping 表示该会话正在收尾（写入器正在 Close、统计正在回填）。
	// 不落盘：它只是内存里的一小段过渡态。收尾期间该房间不接受新的开始请求，
	// 也不会被当成"仍在录制"（否则两个主持人同时点开始/停止会拿到正在死亡的会话）。
	stopping bool
}

// FileInfo 是会话里的一条轨道文件。
type FileInfo struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"` // video | audio
	Source      string `json:"source"`
	Participant string `json:"participant"`
	SizeBytes   int64  `json:"sizeBytes"`
	Frames      uint64 `json:"frames"`
	Dropped     uint64 `json:"dropped"`
	Corrupt     uint64 `json:"corrupt"`
	DurationMs  int64  `json:"durationMs"`
	// FfmpegHint 是把这个文件转成通用 mp4 的一行命令（零转码，-c copy）。
	// 服务端不装 ffmpeg，所以合流/转封装交给使用者本机执行——这一点在 README 里写明。
	FfmpegHint string `json:"ffmpegHint"`
}

// safeName 只允许文件名里出现安全字符，防止路径穿越（API 会用它拼路径）。
var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeName 判断一个名字是否可以安全地当作路径片段使用。
func IsSafeName(s string) bool {
	return safeName.MatchString(s) && !strings.Contains(s, "..")
}

// StartSession 开始一次录制。id 形如 20261013-101500-room-abc，天然按时间排序。
//
// 会话按 roomID 隔离：同一个房间重复开始是幂等的（返回已有会话），
// 不同房间互不影响——见 Manager.sessions 的注释。
func (m *Manager) StartSession(roomID, startedBy string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("录制管理器已关闭")
	}
	if s := m.sessions[roomID]; s != nil {
		if s.stopping {
			// 收尾中：不把正在死亡的会话交出去（调用方会重新发布轨道，写进去也是白写）
			return nil, fmt.Errorf("该房间正在停止录制，请稍后重试")
		}
		return s, nil // 该房间已经在录：幂等，不重复开
	}
	id := time.Now().Format("20060102-150405") + "-" + sanitize(roomID)
	// 同名（同一秒内重复开始）时加序号，避免两个会话目录撞在一起
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(m.dir, id)); os.IsNotExist(err) {
			break
		}
		id = time.Now().Format("20060102-150405") + fmt.Sprintf("-%d-", i) + sanitize(roomID)
	}
	dir := filepath.Join(m.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建录制会话目录失败: %w", err)
	}
	s := &Session{ID: id, RoomID: roomID, StartedAt: time.Now(), StartedBy: startedBy, Files: []FileInfo{}}
	m.sessions[roomID] = s
	m.sessionDirs[roomID] = dir
	m.persistLocked(s)
	logf("录制开始 session=%s room=%s by=%s dir=%s", s.ID, roomID, startedBy, dir)
	return s, nil
}

// StopSession 结束**指定房间**的录制：关掉该房间的写入器并写清单。
// 没有在录时返回 nil，便于调用方做幂等处理。
func (m *Manager) StopSession(roomID string) *Session {
	m.mu.Lock()
	s := m.sessions[roomID]
	if s == nil {
		m.mu.Unlock()
		return nil
	}
	// 标记收尾中：这期间该房间不接新的开始请求，也对外报告"未在录制"。
	// 注意**先不把会话从 map 里摘掉**：写入器 Close 时会把真实统计回调进 updateFile，
	// 它要靠 map 找到会话。旧实现先摘会话再 Close，结果帧数/时长永远回填不进去
	// （清单里全是 0 帧——测试正是这样抓到的）。
	s.stopping = true
	writers := make([]*Writer, 0, len(m.active))
	for path, w := range m.active {
		// 只收本房间的写入器：其他房间的录制必须继续（见 Manager.sessions 的注释）。
		if w.roomID != roomID {
			continue
		}
		writers = append(writers, w)
		delete(m.active, path)
	}
	m.mu.Unlock()

	// 先关写入器（它们在收尾时回填帧数/时长到清单）
	for _, w := range writers {
		w.Close()
	}

	now := time.Now()
	m.mu.Lock()
	s.EndedAt = &now
	s.stopping = false
	delete(m.sessions, roomID)
	delete(m.sessionDirs, roomID)
	m.persistLocked(s)
	m.mu.Unlock()
	logf("录制结束 session=%s room=%s 文件=%d", s.ID, roomID, len(s.Files))
	return s
}

// CurrentSession 返回指定房间当前录制中的会话（没有则 nil）。返回的是副本，调用方随便读。
func (m *Manager) CurrentSession(roomID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[roomID]
	if s == nil {
		return nil
	}
	cp := *s
	cp.Files = append([]FileInfo(nil), s.Files...)
	return &cp
}

// Recording 报告指定房间当前是否正在录制（收尾中的会话不算"正在录制"）。
func (m *Manager) Recording(roomID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[roomID]
	return s != nil && !s.stopping
}

// RecordingRooms 返回正在录制的房间 ID 列表（观测/收尾用；不含收尾中的会话）。
func (m *Manager) RecordingRooms() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id, s := range m.sessions {
		if s.stopping {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// ListSessions 从磁盘读取历史会话（按开始时间倒序）。
//
// 为什么以磁盘清单为准：服务重启后内存状态没了，但"录过什么"必须还在。
// 目录存在但没有清单（例如进程被强杀在第一次写清单之前）会被跳过并记日志。
func (m *Manager) ListSessions() ([]Session, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(m.dir, e.Name(), "manifest.json"))
		if rerr != nil {
			continue // 没有清单的目录（或权限问题）直接跳过
		}
		var s Session
		if jerr := json.Unmarshal(raw, &s); jerr != nil {
			logf("会话清单损坏，跳过 session=%s: %v", e.Name(), jerr)
			continue
		}
		// 磁盘上的实际大小比清单更可信（进程被强杀时清单里的 size 可能是旧的）
		for i := range s.Files {
			if info, serr := os.Stat(filepath.Join(m.dir, s.ID, s.Files[i].Name)); serr == nil {
				s.Files[i].SizeBytes = info.Size()
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// SessionPath 返回某个会话里某个文件的绝对路径；不安全的名字直接拒绝。
func (m *Manager) SessionPath(sessionID, name string) (string, bool) {
	if !IsSafeName(sessionID) || !IsSafeName(name) {
		return "", false
	}
	return filepath.Join(m.dir, sessionID, name), true
}

// ---------- 内部：文件登记与清单落盘 ----------

// registerFileLocked 在指定房间的会话里登记一条文件（Open 时调用，此时大小还是 0）。
func (m *Manager) registerFileLocked(roomID string, fi FileInfo) {
	s := m.sessions[roomID]
	if s == nil {
		return
	}
	s.Files = append(s.Files, fi)
	m.persistLocked(s)
}

// updateFile 在文件收尾后用真实统计刷新清单（大小/帧数/丢帧/时长）。
//
// 带 roomID 而不是只靠文件名：同名文件理论上可能出现在不同房间的会话里，
// 而暂停/收尾是并发的——按房间查会话，才不会把统计写到别人的清单上。
func (m *Manager) updateFile(roomID, name string, frames, dropped, corrupt uint64, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[roomID]
	if s == nil {
		// 会话已经被 StopSession 摘掉（收尾顺序保证先 Close 写入器再摘会话，
		// 所以正常路径不会落在这里）；此时也不再回写，避免写坏已经结束的清单。
		return
	}
	for i := range s.Files {
		if s.Files[i].Name != name {
			continue
		}
		fi := &s.Files[i]
		fi.Frames, fi.Dropped, fi.Corrupt = frames, dropped, corrupt
		fi.DurationMs = duration.Milliseconds()
		if info, err := os.Stat(filepath.Join(m.sessionDirs[roomID], name)); err == nil {
			fi.SizeBytes = info.Size()
		}
		fi.FfmpegHint = muxHint(fi)
		m.persistLocked(s)
		return
	}
}

// persistLocked 把清单写到 <会话目录>/manifest.json（原子替换，避免半截文件）。
func (m *Manager) persistLocked(s *Session) {
	dir := m.sessionDirs[s.RoomID]
	if dir == "" {
		dir = filepath.Join(m.dir, s.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("写清单前建目录失败: %v", err)
		return
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		logf("序列化清单失败: %v", err)
		return
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		logf("写清单失败: %v", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		logf("清单原子替换失败: %v", err)
	}
}

// muxHint 生成把该文件转封装成可播放文件的一行 ffmpeg 命令（零转码）。
//
// 容器必须选 **WebM**：实测 VP8 塞不进 MP4（ffmpeg 报 Could not find tag for codec vp8），
// Opus 在 MP4 里也不是常规组合；WebM 原生支持 VP8 + Opus，浏览器与 VLC 都能直接播。
func muxHint(fi *FileInfo) string {
	if fi.Kind == "video" {
		return fmt.Sprintf("ffmpeg -i %s -c:v copy %s.webm", fi.Name, strings.TrimSuffix(fi.Name, ".ivf"))
	}
	return fmt.Sprintf("ffmpeg -i %s -c:a copy %s.webm", fi.Name, strings.TrimSuffix(fi.Name, ".ogg"))
}
