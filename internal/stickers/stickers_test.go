package stickers

// 本文件只覆盖 marker_test.go 未涉及的面：
// 图片落盘与压缩、路径消毒、CDN 前缀拼接、图床桥接。
// 存储与标记协议的用例在 marker_test.go。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cybercompanion/internal/imagehost"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestRelPathSanitization(t *testing.T) {
	bad := []string{
		"", "../etc/passwd", "/etc/passwd", "a/b/c.png", "uploads/../../x.png",
		`..\windows\system32`, "./", "..",
	}
	for _, p := range bad {
		if got := sanitizeRelPath(p); got != "" {
			t.Errorf("sanitizeRelPath(%q) = %q, want empty", p, got)
		}
	}
	good := map[string]string{
		"defaults/love_0.jpg": "defaults/love_0.jpg",
		`uploads\a1b2.png`:    "uploads/a1b2.png",
		"/uploads/a1b2.png":   "uploads/a1b2.png",
	}
	for in, want := range good {
		if got := sanitizeRelPath(in); got != want {
			t.Errorf("sanitizeRelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveMediaWritesAndCleansUp(t *testing.T) {
	dir := newTestStore(t)

	rel, size, err := SaveMedia(mustPNG(t, 64, 48))
	if err != nil {
		t.Fatalf("SaveMedia 失败: %v", err)
	}
	if size <= 0 || !strings.HasPrefix(rel, "uploads/") {
		t.Fatalf("SaveMedia 返回 %q / %d", rel, size)
	}
	abs := filepath.Join(dir, mediaDirName, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("上传文件未落盘: %v", err)
	}

	if _, err := Add(&Sticker{ID: "mine", Scene: "love", Source: SourceLocal, File: rel, Enabled: true}); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if got, ok := Pick("mine"); !ok || got.ID != "mine" {
		t.Fatal("新增的表情无法被 Pick 命中")
	}

	if err := Delete("mine"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("删除 uploads 来源的表情后应清理文件, stat err = %v", err)
	}
}

func TestSaveMediaRejectsNonImage(t *testing.T) {
	newTestStore(t)
	if _, _, err := SaveMedia([]byte("this is definitely not an image at all")); err == nil {
		t.Fatal("非图片内容应被拒绝")
	}
	if _, _, err := SaveMedia(nil); err == nil {
		t.Fatal("空内容应被拒绝")
	}
	if _, _, err := SaveMedia(make([]byte, MaxMediaBytes+1)); err == nil {
		t.Fatal("超限文件应被拒绝")
	}
}

func TestOptimizeImageShrinksLargeJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1600, 1200))
	seed := uint32(12345)
	for y := 0; y < 1200; y++ {
		for x := 0; x < 1600; x++ {
			seed = seed*1664525 + 1013904223
			src.SetRGBA(x, y, color.RGBA{
				R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	original := buf.Bytes()

	out, mime := optimizeImage(original, "image/jpeg")
	if len(out) >= len(original) {
		t.Fatalf("优化后反而变大: %d -> %d", len(original), len(out))
	}
	if mime != "image/jpeg" {
		t.Fatalf("MIME = %q", mime)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("优化结果不是合法 JPEG: %v", err)
	}
	if cfg.Width > maxDim || cfg.Height > maxDim {
		t.Fatalf("优化后尺寸仍超标: %dx%d", cfg.Width, cfg.Height)
	}
}

// TestOptimizeImageKeepsPNGAlpha 带透明通道的 PNG 不能被压成 JPEG，
// 否则贴纸的透明区域会变成黑块。
func TestOptimizeImageKeepsPNGAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0, A: uint8(255 - x/8)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, mime := optimizeImage(buf.Bytes(), "image/png")
	if mime != "image/png" {
		t.Fatalf("PNG 应保持 PNG，实际 %q", mime)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("优化结果不是合法 PNG: %v", err)
	}
	cfg, _ := png.DecodeConfig(bytes.NewReader(out))
	if cfg.Width > maxDim || cfg.Height > maxDim {
		t.Fatalf("优化后尺寸仍超标: %dx%d", cfg.Width, cfg.Height)
	}
}

// TestCDNPrefixTurnsLocalIntoDirectLink 是「把仓库当图床」的验收点。
func TestCDNPrefixTurnsLocalIntoDirectLink(t *testing.T) {
	newTestStore(t)
	if _, err := Add(&Sticker{ID: "local1", Scene: "love", Source: SourceLocal,
		File: "defaults/love_0.jpg", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	p, err := Sendable("local1")
	if err != nil {
		t.Fatalf("Sendable: %v", err)
	}
	if p.URL != "" {
		t.Fatalf("未配置前缀时不该有直链: %q", p.URL)
	}
	if len(p.Data) == 0 || !strings.HasPrefix(p.MIME, "image/") {
		t.Fatalf("应带上本地字节, len=%d mime=%q", len(p.Data), p.MIME)
	}

	const prefix = "https://cdn.jsdelivr.net/gh/u/r@main/internal/stickers"
	if _, err := UpdateSettings(func(s *Settings) error { s.CDNPrefix = prefix; return nil }); err != nil {
		t.Fatal(err)
	}
	p2, err := Sendable("local1")
	if err != nil {
		t.Fatalf("Sendable: %v", err)
	}
	if p2.URL != prefix+"/defaults/love_0.jpg" {
		t.Fatalf("CDN 直链 = %q", p2.URL)
	}
	if len(p2.Data) == 0 {
		t.Fatal("auto 模式下仍应带上本地字节作为退路")
	}

	// 前缀非法（非 http）应被规范化掉，而不是拼出一个坏链接
	if _, err := UpdateSettings(func(s *Settings) error { s.CDNPrefix = "ftp://nope"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := CurrentSettings().CDNPrefix; got != "" {
		t.Fatalf("非法前缀应被清空, got %q", got)
	}
}

func TestSettingsClampAndDeepCopy(t *testing.T) {
	newTestStore(t)

	if _, err := UpdateSettings(func(s *Settings) error { s.Mode = "bogus"; return nil }); err == nil {
		t.Fatal("非法 mode 应被拒绝")
	}
	if got := CurrentSettings().Mode; got != ModeAuto {
		t.Fatalf("失败后设置不应被改动, Mode = %q", got)
	}

	s, err := UpdateSettings(func(st *Settings) error {
		st.MaxPerReply = 99
		st.CDNPrefix = "https://x.example.com/base"
		st.ExtraKeywords = map[string][]string{"solo": {"单飞"}}
		st.ImageHost.ExtraHeaders = map[string]string{"X-Test": "1"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxPerReply != 5 {
		t.Errorf("MaxPerReply 未夹取, got %d", s.MaxPerReply)
	}
	if !strings.HasSuffix(s.CDNPrefix, "/") {
		t.Errorf("CDNPrefix 未补斜杠: %q", s.CDNPrefix)
	}

	cp := CurrentSettings()
	cp.ExtraKeywords["solo"][0] = "污染"
	cp.ImageHost.ExtraHeaders["X-Test"] = "污染"
	again := CurrentSettings()
	if again.ExtraKeywords["solo"][0] == "污染" {
		t.Error("ExtraKeywords 未深拷贝")
	}
	if again.ImageHost.ExtraHeaders["X-Test"] == "污染" {
		t.Error("ImageHost.ExtraHeaders 未深拷贝")
	}
}

// ─── 图床桥接 ─────────────────────────────────────────────────────────────────

func TestPromoteToImageHost(t *testing.T) {
	newTestStore(t)

	var gotAuth, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotFile = hdr.Filename
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"url": "/f/" + hdr.Filename},
		})
	}))
	defer srv.Close()

	rel, _, err := SaveMedia(mustPNG(t, 16, 16))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(&Sticker{ID: "up1", Scene: "love", Source: SourceLocal, File: rel, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// 没配图床时必须明确报错，而不是静默失败
	if _, _, err := PromoteToImageHost("up1", false); err == nil {
		t.Fatal("未配置图床时应报错")
	}
	if st := ImageHostStatusNow(); st.Ready || st.Reason == "" {
		t.Fatalf("未配置时状态不对: %+v", st)
	}

	if _, err := UpdateSettings(func(s *Settings) error {
		s.ImageHost = imagehost.Config{
			UploadURL:  srv.URL + "/upload",
			FieldName:  "file",
			AuthMode:   imagehost.AuthBearer,
			Token:      "tok-123",
			ResultPath: "data.url",
			PublicBase: "https://img.example.com",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := ImageHostStatusNow(); !st.Ready {
		t.Fatalf("图床应就绪: %+v", st)
	}

	updated, res, err := PromoteToImageHost("up1", false)
	if err != nil {
		t.Fatalf("PromoteToImageHost 失败: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("鉴权头 = %q", gotAuth)
	}
	if !strings.HasSuffix(gotFile, ".png") {
		t.Errorf("上传文件名异常: %q", gotFile)
	}
	if updated.Source != SourceURL || updated.URL != "https://img.example.com/f/"+gotFile {
		t.Fatalf("条目未升级为直链: %+v", updated)
	}
	if updated.File == "" {
		t.Error("dropLocal=false 时应保留本机副本作为退路")
	}
	if res.RawURL == "" || res.RawBody == "" {
		t.Errorf("应保留原始返回便于排障: %+v", res)
	}
	// 直链来源 + 本地副本：URL 与 Data 都要有，发送时先试直链再退副本
	p, err := Sendable("up1")
	if err != nil {
		t.Fatalf("Sendable: %v", err)
	}
	if p.URL != updated.URL || len(p.Data) == 0 {
		t.Fatalf("升级后的载荷不对: url=%q dataLen=%d", p.URL, len(p.Data))
	}
}

func TestPromoteDropsLocalCopy(t *testing.T) {
	newTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://img.example.com/ok.png"}`))
	}))
	defer srv.Close()

	if _, err := UpdateSettings(func(s *Settings) error {
		s.ImageHost = imagehost.Config{UploadURL: srv.URL, FieldName: "file", ResultPath: "url"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rel, _, err := SaveMedia(mustPNG(t, 16, 16))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(&Sticker{ID: "up2", Scene: "love", Source: SourceLocal, File: rel, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	abs := MediaPath(rel)

	updated, _, err := PromoteToImageHost("up2", true)
	if err != nil {
		t.Fatalf("PromoteToImageHost: %v", err)
	}
	if updated.File != "" {
		t.Errorf("dropLocal=true 时应清掉副本引用: %+v", updated)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("dropLocal=true 时应删除本地文件, stat err=%v", err)
	}
}

func TestBatchPromoteKeepsGoingOnFailure(t *testing.T) {
	newTestStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := UpdateSettings(func(s *Settings) error {
		s.ImageHost = imagehost.Config{UploadURL: srv.URL, FieldName: "file", ResultPath: "data.url"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"a1", "a2"} {
		rel, _, err := SaveMedia(mustPNG(t, 8, 8))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Add(&Sticker{ID: id, Scene: "x", Source: SourceLocal, File: rel, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}

	ok, errs := BatchPromote([]string{"a1", "a2"}, false)
	if ok != 0 || len(errs) != 2 {
		t.Fatalf("ok=%d errs=%v", ok, errs)
	}
	// 失败后条目必须保持可用（仍是本地来源）
	for _, id := range []string{"a1", "a2"} {
		got, _ := Get(id)
		if got == nil || got.Source != SourceLocal || !got.Exists() {
			t.Fatalf("上传失败不应破坏条目: %+v", got)
		}
	}
}

func mustPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 PNG 失败: %v", err)
	}
	return buf.Bytes()
}
