// Package signaling 是信令传输层：把 WebSocket 连接与 SFU 房间连接起来。
package signaling

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"

	ws "github.com/fasthttp/websocket"
	fiberws "github.com/gofiber/contrib/websocket"

	"easylive-api/config"
	"easylive-api/protocol"
	"easylive-api/sfu"
)

const (
	writeWait     = 10 * time.Second
	maxMessageLen = 256 * 1024 // 单条信令消息上限（SDP 通常几 KB）
	sendQueueLen  = 32
)

// Client 是一条 WebSocket 连接（一个信令会话）。
//
// 写并发约束：WebSocket 连接不允许并发写，因此所有出站消息都经过 send 通道，
// 由唯一的 writePump goroutine 串行写出。Notify 实现了 sfu.Notifier。
type Client struct {
	ID       string // sessionId：与连接绑定
	RemoteIP string

	conn *fiberws.Conn
	send chan *protocol.Message
	done chan struct{}
	// writerExit 在 writePump 退出时关闭：Close 先等它退出再关连接，
	// 避免"正在写"与"hijack 连接被回收"竞争（见 writePump 的说明）。
	writerExit chan struct{}

	hub  *Hub
	cfg  *config.Config
	once sync.Once

	// mu 保护以下会话字段（由信令处理协程写、writePump/其他协程读）
	mu            sync.RWMutex
	roomID        string
	participantID string
	participant   *sfu.Participant
	resumeToken   string
	// left 表示客户端主动发了 leave。主动离开要立刻清理；
	// 而连接意外断开（网络抖动、标签页休眠）则进入恢复宽限期。
	left bool
}

func newClient(conn *fiberws.Conn, hub *Hub, cfg *config.Config, remoteIP string) *Client {
	return &Client{
		ID:         newID("s"),
		RemoteIP:   remoteIP,
		conn:       conn,
		send:       make(chan *protocol.Message, sendQueueLen),
		done:       make(chan struct{}),
		writerExit: make(chan struct{}),
		hub:        hub,
		cfg:        cfg,
	}
}

// Notify 实现 sfu.Notifier：非阻塞投递，队列满说明客户端消费不过来，直接丢弃并记录。
func (c *Client) Notify(msg *protocol.Message) {
	select {
	case c.send <- msg:
	case <-c.done:
	default:
		log.Printf("signaling: 发送队列已满，丢弃消息 type=%s session=%s", msg.Type, c.ID)
	}
}

// reply 发送请求响应（带请求 id，便于客户端匹配）。
func (c *Client) reply(req *protocol.Message, msg *protocol.Message) {
	if req != nil {
		msg.ID = req.ID
	}
	msg.TS = time.Now().UnixMilli()
	c.Notify(msg)
}

func (c *Client) fail(req *protocol.Message, code, reason string) {
	c.reply(req, &protocol.Message{Type: protocol.TypeError, Code: code, Reason: reason})
}

// writePump 是唯一的写协程：串行写出站消息，并周期性发送 Ping 保活。
func (c *Client) writePump() {
	// defer 顺序（LIFO）：先注册的 writerExit 最后执行，保证"退出信号"是在
	// recover 与连接关闭之后才发出。
	defer close(c.writerExit)

	ticker := time.NewTicker(c.cfg.WSHeartbeat)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	// fasthttp 的 hijack 连接在 handler 返回后底层 net.Conn 会被置空，
	// 此时再写（SetWriteDeadline/Write）会直接空指针 panic 把整个进程带走。
	// 连接断开与写协程退出存在天然竞态，这里必须兜底。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("signaling: 写协程异常退出（连接已被回收）session=%s: %v", c.ID, r)
		}
	}()

	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteJSON(msg); err != nil {
				log.Printf("signaling: 写失败 session=%s: %v", c.ID, err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteControl(ws.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// Close 关闭连接。
//
// 两种语义必须区分开（M4 的关键）：
//   - 客户端主动 leave → 立即离会，清理轨道、订阅与恢复令牌；
//   - 连接意外断开 → 只把参与者标记为"重连中"并登记 resumeToken，
//     轨道与订阅保持不动，宽限期内重连即可对其他人完全无感地恢复。
func (c *Client) Close() {
	c.once.Do(func() {
		close(c.done)

		// 先等写协程退出（最多 500ms，正常情况立刻返回），再去关连接与改房间状态，
		// 这样"停止写入"一定发生在"连接被回收"之前。
		select {
		case <-c.writerExit:
		case <-time.After(500 * time.Millisecond):
		}

		c.mu.RLock()
		roomID, participantID, token, left := c.roomID, c.participantID, c.resumeToken, c.left
		c.mu.RUnlock()

		if roomID != "" && participantID != "" {
			if room, ok := c.hub.manager.Get(roomID); ok {
				if left {
					c.hub.dropResume(participantID)
					room.Leave(participantID) // 内部会关闭 PeerConnection 并广播事件
				} else if room.Detach(participantID) {
					// 进入宽限期：登记令牌，客户端带它重连即可恢复同一参与者身份
					c.hub.registerResume(token, roomID, participantID, c.cfg.ResumeGrace)
				}
			}
		}
		c.hub.Unregister(c)
		_ = c.conn.Close()
	})
}

// markLeft 标记"客户端已主动请求离会"。
func (c *Client) markLeft() {
	c.mu.Lock()
	c.left = true
	c.mu.Unlock()
}

func (c *Client) setResumeToken(token string) {
	c.mu.Lock()
	c.resumeToken = token
	c.mu.Unlock()
}

// session 返回当前会话的房间/参与者状态。
func (c *Client) session() (roomID, participantID string, p *sfu.Participant) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.roomID, c.participantID, c.participant
}

func (c *Client) setSession(roomID, participantID string, p *sfu.Participant) {
	c.mu.Lock()
	c.roomID, c.participantID, c.participant = roomID, participantID, p
	c.mu.Unlock()
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "_" + time.Now().Format("150405.000")
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
