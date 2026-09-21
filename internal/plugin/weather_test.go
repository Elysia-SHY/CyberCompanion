package plugin

import (
	"context"
	"strings"
	"testing"
)

func TestWeatherPlugin(t *testing.T) {
	wp := NewWeatherPlugin(nil)
	res := wp.Execute(context.Background(), map[string]string{
		"city": "合肥",
	}, &ExecContext{})

	if res.Error != nil {
		t.Fatalf("天气查询失败: %v", res.Error)
	}
	if strings.TrimSpace(res.Text) == "" {
		t.Fatal("天气查询返回空文本")
	}
	t.Logf("天气结果: %s", res.Text)
}
