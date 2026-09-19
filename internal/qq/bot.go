package qq

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/llm"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/stickers"

	"github.com/gorilla/websocket"
)

// In-memory ring buffer for recent logs to display in WebUI
var (
	logsLock   sync.RWMutex
	recentLogs []string
	maxLogs    = 150
)

func AddLog(format string, v ...interface{}) {
	msg := fmt.Sprintf("[%s] ", time.Now().Format("15:04:05")) + fmt.Sprintf(format, v...)
	log.Println(msg)

	logsLock.Lock()
	defer logsLock.Unlock()
	recentLogs = append(recentLogs, msg)
	if len(recentLogs) > maxLogs {
		recentLogs = recentLogs[len(recentLogs)-maxLogs:]
	}
}

func GetRecentLogs() []string {
	logsLock.RLock()
	defer logsLock.RUnlock()
	res := make([]string, len(recentLogs))
	copy(res, recentLogs)
	return res
}

// ─── Token Management ────────────────────────────────────────────────────────

var (
	tokenLock   sync.Mutex
	tokenVal    string
	tokenExpiry time.Time
	httpClient  = &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
)

func getAccessToken() (string, error) {
	tokenLock.Lock()
	defer tokenLock.Unlock()

	if tokenVal != "" && time.Now().Before(tokenExpiry) {
		return tokenVal, nil
	}

	cfg := config.Get()
	if cfg.QQAppID == "" || cfg.QQSecret == "" {
		return "", fmt.Errorf("QQ AppID or Secret is empty")
	}

	body, _ := json.Marshal(map[string]string{
		"appId":        cfg.QQAppID,
		"clientSecret": cfg.QQSecret,
	})

	resp, err := httpClient.Post("https://bots.qq.com/app/getAppAccessToken",
		"application/json", bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("empty access_token returned by QQ auth")
	}

	expSec, _ := strconv.Atoi(res.ExpiresIn)
	if expSec <= 0 {
		expSec = 7200
	}
	tokenVal = res.AccessToken
	tokenExpiry = time.Now().Add(time.Duration(expSec-60) * time.Second)
	AddLog("[Auth] Successfully renewed QQ Bot Access Token")
	return tokenVal, nil
}

func authHeader() (string, error) {
	tok, err := getAccessToken()
	if err != nil {
		return "", err
	}
	return "QQBot " + tok, nil
}

// ─── Context Memory ──────────────────────────────────────────────────────────

type MemoryItem struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

type UserSession struct {
	Messages []MemoryItem
	LastSeen time.Time
}

var (
	memLock  sync.Mutex
	sessions = make(map[string]*UserSession)
)

func getSessionHistory(key string) []MemoryItem {
	memLock.Lock()
	defer memLock.Unlock()

	sess, ok := sessions[key]
	if !ok {
		return nil
	}
	if time.Since(sess.LastSeen) > 24*time.Hour {
		delete(sessions, key)
		return nil
	}
	res := make([]MemoryItem, len(sess.Messages))
	copy(res, sess.Messages)
	return res
}

func recordSession(key, userMsg, botReply string, isOwner bool) {
	memLock.Lock()
	defer memLock.Unlock()

	sess, ok := sessions[key]
	if !ok {
		sess = &UserSession{}
		sessions[key] = sess
	}
	now := time.Now()
	sess.LastSeen = now
	sess.Messages = append(sess.Messages,
		MemoryItem{Role: "user", Content: userMsg, Timestamp: now},
		MemoryItem{Role: "assistant", Content: botReply, Timestamp: now},
	)
	// Normal users sliding window 40 msgs (20 rounds)
	if !isOwner && len(sess.Messages) > 40 {
		sess.Messages = sess.Messages[len(sess.Messages)-40:]
	}
}

func clearSession(key string) {
	memLock.Lock()
	defer memLock.Unlock()
	delete(sessions, key)
}

// ─── Message Sending ─────────────────────────────────────────────────────────

var seqCounters sync.Map

