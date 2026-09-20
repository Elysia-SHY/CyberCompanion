package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/memory"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/provider"
	"cybercompanion/internal/store"
)

// ─── 引擎集成测试 ────────────────────────────────────────────────────────────
//
// 这里的目的是闭环验证「模型说要用某个能力 → 能力被执行 → 结果回到调用方」
// 这条链路 —— 它是整个改造的核心价值所在，也是最容易在接线处出错的地方。
//
// 用 httptest 起一个假的模型端点，而不是打桩 provider：这样走的是真实的
// 请求构造、SSE 解析、标记门控与插件执行。

// fakeLLM 起一个返回固定内容的模型端点。
//
// stream 为 true 时按 SSE 逐片返回，并且刻意把调用标记切碎 ——
// 这正是流式路径下最容易出问题的地方。
func fakeLLM(t *testing.T, content string, stream bool) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			flusher, _ := w.(http.Flusher)
			// 按 rune 逐片下发：模拟最恶劣的切分方式
			for _, r := range content {
				chunk := map[string]interface{}{
					"choices": []map[string]interface{}{
						{"delta": map[string]string{"content": string(r)}},
					},
				}
				payload, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useEndpoint 把全局配置指向假端点，并在测试结束后还原。
func useEndpoint(t *testing.T, url string) {
	t.Helper()

	prev := config.Get()
	t.Cleanup(func() {
		_ = config.Update(func(c *config.Config) { *c = *prev })
	})

	if err := config.Update(func(c *config.Config) {
		c.OneAPIURL = url
		c.OneAPIToken = "test-token"
		c.Model = "test-model"
		c.Providers = nil
		c.Routing = config.ModelRouting{}
		c.Memory = config.MemorySettings{}
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
}

// recordingPlugin 记录自己被调用时的参数。
type recordingPlugin struct {
	name  string
	calls []map[string]string
	reply string
}

func (p *recordingPlugin) Name() string        { return p.name }
func (p *recordingPlugin) Description() string { return "测试用能力" }
func (p *recordingPlugin) Schema() plugin.Schema {
	return plugin.Schema{
		Params:   []plugin.Param{{Name: "path", Description: "路径", Required: false}},
		Examples: []string{"看看磁盘"},
	}
}
func (p *recordingPlugin) MinRole() store.Role { return store.RoleGuest }
func (p *recordingPlugin) Permission() string  { return "" }

func (p *recordingPlugin) Execute(_ context.Context, args map[string]string, _ *plugin.ExecContext) plugin.Result {
	p.calls = append(p.calls, args)
	return plugin.Result{Text: p.reply, Handled: true}
}

// buildEngine 组装一个使用假端点的引擎。
func buildEngine(t *testing.T, url string, plugins ...plugin.Plugin) (*Engine, *plugin.Registry) {
	t.Helper()
	useEndpoint(t, url)

	reg := plugin.NewRegistry(nil)
	for _, p := range plugins {
		reg.Register(p)
	}

	providers := provider.NewRegistry(nil)
	mem := memory.NewManager(nil, memory.Config{Enabled: false}, nil)
	return NewEngine(providers, reg, mem), reg
}

// ─── 非流式：标记解析与能力执行 ──────────────────────────────────────────────

func TestEngineExecutesCalledPlugin(t *testing.T) {
	rec := &recordingPlugin{name: "disk", reply: "磁盘已用 42%"}
	srv := fakeLLM(t, "我查一下[能力:disk path=/tmp]马上好", false)
	engine, _ := buildEngine(t, srv.URL, rec)

	resp, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleOwner, UserText: "磁盘占用多少",
	})
	if err != nil {
		t.Fatalf("引擎执行失败: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("能力被调用 %d 次, 期望 1", len(rec.calls))
	}
	if rec.calls[0]["path"] != "/tmp" {
		t.Fatalf("参数传递错误: %+v", rec.calls[0])
	}
	if len(resp.Extra) != 1 || resp.Extra[0] != "磁盘已用 42%" {
		t.Fatalf("能力结果未回传: %+v", resp.Extra)
	}
	if len(resp.UsedPlugins) != 1 || resp.UsedPlugins[0] != "disk" {
		t.Fatalf("UsedPlugins 记录异常: %+v", resp.UsedPlugins)
	}
	// 标记必须从正文里剥掉，用户不该看到内部协议
	if strings.Contains(resp.Text, "能力") || strings.Contains(resp.Text, "[") {
		t.Fatalf("正文里残留了调用标记: %q", resp.Text)
	}
}

func TestEnginePlainChatNoPlugin(t *testing.T) {
	rec := &recordingPlugin{name: "disk", reply: "不该被调用"}
	srv := fakeLLM(t, "今天天气不错，出去走走吧～", false)
	engine, _ := buildEngine(t, srv.URL, rec)

	resp, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleGuest, UserText: "你好呀",
	})
	if err != nil {
		t.Fatalf("引擎执行失败: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatal("纯闲聊不该调用任何能力")
	}
	if resp.Text != "今天天气不错，出去走走吧～" {
		t.Fatalf("正文 = %q", resp.Text)
	}
}

// TestEnginePermissionBlocksPlugin 验证权限在调用点生效。
//
// 提示词里不列该能力只是「不提示」，真正的防线必须在执行处 ——
// 模型完全可能凭记忆或猜测写出一个未列出的能力名。
func TestEnginePermissionBlocksPlugin(t *testing.T) {
	rec := &recordingPlugin{name: "disk", reply: "不该被调用"}
	srv := fakeLLM(t, "[能力:disk path=/]", false)
	engine, _ := buildEngine(t, srv.URL, rec)

	// 记一个需要可信用户的能力
	reg := engine.Plugins()
	reg.Register(&rolePlugin{recordingPlugin: rec, minRole: store.RoleTrusted})

	resp, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "guest1", CallerID: "guest1",
		Role: store.RoleGuest, UserText: "查磁盘",
	})
	if err != nil {
		t.Fatalf("引擎执行失败: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatal("访客不应能执行需要可信用户档位的能力")
	}
	// 失败原因应当以可读形式回传，而不是静默无反应
	if len(resp.Extra) == 0 || !strings.Contains(strings.Join(resp.Extra, " "), "权限") {
		t.Fatalf("权限拒绝未回传原因: %+v", resp.Extra)
	}
}

