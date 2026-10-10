// Package loadtest 是合成压测客户端：在一台机器上模拟 N 路参与者。
//
// 为什么不用浏览器压测：浏览器的编码/解码/渲染能力会先成为瓶颈，
// 测出来的"上限"其实是测试机的能力，而不是服务器的能力。
// 合成客户端只收发 RTP、不编码不解码，因此测的正是**服务端的转发与信令开销**。
//
// 诚实边界：这里发的是合成 RTP 载荷（不是可解码的 VP8/Opus）。
// 服务端不解析媒体，所以对"转发开销"的测量没有影响；
// 但主观画质、端侧解码耗时这类指标必须用真实浏览器测，两者互补而非替代。
package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/websocket"

	"easylive-api/protocol"
)

// wsConn 是最小可用的信令客户端：请求-响应匹配 + 事件通道。
//
// 只实现压测需要的部分（join / publish / subscribe / candidate / leave），
// 刻意不复用测试里的 peer：那是 _test 包，不可导入；而且压测客户端需要
// 暴露统计与错误，而不是在断言失败时 panic。
type wsConn struct {
	conn *websocket.Conn

	mu      sync.Mutex
	pending map[string]chan *protocol.Message
	closed  bool

	events chan *protocol.Message
	errc   chan error
}

func dialWS(ctx context.Context, url string) (*wsConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("连接信令失败: %w", err)
	}
	c := &wsConn{
		conn:    conn,
		pending: map[string]chan *protocol.Message{},
		events:  make(chan *protocol.Message, 256),
		errc:    make(chan error, 1),
	}
	go c.readLoop()
	return c, nil
}

func (c *wsConn) readLoop() {
	defer close(c.events)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.closed = true
			c.mu.Unlock()
			select {
			case c.errc <- err:
			default:
			}
			return
		}
		var msg protocol.Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.Type == protocol.TypeCandidate {
			// 候选走事件通道由 lane 处理（需要判断远端描述是否就绪）
			select {
			case c.events <- &msg:
			default:
			}
			continue
		}
		if msg.ID != "" {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- &msg
				close(ch)
				continue
			}
		}
		select {
		case c.events <- &msg:
		default: // 事件积压时丢弃：压测客户端不关心聊天等非关键事件
		}
	}
}

func (c *wsConn) send(msg *protocol.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("连接已关闭")
	}
	return c.conn.WriteJSON(msg)
}

// request 发送请求并等待指定类型的响应。
func (c *wsConn) request(ctx context.Context, msg *protocol.Message, expect protocol.MsgType, timeout time.Duration) (*protocol.Message, error) {
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	ch := make(chan *protocol.Message, 1)
	c.mu.Lock()
	c.pending[msg.ID] = ch
	c.mu.Unlock()

	if err := c.send(msg); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("连接在等待 %s 时关闭", expect)
		}
		if resp.Type == protocol.TypeError {
			return nil, fmt.Errorf("服务端错误 %s: %s", resp.Code, resp.Reason)
		}
		if resp.Type != expect {
			return nil, fmt.Errorf("期望 %s，收到 %s", expect, resp.Type)
		}
		return resp, nil
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		return nil, fmt.Errorf("等待 %s 超时", expect)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *wsConn) close() {
	_ = c.conn.Close()
}

// ---------- ICE 配置 ----------

type iceServerJSON struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// httpBase 把 ws://host:port/ws 转成 http://host:port
func httpBase(wsURL string) string {
	base := strings.TrimSuffix(wsURL, "/ws")
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	return base
}

// FetchICE 取服务端下发的 ICE 配置（含 TURN 临时凭证）。
//
// 压测必须走这条路径：中继（TURN）与直连是评测里的两个对照条件，
// 而中继凭证是由服务端现签的，硬编码 STUN 就测不了中继。
func FetchICE(ctx context.Context, wsURL, identity string) ([]iceServerJSON, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpBase(wsURL)+"/ice?identity="+identity, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/ice 返回 %d", resp.StatusCode)
	}
	var out struct {
		ICEServers []iceServerJSON `json:"iceServers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.ICEServers, nil
}

// FetchToken 申请令牌；服务端未启用鉴权（501）或失败时返回空串（表示不需要）。
func FetchToken(ctx context.Context, wsURL, roomID, identity string) string {
	body, _ := json.Marshal(map[string]any{"roomId": roomID, "identity": identity, "name": identity})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpBase(wsURL)+"/token", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	// 非 development 环境需要 API Key；压测脚本从环境变量取（见 cmd/loadtest）
	if key := strings.TrimSpace(envTokenAPIKey()); key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return ""
	}
	return out.Token
}
