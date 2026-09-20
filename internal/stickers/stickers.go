// Package stickers 管理机器人的语境表情包。
//
// ─── 为什么重写 ────────────────────────────────────────────────────────────────
//
// 旧实现把 5 张表情以 base64 巨行硬编码在本包源码里（stickers.go 共 801KB），
// 带来三个问题：
//  1. 二进制白白膨胀 800KB，且用户完全无法增删表情 —— 想换一张就得改源码重编译
//  2. 表情只能以「本地内嵌字节」形式发送，接不了图床
//  3. 表情与大模型完全无关，只能靠关键词命中；大模型想发也无从表达
//
// 新实现把表情拆成「数据 + 设置」两层，持久化到配置目录：
//
//	<配置目录>/stickers.json   表情条目与设置（本文件管理的真源）
//	<配置目录>/stickers/       本地图片（defaults/ 内置、uploads/ 用户上传）
//
// 表情条目支持两种来源：
//   - source=url   图床直链，发送时把 URL 交给 QQ 服务端自行拉取
//   - source=local 本地文件，发送时读盘转 base64
//
// 两者可以叠加：本地条目配上 CDN 前缀后会额外得到一个公网直链，
// 于是「把图片放进仓库 → 用 jsDelivr 当图床」这种零成本方案也能直接生效。
package stickers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/imagehost"
)

// ─── 常量 ─────────────────────────────────────────────────────────────────────

const (
	// SourceLocal 表示表情来自本机 stickers/ 目录下的文件
	SourceLocal = "local"
	// SourceURL 表示表情来自外部图床直链
	SourceURL = "url"

	// ModeAuto 优先用图床直链，没有直链时回退本地文件
	ModeAuto = "auto"
	// ModeLocal 只发本地文件，即便配置了 CDN 前缀也不走网络
	ModeLocal = "local"
	// ModeURL 只发图床直链，本地文件不参与
	ModeURL = "url"

	// RandomKey 是「随便来一张」的保留关键字
	RandomKey = "random"

	storeFileName = "stickers.json"
	mediaDirName  = "stickers"

	// storeVersion 用于日后结构升级时做迁移判断
	storeVersion = 1

	// 容量上限：防止面板被刷爆，也防止每张表情都进内存
	maxStickers = 500
	// MaxMediaBytes 单个本地表情文件的大小上限（QQ 对图片本身也有上限）
	MaxMediaBytes = 4 << 20
	// MaxNoteRunes 备注长度上限
	MaxNoteRunes = 120
	// maxIDLen 表情 ID 长度上限
	maxIDLen = 32

	// mediaURLTimeout 下载外部图床图片时的超时
	mediaURLTimeout = 20 * time.Second
)

