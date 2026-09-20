package stickers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cybercompanion/internal/imagehost"
)

// ─── 本地表情 → 图床直链 ───────────────────────────────────────────────────────
//
// 「表情包接图床」的完整闭环就在这两个函数里：
//  1. 面板上传或内置的本地图片，经 UploadLocal 推到用户配置的图床
//  2. 把拿回来的直链写回条目（Promote），之后发送时直接走 URL，不再读盘
//
// 不强制用户先建图床再传图：UploadLocal 单独可用，
// 用户可以只上传拿到链接、手工贴到别处。

// ImageHostStatus 描述图床配置的可用状态，供面板做按钮置灰与提示。
type ImageHostStatus struct {
	Ready      bool   `json:"ready"`
	UploadURL  string `json:"upload_url"`
	AuthMode   string `json:"auth_mode"`
	Provider   string `json:"provider"`
	ResultPath string `json:"result_path"`
	Reason     string `json:"reason,omitempty"`
}

// ImageHostStatusNow 返回当前图床配置的可用性。
func ImageHostStatusNow() ImageHostStatus {
	mu.RLock()
	cfg := settings.ImageHost
	mu.RUnlock()
	cfg.Normalize()

	st := ImageHostStatus{
		Ready:      cfg.Ready(),
		UploadURL:  cfg.UploadURL,
		AuthMode:   cfg.AuthMode,
		Provider:   cfg.Provider,
		ResultPath: cfg.ResultPath,
	}
	switch {
	case !cfg.Ready():
		st.Reason = "尚未填写图床上传地址"
	case cfg.AuthMode != imagehost.AuthNone && cfg.Token == "":
		st.Reason = "鉴权方式已选择，但还没填 Token"
	}
	return st
}

// UploadLocal 把一条表情的本机副本推到图床，返回可公开访问的直链。
// 只上传、不改动库；调用方决定要不要 Promote。
func UploadLocal(id string) (*Sticker, imagehost.Result, error) {
	s, ok := Get(id)
	if !ok {
		return nil, imagehost.Result{}, ErrNotFound
	}
	if s.File == "" {
		return nil, imagehost.Result{}, fmt.Errorf("%w: 该条目没有本机副本，无法上传（已经是纯直链来源）", ErrInvalid)
	}

	data, mime, err := readMediaBytes(s)
	if err != nil {
		return nil, imagehost.Result{}, err
	}

	mu.RLock()
	cfg := settings.ImageHost
	mu.RUnlock()

	name := filepath.Base(s.File)
	if !strings.Contains(name, ".") {
		name += ExtForMIME(mime)
	}
	res, err := imagehost.Upload(cfg, name, mime, data)
	if err != nil {
		return nil, res, err
	}
	return s, res, nil
}

// PromoteToImageHost 上传并把条目改写成直链来源。
//
// dropLocal 为 true 时同时删除本机副本，适合存储紧张的随身设备；
// 为 false 时保留副本作为退路 —— 图床临时抽风时表情仍能发出去。
func PromoteToImageHost(id string, dropLocal bool) (*Sticker, imagehost.Result, error) {
	s, res, err := UploadLocal(id)
	if err != nil {
		return nil, res, err
	}

	fields := UpdateFields{
		Source: stringPtr(SourceURL),
		URL:    stringPtr(res.URL),
	}
	if dropLocal {
		fields.File = stringPtr("")
	}

	updated, err := Update(s.ID, fields)
	if err != nil {
		// 上传成功但落库失败：至少把链接回给用户，不要让人白等一次上传
		return nil, res, fmt.Errorf("图片已上传成功（%s），但保存到表情库失败: %w", res.URL, err)
	}

	if dropLocal && strings.HasPrefix(s.File, "uploads/") {
		if p := MediaPath(s.File); p != "" {
			if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				logMessage("[Stickers] 上传成功但清理本地副本失败 %s: %v", s.File, rmErr)
			}
		}
	}
	return updated, res, nil
}

func stringPtr(s string) *string { return &s }

// BatchPromote 批量上传并把所有本地条目升级为直链。
// 返回成功条数与第一个错误 —— 部分失败时不要把已经成功的回滚掉。
func BatchPromote(ids []string, dropLocal bool) (int, []string) {
	var (
		ok   int
		errs []string
	)
	for _, id := range ids {
		if _, _, err := PromoteToImageHost(id, dropLocal); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		ok++
	}
	return ok, errs
}
