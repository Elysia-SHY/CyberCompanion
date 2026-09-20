package qq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/stickers"
)

// ─── 出站发送队列 ──────────────────────────────────────────────────────────────
//
// 现状问题（优化建议书 2.3）：发送失败被静默丢弃（`_ = SendTextMessage(...)`），
// 用户永远收不到回复，日志里也查不到。
//
// 这里用「单 worker + 有界队列 + 指数退避重试」解决：
//   - 单 worker 串行发送：天然满足 QQ 的频率限制，并保证同一会话的消息顺序
//   - 有界队列：队列满时丢弃新消息而不是阻塞——宁可丢消息，也不能拖垮 WS 接收循环
//   - 退避重试：网络抖动 / 429 限流场景下自动补发，最多 sendMaxAttempts 次

const (
	sendQueueSize   = 256
	sendMaxAttempts = 3
	// sendBaseBackoff 是首次重试的等待基准，按 2 的幂次放大
	sendBaseBackoff = 800 * time.Millisecond
	sendMaxBackoff  = 8 * time.Second
	// sendMinInterval 是两条消息之间的最小间隔，避免瞬时刷屏触发风控
	sendMinInterval = 250 * time.Millisecond
	// senderStopTimeout 是优雅关闭时等待队列排空的上限
	senderStopTimeout = 5 * time.Second
)

// outboundMsg 描述一条待发送的出站消息。
type outboundMsg struct {
	Target    string                // 私聊为 user openid，群聊为 member openid
	Group     string                // 群 openid，私聊时为空
	Content   string                // 文本内容
	MsgID     string                // 被动回复时的引用消息 ID
	Sticker   *stickers.SendPayload // 非空时走媒体通道
	Attempts  int                   // 已尝试次数
	notBefore time.Time             // 退避到期时间
}

var (
	sendQueueCh    chan *outboundMsg
	senderOnce     sync.Once
	senderWG       sync.WaitGroup
	senderStop     chan struct{}
	senderStopOnce sync.Once
	// sendDropped 记录因队列满被丢弃的消息数，便于排障
	sendDropped int64
)

// StartSender 启动后台发送 worker。可重复调用，只有第一次生效。
// 在包初始化时已调用；显式暴露出来是为了让测试能独立控制生命周期。
func StartSender() {
	senderOnce.Do(func() {
		sendQueueCh = make(chan *outboundMsg, sendQueueSize)
		senderStop = make(chan struct{})
		senderWG.Add(1)
		go sendWorker(sendQueueCh, senderStop)
	})
}

func init() {
	// 发送 worker 随包初始化启动，任何调用方都不需要显式启动
	StartSender()
}

// StopSender 停止发送 worker，先排空队列中尚未发出的消息。
// 超过 timeout 仍未排空则放弃剩余，避免退出流程被卡住。
func StopSender() {
	if sendQueueCh == nil {
		return
	}
	senderStopOnce.Do(func() { close(senderStop) })

	done := make(chan struct{})
	go func() {
		senderWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(senderStopTimeout):
		AddLog("[Send] 关闭超时，队列中仍有消息被放弃")
	}
}

// DroppedMessages 返回因队列满而丢弃的消息条数。
func DroppedMessages() int64 {
	return atomic.LoadInt64(&sendDropped)
}

func sendWorker(ch chan *outboundMsg, stop chan struct{}) {
	defer senderWG.Done()

	for {
		select {
		case <-stop:
			// 优雅关闭：尽力把队列里剩下的消息发完
			for {
				select {
				case m := <-ch:
					deliver(ch, m)
				default:
					return
				}
			}
		case m := <-ch:
			if wait := time.Until(m.notBefore); wait > 0 {
				select {
				case <-time.After(wait):
				case <-stop:
				}
			}
			deliver(ch, m)
			// 串行限速
			select {
			case <-time.After(sendMinInterval):
			case <-stop:
			}
		}
	}
}

// sendOnce 执行一次真实投递。抽成变量是为了让测试能替换掉网络调用。
var sendOnce = func(m *outboundMsg) error {
	if m.Sticker != nil {
		return SendStickerMedia(m.Target, m.Group, m.MsgID, m.Sticker)
	}
	return SendTextMessage(m.Target, m.Group, m.Content, m.MsgID)
}

