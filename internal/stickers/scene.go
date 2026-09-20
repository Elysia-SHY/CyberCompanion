package stickers

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ─── 场景与关键词 ─────────────────────────────────────────────────────────────
//
// DetectScene 是「关键词自动发表情」的判定入口，行为与旧实现保持一致：
// 按固定顺序逐个场景检查，命中即返回，因此同一句话只会触发一个场景。

// builtinScene 是一个内置场景：名字、中文标签、触发关键词。
type builtinScene struct {
	Scene    string
	Label    string
	Keywords []string
}

// builtinSceneList 的顺序即 DetectScene 的判定优先级，不要随意调整。
var builtinSceneList = []builtinScene{
	{
		Scene: "love",
		Label: "亲昵",
		Keywords: []string{
			"喜欢你", "爱你", "做我女朋友", "老婆", "抱抱", "亲亲", "么么哒", "永远在一起", "最喜欢", "贴贴",
		},
	},
	{
		Scene: "greeting",
		Label: "问候",
		Keywords: []string{
			"你好", "早上好", "早安", "晚上好", "晚安", "在吗", "嗨", "hello", "hi", "ciallo",
		},
	},
	{
		Scene: "hungry",
		Label: "吃饭",
		Keywords: []string{
			"饿了", "吃白饭", "吃大餐", "吃什么", "干饭", "吃饭", "点外卖", "好饿", "投喂",
		},
	},
	{
		Scene: "tsundere",
		Label: "傲娇",
		Keywords: []string{
			"笨蛋", "大肥鱼", "傻瓜", "过载", "死鱼眼", "晚期痴呆", "傲娇",
		},
	},
	{
		Scene: "panic",
		Label: "慌乱",
		Keywords: []string{
			"救命", "怎么办", "完蛋了", "慌了", "宕机", "哭了", "呜呜",
		},
	},
}

// builtinScenes 供 Scenes() 快速判断「哪些场景是内置的」。
var builtinScenes = func() map[string]builtinScene {
	m := make(map[string]builtinScene, len(builtinSceneList))
	for _, s := range builtinSceneList {
		m[s.Scene] = s
	}
	return m
}()

func sceneLabel(scene string) string {
	if s, ok := builtinScenes[scene]; ok && s.Label != "" {
		return s.Label
	}
	return scene
}

