package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/store"
)

// ─── 测试用 MCP 服务 ────────────────────────────────────────────────────────
//
// 测试里直接复用测试二进制自身作为 MCP 服务进程（Go 的标准做法）：
// 通过环境变量切到「服务端模式」，然后说一遍真实的 JSON-RPC 协议。
//
// 这样验证的是完整链路 —— 进程启动、管道、逐行 JSON、id 分派、握手、
// 工具清单、工具调用 —— 而不是把协议实现替换成桩函数后自说自话。
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVER") == "1" {
		runFakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeServer 实现一个最小但真实的 MCP 服务端。
func runFakeServer() {
	// 便于测试「服务端 stderr 能被转发并用于诊断」
	fmt.Fprintln(os.Stderr, "fake-mcp-server started")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	reply := func(id int64, result interface{}) {
		payload, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": result,
		})
		writer.Write(payload)
		writer.WriteByte('\n')
		writer.Flush()
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int64           `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]interface{}{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{"listChanged": true}},
				"serverInfo":      map[string]interface{}{"name": "fake-server", "version": "9.9.9"},
			})

		case "notifications/initialized":
			// 通知无需回复

		case "tools/list":
			reply(req.ID, map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "echo",
						"description": "原样返回输入文本",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"text": map[string]interface{}{"type": "string", "description": "要回显的文本"},
							},
							"required": []string{"text"},
						},
					},
					{
						"name":        "boom",
						"description": "总是报错，用于测试错误路径",
						"inputSchema": map[string]interface{}{"type": "object"},
					},
				},
			})

		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)

			switch params.Name {
			case "echo":
				reply(req.ID, map[string]interface{}{
					"content": []map[string]string{
						{"type": "text", "text": "echo: " + params.Arguments["text"]},
					},
				})
			case "boom":
				// 协议层错误：走 JSON-RPC 的 error 字段
				payload, _ := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]interface{}{"code": -32603, "message": "内部错误，故意的"},
				})
				writer.Write(payload)
				writer.WriteByte('\n')
				writer.Flush()
			default:
				reply(req.ID, map[string]interface{}{
					"content": []map[string]string{{"type": "text", "text": "未知工具"}},
					"isError": true,
				})
			}
		}
	}
}

// testSpec 返回指向本测试二进制的服务配置。
func testSpec(name string, extraArgs ...string) config.MCPServerSpec {
	return config.MCPServerSpec{
		Name:    name,
		Command: os.Args[0],
		Args:    append([]string{"-test.run=TestMain"}, extraArgs...),
		Env:     []string{"MCP_TEST_SERVER=1"},
	}
}

// ─── 连接与握手 ──────────────────────────────────────────────────────────────

func TestConnectAndHandshake(t *testing.T) {
	ctx := context.Background()
	client, err := Connect(ctx, testSpec("fake"), 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer client.Close()

	name, version, protocol := client.ServerInfo()
	if name != "fake-server" || version != "9.9.9" {
		t.Fatalf("服务信息 = %s/%s, 期望 fake-server/9.9.9", name, version)
	}
	if protocol != ProtocolVersion {
		t.Fatalf("协议版本 = %s, 期望 %s", protocol, ProtocolVersion)
	}
	if !client.SupportsTools() {
		t.Fatal("服务声明了 tools 能力，却判定为不支持")
	}
}

func TestConnectCommandNotFound(t *testing.T) {
	ctx := context.Background()
	_, err := Connect(ctx, config.MCPServerSpec{Name: "missing", Command: "/nonexistent/binary-xyz"}, time.Second, time.Second)
	if err == nil {
		t.Fatal("启动不存在的命令应当失败")
	}
}

func TestConnectEmptyCommand(t *testing.T) {
	_, err := Connect(context.Background(), config.MCPServerSpec{Name: "empty"}, time.Second, time.Second)
	if err == nil {
		t.Fatal("未配置 command 应当失败")
	}
}

// ─── 工具清单与调用 ──────────────────────────────────────────────────────────

func TestListTools(t *testing.T) {
	ctx := context.Background()
	client, err := Connect(ctx, testSpec("fake"), 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer client.Close()

	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("拉取工具失败: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d, 期望 2", len(tools))
	}
	if tools[0].Name != "echo" && tools[1].Name != "echo" {
		t.Fatalf("未找到 echo 工具: %+v", tools)
	}
}

func TestCallTool(t *testing.T) {
	ctx := context.Background()
	client, err := Connect(ctx, testSpec("fake"), 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer client.Close()

	result, err := client.Call(ctx, "echo", map[string]string{"text": "你好"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if result.Text() != "echo: 你好" {
		t.Fatalf("返回 = %q, 期望 %q", result.Text(), "echo: 你好")
	}
	if result.IsError {
		t.Fatal("成功的调用不应标记为错误")
	}
}

// TestCallToolServerError 验证协议层错误被如实上报。
func TestCallToolServerError(t *testing.T) {
	ctx := context.Background()
	client, err := Connect(ctx, testSpec("fake"), 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer client.Close()

	_, err = client.Call(ctx, "boom", nil)
	if err == nil {
		t.Fatal("服务端返回 error 时应当报错")
	}
	if !strings.Contains(err.Error(), "内部错误") {
		t.Fatalf("错误信息未透传: %v", err)
	}
}

// TestToolResultIsError 验证 isError 标记被识别。
func TestToolResultIsError(t *testing.T) {
	ctx := context.Background()
	client, err := Connect(ctx, testSpec("fake"), 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer client.Close()

	result, err := client.Call(ctx, "unknown_tool", nil)
	if err != nil {
		t.Fatalf("协议层不应报错: %v", err)
	}
	if !result.IsError {
		t.Fatal("服务端标记 isError 时应当被识别")
	}
}

// ─── 插件桥接 ────────────────────────────────────────────────────────────────

func TestToolPluginRegistration(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)

	mgr := NewManager(nil)
	defer mgr.Close()

	n := mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg)
	if n != 1 {
		t.Fatalf("接入服务数 = %d, 期望 1", n)
	}

	names := reg.Names()
	found := false
	for _, name := range names {
		if name == "fake_echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未注册 fake_echo，已注册: %v", names)
	}

	// 工具名必须能被 [能力:名称 参数=值] 标记安全使用
	for _, name := range names {
		if strings.ContainsAny(name, " :[]") {
			t.Fatalf("插件名 %q 含有会破坏标记解析的字符", name)
		}
	}
}

func TestToolPluginExecute(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)
	mgr := NewManager(nil)
	defer mgr.Close()

	if n := mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg); n != 1 {
		t.Fatal("接入失败")
	}

	// 用主人身份调用：默认档位是 trusted，访客应当被挡下
	ec := &plugin.ExecContext{Scope: store.ScopePrivate, OwnerID: "u1", CallerID: "u1", Role: store.RoleOwner}
	res := reg.Call(ctx, "fake_echo", map[string]string{"text": "hello"}, ec)
	if res.Error != nil {
		t.Fatalf("调用失败: %v", res.Error)
	}
	if res.Text != "echo: hello" {
		t.Fatalf("返回 = %q", res.Text)
	}
	if !res.Handled {
		t.Fatal("外部能力的结果应直接返回，不该再交给模型复述")
	}
}

// TestMCPToolsRespectPermission 验证外部能力受权限约束。
//
// MCP 工具执行的是别人写的代码，默认档位是 trusted；
// 访客不该能通过「模型提到了这个名字」就把它调起来。
func TestMCPToolsRespectPermission(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)
	mgr := NewManager(nil)
	defer mgr.Close()

	if n := mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg); n != 1 {
		t.Fatal("接入失败")
	}

	guest := &plugin.ExecContext{Scope: store.ScopePrivate, OwnerID: "u2", CallerID: "u2", Role: store.RoleGuest}
	res := reg.Call(ctx, "fake_echo", map[string]string{"text": "hi"}, guest)
	if res.Error == nil {
		t.Fatal("访客调用 MCP 工具应当被拒绝")
	}

	// 提示词里也不该出现没有权限的能力
	hint := reg.PromptHint(guest)
	if strings.Contains(hint, "fake_echo") {
		t.Fatal("无权限的能力不应出现在模型的能力清单里")
	}
}

// TestMCPDisabledByDefault 验证 MCP 默认关闭。
//
// 与记忆、调度不同：MCP 会启动外部进程，属于必须由用户明确开启的能力。
func TestMCPDisabledByDefault(t *testing.T) {
	reg := plugin.NewRegistry(nil)
	mgr := NewManager(nil)

	if n := mgr.StartAll(context.Background(), config.MCPSettings{
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg); n != 0 {
		t.Fatal("未显式启用时不该接入任何服务")
	}
	if len(reg.Names()) != 0 {
		t.Fatal("未启用时不该注册任何插件")
	}
}

// TestMCPDoesNotShadowBuiltin 验证外部服务不能顶掉内置能力。
func TestMCPDoesNotShadowBuiltin(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)

	// 先注册一个与外部工具同名的内置能力
	builtin := &stubPlugin{name: "fake_echo", desc: "内置能力"}
	reg.Register(builtin)

	mgr := NewManager(nil)
	defer mgr.Close()
	mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg)

	got, ok := reg.Get("fake_echo")
	if !ok {
		t.Fatal("能力丢失")
	}
	if got.Description() != "内置能力" {
		t.Fatal("内置能力被外部服务顶掉了")
	}
}

// TestMCPStatusReported 验证状态可被面板读取。
func TestMCPStatusReported(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)
	mgr := NewManager(nil)
	defer mgr.Close()

	mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{testSpec("fake")},
	}, reg)

	status := mgr.Status()
	if len(status) != 1 {
		t.Fatalf("状态条目 = %d, 期望 1", len(status))
	}
	if !status[0].Connected || status[0].ToolCount != 2 {
		t.Fatalf("状态异常: %+v", status[0])
	}
	if status[0].ServerName != "fake-server" {
		t.Fatalf("服务名 = %q", status[0].ServerName)
	}

	servers, tools := mgr.Count()
	if servers != 1 || tools != 2 {
		t.Fatalf("计数 = %d 服务 / %d 工具, 期望 1/2", servers, tools)
	}
}

func TestMCPFailureDoesNotAbortOthers(t *testing.T) {
	ctx := context.Background()
	reg := plugin.NewRegistry(nil)
	mgr := NewManager(nil)
	defer mgr.Close()

	n := mgr.StartAll(ctx, config.MCPSettings{
		Enabled: boolPtr(true),
		Servers: []config.MCPServerSpec{
			{Name: "broken", Command: "/nonexistent/binary-xyz"},
			testSpec("fake"),
		},
	}, reg)

	if n != 1 {
		t.Fatalf("接入数 = %d, 期望 1（坏服务不该影响好服务）", n)
	}
	status := mgr.Status()
	if len(status) != 2 {
		t.Fatalf("状态条目 = %d, 期望 2（含失败项）", len(status))
	}
	// 失败项要留下可诊断的错误信息
	for _, st := range status {
		if st.Name == "broken" && (st.Connected || st.Error == "") {
			t.Fatalf("失败服务应有错误信息: %+v", st)
		}
	}
}

// ─── 纯函数 ──────────────────────────────────────────────────────────────────

func TestSanitizeToolName(t *testing.T) {
	cases := []struct {
		prefix, tool, want string
	}{
		{"fs", "read_file", "fs_read_file"},
		{"fs", "read file", "fs_read_file"},
		{"fs", "read:file", "fs_read_file"},
		{"fs", "Read.File", "fs_read_file"},
		{"My Server", "list", "myserver_list"},
		{"", "list", "list"},
		{"fs", "!!!", "fs_tool"},
	}
	for _, c := range cases {
		got := sanitizeToolName(c.prefix, c.tool)
		if got != c.want {
			t.Errorf("sanitizeToolName(%q,%q) = %q, 期望 %q", c.prefix, c.tool, got, c.want)
		}
	}
}

func TestSchemaParams(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string","description":"搜索词"},
			"limit":{"type":"integer"}
		},
		"required":["query"]
	}`)

	params := schemaParams(raw)
	if len(params) != 2 {
		t.Fatalf("参数数 = %d, 期望 2", len(params))
	}
	// 必须按名字排序，否则提示词每次都不同，影响模型选择的稳定性
	if params[0].Name != "limit" || params[1].Name != "query" {
		t.Fatalf("参数顺序不稳定: %+v", params)
	}
	if !params[1].Required {
		t.Fatal("query 应为必填")
	}
	if params[1].Description != "搜索词" {
		t.Fatalf("描述 = %q", params[1].Description)
	}

	// schema 损坏时返回空而不是 panic
	if got := schemaParams(json.RawMessage(`{{{`)); got != nil {
		t.Fatalf("损坏的 schema 应返回 nil，得到 %+v", got)
	}
	if got := schemaParams(nil); got != nil {
		t.Fatal("空 schema 应返回 nil")
	}
}

func TestCallToolResultText(t *testing.T) {
	r := callToolResult{Content: []contentItem{
		{Type: "text", Text: "第一段"},
		{Type: "text", Text: "第二段"},
		{Type: "image"}, // 不支持的类型要明确说明，而不是静默丢弃
	}}
	got := r.Text()
	if !strings.Contains(got, "第一段") || !strings.Contains(got, "第二段") {
		t.Fatalf("文本拼接异常: %q", got)
	}
	if !strings.Contains(got, "image") {
		t.Fatalf("不支持的内容类型应给出说明: %q", got)
	}
}

// ─── 辅助 ────────────────────────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

// stubPlugin 是一个仅用于重名测试的最小插件。
type stubPlugin struct {
	name string
	desc string
}

func (p *stubPlugin) Name() string          { return p.name }
func (p *stubPlugin) Description() string   { return p.desc }
func (p *stubPlugin) Schema() plugin.Schema { return plugin.Schema{} }
func (p *stubPlugin) MinRole() store.Role   { return store.RoleGuest }
func (p *stubPlugin) Permission() string    { return "" }
func (p *stubPlugin) Execute(context.Context, map[string]string, *plugin.ExecContext) plugin.Result {
	return plugin.Result{Handled: true}
}
