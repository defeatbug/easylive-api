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

// Manager 管理一次录制会话的所有轨道文件。
type Manager struct {
	dir string

	mu     sync.Mutex
	active map[string]*Writer
	closed bool
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
	return &Manager{dir: abs, active: map[string]*Writer{}}, nil
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
	name := fmt.Sprintf("%s-%s-%s-%s.%s",
		time.Now().Format("20060102-150405"),
		sanitize(roomID), sanitize(participantID), sanitize(source), ext(c.Name))
	path := filepath.Join(m.dir, name)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("创建录制文件失败: %w", err)
	}
	w := &Writer{
		path:   path,
		file:   f,
		codec:  c,
		frames: make(chan *rtp.Packet, queueSize),
		done:   make(chan struct{}),
	}
	if strings.EqualFold(c.Name, "VP8") {
		w.ivf = &IVFWriter{w: f}
	}
	go w.run()
	m.active[path] = w
	return w, nil
}

// CloseAll 结束所有录制（服务退出或录制被关闭时调用）。
func (m *Manager) CloseAll() {
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
	path  string
	file  *os.File
	codec Codec
	ivf   *IVFWriter

	frames chan *rtp.Packet
	done   chan struct{}
	// closeOnce 让 Close 幂等：管理器统一收尾时可能对同一条轨道再关一次，
	// 直接 close 已关闭的 channel 会 panic（测试就抓到了这一点）。
	closeOnce sync.Once

	dropped atomic.Uint64
	written atomic.Uint64
}

// Write 把包交给后台写协程。**永不阻塞**：队列满时丢包并计数。
func (w *Writer) Write(pkt *rtp.Packet) {
	select {
	case w.frames <- pkt:
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
		close(w.frames)
		<-w.done
	})
	return nil
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
		_ = w.file.Close()
		frames, dropped := uint64(0), w.dropped.Load()
		if w.ivf != nil {
			frames = uint64(w.ivf.Frames())
		}
		logf("录制结束 file=%s 帧=%d 丢=%d 编码=%s", filepath.Base(w.path), frames, dropped, w.codec)
	}()

	for pkt := range w.frames {
		if w.ivf == nil {
			continue // 音频（Opus）在下一轮实现，先如实跳过
		}
		if err := w.ivf.WritePacket(pkt); err != nil {
			logf("录制写入失败 file=%s: %v", filepath.Base(w.path), err)
			return
		}
		w.written.Add(1)
	}
}

func ext(codec string) string {
	if strings.EqualFold(codec, "VP8") {
		return "ivf"
	}
	return "bin"
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
