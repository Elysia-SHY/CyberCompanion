package qq

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cybercompanion/internal/agent"
	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/stickers"
	"cybercompanion/internal/store"

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
	// httpClient 使用显式 Transport：不再关闭 TLS 证书校验。
	// 原实现 InsecureSkipVerify: true 会让 HTTPS 完全失去中间人防护，
	// 等于把 access_token、消息内容、AppSecret 暴露给任意中间设备。
	httpClient = &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://bots.qq.com/app/getAppAccessToken", bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 响应体有界读取，避免异常服务端返回超大内容撑爆内存
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("QQ 鉴权接口返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("解析 QQ 鉴权响应失败: %w", err)
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("empty access_token returned by QQ auth")
	}

	expSec, _ := strconv.Atoi(res.ExpiresIn)
	if expSec <= 0 {
		expSec = 7200
	}
	// 提前 5 分钟续期，避免临界点使用已过期 token
	lead := 300
	if expSec <= lead {
		lead = expSec / 10
	}
	tokenVal = res.AccessToken
	tokenExpiry = time.Now().Add(time.Duration(expSec-lead) * time.Second)
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

// ─── Message Sending ─────────────────────────────────────────────────────────
// 发送相关实现见 sender.go，会话记忆见 session.go。

// ─── Incoming Message Processor ──────────────────────────────────────────────

// HandleIncomingMessage 处理一条入站消息。
//
// 保留原有签名以兼容既有调用方与测试；群聊中「是否 @ 了机器人」
// 的信息由内部入口 handleIncoming 携带。
func HandleIncomingMessage(senderOpenID, groupOpenID, text, msgID string, attachments []Attachment) {
	handleIncoming(senderOpenID, groupOpenID, text, msgID, attachments, false)
}