// idPattern 限制表情 ID 的字符集：它会出现在 [表情:xxx] 标记里，
// 过于宽松会让标记解析变得含糊。
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,` + fmt.Sprint(maxIDLen) + `}$`)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

// Sticker 是一条表情条目。
//
// Source 决定「这条表情的来源是什么」，File 则是它可选的本机副本：
//   - source=local 必须带 File，发送时读盘转 base64
//   - source=url   以 URL 为准，File 可选；带副本时发送会先试直链、
//     直链失败再退回副本 —— 图床被墙或临时挂掉时不会直接哑掉
type Sticker struct {
	ID      string `json:"id"`
	Scene   string `json:"scene"`
	Source  string `json:"source"`
	URL     string `json:"url,omitempty"`
	File    string `json:"file,omitempty"`
	Note    string `json:"note,omitempty"`
	Enabled bool   `json:"enabled"`
}

// Settings 是表情包功能的运行设置。全部可通过 WebUI 修改。
type Settings struct {
	// Mode 决定发送时优先使用图床直链还是本地文件（auto / local / url）
	Mode string `json:"mode"`
	// CDNPrefix 是「把本地文件映射成公网直链」的前缀。
	// 例如 https://cdn.jsdelivr.net/gh/USER/REPO@main/internal/stickers/，
	// 配上之后本地条目会自动获得 <前缀><File> 形式的直链。
	CDNPrefix string `json:"cdn_prefix"`
	// SmartSend 允许大模型用 [表情:xxx] 标记自主发表情
	SmartSend bool `json:"smart_send"`
	// KeywordSend 命中关键词时自动补一张表情（旧行为，默认保留）
	KeywordSend bool `json:"keyword_send"`
	// MaxPerReply 单条回复最多发送的表情数量
	MaxPerReply int `json:"max_per_reply"`
	// ExtraKeywords 在内置关键词表之外追加的场景关键词
	ExtraKeywords map[string][]string `json:"extra_keywords,omitempty"`
	// ImageHost 是图床配置。填齐之后，面板上的「上传到图床」按钮会把
	// 本地表情推到图床并把条目改成直链来源 —— 这是「接图床」的落地开关。
	ImageHost imagehost.Config `json:"image_host"`
}

// storeData 是 stickers.json 的磁盘结构。
type storeData struct {
	Version  int        `json:"version"`
	Settings Settings   `json:"settings"`
	Items    []*Sticker `json:"items"`
}

// SceneInfo 描述一个场景，供面板展示与提示词注入。
type SceneInfo struct {
	Scene    string   `json:"scene"`
	Label    string   `json:"label"`
	Keywords []string `json:"keywords"`
	Count    int      `json:"count"`
}

// ─── 存储状态 ─────────────────────────────────────────────────────────────────

var (
	mu        sync.RWMutex
	rootDir   string
	mediaDir  string
	storeFile string

	settings Settings
	items    []*Sticker
	byID     map[string]*Sticker
	byScene  map[string][]*Sticker

	// loaded 标记 Load 是否成功执行过。未加载时 DetectScene 退化为内置关键词表。
	loaded bool

	// logf 由外部注入，避免本包反向依赖 qq 包造成循环导入
	logf = func(string, ...interface{}) {}
)

// SetLogger 注入日志回调。main 启动时把它接到面板日志上。
func SetLogger(fn func(format string, v ...interface{})) {
	if fn == nil {
		return
	}
	mu.Lock()
	logf = fn
	mu.Unlock()
}

func logMessage(format string, v ...interface{}) {
	mu.RLock()
	fn := logf
	mu.RUnlock()
	fn(format, v...)
}

// DefaultSettings 返回出厂设置。
func DefaultSettings() Settings {
	return Settings{
		Mode:        ModeAuto,
		CDNPrefix:   "",
		SmartSend:   true,
		KeywordSend: true,
		MaxPerReply: 1,
		ImageHost:   imagehost.Defaults(),
	}
}

// ─── 加载与持久化 ─────────────────────────────────────────────────────────────

// Load 初始化表情库：解压内置表情、读取 stickers.json、建立索引。
//
// dir 为配置目录（与 config.json 同级）。dir 为空时退化为纯内存模式，
// 使用内置库但不落盘 —— 供单元测试与 `-hardware` 这类只读路径使用。
func Load(dir string) error {
	mu.Lock()
	rootDir = dir
	if dir == "" {
		mediaDir = ""
		storeFile = ""
	} else {
		mediaDir = filepath.Join(dir, mediaDirName)
		storeFile = filepath.Join(dir, storeFileName)
	}
	settings = DefaultSettings()
	media, file := mediaDir, storeFile
	mu.Unlock()

	if media != "" {
		if err := os.MkdirAll(media, 0o755); err != nil {
			return fmt.Errorf("创建表情目录失败: %w", err)
		}
		if err := extractDefaults(media); err != nil {
			// 解压失败不该让机器人起不来：内置库仍可用（走 embed 直读）
			logMessage("[Stickers] ⚠️ 内置表情解压失败，将继续使用内嵌副本: %v", err)
		}
	}

	raw, err := readStoreFile()
	switch {
	case err == nil:
		// 正常读取
	case errors.Is(err, os.ErrNotExist):
		raw = &storeData{Version: storeVersion, Settings: DefaultSettings(), Items: defaultLibrary()}
		logMessage("[Stickers] 首次运行，已初始化 %d 张内置表情", len(raw.Items))
	default:
		// 文件存在但解析失败：先备份再重建，绝不静默丢用户数据
		if file != "" {
			backup := file + ".corrupt-" + time.Now().Format("20060102-150405")
			if renameErr := os.Rename(file, backup); renameErr == nil {
				logMessage("[Stickers] ⚠️ 表情库解析失败，原文件已备份为 %s: %v", filepath.Base(backup), err)
			}
		}
		raw = &storeData{Version: storeVersion, Settings: DefaultSettings(), Items: defaultLibrary()}
	}

	normalizeStore(raw)

	// 落盘与内存更新必须在同一把写锁内完成，否则面板并发读到半截状态
	mu.Lock()
	settings = raw.Settings
	items = raw.Items
	loaded = true
	rebuildIndexLocked()
	if file != "" {
		if saveErr := saveLocked(); saveErr != nil {
			logMessage("[Stickers] ⚠️ 表情库落盘失败: %v", saveErr)
		}
	}
	mu.Unlock()
	return nil
}

// IsLoaded 报告表情库是否已完成初始化。
func IsLoaded() bool {
	mu.RLock()
	defer mu.RUnlock()
	return loaded
}

func readStoreFile() (*storeData, error) {
	mu.RLock()
	path := storeFile
	mu.RUnlock()
	if path == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw storeData
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", filepath.Base(path), err)
	}
	return &raw, nil
}

// normalizeStore 补齐缺失字段并丢弃非法条目。返回是否有修改。
func normalizeStore(raw *storeData) bool {
	changed := false
	if raw.Version != storeVersion {
		raw.Version = storeVersion
		changed = true
	}

	def := DefaultSettings()
	s := &raw.Settings
	if s.Mode != ModeLocal && s.Mode != ModeURL && s.Mode != ModeAuto {
		s.Mode = def.Mode
		changed = true
	}
	if s.MaxPerReply <= 0 {
		s.MaxPerReply = def.MaxPerReply
		changed = true
	}
	if s.MaxPerReply > 5 {
		s.MaxPerReply = 5
		changed = true
	}
	s.CDNPrefix = normalizeCDNPrefix(s.CDNPrefix)
	s.ImageHost.Normalize()

	seen := make(map[string]bool, len(raw.Items))
	kept := make([]*Sticker, 0, len(raw.Items))
	for _, it := range raw.Items {
		if it == nil {
			changed = true
			continue
		}
		if !idPattern.MatchString(it.ID) {
			// 手工编辑过的配置文件可能带非法 ID：自动纠正而不是丢弃
			it.ID = newID()
			changed = true
		}
		if seen[it.ID] {
			it.ID = newID()
			changed = true
		}
		if it.Scene == "" {
			it.Scene = "default"
			changed = true
		}
		it.Scene = normalizeScene(it.Scene)
		it.Source = strings.ToLower(strings.TrimSpace(it.Source))
		if it.Source != SourceURL {
			it.Source = SourceLocal
		}
		it.URL = strings.TrimSpace(it.URL)
		it.File = sanitizeRelPath(it.File)

		// 本地副本：路径非法或文件已不在就清掉，但不因此丢弃整条表情
		if it.File != "" {
			if _, err := os.Stat(MediaPath(it.File)); err != nil {
				if !strings.HasPrefix(it.File, defaultsPrefix) {
					it.File = ""
					changed = true
				}
			}
		}

		if it.Source == SourceURL {
			if !isHTTPURL(it.URL) {
				// 直链非法、又没有本地副本作退路：这条彻底没法发了
				if it.File == "" {
					changed = true
					continue
				}
				// 有副本就降级成本地来源，比整条丢掉更合理
				it.Source = SourceLocal
				changed = true
			}
		} else if it.File == "" {
			changed = true
			continue
		}
		if len([]rune(it.Note)) > MaxNoteRunes {
			it.Note = string([]rune(it.Note)[:MaxNoteRunes])
			changed = true
		}
		seen[it.ID] = true
		kept = append(kept, it)
	}
	if len(kept) != len(raw.Items) {
		changed = true
	}
	if len(kept) > maxStickers {
		kept = kept[:maxStickers]
		changed = true
	}
	raw.Items = kept
	return changed
}

// rebuildIndexLocked 重建 ID / 场景索引。调用方须持有写锁。
func rebuildIndexLocked() {
	byID = make(map[string]*Sticker, len(items))
	byScene = make(map[string][]*Sticker, 8)
	for _, it := range items {
		byID[it.ID] = it
		if len(byScene[it.Scene]) < maxStickers {
			byScene[it.Scene] = append(byScene[it.Scene], it)
		}
	}
}

// saveLocked 把当前状态原子写盘。调用方须持有写锁。
func saveLocked() error {
	if storeFile == "" {
		return nil
	}
	raw := storeData{Version: storeVersion, Settings: settings, Items: items}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(storeFile, data, 0o600)
}

// Save 显式落盘（供 WebUI 在批量修改后调用）。
func Save() error {
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return errors.New("表情库尚未初始化")
	}
	return saveLocked()
}

// ─── 查询 ─────────────────────────────────────────────────────────────────────

// CurrentSettings 返回设置的深拷贝。
func CurrentSettings() Settings {
	mu.RLock()
	defer mu.RUnlock()
	cp := settings
	if settings.ExtraKeywords != nil {
		cp.ExtraKeywords = make(map[string][]string, len(settings.ExtraKeywords))
		for k, v := range settings.ExtraKeywords {
			cp.ExtraKeywords[k] = append([]string(nil), v...)
		}
	}
	cp.ImageHost.ExtraForm = copyStringMap(settings.ImageHost.ExtraForm)
	cp.ImageHost.ExtraHeaders = copyStringMap(settings.ImageHost.ExtraHeaders)
	return cp
}

// copyStringMap 复制一个字符串映射，nil 保持 nil（避免面板把空对象写回配置）。
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// All 返回全部表情条目的拷贝（按场景与 ID 排序，便于面板稳定展示）。
func All() []*Sticker {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]*Sticker, 0, len(items))
	for _, it := range items {
		cp := *it
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scene != out[j].Scene {
			return out[i].Scene < out[j].Scene
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get 按 ID 取一条表情。
func Get(id string) (*Sticker, bool) {
	mu.RLock()
	defer mu.RUnlock()
	it, ok := byID[id]
	if !ok {
		return nil, false
	}
	cp := *it
	return &cp, true
}

// Scenes 返回已有表情的场景清单，每个场景附上内置关键词与启用数量。
func Scenes() []SceneInfo {
	mu.RLock()
	counts := make(map[string]int, len(byScene))
	for scene, list := range byScene {
		for _, it := range list {
			if it.Enabled {
				counts[scene]++
			}
		}
	}
	extra := settings.ExtraKeywords
	mu.RUnlock()

	// 场景集合 = 内置场景 ∪ 用户自定义关键词场景 ∪ 实际有条目的场景
	names := make(map[string]bool, 16)
	for scene := range builtinScenes {
		names[scene] = true
	}
	for scene := range extra {
		names[scene] = true
	}
	for scene := range counts {
		names[scene] = true
	}

	out := make([]SceneInfo, 0, len(names))
	for scene := range names {
		out = append(out, SceneInfo{
			Scene:    scene,
			Label:    sceneLabel(scene),
			Keywords: KeywordsOf(scene),
			Count:    counts[scene],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scene < out[j].Scene })
	return out
}

// ─── 变更 ─────────────────────────────────────────────────────────────────────

// ErrNotFound / ErrInvalid 便于 WebUI 层把错误映射成合适的 HTTP 状态码
var (
	ErrNotFound = errors.New("表情不存在")
	ErrInvalid  = errors.New("表情参数非法")
)

// Add 新增一条表情。ID 为空时自动生成。
func Add(s *Sticker) (*Sticker, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: 空条目", ErrInvalid)
	}
	cp := *s

	cp.Scene = normalizeScene(cp.Scene)
	if cp.Scene == "" {
		return nil, fmt.Errorf("%w: 场景名不能为空", ErrInvalid)
	}
	cp.Source = strings.ToLower(strings.TrimSpace(cp.Source))
	cp.URL = strings.TrimSpace(cp.URL)
	cp.File = sanitizeRelPath(cp.File)

	switch cp.Source {
	case SourceURL:
		if !isHTTPURL(cp.URL) {
			return nil, fmt.Errorf("%w: 图床直链必须是 http/https 地址", ErrInvalid)
		}
		// File 是可选的本机副本，路径非法或文件不存在就丢掉，不影响入库
		if cp.File != "" {
			if _, err := os.Stat(MediaPath(cp.File)); err != nil {
				cp.File = ""
			}
		}
	case SourceLocal:
		if cp.File == "" {
			return nil, fmt.Errorf("%w: 本地表情缺少文件名", ErrInvalid)
		}
		cp.URL = ""
		// 确认文件真实存在，避免写进库里却在发送时才失败
		if _, err := os.Stat(MediaPath(cp.File)); err != nil {
			return nil, fmt.Errorf("%w: 本地文件不存在: %s", ErrInvalid, cp.File)
		}
	default:
		return nil, fmt.Errorf("%w: source 只能是 local 或 url", ErrInvalid)
	}

	// ID：留空则自动生成；给了但不合法就报错，而不是悄悄换掉。
	// 静默替换会让用户在面板上填的 ID 变成一串随机字符，且毫无提示。
	if strings.TrimSpace(cp.ID) == "" {
		cp.ID = newID()
	} else if !idPattern.MatchString(cp.ID) {
		return nil, fmt.Errorf("%w: ID 只能包含字母、数字、下划线、连字符，长度 1-%d", ErrInvalid, maxIDLen)
	}
	if len([]rune(cp.Note)) > MaxNoteRunes {
		cp.Note = string([]rune(cp.Note)[:MaxNoteRunes])
	}

	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return nil, errors.New("表情库尚未初始化")
	}
	if len(items) >= maxStickers {
		return nil, fmt.Errorf("%w: 表情数量已达上限 %d", ErrInvalid, maxStickers)
	}
	if _, exists := byID[cp.ID]; exists {
		// 极低概率的 ID 碰撞（或用户手动指定了重复 ID）：让它失败而不是悄悄覆盖
		if idPattern.MatchString(strings.TrimSpace(s.ID)) && s.ID == cp.ID {
			return nil, fmt.Errorf("%w: ID %q 已存在", ErrInvalid, cp.ID)
		}
		cp.ID = newID()
	}
	items = append(items, &cp)
	rebuildIndexLocked()
	if err := saveLocked(); err != nil {
		// 落盘失败就回滚内存，保证内存与磁盘一致
		items = items[:len(items)-1]
		rebuildIndexLocked()
		return nil, err
	}
	out := cp
	return &out, nil
}

// UpdateFields 描述一次局部更新：nil 表示「这一项不动」。
//
// 之所以不用「非空即覆盖」的老写法：URL 与 File 都可能是空串，
// 而空串有时代表「不要改」，有时代表「清掉」。用指针才能区分，
// 面板也就能表达「把这条从本地改成直链」「把直链清掉退回本地」这类操作。
type UpdateFields struct {
	Scene   *string `json:"scene,omitempty"`
	Source  *string `json:"source,omitempty"`
	URL     *string `json:"url,omitempty"`
	File    *string `json:"file,omitempty"`
	Note    *string `json:"note,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