// KeywordsOf 返回某个场景当前生效的全部关键词（内置 + 用户追加）。
func KeywordsOf(scene string) []string {
	seen := make(map[string]bool, 16)
	out := make([]string, 0, 16)
	if s, ok := builtinScenes[scene]; ok {
		for _, k := range s.Keywords {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}

	mu.RLock()
	extra := append([]string(nil), settings.ExtraKeywords[scene]...)
	// 用户可以为「非内置场景」定义关键词，这类场景也要列出来
	if len(extra) > 0 {
		found := false
		for _, s := range builtinSceneList {
			if s.Scene == scene {
				found = true
				break
			}
		}
		if !found {
			out = out[:0]
		}
	}
	mu.RUnlock()

	for _, k := range extra {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// DetectScene 根据用户输入判断应当触发哪个场景，无命中返回空串。
func DetectScene(text string) string {
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)

	// 内置场景按固定优先级判定
	for _, s := range builtinSceneList {
		if containsAny(lower, s.Keywords...) {
			return s.Scene
		}
	}

	// 用户自定义场景：名称排序保证结果稳定
	mu.RLock()
	custom := make([]string, 0, len(settings.ExtraKeywords))
	for scene, kws := range settings.ExtraKeywords {
		if _, builtin := builtinScenes[scene]; builtin {
			continue
		}
		if containsAny(lower, kws...) {
			custom = append(custom, scene)
		}
	}
	mu.RUnlock()
	if len(custom) == 0 {
		return ""
	}
	sort.Strings(custom)
	return custom[0]
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ─── 选取与装载 ───────────────────────────────────────────────────────────────

// Pick 按标记里的键选取一条表情。
//
// 解析顺序：
//  1. random            → 全部启用条目里随机
//  2. 场景名            → 该场景下随机一条（场景优先，符合直觉）
//  3. 表情 ID           → 精确命中
//  4. 场景关键词        → 走 DetectScene 再取
func Pick(key string) (*Sticker, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	mu.RLock()
	defer mu.RUnlock()

	if key == "" {
		return nil, false
	}

	if key == RandomKey {
		if it := randomFromLocked(items); it != nil {
			cp := *it
			return &cp, true
		}
		return nil, false
	}

	if list, ok := byScene[key]; ok {
		if it := randomFromLocked(list); it != nil {
			cp := *it
			return &cp, true
		}
	}

	if it, ok := byID[key]; ok && it.Enabled {
		cp := *it
		return &cp, true
	}

	// 词表兜底：允许 [表情:早上好] 这种写法直接命中 greeting
	if scene := detectSceneLocked(key); scene != "" {
		if it := randomFromLocked(byScene[scene]); it != nil {
			cp := *it
			return &cp, true
		}
	}
	return nil, false
}

// detectSceneLocked 是 DetectScene 的无锁版本，调用方须持有至少读锁。
// 避免 Pick 内部再次加锁造成死锁。
func detectSceneLocked(lower string) string {
	for _, s := range builtinSceneList {
		if containsAny(lower, s.Keywords...) {
			return s.Scene
		}
	}
	for scene, kws := range settings.ExtraKeywords {
		if _, builtin := builtinScenes[scene]; builtin {
			continue
		}
		if containsAny(lower, kws...) {
			return scene
		}
	}
	return ""
}

// randomFromLocked 在启用条目里随机挑一条。调用方须持有锁。
func randomFromLocked(list []*Sticker) *Sticker {
	if len(list) == 0 {
		return nil
	}
	// 先收集启用项，避免在锁内做无谓的多次尝试
	enabled := make([]*Sticker, 0, len(list))
	for _, it := range list {
		if it.Enabled {
			enabled = append(enabled, it)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	return enabled[randomInt(len(enabled))]
}

// ─── 发送载荷 ─────────────────────────────────────────────────────────────────

// SendPayload 是一次表情发送所需的全部素材。
//
// URL 与 Data 都可能为空，具体用哪个由 qq 层按设置与降级顺序决定：
// 先试图床直链，直链失败再退回本地字节。
type SendPayload struct {
	ID       string
	Scene    string
	URL      string
	Data     []byte
	MIME     string
	Filename string
}

// Describe 返回一句可读描述，用于日志。
func (p *SendPayload) Describe() string {
	if p == nil {
		return "<nil>"
	}
	via := "local"
	if p.URL != "" {
		via = "url"
	}
	return fmt.Sprintf("%s(%s, %s, %d bytes)", p.ID, p.Scene, via, len(p.Data))
}

// Sendable 把标记里的键解析成可直接交给发送层的载荷。
//
// key 支持三种形式：
//   - http(s) 直链：直接以该图床地址发送，无需先入库
//   - 场景名 / 表情 ID / 场景关键词：查库
//   - random：随机
func Sendable(key string) (*SendPayload, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: 空的表情标记", ErrInvalid)
	}

	if isHTTPURL(key) {
		return &SendPayload{ID: "inline", URL: key, Filename: filenameFromURL(key)}, nil
	}

	s, ok := Pick(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}

	mu.RLock()
	mode := settings.Mode
	prefix := settings.CDNPrefix
	mu.RUnlock()

	payload := &SendPayload{
		ID:    s.ID,
		Scene: s.Scene,
		URL:   s.PublicURL(prefix),
	}
	if s.Source == SourceURL && s.URL != "" {
		payload.Filename = filenameFromURL(s.URL)
	}
	if payload.Filename == "" && s.File != "" {
		payload.Filename = filepath.Base(s.File)
	}

	// 只要本地有副本就顺手带上：它是直链失败后的唯一退路，
	// 多花一次读盘远好过图床抖一下表情就发不出去。
	if s.File != "" {
		if data, mime, err := readMediaBytes(s); err == nil {
			payload.Data = data
			payload.MIME = mime
		} else if mode == ModeLocal && payload.URL == "" {
			return nil, err
		}
	}
	if mode == ModeURL && payload.URL == "" {
		return nil, fmt.Errorf("%w: 表情 %s 没有图床直链（可在面板配置 CDN 前缀，或把该条改为直链来源）", ErrInvalid, s.ID)
	}
	if payload.URL == "" && len(payload.Data) == 0 {
		return nil, fmt.Errorf("%w: 表情 %s 既无直链也读不到本地文件", ErrInvalid, s.ID)
	}
	return payload, nil
}

// readMediaBytes 读取一条表情的本机副本字节。
// 副本文件不在时回退到内嵌副本（对应 defaults/ 下的内置表情）。
func readMediaBytes(s *Sticker) ([]byte, string, error) {
	if s == nil || s.File == "" {
		return nil, "", fmt.Errorf("%w: 该表情没有本地文件", ErrInvalid)
	}
	if p := MediaPath(s.File); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			if len(data) > MaxMediaBytes {
				return nil, "", fmt.Errorf("%w: 表情文件超过 %d 字节", ErrInvalid, MaxMediaBytes)
			}
			return data, sniffImageMIME(data), nil
		}
	}
	if strings.HasPrefix(s.File, defaultsPrefix) {
		if data, err := ReadEmbeddedDefault(s.File); err == nil {
			return data, sniffImageMIME(data), nil
		}
	}
	return nil, "", fmt.Errorf("%w: 读取本地表情失败: %s", ErrNotFound, s.File)
}

// sniffImageMIME 按魔数判断图片类型。QQ 侧只认图片，这里不做解码，
// 只要能把 base64 与 MIME 一起交出去就够了。
func sniffImageMIME(b []byte) string {
	switch {
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) > 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) > 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	}
	return "application/octet-stream"
}

// ExtForMIME 给出与 MIME 对应的文件扩展名。
func ExtForMIME(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".bin"
}

func filenameFromURL(u string) string {
	base := u
	if i := strings.LastIndexAny(base, "/?"); i >= 0 {
		base = base[:i]
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		return "sticker"
	}
	return base
}
