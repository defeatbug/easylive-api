package mediarec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// queueSize 是每条轨道的写队列长度（约 2 秒的视频帧）。
//
// 队列存在的意义：**磁盘慢绝不能拖住转发**。队列满就丢帧并计数，
// 由 dropped 指标对外暴露（"录到多少、丢了多少"是可解释的，而不是悄悄丢）。
const queueSize = 120

// Manager 管理所有房间的录制会话及其轨道文件。
type Manager struct {
	dir string

	mu     sync.Mutex
	active map[string]*Writer
	closed bool
	// sessions/sessionDirs 按 roomID 维护当前录制会话（见 session.go）。
	//
	// 为什么必须按房间而不是全局单例：录制开关是**房间级** API
	// （主持人在自己房间里点录制），而同一节点上可以有多个房间同时开会。
	// 旧实现用单字段 session/sessionDir，于是 B 房开始录制会拿到 A 房的会话
	// （文件写进 A 的目录、清单也记在 A 上），A 房空房回收时还会把 B 房的录制一起停掉。
	sessions    map[string]*Session
	sessionDirs map[string]string
	// rawDump 打开后，每条轨道额外落一份**原始 RTP 转储**（.rtp），
	// 供离线分析/复盘使用。默认关闭：它会让磁盘占用翻倍。
	rawDump bool
}

// EnableRawDump 打开原始 RTP 转储（调试/离线分析用）。
func (m *Manager) EnableRawDump() {
	m.mu.Lock()
	m.rawDump = true
	m.mu.Unlock()
}

// RawDumpEnabled 报告是否开启了原始转储。
func (m *Manager) RawDumpEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rawDump
}

// New 创建录制管理器；dir 不存在会自动创建。
func New(dir string) (*Manager, error) {
	if dir == "" {
		dir = "recordings"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建录制目录失败: %w", err)
	}
	return &Manager{
		dir:         abs,
		active:      map[string]*Writer{},
		sessions:    map[string]*Session{},
		sessionDirs: map[string]string{},
	}, nil
}

// Dir 返回录制根目录。
func (m *Manager) Dir() string { return m.dir }

// Open 为一条轨道创建落盘文件。文件名包含房间/参与者/来源与时间戳，便于事后定位。
func (m *Manager) Open(roomID, participantID, source string, c Codec) (*Writer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("录制已停止")
	}
	dir := m.dir
	if d, ok := m.sessionDirs[roomID]; ok {
		dir = d
	}
	// 同一会话里按毫秒区分，避免同名参与者多次发布会话内重名覆盖
	name := fmt.Sprintf("%s-%s-%s-%s.%s",
		time.Now().Format("150405.000"),
		sanitize(roomID), sanitize(participantID), sanitize(source), ext(c.Name))
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("创建录制文件失败: %w", err)
	}
	kind := "video"
	if strings.EqualFold(c.Name, "opus") {
		kind = "audio"
	}
	w := &Writer{
		path:      path,
		roomID:    roomID,
		manager:   m,
		file:      f,
		codec:     c,
		frames:    make(chan *rtp.Packet, queueSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		clockRate: c.ClockRate,
		onClosed: func(frames, dropped, corrupt uint64, dur time.Duration) {
			m.updateFile(roomID, name, frames, dropped, corrupt, dur)
		},
	}
	m.registerFileLocked(roomID, FileInfo{
		Name: name, Kind: kind, Source: source, Participant: participantID,
		FfmpegHint: muxHint(&FileInfo{Name: name, Kind: kind}),
	})
	// 原始转储（可选）：把每个包原样另存一份，供离线复盘录制缺陷
	if m.rawDump {
		kind := "video"
		if strings.EqualFold(c.Name, "opus") {
			kind = "audio"
		}
		d, derr := newRawDump(path+".rtp", rawDumpHeader{
			Codec: c.Name, ClockRate: c.ClockRate, PayloadType: c.PayloadType, SSRC: c.SSRC,
			Room: roomID, Participant: participantID, Source: source, Kind: kind,
		})
		if derr == nil {
			w.dump = d
		} else {
			logf("原始转储创建失败: %v", derr)
		}
	}
	switch {
	case strings.EqualFold(c.Name, "VP8"):
		w.ivf = &IVFWriter{w: f}
	case strings.EqualFold(c.Name, "opus"):
		// 音频写 Ogg Opus：ffmpeg / VLC / 浏览器可直接播放
		w.ogg = newOggWriter(f, 2, uint32(time.Now().UnixNano()))
	}
	go w.run()
	m.active[path] = w
	return w, nil
}