// Update 按 ID 局部更新一条表情。
func Update(id string, f UpdateFields) (*Sticker, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: 缺少 ID", ErrInvalid)
	}

	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return nil, errors.New("表情库尚未初始化")
	}
	cur, ok := byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	prev := *cur
	next := *cur

	if f.Scene != nil {
		scene := normalizeScene(*f.Scene)
		if scene == "" {
			return nil, fmt.Errorf("%w: 场景名非法", ErrInvalid)
		}
		next.Scene = scene
	}
	if f.Source != nil {
		src := strings.ToLower(strings.TrimSpace(*f.Source))
		if src != SourceLocal && src != SourceURL {
			return nil, fmt.Errorf("%w: source 只能是 local 或 url", ErrInvalid)
		}
		next.Source = src
	}
	if f.URL != nil {
		next.URL = strings.TrimSpace(*f.URL)
	}
	if f.File != nil {
		next.File = sanitizeRelPath(*f.File)
		if *f.File != "" && next.File == "" {
			return nil, fmt.Errorf("%w: 本地文件路径非法", ErrInvalid)
		}
	}
	if f.Note != nil {
		next.Note = strings.TrimSpace(*f.Note)
		if len([]rune(next.Note)) > MaxNoteRunes {
			next.Note = string([]rune(next.Note)[:MaxNoteRunes])
		}
	}
	if f.Enabled != nil {
		next.Enabled = *f.Enabled
	}

	// 来源与素材的一致性检查
	switch next.Source {
	case SourceURL:
		if !isHTTPURL(next.URL) {
			if next.File == "" {
				return nil, fmt.Errorf("%w: 直链来源必须提供 http/https 地址", ErrInvalid)
			}
			// 直链非法但有副本：降级为本地来源
			next.Source = SourceLocal
		}
	case SourceLocal:
		if next.File == "" {
			return nil, fmt.Errorf("%w: 本地来源必须有文件，或先提供直链再切到 url 来源", ErrInvalid)
		}
		if _, err := os.Stat(mediaPathLocked(next.File)); err != nil {
			return nil, fmt.Errorf("%w: 本地文件不存在: %s", ErrInvalid, next.File)
		}
	}
	if next.Source == SourceLocal && next.URL != "" {
		// 本地来源不存直链：直链只用来构建 PublicURL（走 CDN 前缀）
		next.URL = ""
	}

	*cur = next
	rebuildIndexLocked()
	if err := saveLocked(); err != nil {
		*cur = prev
		rebuildIndexLocked()
		return nil, err
	}
	out := next
	return &out, nil
}

