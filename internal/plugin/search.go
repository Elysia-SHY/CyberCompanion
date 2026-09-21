package plugin

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cybercompanion/internal/store"
)

// SearchResult 是一条搜索结果。
type SearchResult struct {
	Title   string
	Snippet string
	URL     string
}

// SearchPlugin 提供免 Key 互联网实时检索能力。
type SearchPlugin struct {
	client *http.Client
}

// NewSearchPlugin 构造搜索插件。
func NewSearchPlugin() *SearchPlugin {
	return &SearchPlugin{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		},
	}
}

func (s *SearchPlugin) Name() string { return "search" }

func (s *SearchPlugin) Description() string {
	return "联网搜索互联网公开信息、实时热点、动漫影视、百科知识与最新动态"
}

func (s *SearchPlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "query", Description: "搜索关键词", Required: true},
		},
		Examples: []string{"搜索 超时空辉夜姬", "查一下今天的热点新闻", "搜搜xxx上映时间"},
	}
}

func (s *SearchPlugin) MinRole() store.Role { return store.RoleGuest }
func (s *SearchPlugin) Permission() string  { return "" }

func (s *SearchPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	query := strings.TrimSpace(args["query"])
	if query == "" {
		return Result{Text: "请告诉我你想搜索什么内容哦～", Handled: true}
	}

	results, err := s.Search(ctx, query, 3)
	if err != nil {
		return Result{Error: fmt.Errorf("联网搜索失败: %v", err)}
	}
	if len(results) == 0 {
		return Result{Text: fmt.Sprintf("在网上没有找到关于「%s」的内容呢喵～", query), Handled: true}
	}

	formatted := FormatSearchResults(query, results)
	return Result{Text: formatted}
}

// Search 顺序尝试 Bing 与 DuckDuckGo HTML 搜索
func (s *SearchPlugin) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}

	// 1. 优先尝试 Bing 搜索
	results, err := s.searchBing(ctx, query, maxResults)
	if err == nil && len(results) > 0 {
		return results, nil
	}

	// 2. 备用尝试 DuckDuckGo Lite
	ddgResults, ddgErr := s.searchDDG(ctx, query, maxResults)
	if ddgErr == nil && len(ddgResults) > 0 {
		return ddgResults, nil
	}

	if err != nil {
		return nil, err
	}
	return nil, ddgErr
}

var (
	reHTMLTag     = regexp.MustCompile(`<[^>]*>`)
	reBingAlgo    = regexp.MustCompile(`(?s)<li class="b_algo"[^>]*>(.*?)</li>`)
	reBingTitle   = regexp.MustCompile(`(?s)<h2[^>]*><a[^>]*>(.*?)</a></h2>`)
	reBingURL     = regexp.MustCompile(`href="([^"]+)"`)
	reBingSnippet = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)

	reDDGSnippet = regexp.MustCompile(`(?s)<a class="result__snippet[^>]*>(.*?)</a>`)
	reDDGTitle   = regexp.MustCompile(`(?s)<a class="result__url[^>]*>(.*?)</a>`)
)

func cleanHTML(s string) string {
	s = reHTMLTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.TrimSpace(s)
}

func (s *SearchPlugin) searchBing(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	u := "https://cn.bing.com/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	htmlStr := string(body)

	matches := reBingAlgo.FindAllStringSubmatch(htmlStr, maxResults*2)
	var list []SearchResult
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		item := m[1]
		tm := reBingTitle.FindStringSubmatch(item)
		if len(tm) < 2 {
			continue
		}
		title := cleanHTML(tm[1])

		sm := reBingSnippet.FindStringSubmatch(item)
		snippet := ""
		if len(sm) >= 2 {
			snippet = cleanHTML(sm[1])
		}

		link := ""
		lm := reBingURL.FindStringSubmatch(tm[0])
		if len(lm) >= 2 {
			link = lm[1]
		}

		if title != "" && snippet != "" {
			list = append(list, SearchResult{
				Title:   title,
				Snippet: snippet,
				URL:     link,
			})
			if len(list) >= maxResults {
				break
			}
		}
	}
	return list, nil
}

func (s *SearchPlugin) searchDDG(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	htmlStr := string(body)

	snippets := reDDGSnippet.FindAllStringSubmatch(htmlStr, maxResults)
	var list []SearchResult
	for i, sm := range snippets {
		if len(sm) < 2 {
			continue
		}
		snip := cleanHTML(sm[1])
		if snip != "" {
			list = append(list, SearchResult{
				Title:   fmt.Sprintf("结果 #%d", i+1),
				Snippet: snip,
			})
			if len(list) >= maxResults {
				break
			}
		}
	}
	return list, nil
}

// FormatSearchResults 把结果格式化为易读的文本
func FormatSearchResults(query string, results []SearchResult) string {
	if len(results) == 0 {
		return "（未找到相关内容）"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🔍 搜索结果【%s】：\n", query))
	for i, r := range results {
		b.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.Snippet))
	}
	return strings.TrimRight(b.String(), "\n")
}
