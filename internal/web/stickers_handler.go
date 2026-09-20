package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cybercompanion/internal/imagehost"
	"cybercompanion/internal/stickers"
)

// ─── 表情包与图床管理 API ────────────────────────────────────────────────────
//
// 这一系列端点让 WebUI 能完整管理「接图床 + 大模型自己发 + 关键词本地发」：
//   - GET  /api/stickers            列表 + 设置 + 场景词表 + 图床状态（一屏拉全）
//   - POST /api/stickers            新增一条表情
//   - PUT  /api/stickers            更新一条表情（按 ID，字段可选）
//   - DELETE /api/stickers?id=...   删除
//   - GET/PUT /api/stickers/settings 发表情规则 + 图床配置
//   - GET /api/stickers/scenes      场景词表（含内置关键词，供下拉与参考）
//   - POST /api/stickers/upload     本地图片落盘 → 可选推图床 → 入库
//   - POST /api/stickers/host-test  用一张样例图试上传，回传原始响应便于排障
//   - GET  /api/stickers/media/<rel> 取表情缩略图（defaults/ 与 uploads/ 安全读取）

const stickerMaxUploadBytes = 16 << 20 // 单文件上限 16MB，防止面板被拿来做文件仓库

// tinyPNG 是一张 1x1 的合法 PNG，用作图床连通性测试时的默认样例图，
// 避免每次测试都要用户先传一张图。初始化时现生成，保证结构合法（CRC 正确）。
var tinyPNG []byte

