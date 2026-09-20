package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cybercompanion/internal/config"
)

// maxLineBytes 是单行消息的长度上限。
//
// MCP 的 stdio 传输是「一行一条 JSON」。默认 64KB 的 scanner 缓冲在遇到
// 大结果（比如列出一棵目录树）时会直接报错中断连接，因此放大到 4MB。
const maxLineBytes = 4 << 20

// stderrKeepLines 是转发的 stderr 行数上限，防止一个话痨服务刷爆日志。
const stderrKeepLines = 50

// Client 是一个已连接的 MCP 服务进程。
type Client struct {
	name string
	spec config.MCPServerSpec

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	initTimeout time.Duration
	callTimeout time.Duration

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse

	serverInfo initializeResult

	closed atomic.Bool
	wg     sync.WaitGroup

	// stderrTail 保留服务端最后若干行 stderr，供连接失败时给出可诊断的信息
	stderrMu   sync.Mutex
	stderrTail []string
}

// Connect 启动并握手一个 MCP 服务。
//
// 任何一步失败都会回收已启动的进程：留下一个僵尸服务进程比连接失败本身
// 更麻烦 —— 用户看不到它，却一直在占内存。
func Connect(ctx context.Context, spec config.MCPServerSpec, initTimeout, callTimeout time.Duration) (*Client, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("MCP 服务 %q 未配置 command", spec.Name)
	}
	if initTimeout <= 0 {
		initTimeout = 20 * time.Second
	}
	if callTimeout <= 0 {
		callTimeout = 45 * time.Second
	}

	c := &Client{
		name:        spec.Name,
		spec:        spec,
		initTimeout: initTimeout,
		callTimeout: callTimeout,
		pending:     map[int64]chan rpcResponse{},
	}

	if err := c.start(); err != nil {
		return nil, err
	}

	// 握手必须在超时保护下进行：一个卡在启动阶段的服务会拖住整个程序启动
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	if err := c.initialize(initCtx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("与 MCP 服务 %q 握手失败: %w%s", spec.Name, err, c.stderrHint())
	}
	return c, nil
}

// start 拉起子进程并接好管道与读取循环。
func (c *Client) start() error {
	cmd := exec.Command(c.spec.Command, c.spec.Args...)
	if len(c.spec.Env) > 0 {
		// 追加而不是替换：MCP 服务通常需要继承 PATH 等基础环境
		cmd.Env = append(cmd.Environ(), c.spec.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 MCP 服务进程失败: %w", err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewScanner(stdout)
	c.stdout.Buffer(make([]byte, 64*1024), maxLineBytes)

	// stderr 必须被消费：管道写满会让子进程阻塞在写日志上，
	// 表现出来就是「服务莫名其妙卡住不回消息」。
	c.wg.Add(1)
	go c.drainStderr(stderr)

	c.wg.Add(1)
	go c.readLoop()

	// 进程退出时唤醒全部等待者，避免调用方永久阻塞
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = cmd.Wait()
		c.failAllPending(fmt.Errorf("MCP 服务 %q 已退出", c.name))
	}()

	return nil
}

// drainStderr 读取并保留服务端 stderr。
func (c *Client) drainStderr(r io.Reader) {
	defer c.wg.Done()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.stderrMu.Lock()
		c.stderrTail = append(c.stderrTail, line)
		if len(c.stderrTail) > stderrKeepLines {
			c.stderrTail = c.stderrTail[len(c.stderrTail)-stderrKeepLines:]
		}
		c.stderrMu.Unlock()
	}
}

// stderrHint 返回服务端 stderr 的尾部，用于失败诊断。
func (c *Client) stderrHint() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	if len(c.stderrTail) == 0 {
		return ""
	}
	return "\n服务端输出：\n  " + strings.Join(c.stderrTail, "\n  ")
}