// handleIncoming 是消息处理的实现。
//
// 与旧版相比，这个函数瘦了很多：能力分发交给了 plugin，
// 上下文组装与能力调用交给了 agent，` 这里只保留「必须贴着 QQ 平台做」
// 的部分 —— 附件处理、口令认证、分段发送、频率控制、表情派发。
func handleIncoming(senderOpenID, groupOpenID, text, msgID string, attachments []Attachment, atBot bool) {
	cfg := config.Get()
	isGroup := groupOpenID != ""

	// 收集附件：图片本地下载后内联（带类型与大小校验），语音/视频给出明确说明。
	// 之前是把附件 URL 原样转给服务商：签名 URL 会过期、CDN 可能防盗链、
	// 且没有任何类型与体积校验，一个超大非图片文件会被直接塞进请求体。
	imageURLs, mediaNote := ProcessAttachments(attachments)
	if mediaNote != "" {
		text += mediaNote
	}

	cleanText := strings.TrimSpace(text)
	if cleanText == "" && len(imageURLs) == 0 {
		cleanText = "[向你发送了一个表情互动]"
	}

	// ── 记忆与能力的隔离边界 ──
	//
	// 私聊按人隔离；群聊按群隔离（IsolateMemory 开启时），
	// 这样 A 群的信息不会漏进 B 群，群里的内容也不会污染私聊记忆。
	sessionKey := senderOpenID
	scope := store.ScopePrivate
	ownerID := senderOpenID
	if isGroup {
		sessionKey = groupOpenID + "_" + senderOpenID
		if cfg.Groups.IsolateMemoryOr() {
			scope = store.ScopeGroup
			ownerID = groupOpenID
		}
		// 关闭隔离时仍按人隔离（只是不与私聊合并），
		// 「完全不隔离」意味着把所有人的记忆混在一起，那不是配置项该提供的选项
	}

	// ── 群聊策略 ──
	if isGroup {
		policy := decideGroupReply(groupOpenID, cleanText, atBot)
		if !policy.Reply {
			AddLog("[Group] 跳过回复（%s）: %s", policy.Reason, truncate(cleanText, 40))
			return
		}
	}

	target := replyTarget{Sender: senderOpenID, Group: groupOpenID, MsgID: msgID}

	// ── 身份 ──
	role := resolveRole(senderOpenID, cfg)
	registerUser(senderOpenID, "")
	ensureSession(sessionKey, scope, ownerID, groupOpenID)

	lowerText := strings.ToLower(cleanText)
	passcode := strings.ToLower(strings.TrimSpace(cfg.Passcode))

	var replyContent string

	// 1. 口令认证
	// 归一化后做常量时间比较，避免通过响应时间逐字符猜解口令。
	// 同时加入尝试限流：口令是唯一的主人权柄入口，不限速等于允许无限爆破。
	if passcode != "" {
		normInput := strings.ReplaceAll(lowerText, " ", "")
		normPass := strings.ReplaceAll(passcode, " ", "")
		if isPasscodeAttempt(normInput, normPass, role.AtLeast(store.RoleOwner)) {
			if ok, waitMin := authLimiter.allow(senderOpenID); !ok {
				AddLog("[Auth] 口令尝试过于频繁，已临时锁定 %s（%d 分钟）", maskOpenID(senderOpenID), waitMin)
				replyContent = fmt.Sprintf("🚫 口令尝试次数过多，请 %d 分钟后再试。", waitMin)
			} else if constantTimeStringEqual(normInput, normPass) {
				authLimiter.reset(senderOpenID)
				if err := grantOwner(senderOpenID); err != nil {
					AddLog("[Auth] %s 通过口令认证，但写入权限失败: %v", maskOpenID(senderOpenID), err)
					replyContent = "🎉 口令正确！但保存权限时出了点问题，请稍后再试一次。"
				} else {
					role = store.RoleOwner
					AddLog("[Auth] %s 通过口令认证成为主人", maskOpenID(senderOpenID))
					replyContent = "🎉 呜哇！主人！是真正的主人！💙\n\n已成功将您认证为【最高权限主人】✨\n已为您解除所有限制，开启长期上下文记忆与多模态视觉能力！硬件状态、系统控制全数解锁！"
				}
			} else {
				authLimiter.fail(senderOpenID)
			}
		}
	}

	ec := execContext(scope, ownerID, senderOpenID, role, cfg)

	switch {
	case replyContent != "":
		// 认证结果直接返回，不再走后续分支

	default:
		if qc, ok := matchQuickCommand(lowerText); ok {
			// 2. 快捷命令：确定、零延迟、零 token
			if role.AtLeast(qc.minRole) {
				out, _ := runPlugin(qc.plugin, qc.args, ec)
				replyContent = out
			} else {
				replyContent = denyMessage(qc)
			}
			break
		}

		switch {
		case role.AtLeast(store.RoleOwner) && (lowerText == "重启" || lowerText == "/reboot"):
			replyContent = "⚠️ 正在执行设备远程重启，预计 1 分钟后恢复在线。"
			go func() {
				time.Sleep(3 * time.Second)
				driver := hal.GetDriver()
				_, _ = driver.ExecuteRootCmd("reboot")
			}()

		case role.AtLeast(store.RoleOwner) && (strings.HasPrefix(cleanText, "人设 ") || strings.HasPrefix(cleanText, "/persona ")):
			replyContent = switchPersona(cleanText, sessionKey)

		case role.AtLeast(store.RoleOwner) && strings.HasPrefix(cleanText, "设定人设 "):
			replyContent = setCustomPersona(cleanText, sessionKey)

		case role.AtLeast(store.RoleOwner) && (strings.HasPrefix(cleanText, "/exec ") || strings.HasPrefix(cleanText, "/shell ")):
			cmdStr := strings.TrimSpace(cleanText[strings.Index(cleanText, " "):])
			out, _ := runPlugin("exec", map[string]string{"cmd": cmdStr}, ec)
			replyContent = out

		default:
			// 3. 正常对话：交给 agent 引擎编排（记忆 + 权限 + 能力 + 模型）
			sendThinking(senderOpenID, groupOpenID, msgID)
			var extra []string
			replyContent, extra = chatWithEngine(target, sessionKey, scope, ownerID, senderOpenID, role, cleanText, imageURLs)

			// 能力执行结果先发：它们通常是用户真正想要的数据，
			// 而模型那句「让我看看」只是铺垫。
			for _, e := range extra {
				replyContent, _ = stickers.ParseMarkers(replyContent)
				SendTextSegmented(senderOpenID, groupOpenID, e, "")
			}
		}
	}

	// Clean any internal markup
	// 把模型写的 [表情:xxx] 与 [能力:xxx] 标记摘出来。标记是给程序看的，
	// 用户不该看到它们，所以必须在发正文之前剥离。
	replyContent, modelStickerKeys := stickers.ParseMarkers(replyContent)
	replyContent = agent.StripCalls(replyContent)

	// 发送回复：按 QQ 单条长度上限切分后入队，失败自动重试。
	// 之前是一条 SendTextMessage 直接丢出去，超长会被服务端拒绝且无日志记录。
	if replyContent != "" {
		SendTextSegmented(senderOpenID, groupOpenID, replyContent, msgID)
	}

	// 归档到消息流水（记忆的原料），并触发异步记忆提炼
	if replyContent != "" {
		archiveMessage(sessionKey, scope, ownerID, "user", cleanText, 0)
		archiveMessage(sessionKey, scope, ownerID, "assistant", replyContent, 0)
		maybeExtractMemory(sessionKey, scope, ownerID)
	}

	dispatchStickers(senderOpenID, groupOpenID, msgID, cleanText, modelStickerKeys)
}