func init() {
	buf := &bytes.Buffer{}
	// 1x1 透明像素即可，重点是被图床接受返回 URL
	if err := png.Encode(buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err == nil {
		tinyPNG = buf.Bytes()
	}
}

func (s *Server) handleStickers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp := map[string]interface{}{
			"items":      stickers.All(),
			"settings":   stickers.CurrentSettings(),
			"scenes":     stickers.Scenes(),
			"hostStatus": stickers.ImageHostStatusNow(),
			"loaded":     stickers.IsLoaded(),
		}
		writeJSON(w, resp)

	case http.MethodPost:
		var in stickers.Sticker
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
			http.Error(w, "请求格式错误: "+err.Error(), http.StatusBadRequest)
			return
		}
		created, err := stickers.Add(&in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log("[Stickers] 新增表情 %s (场景 %s, 来源 %s)", created.ID, created.Scene, created.Source)
		writeJSON(w, created)

	case http.MethodPut:
		var in struct {
			ID      string  `json:"id"`
			Scene   *string `json:"scene,omitempty"`
			Source  *string `json:"source,omitempty"`
			URL     *string `json:"url,omitempty"`
			File    *string `json:"file,omitempty"`
			Note    *string `json:"note,omitempty"`
			Enabled *bool   `json:"enabled,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
			http.Error(w, "请求格式错误: "+err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := stickers.Update(in.ID, stickers.UpdateFields{
			Scene:   in.Scene,
			Source:  in.Source,
			URL:     in.URL,
			File:    in.File,
			Note:    in.Note,
			Enabled: in.Enabled,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log("[Stickers] 更新表情 %s", updated.ID)
		writeJSON(w, updated)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "缺少 id 参数", http.StatusBadRequest)
			return
		}
		if err := stickers.Delete(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log("[Stickers] 删除表情 %s", id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStickerSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, stickers.CurrentSettings())

	case http.MethodPut:
		// 增量合入当前设置：只覆盖前端实际提交的字段，未提交的不清零
		// （ExtraKeywords / ImageHost 嵌套对象尤其不能因为没传就被抹掉）。
		cur := stickers.CurrentSettings()
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&cur); err != nil {
			http.Error(w, "请求格式错误: "+err.Error(), http.StatusBadRequest)
			return
		}
		next, err := stickers.UpdateSettings(func(s *stickers.Settings) error {
			*s = cur
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log("[Stickers] 发表情规则已更新 (来源=%s, 智能=%v, 关键词=%v, 上限=%d)",
			next.Mode, next.SmartSend, next.KeywordSend, next.MaxPerReply)
		writeJSON(w, next)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStickerScenes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, stickers.Scenes())
}

// handleStickerUpload 接收多张本地图片：落盘 → 可选推到图床 → 入库。
//
// 图床未配置或上传失败时优雅降级：表情仍作为本地来源入库，发送时走本地副本或 CDN 前缀。
// 绝不因为图床抽风就让整批上传失败。
func (s *Server) handleStickerUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(stickerMaxUploadBytes); err != nil {
		http.Error(w, "解析上传失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	scene := strings.TrimSpace(r.FormValue("scene"))
	note := strings.TrimSpace(r.FormValue("note"))
	target := strings.TrimSpace(r.FormValue("target")) // "local" | "imagehost"
	dropLocal := r.FormValue("drop_local") == "1" || r.FormValue("drop_local") == "true"

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "未选择任何图片", http.StatusBadRequest)
		return
	}

	var created []*stickers.Sticker
	var warnings []string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: 打开失败 %v", fh.Filename, err))
			continue
		}
		data, err := stickers.DecodeImageFromReader(f, stickerMaxUploadBytes)
		f.Close()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", fh.Filename, err))
			continue
		}

		rel, _, err := stickers.SaveMedia(data)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", fh.Filename, err))
			continue
		}

		st, err := stickers.Add(&stickers.Sticker{
			Scene:   scene,
			Source:  stickers.SourceLocal,
			File:    rel,
			Note:    note,
			Enabled: true,
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", fh.Filename, err))
			continue
		}

		// 目标选图床且图床就绪：推上去并改写成直链来源
		if target == "imagehost" {
			if stickers.ImageHostStatusNow().Ready {
				if _, _, perr := stickers.PromoteToImageHost(st.ID, dropLocal); perr != nil {
					warnings = append(warnings, fmt.Sprintf("%s: 已存为本地（图床上传失败：%v）", fh.Filename, perr))
				}
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: 图床未配置，已存为本地", fh.Filename))
			}
		}
		created = append(created, st)
	}

	s.log("[Stickers] 批量上传 %d 张（成功 %d）", len(files), len(created))
	writeJSON(w, map[string]interface{}{
		"created":   created,
		"warnings":  warnings,
		"hostReady": stickers.ImageHostStatusNow().Ready,
	})
}

// handleStickerHostTest 用一张样例图（用户传的或内置 1x1 PNG）试上传到图床，
// 回传 URL 与原始响应体，方便把「路径填错了」「前缀没填」这类问题直接暴露出来。
func (s *Server) handleStickerHostTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 优先用请求体里的 image_host（用户刚填的、尚未保存的配置）；
	// 没传或不可用时退回服务端已保存的图床配置。
	cfg := stickers.CurrentSettings().ImageHost
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			ImageHost *imagehost.Config `json:"image_host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err == nil && body.ImageHost != nil {
			cfg = *body.ImageHost
		}
	}

	var data []byte
	if r.MultipartForm != nil {
		if fhs := r.MultipartForm.File["file"]; len(fhs) > 0 {
			f, err := fhs[0].Open()
			if err == nil {
				data, err = stickers.DecodeImageFromReader(f, stickerMaxUploadBytes)
				f.Close()
			}
		}
	}
	if len(data) == 0 {
		data = tinyPNG // 没有上传文件时退化为 1x1 透明 PNG
	}
	if len(data) == 0 {
		http.Error(w, "无法生成测试图片", http.StatusInternalServerError)
		return
	}

	start := time.Now()
	res, err := imagehost.Upload(cfg, "host-test.png", "image/png", data)
	elapsed := time.Since(start)

	resp := map[string]interface{}{
		"url":         res.URL,
		"raw_url":     res.RawURL,
		"raw_body":    res.RawBody,
		"status_code": res.StatusCode,
		"elapsed_ms":  elapsed.Milliseconds(),
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

// handleStickerMedia 取表情缩略图。路径经过 stickers.MediaPath 消毒，
// 只能落在 mediaDir 下的 defaults/ 或 uploads/ 两层内，杜绝目录穿越。
func (s *Server) handleStickerMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/api/stickers/media/")
	rel = strings.Trim(rel, "/")
	if rel == "" || strings.Contains(rel, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	p := stickers.MediaPath(rel)
	if p == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeByExt(p))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size()))
	http.ServeFile(w, r, p)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mimeByExt(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}
