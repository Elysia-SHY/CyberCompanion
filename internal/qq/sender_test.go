package qq

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// withFakeSend 临时替换真实投递实现，返回调用次数计数器。
// 目的：让发送队列的重试与丢弃逻辑可以在不发真实网络请求的前提下测试。
func withFakeSend(t *testing.T, err error) *int64 {
	t.Helper()
	orig := sendOnce
	var calls int64
	sendOnce = func(m *outboundMsg) error {
		atomic.AddInt64(&calls, 1)
		return err
	}
	t.Cleanup(func() { sendOnce = orig })
	return &calls
}

func TestDeliver_RetriesUntilAttemptLimit(t *testing.T) {
	withFakeSend(t, errors.New("模拟网络失败"))

	ch := make(chan *outboundMsg, 4)
	m := &outboundMsg{Target: "u1", Content: "hi"}

	deliver(ch, m)
	if m.Attempts != 1 {
		t.Fatalf("第一次失败后 Attempts 应为 1，实际 %d", m.Attempts)
	}
	if len(ch) != 1 {
		t.Fatalf("失败后应重新入队等待重试，队列长度 %d", len(ch))
	}
	if m.notBefore.IsZero() {
		t.Error("重试应设置退避到期时间")
	}

	// 第二次失败仍会重入队（队列变为 2 条）
	deliver(ch, m)
	if m.Attempts != 2 {
		t.Fatalf("第二次失败后 Attempts 应为 2，实际 %d", m.Attempts)
	}
	if got := len(ch); got != 2 {
		t.Fatalf("第二次失败后应再次入队，队列长度 %d", got)
	}

	// 第三次达到上限：不再入队
	deliver(ch, m)
	if m.Attempts != sendMaxAttempts {
		t.Errorf("达到上限后不应继续重试，Attempts=%d", m.Attempts)
	}
	if got := len(ch); got != 2 {
		t.Errorf("放弃后不应再入队，队列长度 %d（应为 2）", got)
	}
}

func TestDeliver_SuccessDoesNotRequeue(t *testing.T) {
	calls := withFakeSend(t, nil)
	ch := make(chan *outboundMsg, 4)
	deliver(ch, &outboundMsg{Target: "u1", Content: "hi"})
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("成功投递应只调用一次，实际 %d", got)
	}
	if len(ch) != 0 {
		t.Error("成功投递不应重新入队")
	}
}

func TestDeliver_DropsWhenRetryQueueFull(t *testing.T) {
	withFakeSend(t, errors.New("模拟网络失败"))
	ch := make(chan *outboundMsg, 1)
	// 先把队列占满
	ch <- &outboundMsg{Target: "blocker"}

	before := atomic.LoadInt64(&sendDropped)
	deliver(ch, &outboundMsg{Target: "u1", Content: "hi"})
	if atomic.LoadInt64(&sendDropped) != before+1 {
		t.Error("重试队列满时应记录丢弃")
	}
}

func TestEnqueue_ReportsQueueFull(t *testing.T) {
	if sendQueueCh == nil {
		t.Skip("发送队列未初始化")
	}
	// 用一个临时 channel 验证边界行为（不影响全局 worker）
	orig := sendQueueCh
	defer func() { sendQueueCh = orig }()

	sendQueueCh = make(chan *outboundMsg, 1)
	if !enqueue(&outboundMsg{Content: "第一条"}) {
		t.Fatal("第一条消息应入队成功")
	}
	if enqueue(&outboundMsg{Content: "第二条"}) {
		t.Fatal("队列已满时入队应返回 false（丢弃而非阻塞）")
	}
}

func TestSplitMessage_AppliedBySendTextSegmented(t *testing.T) {
	orig := sendQueueCh
	defer func() { sendQueueCh = orig }()

	sendQueueCh = make(chan *outboundMsg, 32)
	long := make([]rune, 0, 1200)
	for i := 0; i < 1200; i++ {
		long = append(long, '字')
	}
	SendTextSegmented("u1", "", string(long), "msg-1")

	got := len(sendQueueCh)
	if got < 2 {
		t.Fatalf("超长回复应被切分为多条，实际 %d 条", got)
	}
	if got > maxSegments {
		t.Fatalf("切分段数应受 maxSegments(%d) 限制，实际 %d", maxSegments, got)
	}

	// 只有第一段携带 msg_id，避免重复引用同一条消息
	first := <-sendQueueCh
	if first.MsgID != "msg-1" {
		t.Errorf("第一段应携带 msg_id，实际 %q", first.MsgID)
	}
	second := <-sendQueueCh
	if second.MsgID != "" {
		t.Errorf("后续分段不应重复携带 msg_id，实际 %q", second.MsgID)
	}
}

func TestStopSender_DrainsQueue(t *testing.T) {
	if sendQueueCh == nil {
		t.Skip("发送队列未初始化")
	}
	calls := withFakeSend(t, nil)

	// 注意：worker 持有的是启动时那条 channel，这里必须往全局队列投递，
	// 替换 sendQueueCh 的话消息不会进入 worker。
	if !enqueue(&outboundMsg{Content: "退出前补发"}) {
		t.Fatal("消息入队失败")
	}

	done := make(chan struct{})
	go func() {
		StopSender()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(senderStopTimeout + 3*time.Second):
		t.Fatal("StopSender 未能在预期时间内返回")
	}

	if atomic.LoadInt64(calls) < 1 {
		t.Error("退出前应尽力把队列里的消息发出去")
	}
}