// grantOwner 把某人提升为主人，同时写入配置文件与数据库。
//
// 两边都写是有意的：配置文件是老版本与人手编辑的入口，数据库是新的权威来源。
// 只写一边都会让用户在某个界面里看到不一致的权限。
func grantOwner(openid string) error {
	if err := config.AddOwner(openid); err != nil {
		return err
	}
	if db := store.Get(); db != nil {
		if _, err := db.SetRole(openid, store.RoleOwner, "passcode"); err != nil {
			return err
		}
	}
	return nil
}

// denyMessage 生成权限不足的提示。
func denyMessage(qc quickCommand) string {
	if qc.denyMessage != "" {
		return qc.denyMessage
	}
	return fmt.Sprintf("🚫【权限受限】这个操作需要%s及以上权限哦～", qc.minRole.Label())
}

// switchPersona 按名字切换预设人格。
func switchPersona(cleanText, sessionKey string) string {
	arg := strings.TrimSpace(cleanText[strings.Index(cleanText, " "):])
	for _, p := range persona.GetAllPresets() {
		if strings.Contains(strings.ToLower(p.Name), strings.ToLower(arg)) || strings.Contains(p.ID, arg) {
			_ = persona.SetPersona(p.ID, "")
			clearSession(sessionKey)
			// 人格切换后应当清掉这个会话的长期记忆吗？答案是「不」——
			// 换的是说话方式，不是对用户的了解。仅清空对话上下文即可。
			return fmt.Sprintf("✨ 灵魂蜕变成功！已实时切换为【%s】（%s）！对话上下文已重置，但关于你的记忆我留着呢。", p.Name, p.Title)
		}
	}
	return "❓ 未找到该预设呢。可选预设：【deepseek_chan】、【elysia】、【neko】、【jarvis】"
}

// setCustomPersona 设定自定义人格。
func setCustomPersona(cleanText, sessionKey string) string {
	customPrompt := strings.TrimSpace(cleanText[len("设定人设 "):])
	if len([]rune(customPrompt)) < 5 {
		return "⚠️ 人设内容太短啦，请多写几句性格、称呼和说话习惯吧~"
	}
	_ = persona.SetPersona("custom", customPrompt)
	clearSession(sessionKey)
	return "🎨 全新自定义人设已实时生效并保存！快来找我打招呼吧~"
}

