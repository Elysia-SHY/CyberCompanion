package qq

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/stickers"
)

// ─── QQ 富媒体（图片表情）发送 ─────────────────────────────────────────────────
//
// 官方对富媒体消息的推荐路径是「两步走」，这里照做：
//
//	第一步  POST /v2/{groups|users}/{id}/files   srv_send_msg=false，只上传
//	        → 返回 file_info（base64 串）+ ttl（秒）
//	第二步  POST /v2/{groups|users}/{id}/messages msg_type=7 + media.file_info
//
// 相比之下旧实现是一步到位（srv_send_msg=true），官方源码里明确写着它的两个缺点：
//   - 占用主动消息频率（被动回复配额白白浪费）
//   - 文件不能复用，同一张表情每发一次都要重传一遍
//
// 走两步之后，同一个 file_info 在 ttl 内可以重复发同一张图，
// 上限内的重复表情不再产生任何上传流量 —— 对随身 WiFi 的流量是实打实的节省。
//
// file_info 与「目标会话 + 同类型媒体」绑定，所以缓存键必须带上目标；
// 直接命中 QQ 的语义，不做跨会话复用。

const (
	// fileTypeImage 是富媒体业务类型里的图片
	fileTypeImage = 1
	// msgTypeRichMedia 是消息类型里的富媒体
	msgTypeRichMedia = 7

	// stickerCacheMaxEntries 缓存条目上限，防止被大量不同目标刷爆
	stickerCacheMaxEntries = 128
	// stickerTTLSafety 从服务端给的 ttl 里扣掉的安全余量。
	// 卡在过期边界上传出来的 file_info 一发就失败，不如提前一点重新上传。
	stickerTTLSafety = 30 * time.Second
	// stickerUploadTimeout 上传大图时的超时
	stickerUploadTimeout = 30 * time.Second
	// stickerDownloadLimit 从图床下载图片的大小上限
	stickerDownloadLimit = stickers.MaxMediaBytes
)

// ─── file_info 缓存 ───────────────────────────────────────────────────────────

type cachedFileInfo struct {
	fileInfo string
	expireAt time.Time
}

var stickerCache = struct {
	sync.Mutex
	m map[string]cachedFileInfo
}{m: make(map[string]cachedFileInfo)}

// stickerCacheKey 把「发到哪 + 发的是哪张图」拼成缓存键。
//
// 一定要带上内容指纹：用户改了表情库里的图之后，
// 旧 file_info 对应的还是老图，只按 ID 缓存会让改动迟迟不生效。
func stickerCacheKey(target, group string, p *stickers.SendPayload) string {
	scope := "c2c:" + target
	if group != "" {
		scope = "group:" + group
	}
	fingerprint := p.URL
	if fingerprint == "" {
		fingerprint = fmt.Sprintf("data:%d:%d", len(p.Data), crc32Of(p.Data))
	}
	return scope + "|" + p.ID + "|" + fingerprint
}

func crc32Of(b []byte) uint32 {
	// 只取头尾各 64 字节做指纹，避免为一张图遍历整个 body
	const n = 64
	var h uint32 = 2166136261
	mix := func(bs []byte) {
		for _, c := range bs {
			h ^= uint32(c)
			h *= 16777619
		}
	}
	if len(b) <= 2*n {
		mix(b)
	} else {
		mix(b[:n])
		mix(b[len(b)-n:])
	}
	return h
}

func lookupFileInfo(key string) (string, bool) {
	stickerCache.Lock()
	defer stickerCache.Unlock()
	ent, ok := stickerCache.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(ent.expireAt) {
		delete(stickerCache.m, key)
		return "", false
	}
	return ent.fileInfo, true
}