func nextMsgSeq(target string) int {
	v, _ := seqCounters.LoadOrStore(target, 0)
	n := v.(int) + 1
	seqCounters.Store(target, n)
	return n
}

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
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Union-Appid", cfg.QQAppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func SendStickerMedia(targetOpenID string, groupOpenID string, base64Data string) error {
	auth, err := authHeader()
	if err != nil {
		return err
	}
	cfg := config.Get()
	var url string
	if groupOpenID != "" {
		url = fmt.Sprintf("https://api.sgroup.qq.com/v2/groups/%s/files", groupOpenID)
	} else {
		url = fmt.Sprintf("https://api.sgroup.qq.com/v2/users/%s/files", targetOpenID)
	}

	payload := map[string]interface{}{
		"file_type":    1,
		"srv_send_msg": true,
		"file_data":    base64Data,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Union-Appid", cfg.QQAppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ─── Incoming Message Processor ──────────────────────────────────────────────

func HandleIncomingMessage(senderOpenID, groupOpenID, text, msgID string, attachments []Attachment) {
	cfg := config.Get()
	isGroup := groupOpenID != ""
	cleanText := strings.TrimSpace(text)

	// Collect images
	var imageURLs []string
	for _, a := range attachments {
		if a.URL != "" {
			u := a.URL
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				u = "https://" + u
			}
			imageURLs = append(imageURLs, u)
		}
	}

	if cleanText == "" && len(imageURLs) == 0 {
		cleanText = "[向你发送了一个表情互动]"
	}

	sessionKey := senderOpenID
	if isGroup {
		sessionKey = groupOpenID + "_" + senderOpenID
	}

	isOwner := cfg.IsOwner(senderOpenID)
	lowerText := strings.ToLower(cleanText)
	passcode := strings.ToLower(strings.TrimSpace(cfg.Passcode))

	var replyContent string

	// 1. Passcode Check
	if passcode != "" && strings.ReplaceAll(lowerText, " ", "") == strings.ReplaceAll(passcode, " ", "") {
		_ = config.AddOwner(senderOpenID)
		replyContent = "🎉 呜哇！主人！是真正的主人！💙\n\n已成功将您认证为【最高权限主人】✨\n已为您解除所有限制，开启长期上下文记忆与多模态视觉能力！硬件状态、系统控制全数解锁！"
	} else if !isOwner {
		// 2. Normal User
		if lowerText == "状态" || lowerText == "/status" || lowerText == "重启" {
			replyContent = "🚫【权限受限】普通访客不能操作随身硬件设备哦！不过我们可以正常闲聊与多模态识图~"
		} else {
			replyContent = executeLLMChat(sessionKey, cleanText, imageURLs, false)
		}
	} else {
		// 3. Owner Command Dispatch
		switch {
		case lowerText == "状态" || lowerText == "/status" || lowerText == "info":
			driver := hal.GetDriver()
			info := driver.GetInfo()
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("💻 设备型号：%s\n", info.DeviceType))
			sb.WriteString(fmt.Sprintf("⏱️ 运行时间：%s\n", info.Uptime))
			sb.WriteString(fmt.Sprintf("📶 网络模式：%s (%s)\n", info.NetworkType, info.SignalRSRP))
			if info.TrafficToday != "" {
				sb.WriteString(fmt.Sprintf("📊 当天流量：%s\n", info.TrafficToday))
			}
			if len(info.Temperatures) > 0 {
				sb.WriteString(fmt.Sprintf("🌡️ 核心温度：%s\n", strings.Join(info.Temperatures, " | ")))
			}
			if info.MemoryTotalMB > 0 {
				sb.WriteString(fmt.Sprintf("🧠 内存占用：%d MB / %d MB\n", info.MemoryUsedMB, info.MemoryTotalMB))
			}
			replyContent = sb.String()

		case lowerText == "重启" || lowerText == "/reboot":
			replyContent = "⚠️ 正在执行设备远程重启，预计 1 分钟后恢复在线。"
			go func() {
				time.Sleep(3 * time.Second)
				driver := hal.GetDriver()
				_, _ = driver.ExecuteRootCmd("reboot")
			}()

		case lowerText == "清除记忆" || lowerText == "/clear":
			clearSession(sessionKey)
			replyContent = "🧹 已成功清空我们之间的多轮上下文记忆啦！"

		case strings.HasPrefix(cleanText, "人设 ") || strings.HasPrefix(cleanText, "/persona "):
			arg := strings.TrimSpace(cleanText[strings.Index(cleanText, " "):])
			found := false
			for _, p := range persona.GetAllPresets() {
				if strings.Contains(strings.ToLower(p.Name), strings.ToLower(arg)) || strings.Contains(p.ID, arg) {
					_ = persona.SetPersona(p.ID, "")
					clearSession(sessionKey)
					replyContent = fmt.Sprintf("✨ 灵魂蜕变成功！已实时切换为【%s】（%s）！记忆已重置。", p.Name, p.Title)
					found = true
					break
				}
			}
			if !found {
				replyContent = "❓ 未找到该预设呢。可选预设：【deepseek_chan】、【elysia】、【neko】、【jarvis】"
			}

		case strings.HasPrefix(cleanText, "设定人设 "):
			customPrompt := strings.TrimSpace(cleanText[len("设定人设 "):])
			if len(customPrompt) < 5 {
				replyContent = "⚠️ 人设内容太短啦，请多写几句性格、称呼和说话习惯吧~"
			} else {
				_ = persona.SetPersona("custom", customPrompt)
				clearSession(sessionKey)
				replyContent = "🎨 全新自定义人设已实时生效并保存！快来找我打招呼吧~"
			}

		case strings.HasPrefix(cleanText, "/exec ") || strings.HasPrefix(cleanText, "/shell "):
			cmdStr := strings.TrimSpace(cleanText[strings.Index(cleanText, " "):])
			driver := hal.GetDriver()
			out, err := driver.ExecuteRootCmd(cmdStr)
			if err != nil {
				replyContent = fmt.Sprintf("❌ 执行失败: %v\n%s", err, out)
			} else {
				if len(out) > 800 {
					out = out[:800] + "...(截断)"
				}
				replyContent = fmt.Sprintf("⚙️ 执行结果:\n%s", out)
			}

		default:
			replyContent = executeLLMChat(sessionKey, cleanText, imageURLs, true)
		}
	}

	// Clean any internal markup
	reClean := regexp.MustCompile(`\[EMOJI:[a-zA-Z0-9_]+\]`)
	replyContent = strings.TrimSpace(reClean.ReplaceAllString(replyContent, ""))

	// Send text reply
	_ = SendTextMessage(senderOpenID, groupOpenID, replyContent, msgID)

	// Context Scene Sticker Dispatch
	if cfg.EnableStickers {
		scene := stickers.DetectScene(cleanText)
		if scene != "" {
			stickerBase64 := stickers.GetRandomSceneSticker(scene)
			if stickerBase64 != "" {
				go func() {
					time.Sleep(500 * time.Millisecond)
					_ = SendStickerMedia(senderOpenID, groupOpenID, stickerBase64)
				}()
			}
		}
	}
}