// CloseAll 结束所有录制（服务退出或录制被关闭时调用）。
func (m *Manager) CloseAll() {
	// 先正常收尾**每个房间**的会话（各自写清单、回填统计），再关掉残余写入器。
	// 不能只收一个：多房间同时录制时会把其他房间的清单丢掉。
	m.mu.Lock()
	roomIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		roomIDs = append(roomIDs, id)
	}
	m.mu.Unlock()
	for _, id := range roomIDs {
		m.StopSession(id)
	}

	m.mu.Lock()
	m.closed = true
	writers := make([]*Writer, 0, len(m.active))
	for _, w := range m.active {
		writers = append(writers, w)
	}
	m.active = map[string]*Writer{}
	m.mu.Unlock()
	for _, w := range writers {
		w.Close()
	}
}

// Active 返回进行中的录制数（观测用）。
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Writer 是一条轨道的落盘写入器：对外只暴露非阻塞的 Write。
type Writer struct {
	path   string
	roomID string
	// manager 用于关闭后把自己从 Manager.active 摘掉（见 Close）。
	// 观测计数必须如实反映"还在写的轨道"，否则 Active() 会长期虚高，
	// 已关闭的 Writer 也会被 map 一直引用无法回收。
	manager *Manager
	file    *os.File
	codec   Codec
	ivf     *IVFWriter
	ogg     *oggWriter
	dump    *rawDump

	frames chan *rtp.Packet
	// stop 用来通知写协程"收尾并退出"。
	//
	// **绝不能靠 close(frames) 收尾**：Write 在媒体热路径上，调用方顺序是
	// 「热路径 Load 到写入器 → 调 Write」，中间没有同步点。若 Close 在此处
	// close(w.frames)，Write 的 send 就会撞上已关闭的 channel 而 panic；
	// 热路径跑在 safeGo 里，panic 被兜住后这条轨道再也不会转发——
	// 表现是"停止录制之后，某个人的画面/声音对所有人永久消失"。
	// 所以用单独的信号 channel：stop 只关一次，frames 永不关闭，
	// 写协程收到 stop 后把队列里剩余的包写完再退出。
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	dropped atomic.Uint64
	written atomic.Uint64

	// 时长统计：用 RTP 时间戳的跨度换算，不需要额外解码
	clockRate uint32
	firstTS   uint32
	lastTS    uint32
	haveTS    bool

	// onClosed 在文件收尾后被调用，用于把真实统计刷进会话清单
	onClosed func(frames, dropped, corrupt uint64, dur time.Duration)
}

// Write 把包交给后台写协程。**永不阻塞**：队列满时丢包并计数。
func (w *Writer) Write(pkt *rtp.Packet) {
	// 已停止：直接丢弃，不再入队（也避免与收尾流程竞争）。
	select {
	case <-w.stop:
		return
	default:
	}
	// 必须克隆：热路径在 Write 返回后会重写这个包的 Header（剥离头扩展），
	// 而写协程是并发读它的另一个 goroutine。不克隆时 race detector 会直接报数据竞争
	// （rawDump 的 Marshal 会把整个 header 读一遍，最容易命中）。
	// 代价是录制期间每包一次分配——只在录制时付，且与远端磁盘 IO 同一量级。
	cp := pkt.Clone()
	select {
	case w.frames <- cp:
	case <-w.stop:
	default:
		// 队列满：丢帧。原因通常是磁盘慢，记下来而不是拖住转发。
		if n := w.dropped.Add(1); n == 1 || n%500 == 0 {
			logf("录制队列已满，丢帧（累计 %d）file=%s", n, filepath.Base(w.path))
		}
	}
}

// Close 停止写协程、flush 并关文件。可重复调用（幂等）。
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
		// 写协程退出后再把自己从 active 表摘掉：
		// 轨道结束（Pump 的 defer）、停止录制、空房回收都会走到 Close，但旧实现只有
		// StopSession/CloseAll 清表，于是 active 里堆着已关闭的 Writer——Active() 虚高，
		// 且这些对象永远不被回收（内存与文件句柄的观测口径都会被污染）。
		if w.manager != nil {
			w.manager.forgetWriter(w.path, w)
		}
	})
	return nil
}