// deliver 执行一次投递，失败则按退避重新入队。
func deliver(ch chan *outboundMsg, m *outboundMsg) {
	err := sendOnce(m)
	if err == nil {
		return
	}

	m.Attempts++
	if m.Attempts >= sendMaxAttempts {
		AddLog("[Send] ❌ 放弃投递（已重试 %d 次）: %v", m.Attempts, err)
		return
	}

	backoff := sendBaseBackoff * time.Duration(1<<(m.Attempts-1))
	if backoff > sendMaxBackoff {
		backoff = sendMaxBackoff
	}
	// 加抖动，避免同一时刻批量重试再次撞上限流
	backoff += time.Duration(rand.Int63n(int64(sendBaseBackoff)))
	m.notBefore = time.Now().Add(backoff)

	select {
	case ch <- m:
		AddLog("[Send] 投递失败，%v 后重试（第 %d 次）: %v", backoff.Round(time.Millisecond), m.Attempts, err)
	default:
		atomic.AddInt64(&sendDropped, 1)
		AddLog("[Send] ❌ 重试队列已满，丢弃消息: %v", err)
	}
}

// enqueue 把消息放入发送队列。队列满时丢弃并返回 false。
func enqueue(m *outboundMsg) bool {
	if sendQueueCh == nil {
		return false
	}
	select {
	case sendQueueCh <- m:
		return true
	default:
		atomic.AddInt64(&sendDropped, 1)
		AddLog("[Send] ⚠️ 发送队列已满（%d 条），丢弃一条消息", sendQueueSize)
		return false
	}
}

// SendText 异步发送一条文本消息（入队，失败自动重试）。
func SendText(target, group, content, msgID string) {
	enqueue(&outboundMsg{Target: target, Group: group, Content: content, MsgID: msgID})
}

// SendTextSegmented 把超长回复按语义边界切分后依次入队。
//
// QQ 单条消息有长度上限，超出会被服务端直接拒绝；此前未分段也未检查状态码，
// 结果是长回复「静默消失」。这里复用 text.go 里已经写好的 SplitMessage。
func SendTextSegmented(target, group, content, msgID string) {
	parts := SplitMessage(content, maxMessageRunes)
	for i, part := range parts {
		// 只有第一条携带 msg_id，后续为普通追加消息，避免重复引用同一条
		id := msgID
		if i > 0 {
			id = ""
		}
		enqueue(&outboundMsg{Target: target, Group: group, Content: part, MsgID: id})
	}
}

// SendSticker / SendStickerMedia 已迁到 sticker.go（两步式富媒体实现）。

// ─── 底层单次发送 ──────────────────────────────────────────────────────────────

// checkHTTPResponse 校验状态码，非 2xx 时把响应体片段带回错误信息。
//
// 之前两个发送函数都只 `defer resp.Body.Close()` 就返回 nil —— HTTP 403/429/500
// 全部被当成成功，消息根本没发出去却没有任何记录（审查报告 C-4）。
func checkHTTPResponse(statusCode int, body io.Reader, what string) error {
	if statusCode >= 200 && statusCode < 300 {
		// 读完并丢弃，让底层连接可以复用
		_, _ = io.Copy(io.Discard, io.LimitReader(body, 4*1024))
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 4*1024))
	return fmt.Errorf("%s 返回 HTTP %d: %s", what, statusCode, truncate(string(raw), 200))
}

// SendTextMessage 发送一条文本消息（同步、单次，不含重试）。
// 需要自动重试的场景请用 SendText / SendTextSegmented。
func SendTextMessage(targetOpenID string, groupOpenID string, content, msgID string) error {
	auth, err := authHeader()
	if err != nil {
		return err
	}
	cfg := config.Get()
	isGroup := groupOpenID != ""
	var url string
	replyKey := targetOpenID
	if isGroup {
		url = fmt.Sprintf("https://api.sgroup.qq.com/v2/groups/%s/messages", groupOpenID)
		replyKey = groupOpenID
	} else {
		url = fmt.Sprintf("https://api.sgroup.qq.com/v2/users/%s/messages", targetOpenID)
	}

	payload := map[string]interface{}{
		"content":  content,
		"msg_type": 0,
		"msg_seq":  nextMsgSeq(replyKey),
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Union-Appid", cfg.QQAppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkHTTPResponse(resp.StatusCode, resp.Body, "QQ 消息接口")
}