// rolePlugin 是带权限要求的测试能力。
type rolePlugin struct {
	*recordingPlugin
	minRole store.Role
}

func (p *rolePlugin) MinRole() store.Role { return p.minRole }

// ─── 流式：门控必须挡住碎片化的标记 ──────────────────────────────────────────

func TestEngineStreamingHidesMarker(t *testing.T) {
	rec := &recordingPlugin{name: "disk", reply: "磁盘已用 42%"}
	// 假端点逐字符下发，标记会被彻底切碎
	srv := fakeLLM(t, "查一下[能力:disk path=/tmp]结果出来了", true)
	engine, _ := buildEngine(t, srv.URL, rec)

	var visible strings.Builder
	resp, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleOwner, UserText: "磁盘",
		OnDelta: func(d string) { visible.WriteString(d) },
	})
	if err != nil {
		t.Fatalf("流式执行失败: %v", err)
	}

	got := visible.String()
	if strings.Contains(got, "能力") || strings.Contains(got, "disk") {
		t.Fatalf("流式输出泄露了内部标记: %q", got)
	}
	if !strings.Contains(got, "查一下") || !strings.Contains(got, "结果出来了") {
		t.Fatalf("正文被门控误吞: %q", got)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("流式路径下能力被调用 %d 次, 期望 1", len(rec.calls))
	}
	if !resp.Streamed {
		t.Fatal("流式路径应标记 Streamed")
	}
	if resp.Text != "" {
		t.Fatalf("流式路径不该再返回正文（会重复发送）: %q", resp.Text)
	}
}

// TestEngineStreamingUnclosedMarkerKept 验证被截断的标记不丢字。
func TestEngineStreamingUnclosedMarkerKept(t *testing.T) {
	rec := &recordingPlugin{name: "disk", reply: "x"}
	// 模型写了半个标记就结束（被 max_tokens 截断的典型形态）
	srv := fakeLLM(t, "我看看[能力:dis", true)
	engine, _ := buildEngine(t, srv.URL, rec)

	var visible strings.Builder
	if _, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleOwner, UserText: "查",
		OnDelta: func(d string) { visible.WriteString(d) },
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if len(rec.calls) != 0 {
		t.Fatal("未闭合的标记不该触发能力调用")
	}
	if !strings.Contains(visible.String(), "能力") {
		t.Fatalf("被截断的内容被静默吞掉了: %q", visible.String())
	}
}

// TestEngineProviderError 验证模型层错误被如实上抛（由上层翻译成人话）。
func TestEngineProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	engine, _ := buildEngine(t, srv.URL)
	_, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleGuest, UserText: "在吗",
	})
	if err == nil {
		t.Fatal("上游 5xx 应当报错")
	}
}

// TestEngineMemoryDisabled 验证人格关掉记忆时不做检索。
func TestEngineMemoryDisabled(t *testing.T) {
	srv := fakeLLM(t, "好的", false)
	engine, _ := buildEngine(t, srv.URL)

	resp, err := engine.Run(context.Background(), Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleGuest, UserText: "你好",
		// 记忆系统本身未启用（NewManager 传了 nil db），这里再显式关一次
		DisableMemory: true,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if resp.Memories != 0 {
		t.Fatalf("记忆被禁用时不应注入任何记忆，实际 %d 条", resp.Memories)
	}
}

// TestEngineTimeout 验证上游超时不会永久挂住调用方。
func TestEngineTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"慢"}}]}`))
	}))
	defer srv.Close()

	engine, _ := buildEngine(t, srv.URL)

	// 用短超时的 context 模拟调用方主动放弃
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := engine.Run(ctx, Request{
		Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1",
		Role: store.RoleGuest, UserText: "在吗",
	})
	if err == nil {
		t.Fatal("上下文超时应当返回错误，而不是永久阻塞")
	}
}