// forgetWriter 从 active 表里摘掉一个写入器。
//
// 必须比对指针：文件名带毫秒时间戳理论上可能复用（极端并发下），
// 直接 delete(path) 有极小概率把刚注册的新 Writer 删掉。
func (m *Manager) forgetWriter(path string, w *Writer) {
	m.mu.Lock()
	if cur, ok := m.active[path]; ok && cur == w {
		delete(m.active, path)
	}
	m.mu.Unlock()
}

// Path / Dropped / Written 供日志与观测使用。
func (w *Writer) Path() string    { return w.path }
func (w *Writer) Dropped() uint64 { return w.dropped.Load() }
func (w *Writer) Written() uint64 { return w.written.Load() }
func (w *Writer) Codec() Codec    { return w.codec }

func (w *Writer) run() {
	defer close(w.done)
	defer func() {
		if w.ivf != nil {
			if err := w.ivf.Close(); err != nil {
				logf("录制回填帧数失败 file=%s: %v", filepath.Base(w.path), err)
			}
		}
		if w.ogg != nil {
			// 不在这里 Flush：让 Close 把最后一页（含待写包）作为 EOS 页写出去，
			// 否则会落到"无待写数据"分支，历史上正是它产生了非法空包页。
			if err := w.ogg.Close(); err != nil {
				logf("录制收尾 Ogg 失败 file=%s: %v", filepath.Base(w.path), err)
			}
		}
		_ = w.file.Close()
		if w.dump != nil {
			if err := w.dump.Close(); err != nil {
				logf("原始转储收尾失败: %v", err)
			}
		}
		frames, dropped, corrupt := uint64(0), w.dropped.Load(), uint64(0)
		if w.ivf != nil {
			frames = uint64(w.ivf.Frames())
			corrupt = w.ivf.CorruptFrames()
		}
		if w.ogg != nil {
			frames = w.written.Load()
		}
		dup := uint64(0)
		if w.ivf != nil {
			dup = w.ivf.DuplicatePackets()
		}
		var dur time.Duration
		if w.haveTS && w.clockRate > 0 {
			dur = time.Duration(float64(w.lastTS-w.firstTS)/float64(w.clockRate)*1000) * time.Millisecond
		}
		logf("录制结束 file=%s 帧=%d 队列丢弃=%d 缺包丢帧=%d 重复包去重=%d 时长=%s 编码=%s",
			filepath.Base(w.path), frames, dropped, corrupt, dup, dur.Round(time.Millisecond), w.codec)
		if w.onClosed != nil {
			w.onClosed(frames, dropped, corrupt, dur)
		}
	}()

	// 收尾协议见 Writer.stop 的注释：frames 永不关闭，收到 stop 后把剩余包写完再退出。
	for {
		select {
		case pkt := <-w.frames:
			if !w.writePacket(pkt) {
				return
			}
		case <-w.stop:
			// 把队列里已经入队的包排空（数量有界，≤ queueSize），
			// 避免"停止录制时最后几帧被静默丢弃"。
			for {
				select {
				case pkt := <-w.frames:
					if !w.writePacket(pkt) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// writePacket 处理一个包；返回 false 表示发生不可恢复的写入错误，写协程应退出。
func (w *Writer) writePacket(pkt *rtp.Packet) bool {
	var err error
	if !w.haveTS {
		w.firstTS, w.haveTS = pkt.Timestamp, true
	}
	w.lastTS = pkt.Timestamp
	if w.dump != nil {
		w.dump.write(pkt) // 原始包先落盘，供离线复盘
	}
	switch {
	case w.ivf != nil:
		err = w.ivf.WritePacket(pkt)
	case w.ogg != nil:
		// 空载荷包（padding/RTX 等）不是有效的 Opus 包，写进去会让 demuxer 直接报错
		if len(pkt.Payload) == 0 {
			return true
		}
		w.ogg.WritePacket(pkt.Payload, pkt.Timestamp)
		err = nil
	default:
		return true
	}
	if err != nil {
		logf("录制写入失败 file=%s: %v", filepath.Base(w.path), err)
		return false
	}
	w.written.Add(1)
	return true
}

func ext(codec string) string {
	switch {
	case strings.EqualFold(codec, "VP8"):
		return "ivf"
	case strings.EqualFold(codec, "opus"):
		return "ogg"
	default:
		return "bin"
	}
}

// sanitize 把 ID 里的路径分隔符与冒号换成下划线，避免录制文件写到目录外。
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "unknown"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