// dispatchStickers 决定这条回复要发哪些表情。
//
// 两条来源、两个开关，互不干扰：
//   - 模型自主（smart_send）：从回复里摘出的 [表情:xxx] 标记
//   - 关键词兜底（keyword_send）：命中场景词表就补一张
//
// 模型已经表达了意图时就不再叠加关键词兜底 —— 一条回复里莫名冒出两张表情
// 比一张都不发更让人困惑。
func dispatchStickers(sender, group, msgID, userText string, modelKeys []string) {
	// 总开关：config.json 的 enable_stickers=false 时整条路径都短路，
	// 比逐条改配置更省事，也兼容历史版本的单一开关。
	if !config.Get().EnableStickers {
		return
	}
	settings := stickers.CurrentSettings()
	if !settings.SmartSend && !settings.KeywordSend {
		return
	}

	limit := settings.MaxPerReply
	if limit <= 0 {
		limit = 1
	}

	sent := 0
	send := func(key string) {
		p, err := stickers.Sendable(key)
		if err != nil {
			AddLog("[Sticker] 跳过 %q: %v", key, err)
			return
		}
		AddLog("[Sticker] 发送 %s", p.Describe())
		SendSticker(sender, group, msgID, p)
		sent++
	}

	if settings.SmartSend {
		for _, key := range modelKeys {
			if sent >= limit {
				break
			}
			send(key)
		}
	}
	if sent > 0 || !settings.KeywordSend {
		return
	}

	scene := stickers.DetectScene(userText)
	if scene == "" {
		return
	}
	send(scene)
}

// sendThinking 在真正调用大模型之前给用户一个即时反馈。
//
// 阻塞式调用在长回复场景要等十几秒，期间用户完全不知道机器人是否还活着
// （优化建议书 1.4）。只在私聊发送：群聊里每条消息都跟一个气泡会显得吵。
func sendThinking(senderOpenID, groupOpenID, msgID string) {
	if groupOpenID != "" {
		return
	}
	SendText(senderOpenID, groupOpenID, "💭", msgID)
}

// replyTarget 描述一条回复要发到哪里。
// executeLLMChat 需要它才能在流式输出时边生成边发。
type replyTarget struct {
	Sender string // 私聊为 user openid
	Group  string // 群 openid，私聊时为空
	MsgID  string // 被动回复引用的消息 ID
}

// 流式分段追加的节流参数：
// 每 streamFlushInterval 最多补发一次，且累积增量不少于 streamFlushMinChars，
// 避免每个 token 都发一条消息把会话刷爆。
const (
	streamFlushInterval = 1500 * time.Millisecond
	streamFlushMinChars = 80
	streamMaxFlushChars = 400
)