// Delete 删除一条表情。附带删除 uploads/ 下的本地文件（defaults/ 保留，
// 因为它是随二进制分发的只读副本，删了下次启动还会被解压回来）。
func Delete(id string) error {
	if id == "" {
		return fmt.Errorf("%w: 缺少 ID", ErrInvalid)
	}
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return errors.New("表情库尚未初始化")
	}
	idx := -1
	for i, it := range items {
		if it.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	removed := items[idx]
	items = append(items[:idx:idx], items[idx+1:]...)
	rebuildIndexLocked()
	if err := saveLocked(); err != nil {
		// 回滚
		items = append(items, nil)
		copy(items[idx+1:], items[idx:])
		items[idx] = removed
		rebuildIndexLocked()
		return err
	}

	if removed.Source == SourceLocal && strings.HasPrefix(removed.File, "uploads/") {
		if p := mediaPathLocked(removed.File); p != "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				logMessage("[Stickers] 清理表情文件失败 %s: %v", removed.File, err)
			}
		}
	}
	return nil
}

// UpdateSettings 修改设置并落盘。
func UpdateSettings(apply func(*Settings) error) (Settings, error) {
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return settings, errors.New("表情库尚未初始化")
	}
	prev := settings
	next := settings
	if next.ExtraKeywords == nil {
		next.ExtraKeywords = nil
	}
	if err := apply(&next); err != nil {
		settings = prev
		return prev, err
	}
	if next.Mode != ModeLocal && next.Mode != ModeURL && next.Mode != ModeAuto {
		return prev, fmt.Errorf("%w: mode 只能是 auto / local / url", ErrInvalid)
	}
	next.CDNPrefix = normalizeCDNPrefix(next.CDNPrefix)
	next.ImageHost.Normalize()
	if next.MaxPerReply <= 0 {
		next.MaxPerReply = 1
	}
	if next.MaxPerReply > 5 {
		next.MaxPerReply = 5
	}
	settings = next
	if err := saveLocked(); err != nil {
		settings = prev
		return prev, err
	}
	cp := next
	return cp, nil
}