// readLoop 逐行读取服务端消息并按 id 分派。
func (c *Client) readLoop() {
	defer c.wg.Done()
	defer c.failAllPending(fmt.Errorf("MCP 服务 %q 连接已关闭", c.name))

	for c.stdout.Scan() {
		line := strings.TrimSpace(c.stdout.Text())
		if line == "" {
			continue
		}

		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			// 单行损坏不该中断整条连接：服务端可能往 stdout 打了非协议内容
			continue
		}

		// 无 id 的是通知（例如日志、进度、resources 变更），当前不消费
		if resp.ID == 0 {
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()

		if ok {
			ch <- resp
		}
		// 找不到等待者的应答直接丢弃：多半是超时后迟到的回复
	}
}

// failAllPending 让所有在途请求立即失败。
func (c *Client) failAllPending(err error) {
	c.mu.Lock()
	waiters := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()

	for _, ch := range waiters {
		select {
		case ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
			// 通道已满说明对方已经在处理别的结果，无需再塞
		}
	}
}

// call 发送一次请求并等待应答。
func (c *Client) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	if c.closed.Load() {
		return fmt.Errorf("MCP 服务 %q 连接已关闭", c.name)
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	// 清理：无论成功失败都要把等待者摘掉，否则 pending 会随超时请求持续增长
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	// stdio 传输规范：一条消息一行，以换行分隔
	payload = append(payload, '\n')

	c.mu.Lock()
	_, werr := c.stdin.Write(payload)
	c.mu.Unlock()
	if werr != nil {
		return fmt.Errorf("写入 MCP 服务失败: %w", werr)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("调用 %s 超时: %w", method, ctx.Err())
	case resp := <-ch:
		if resp.Error != nil {
			return errors.New(resp.Error.Error())
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("解析 %s 结果失败: %w", method, err)
		}
		return nil
	}
}

// notify 发送一次不需要回复的通知。
func (c *Client) notify(method string, params interface{}) error {
	if c.closed.Load() {
		return fmt.Errorf("连接已关闭")
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.stdin.Write(payload)
	return err
}

// initialize 完成 MCP 握手。
func (c *Client) initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		// 只声明用得到的客户端能力。多声明一个能力就意味着服务端可能
		// 主动发来一类我们并不处理的消息。
		Capabilities: map[string]any{},
		ClientInfo:   clientInfo{Name: "CyberCompanion", Version: "1.0"},
	}

	var result initializeResult
	if err := c.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	c.serverInfo = result

	// 规范要求握手后发这条通知，服务端据此才认为连接就绪
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("发送 initialized 通知失败: %w", err)
	}
	return nil
}

// ServerInfo 返回握手得到的信息，用于日志展示。
func (c *Client) ServerInfo() (name, version, protocol string) {
	return c.serverInfo.ServerInfo.Name, c.serverInfo.ServerInfo.Version, c.serverInfo.ProtocolVersion
}

// SupportsTools 报告服务端是否提供工具。
func (c *Client) SupportsTools() bool { return c.serverInfo.SupportsTools() }

// Tools 拉取服务端暴露的全部工具（自动跟随分页游标）。
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var (
		all    []Tool
		cursor string
	)

	// 循环上限是防御性的：一个游标实现有 bug 的服务端会让这里无限转下去
	for page := 0; page < 10; page++ {
		params := map[string]interface{}{}
		if cursor != "" {
			params["cursor"] = cursor
		}

		var result toolsListResult
		if err := c.call(ctx, "tools/list", params, &result); err != nil {
			return all, err
		}
		all = append(all, result.Tools...)

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// Call 调用一个工具。
func (c *Client) Call(ctx context.Context, name string, args map[string]string) (callToolResult, error) {
	var result callToolResult
	params := callToolParams{Name: name, Arguments: args}
	err := c.call(ctx, "tools/call", params, &result)
	return result, err
}

// Timeout 返回该服务的调用超时。
func (c *Client) Timeout() time.Duration { return c.callTimeout }

// Close 关闭连接并回收子进程。
//
// 先关 stdin 让服务端有机会优雅退出，超时后才强杀：直接 Kill 会让
// 有状态的服务来不及落盘。
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-done
	}
	return nil
}