// streamWithEngine 走引擎并边生成边发送。
//
// 采用「累积到阈值就补发一段」而不是逐 token 发送：QQ 官方 API 不支持编辑已发消息，
// 只能追加，过于频繁会被判刷屏。
//
// 这条路径上有三层过滤，顺序不能变：
//  1. 引擎内部的能力调用门控（agent.Gate）—— 扣住 [能力:xxx]，绝不让它出现在可见文本里
//  2. 这里的表情标记过滤器 —— 扣住可能被切成两半的 [表情:xxx]
//  3. 分段发送 —— 按 QQ 单条长度上限切分
//
// 前两层都是「有可能被切开」的协议标记，因此都必须做增量过滤，
// 不能等收完再一把梭地正则替换。
func streamWithEngine(target replyTarget, req agent.Request) (string, error) {
	var (
		mu       sync.Mutex
		visible  strings.Builder // 摘掉标记后、真正发给用户的正文
		sent     int             // visible 里已经发出去的字节数
		lastSent time.Time
		filter   = stickers.NewMarkerFilter()
	)

	// flush 从 visible 里切一段发出去。force 用于收尾补发尾段。
	flush := func(force bool) {
		pending := visible.Len() - sent
		if pending <= 0 {
			return
		}
		if !force {
			enough := pending >= streamMaxFlushChars ||
				(pending >= streamFlushMinChars && time.Since(lastSent) >= streamFlushInterval)
			if !enough {
				return
			}
		}
		part := visible.String()[sent:]
		sent = visible.Len()
		lastSent = time.Now()
		SendTextSegmented(target.Sender, target.Group, part, target.MsgID)
	}

	// 引擎的增量回调：先过表情标记过滤，再交给分段发送
	req.OnDelta = func(delta string) {
		mu.Lock()
		defer mu.Unlock()
		if out := filter.Feed(delta); out != "" {
			visible.WriteString(out)
		}
		flush(false)
	}

	resp, err := botEngine.Run(context.Background(), req)

	mu.Lock()
	defer mu.Unlock()

	// 引擎在「服务端忽略 stream 参数」等情况下可能一次都不回调，
	// 此时必须把整段正文补发出去，否则流式开关会让回复彻底消失。
	if visible.Len() == 0 && resp.Text != "" {
		if out := filter.Feed(resp.Text); out != "" {
			visible.WriteString(out)
		}
	}
	if rest := filter.Flush(); rest != "" {
		visible.WriteString(rest)
	}
	flush(true)

	keys := filter.Keys()

	// 能力执行结果补发在正文之后：先说话，再给数据，读起来才顺。
	for _, e := range resp.Extra {
		SendTextSegmented(target.Sender, target.Group, e, "")
	}

	// 表情要等正文发完再发：图还没生成完就抢着发表情，像是在打断自己说话。
	// 这里单独起协程，避免发送队列的限速把流式的收尾拖慢。
	if len(keys) > 0 {
		go dispatchStickers(target.Sender, target.Group, "", "", keys)
	}

	return visible.String(), err
}

// ─── WebSocket Engine ────────────────────────────────────────────────────────

var (
	// wsDialer 保持 TLS 证书校验开启。
	// 原实现 InsecureSkipVerify: true 使 WebSocket 长连接可被中间人劫持，
	// 攻击者可读取并篡改全部 QQ 消息内容与事件推送。
	wsDialer = websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	wsConnected bool
	wsConnLock  sync.RWMutex
)

func IsWSConnected() bool {
	wsConnLock.RLock()
	defer wsConnLock.RUnlock()
	return wsConnected
}

// StartBotGateway 启动 QQ 网关连接循环，直到 ctx 被取消。
//
// ctx 取消时循环退出并主动关闭 WebSocket，不再依赖进程退出时的隐式清理
// （优化建议书 2.4）。
func StartBotGateway(ctx context.Context) {
	go func() {
		// 重连退避：连续失败时逐步拉长间隔，避免服务端异常期间疯狂重连
		backoff := 10 * time.Second
		const maxBackoff = 5 * time.Minute

		for {
			select {
			case <-ctx.Done():
				AddLog("[Bot] 网关已停止（收到退出信号）")
				return
			default:
			}

			cfg := config.Get()
			if cfg.QQAppID == "" || cfg.QQSecret == "" {
				AddLog("[Bot] 等待在面板中填写 QQ AppID 与 Secret...")
				if !sleepCtx(ctx, 10*time.Second) {
					return
				}
				continue
			}

			if err := runGatewaySession(ctx); err != nil {
				AddLog("[Bot Gateway] 连接中断: %v，%v 后重连", err, backoff)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			// 正常返回（通常是服务端关闭连接）：退避重置
			backoff = 10 * time.Second
			AddLog("[Bot Gateway] 连接结束，准备重连")
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
		}
	}()
}

// sleepCtx 可被取消的 sleep，被取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runGatewaySession(ctx context.Context) error {
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
	conn, _, err := wsDialer.DialContext(ctx, gwData.URL, nil)
	if err != nil {
		return fmt.Errorf("dial error: %w", err)
	}

	// 退出时先发 Close 帧再关连接：让服务端立即释放 session，
	// 否则要等心跳超时才知道我们已经走了（优化建议书 2.4）。
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(2*time.Second),
			)
			_ = conn.Close()
		case <-sessionDone:
		}
	}()

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
					AddLog("[Message] C2C from %s: %s", maskOpenID(msg.Author.UserOpenID), truncate(msg.Content, 80))
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
					AddLog("[Message] Group@ in %s: %s", maskOpenID(msg.GroupOpenID), truncate(content, 80))
					// atBot=true：这个事件本身就意味着机器人被 @ 了。
					//
					// QQ 官方 Bot API 只推送 GROUP_AT_MESSAGE_CREATE，不推送
					// 未被 @ 的普通群消息，因此群聊里的每一次入站都是「点名」。
					// groups.reply_chance 只在适配器能提供全量群消息时才有意义
					// （例如自建的 OneBot 网关），这也是该配置项仍然保留的原因。
					go handleIncoming(msg.Author.MemberOpenID, msg.GroupOpenID, content, msg.ID, msg.Attachments, true)
				}
			case "GROUP_MESSAGE_CREATE":
				// 未被 @ 的普通群消息。官方 API 目前不推送这个事件，
				// 但部分第三方适配器会推送；处理它才能让 groups.reply_chance 生效。
				var msg InMessage
				dBytes, _ := json.Marshal(payload.D)
				if err := json.Unmarshal(dBytes, &msg); err == nil {
					go handleIncoming(msg.Author.MemberOpenID, msg.GroupOpenID,
						strings.TrimSpace(msg.Content), msg.ID, msg.Attachments, false)
				}
			}
		}
	}
}