func storeFileInfo(key, fileInfo string, ttl time.Duration) {
	if fileInfo == "" {
		return
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	stickerCache.Lock()
	defer stickerCache.Unlock()
	if len(stickerCache.m) >= stickerCacheMaxEntries {
		evictStickerCacheLocked()
	}
	stickerCache.m[key] = cachedFileInfo{fileInfo: fileInfo, expireAt: time.Now().Add(ttl)}
}

// evictStickerCacheLocked 清掉已过期项；仍然满则丢掉最快到期的一批。
func evictStickerCacheLocked() {
	now := time.Now()
	for k, v := range stickerCache.m {
		if now.After(v.expireAt) {
			delete(stickerCache.m, k)
		}
	}
	if len(stickerCache.m) < stickerCacheMaxEntries {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	list := make([]kv, 0, len(stickerCache.m))
	for k, v := range stickerCache.m {
		list = append(list, kv{k, v.expireAt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.Before(list[j].t) })
	for i := 0; i < len(list)/2; i++ {
		delete(stickerCache.m, list[i].k)
	}
}

// StickerCacheStats 返回缓存条目数，供面板与排障展示。
func StickerCacheStats() int {
	stickerCache.Lock()
	defer stickerCache.Unlock()
	return len(stickerCache.m)
}

// ─── 上传 / 发送 ──────────────────────────────────────────────────────────────

type richMediaUpload struct {
	FileInfo string `json:"file_info"`
	FileUUID string `json:"file_uuid"`
	TTL      int    `json:"ttl"`
}

func richMediaFilesURL(targetOpenID, groupOpenID string) string {
	if groupOpenID != "" {
		return fmt.Sprintf("https://api.sgroup.qq.com/v2/groups/%s/files", groupOpenID)
	}
	return fmt.Sprintf("https://api.sgroup.qq.com/v2/users/%s/files", targetOpenID)
}

func richMediaMessagesURL(targetOpenID, groupOpenID string) string {
	if groupOpenID != "" {
		return fmt.Sprintf("https://api.sgroup.qq.com/v2/groups/%s/messages", groupOpenID)
	}
	return fmt.Sprintf("https://api.sgroup.qq.com/v2/users/%s/messages", targetOpenID)
}

// uploadRichMedia 执行第一步：把图片交给 QQ 存起来，拿到可复用的 file_info。
func uploadRichMedia(targetOpenID, groupOpenID string, payload map[string]interface{}) (*richMediaUpload, error) {
	auth, err := authHeader()
	if err != nil {
		return nil, err
	}
	cfg := config.Get()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, richMediaFilesURL(targetOpenID, groupOpenID), bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Union-Appid", cfg.QQAppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("QQ 媒体上传返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var up richMediaUpload
	if err := json.Unmarshal(raw, &up); err != nil {
		return nil, fmt.Errorf("解析 QQ 媒体上传响应失败: %v（原始: %s）", err, truncate(string(raw), 200))
	}
	if strings.TrimSpace(up.FileInfo) == "" {
		return nil, fmt.Errorf("QQ 媒体上传未返回 file_info（原始: %s）", truncate(string(raw), 200))
	}
	return &up, nil
}

// sendRichMediaMessage 执行第二步：用 file_info 把图片作为消息发出去。
func sendRichMediaMessage(targetOpenID, groupOpenID, fileInfo, msgID string) error {
	auth, err := authHeader()
	if err != nil {
		return err
	}
	cfg := config.Get()

	replyKey := targetOpenID
	if groupOpenID != "" {
		replyKey = groupOpenID
	}
	payload := map[string]interface{}{
		"msg_type": msgTypeRichMedia,
		"media":    map[string]interface{}{"file_info": fileInfo},
		"msg_seq":  nextMsgSeq(replyKey),
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, richMediaMessagesURL(targetOpenID, groupOpenID), bytes.NewBuffer(body))
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("QQ 富媒体消息返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// ─── 对外入口 ─────────────────────────────────────────────────────────────────

// SendSticker 异步发送一张表情（入队，失败自动重试）。
// msgID 是触发这条回复的被动消息 ID，为空则作为主动消息发出。
func SendSticker(target, group, msgID string, p *stickers.SendPayload) {
	if p == nil {
		return
	}
	enqueue(&outboundMsg{Target: target, Group: group, MsgID: msgID, Sticker: p})
}

// SendStickerMedia 发送一条图片表情（同步、单次，不含重试）。
//
// 按配置的 Mode 决定优先用图床直链还是本机副本，任何一步失败都会
// 依次退到下一个可用来源，最后才降级到旧的「一步到位」接口。
func SendStickerMedia(targetOpenID, groupOpenID, msgID string, p *stickers.SendPayload) error {
	if p == nil {
		return fmt.Errorf("表情载荷为空")
	}

	cacheKey := stickerCacheKey(targetOpenID, groupOpenID, p)

	// 命中缓存：省掉一次上传，直接发
	if fileInfo, ok := lookupFileInfo(cacheKey); ok {
		if err := sendRichMediaMessage(targetOpenID, groupOpenID, fileInfo, msgID); err == nil {
			return nil
		}
		// 缓存里的 file_info 可能已经被服务端清掉，删掉它继续走正常流程
		stickerCache.Lock()
		delete(stickerCache.m, cacheKey)
		stickerCache.Unlock()
	}

	mode := stickers.CurrentSettings().Mode
	var attempts []map[string]interface{}

	// 1) 图床直链：交给 QQ 自己去拉，我们这边零上行流量
	if mode != stickers.ModeLocal && p.URL != "" {
		attempts = append(attempts, map[string]interface{}{
			"file_type": fileTypeImage,
			"url":       p.URL,
		})
	}
	// 2) 本机副本：转 base64 上传
	if mode != stickers.ModeURL && len(p.Data) > 0 && len(p.Data) <= stickerDownloadLimit {
		attempts = append(attempts, map[string]interface{}{
			"file_type": fileTypeImage,
			"file_data": base64.StdEncoding.EncodeToString(p.Data),
		})
	}
	// 3) 还有直链但上面的分支都没用上（本机副本读不到），先自己下载再上传
	if len(attempts) == 0 && p.URL != "" && mode != stickers.ModeLocal {
		data, _, err := fetchStickerBytes(p.URL)
		if err != nil {
			return fmt.Errorf("获取表情图片失败: %w", err)
		}
		attempts = append(attempts, map[string]interface{}{
			"file_type": fileTypeImage,
			"file_data": base64.StdEncoding.EncodeToString(data),
		})
	}
	if len(attempts) == 0 {
		return fmt.Errorf("表情 %s 没有可用的图片来源", p.ID)
	}

	var lastErr error
	for _, base := range attempts {
		up, err := uploadRichMedia(targetOpenID, groupOpenID, withUploadFlag(base))
		if err != nil {
			lastErr = err
			continue
		}
		if err := sendRichMediaMessage(targetOpenID, groupOpenID, up.FileInfo, msgID); err != nil {
			lastErr = err
			continue
		}
		ttl := time.Duration(up.TTL) * time.Second
		if ttl > 0 {
			ttl -= stickerTTLSafety
		}
		storeFileInfo(cacheKey, up.FileInfo, ttl)
		return nil
	}

	// 4) 全部两步式路径都失败了，退回「一步到位」的老接口：
	//    它占用主动消息频率、也不能复用，但胜在只需要一次请求。
	if err := sendStickerOneShot(targetOpenID, groupOpenID, p); err == nil {
		AddLog("[Sticker] ⚠️ 两步式发送失败（%v），已用一步式兜底发出：%s", lastErr, p.Describe())
		return nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的图片来源")
	}
	return fmt.Errorf("发送表情失败: %w", lastErr)
}

// withUploadFlag 补上「只上传、不直接发送」标记。
//
// 必须放在这里统一设置：srv_send_msg=true 会同时发送并占用主动消息频率，
// 那正是我们要避开的旧路径。
func withUploadFlag(payload map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	out["srv_send_msg"] = false
	return out
}

// sendStickerOneShot 是旧实现：上传即发送，一次请求搞定。
// 只作为两步式失败时的兜底。
func sendStickerOneShot(targetOpenID, groupOpenID string, p *stickers.SendPayload) error {
	payload := map[string]interface{}{
		"file_type":    fileTypeImage,
		"srv_send_msg": true,
	}
	switch {
	case p.URL != "":
		payload["url"] = p.URL
	case len(p.Data) > 0:
		payload["file_data"] = base64.StdEncoding.EncodeToString(p.Data)
	default:
		return fmt.Errorf("表情 %s 没有可用的图片来源", p.ID)
	}
	// 一步式上传成功后服务端已直接把图发出去了，拿到响应即视为完成。
	if _, err := uploadRichMedia(targetOpenID, groupOpenID, payload); err != nil {
		return err
	}
	return nil
}

// fetchStickerBytes 从图床下载图片字节（走完整 TLS 校验的 safeImageClient）。
func fetchStickerBytes(u string) ([]byte, string, error) {
	resp, err := safeImageClient.Get(normalizeURL(u))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, stickerDownloadLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > stickerDownloadLimit {
		return nil, "", fmt.Errorf("图片超过 %d KB", stickerDownloadLimit>>10)
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexAny(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return data, strings.TrimSpace(ct), nil
}