func executeLLMChat(sessionKey, userText string, imageURLs []string, isOwner bool) string {
	_, activePrompt := persona.GetActivePersona()

	var sysPrompt string
	if isOwner {
		sysPrompt = activePrompt + "\n\n【权限状态：当前对话者为最高权限主人】\n你对主人完全开放、亲切依赖、无拘无束。"
	} else {
		sysPrompt = activePrompt + "\n\n【权限状态：当前对话者为普通访客】\n正常聊天，严禁透露底层硬件控制或执行管理指令。"
	}

	var messages []llm.Message
	messages = append(messages, llm.Message{Role: "system", Content: sysPrompt})

	history := getSessionHistory(sessionKey)
	for _, h := range history {
		messages = append(messages, llm.Message{Role: h.Role, Content: h.Content})
	}

	if len(imageURLs) > 0 {
		var parts []llm.MessageContentPart
		if userText != "" {
			parts = append(parts, llm.MessageContentPart{Type: "text", Text: userText})
		} else {
			parts = append(parts, llm.MessageContentPart{Type: "text", Text: "看看这张图片，分析一下这是什么~"})
		}
		for _, u := range imageURLs {
			parts = append(parts, llm.MessageContentPart{
				Type:     "image_url",
				ImageURL: &llm.ImageURL{URL: u},
			})
		}
		messages = append(messages, llm.Message{Role: "user", Content: parts})
	} else {
		messages = append(messages, llm.Message{Role: "user", Content: userText})
	}

	reply, err := llm.CallLLM(messages)
	if err != nil {
		AddLog("[LLM Error] %v", err)
		return fmt.Sprintf("AI 思考超时或异常: %v", err)
	}

	storedMsg := userText
	if len(imageURLs) > 0 {
		storedMsg = fmt.Sprintf("%s [附带图片]", userText)
	}
	recordSession(sessionKey, storedMsg, reply, isOwner)
	return reply
}

// ─── WebSocket Engine ────────────────────────────────────────────────────────

var (
	wsDialer = websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	wsConnected bool
	wsConnLock  sync.RWMutex
)

func IsWSConnected() bool {
	wsConnLock.RLock()
	defer wsConnLock.RUnlock()
	return wsConnected
}