// ─── 口令认证防护 ─────────────────────────────────────────────────────────────

const (
	authMaxAttempts = 5
	authWindow      = 10 * time.Minute
	authLockTime    = 30 * time.Minute
)

type authEntry struct {
	count       int
	firstAt     time.Time
	lockedUntil time.Time
}

type authLimiterStore struct {
	mu      sync.Mutex
	entries map[string]*authEntry
}

var authLimiter = &authLimiterStore{entries: make(map[string]*authEntry)}

func init() {
	// 定期回收长期不活跃的限流记录与消息序号计数器，避免 key 无限堆积
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			authLimiter.gc()
			gcSeqCounters()
		}
	}()
}

func (s *authLimiterStore) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-1 * time.Hour)
	for k, e := range s.entries {
		if e.lockedUntil.Before(cutoff) && e.firstAt.Before(cutoff) {
			delete(s.entries, k)
		}
	}
}

// allow 返回 (是否放行, 剩余锁定分钟数)
func (s *authLimiterStore) allow(key string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return true, 0
	}
	if time.Now().Before(e.lockedUntil) {
		return false, int(time.Until(e.lockedUntil).Minutes()) + 1
	}
	return true, 0
}

func (s *authLimiterStore) fail(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	e, ok := s.entries[key]
	if !ok {
		s.entries[key] = &authEntry{count: 1, firstAt: now}
		return
	}
	// 超出统计窗口则重新计数
	if now.Sub(e.firstAt) > authWindow {
		e.count = 1
		e.firstAt = now
		e.lockedUntil = time.Time{}
		return
	}
	e.count++
	if e.count >= authMaxAttempts {
		e.lockedUntil = now.Add(authLockTime)
		AddLog("[Auth] %s 口令连续错误 %d 次，锁定 %v", maskOpenID(key), e.count, authLockTime)
	}
}