// ─── 工具 ─────────────────────────────────────────────────────────────────────

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "s" + fmt.Sprint(time.Now().UnixNano()%1_000_000_000)
	}
	return hex.EncodeToString(b[:])
}

func normalizeScene(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	// 场景名会被拼进提示词与标记，只保留安全字符
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

// sanitizeRelPath 只保留「媒体目录内的相对路径」，挡掉 ../、绝对路径与
// 任意层级嵌套，避免面板的写入接口被用来读写媒体目录以外的文件。
//
// 允许的形态只有两种，与磁盘布局一一对应：
//
//	defaults/<文件名>   随二进制分发的内置表情
//	uploads/<文件名>    用户在面板上传的表情
func sanitizeRelPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return ""
	}
	parts := strings.Split(clean, "/")
	if len(parts) != 2 {
		return ""
	}
	switch parts[0] {
	case "defaults", "uploads":
	default:
		return ""
	}
	if parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return ""
	}
	return clean
}

// mediaPathLocked 与 MediaPath 等价，但假定调用方已经持有锁。
//
// 必须存在这个「无锁版本」：sync.RWMutex 不可重入，Delete / Update 已经
// 持有写锁，再去调 MediaPath 的 RLock 会直接把自己锁死。
func mediaPathLocked(rel string) string {
	clean := sanitizeRelPath(rel)
	if clean == "" || mediaDir == "" {
		return ""
	}
	return filepath.Join(mediaDir, filepath.FromSlash(clean))
}

func normalizeCDNPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !isHTTPURL(p) {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func isHTTPURL(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// MediaPath 把库内相对路径映射成本机绝对路径。
func MediaPath(rel string) string {
	mu.RLock()
	defer mu.RUnlock()
	return mediaPathLocked(rel)
}

// MediaDir 返回本地表情文件所在目录。
func MediaDir() string {
	mu.RLock()
	defer mu.RUnlock()
	return mediaDir
}

// Exists 报告某条表情当前是否可用（本地文件存在 / 直链非空）。
func (s *Sticker) Exists() bool {
	if s == nil {
		return false
	}
	if s.Source == SourceURL && isHTTPURL(s.URL) {
		return true
	}
	if s.File != "" {
		if _, err := os.Stat(MediaPath(s.File)); err == nil {
			return true
		}
		if strings.HasPrefix(s.File, defaultsPrefix) {
			if _, err := ReadEmbeddedDefault(s.File); err == nil {
				return true
			}
		}
	}
	return false
}

// PublicURL 返回这条表情在图床上的可公开访问地址。
// 本地条目只有配置了 CDN 前缀才有直链 —— 这正是「把仓库当图床」的落点。
func (s *Sticker) PublicURL(cdnPrefix string) string {
	if s == nil {
		return ""
	}
	if s.Source == SourceURL && s.URL != "" {
		return s.URL
	}
	if s.File == "" {
		return ""
	}
	prefix := normalizeCDNPrefix(cdnPrefix)
	if prefix == "" {
		return ""
	}
	return prefix + strings.TrimPrefix(s.File, "/")
}

// LocalPath 返回本机副本的绝对路径，没有副本时返回空串。
func (s *Sticker) LocalPath() string {
	if s == nil || s.File == "" {
		return ""
	}
	return MediaPath(s.File)
}