func StartBotGateway() {
	go func() {
		for {
			cfg := config.Get()
			if cfg.QQAppID == "" || cfg.QQSecret == "" {
				AddLog("[Bot] Waiting for QQ AppID and Secret to be configured in WebUI...")
				time.Sleep(5 * time.Second)
				continue
			}

			if err := runGatewaySession(); err != nil {
				AddLog("[Bot Gateway] Connection lost: %v. Retrying in 10s...", err)
			}
			time.Sleep(10 * time.Second)
		}
	}()
}

func runGatewaySession() error {
	auth, err := authHeader()
	if err != nil {
		return fmt.Errorf("auth token error: %w", err)
	}

	cfg := config.Get()
	gwReq, _ := http.NewRequest("GET", "https://api.sgroup.qq.com/gateway/bot", nil)
	gwReq.Header.Set("Authorization", auth)
	gwReq.Header.Set("X-Union-Appid", cfg.QQAppID)

	gwResp, err := httpClient.Do(gwReq)
	if err != nil {
		return fmt.Errorf("gateway request error: %w", err)
	}
	defer gwResp.Body.Close()

	var gwData struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(gwResp.Body).Decode(&gwData)
	if gwData.URL == "" {
		gwData.URL = "wss://api.sgroup.qq.com/websocket"
	}

	AddLog("[Bot Gateway] Connecting to %s", gwData.URL)
	conn, _, err := wsDialer.Dial(gwData.URL, nil)
	if err != nil {
		return fmt.Errorf("dial error: %w", err)
	}
	defer conn.Close()

	wsConnLock.Lock()
	wsConnected = true
	wsConnLock.Unlock()

	defer func() {
		wsConnLock.Lock()
		wsConnected = false
		wsConnLock.Unlock()
	}()

	var heartbeatInterval time.Duration
	var lastSeq int64
	done := make(chan struct{})
	defer close(done)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var payload WSPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}

		if payload.S != nil {
			lastSeq = *payload.S
		}

		switch payload.Op {
		case 10: // Hello
			var hello WSHelloData
			dBytes, _ := json.Marshal(payload.D)
			_ = json.Unmarshal(dBytes, &hello)
			heartbeatInterval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
			if heartbeatInterval <= 0 {
				heartbeatInterval = 30 * time.Second
			}

			// Start Heartbeat loop
			go func() {
				ticker := time.NewTicker(heartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ticker.C:
						_ = conn.WriteJSON(WSPayload{Op: 1, D: lastSeq})
					}
				}
			}()

			// Send Identify
			tok, _ := getAccessToken()
			identify := WSPayload{
				Op: 2,
				D: WSIdentifyData{
					Token:   "QQBot " + tok,
					Intents: (1 << 25), // GROUP_AND_C2C_EVENT
					Shard:   []int{0, 1},
					Properties: struct {
						OS      string `json:"$os"`
						Browser string `json:"$browser"`
						Device  string `json:"$device"`
					}{
						OS:      "cybercompanion",
						Browser: "cybercompanion_engine",
						Device:  "edge_companion",
					},
				},
			}
			_ = conn.WriteJSON(identify)
			AddLog("[Bot Gateway] Sent Identify handshake")

		case 0: // Dispatch
			switch payload.T {
			case "READY":
				AddLog("[Bot Gateway] Bot online and READY!")
			case "C2C_MESSAGE_CREATE":
				var msg InMessage
				dBytes, _ := json.Marshal(payload.D)
				if err := json.Unmarshal(dBytes, &msg); err == nil {
					AddLog("[Message] C2C from %s: %s", msg.Author.UserOpenID, msg.Content)
					go HandleIncomingMessage(msg.Author.UserOpenID, "", msg.Content, msg.ID, msg.Attachments)
				}
			case "GROUP_AT_MESSAGE_CREATE":
				var msg InMessage
				dBytes, _ := json.Marshal(payload.D)
				if err := json.Unmarshal(dBytes, &msg); err == nil {
					content := strings.TrimSpace(msg.Content)
					if idx := strings.Index(content, ">"); idx != -1 && strings.HasPrefix(content, "<@") {
						content = strings.TrimSpace(content[idx+1:])
					}
					AddLog("[Message] Group@ in %s: %s", msg.GroupOpenID, content)
					go HandleIncomingMessage(msg.Author.MemberOpenID, msg.GroupOpenID, content, msg.ID, msg.Attachments)
				}
			}
		}
	}
}