func (s *authLimiterStore) reset(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// isPasscodeAttempt 判断一条消息是否「像是口令尝试」。
// 目的：只对疑似爆破的消息计数，正常闲聊不该被限流误伤。
// 已是主人的用户不再参与限流判定。
func isPasscodeAttempt(normInput, normPass string, isOwner bool) bool {
	if isOwner {
		// 主人重发口令应被接受（例如换了设备），但不消耗限流额度
		return true
	}
	// 纯闲聊通常较长或含明显语气词，不做限制
	if utf8.RuneCountInString(normInput) > 64 {
		return false
	}
	if normInput == "" {
		return false
	}
	return true
}

// constantTimeStringEqual 常量时间字符串比较，避免时序侧信道。
func constantTimeStringEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// maskOpenID 对日志中的 OpenID 脱敏，避免明文身份标识落盘。
func maskOpenID(id string) string {
	if len(id) <= 8 {
		return "***"
	}
	return id[:4] + "****" + id[len(id)-4:]
}

// formatHardwareReport 把硬件采集结果整理成 QQ 消息。
// 改动要点：原实现只输出 5 项，且 CPU 占用、磁盘、内核等信息完全没有暴露；
// 拿不到的项直接不显示，避免出现"当天流量：无上限"这类与硬件无关的假数据。
func formatHardwareReport(info hal.DeviceInfo) string {
	d := info.Details
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("💻 设备型号：%s\n", info.DeviceType))
	sb.WriteString(fmt.Sprintf("🖥️ 系统架构：%s / %s\n", info.OS, info.Arch))
	sb.WriteString(fmt.Sprintf("⏱️ 系统运行：%s\n", info.Uptime))

	// CPU
	if d.CPUModel != "" {
		sb.WriteString(fmt.Sprintf("⚙️ 处理器：%s\n", truncate(d.CPUModel, 60)))
	}
	cpuLine := "🔥 CPU 占用："
	if d.CPUCores > 0 {
		cpuLine += fmt.Sprintf("%d 核 / ", d.CPUCores)
	}
	cpuLine += fmt.Sprintf("%.1f%%\n", d.CPUUsage)
	sb.WriteString(cpuLine)
	if d.LoadAvg != "" {
		sb.WriteString(fmt.Sprintf("📈 平均负载：%s\n", d.LoadAvg))
	}

	// 内存
	if d.MemoryTotalMB > 0 {
		sb.WriteString(fmt.Sprintf("🧠 内存占用：%d MB / %d MB (%d%%)\n",
			d.MemoryUsedMB, d.MemoryTotalMB, d.MemoryPercent))
	}
	if d.SwapTotalMB > 0 {
		sb.WriteString(fmt.Sprintf("🔁 交换分区：%d MB / %d MB\n", d.SwapUsedMB, d.SwapTotalMB))
	}

	// 磁盘
	if d.DiskTotalGB > 0 {
		sb.WriteString(fmt.Sprintf("💾 存储空间：%.1f GB / %.1f GB (%s)\n",
			d.DiskUsedGB, d.DiskTotalGB, d.DiskMount))
	}

	// 温度：拿不到就跳过，不再显示编造的读数
	if len(info.Temperatures) > 0 {
		sb.WriteString(fmt.Sprintf("🌡️ 核心温度：%s\n", strings.Join(info.Temperatures, " | ")))
	}

	// 电源
	if d.BatteryLevel >= 0 {
		sb.WriteString(fmt.Sprintf("🔋 电池电量：%d%% (%s)\n", d.BatteryLevel, d.BatteryStatus))
	}

	// 网络
	sb.WriteString(fmt.Sprintf("📶 网络模式：%s (%s)\n", info.NetworkType, info.SignalRSRP))
	if d.TrafficTotal != "" {
		sb.WriteString(fmt.Sprintf("📊 累计流量：%s\n", d.TrafficTotal))
	}
	if len(d.NetworkIPs) > 0 {
		sb.WriteString(fmt.Sprintf("🌐 本机地址：%s\n", truncate(strings.Join(d.NetworkIPs, ", "), 80)))
	}

	if d.Kernel != "" {
		sb.WriteString(fmt.Sprintf("🧩 内核版本：%s\n", d.Kernel))
	}
	if d.ProcessCount > 0 {
		sb.WriteString(fmt.Sprintf("📦 系统进程：%d 个\n", d.ProcessCount))
	}

	return strings.TrimRight(sb.String(), "\n")
}
