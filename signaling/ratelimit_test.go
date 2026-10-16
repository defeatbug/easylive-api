package signaling

import (
	"testing"
	"time"
)

// TestAllowMessageBudget 验证单连接消息总量预算。
//
// 白板有自己的限速（30/s），但其它消息类型此前没有任何总量约束：
// 一个客户端可以无限刷 chat / publish 等，而每条都可能引发一次全员广播。
// 这层预算就是那个缺口。
func TestAllowMessageBudget(t *testing.T) {
	c := &Client{}
	// 预算内应当全部放行
	for i := 0; i < maxMessagesPerSecond; i++ {
		if !c.allowMessage() {
			t.Fatalf("第 %d 条不应被拒绝（预算 %d）", i+1, maxMessagesPerSecond)
		}
	}
	// 超出预算必须被拒绝
	if c.allowMessage() {
		t.Fatalf("超过 %d 条/秒后应当被拒绝", maxMessagesPerSecond)
	}
	// 窗口滑过后恢复
	c.mu.Lock()
	c.msgWindow = time.Now().Add(-2 * time.Second)
	c.mu.Unlock()
	if !c.allowMessage() {
		t.Fatal("窗口滑过后应当重新放行")
	}
}

// TestMessageFloodCounter 验证超速计数存在（观测用：这个数字不该增长）。
func TestMessageFloodCounter(t *testing.T) {
	before := MessageFloodCount()
	messageFloods.Add(1)
	if MessageFloodCount() != before+1 {
		t.Fatal("计数未生效")
	}
}
