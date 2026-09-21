package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cybercompanion/internal/store"
)

// WeatherPlugin 提供免 Key 实时天气查询能力。
type WeatherPlugin struct {
	client *http.Client
	search *SearchPlugin
}

// NewWeatherPlugin 构造天气插件。
func NewWeatherPlugin(search *SearchPlugin) *WeatherPlugin {
	if search == nil {
		search = NewSearchPlugin()
	}
	return &WeatherPlugin{
		client: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		},
		search: search,
	}
}

func (w *WeatherPlugin) Name() string { return "weather" }

func (w *WeatherPlugin) Description() string {
	return "查询指定城市或地区的实时天气状况与温度预报（如：合肥天气、北京明天天气）"
}

func (w *WeatherPlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "city", Description: "要查询的城市或区县名称（如：合肥、北京、上海、广州等）", Required: true},
			{Name: "when", Description: "查询的时间，如 '今天'、'明天'、'后天'（默认今天）", Required: false},
		},
		Examples: []string{
			"合肥今天天气怎么样 -> [能力:weather city=合肥 when=今天]",
			"北京天气 -> [能力:weather city=北京]",
		},
	}
}

func (w *WeatherPlugin) MinRole() store.Role { return store.RoleGuest }
func (w *WeatherPlugin) Permission() string  { return "" }

func (w *WeatherPlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	city := strings.TrimSpace(args["city"])
	when := strings.TrimSpace(args["when"])
	if when == "" {
		when = "今天"
	}
	if city == "" {
		city = "本地"
	}

	// 1. 优先尝试公共轻量天气接口 wttr.in
	if res, err := w.queryWttr(ctx, city, when); err == nil && res != "" {
		return Result{Text: res, Handled: true}
	}

	// 2. 备用降级方案：调用免 Key 联网搜索聚合天气信息
	searchQuery := fmt.Sprintf("%s %s 天气", city, when)
	results, err := w.search.Search(ctx, searchQuery, 2)
	if err == nil && len(results) > 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("【%s天气检索】：\n", city))
		for _, r := range results {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", r.Title, r.Snippet))
		}
		return Result{Text: sb.String(), Handled: true}
	}

	return Result{Text: fmt.Sprintf("暂未获取到%s的天气信息呢喵~", city), Handled: true}
}

func (w *WeatherPlugin) queryWttr(ctx context.Context, city, when string) (string, error) {
	// 如果是查明天，wttr.in/city 支持详细或者 format
	cleanCity := strings.TrimSuffix(city, "市")
	cleanCity = strings.TrimSuffix(cleanCity, "区")
	cleanCity = strings.TrimSuffix(cleanCity, "县")
	if cleanCity == "本地" {
		cleanCity = ""
	}

	endpoint := fmt.Sprintf("https://wttr.in/%s?format=%%l:+%%c+%%C+气温%%t+湿度%%h+风速%%w&m&lang=zh", url.PathEscape(cleanCity))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8.0")

	resp, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(body))
	if strings.Contains(text, "<html>") || strings.Contains(text, "<title>") || len(text) < 3 {
		return "", fmt.Errorf("返回非有效文本天气")
	}

	return fmt.Sprintf("【%s实时天气】：%s", city, text), nil
}
