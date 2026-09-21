package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"cybercompanion/internal/store"
)

func TestParseRemindTime(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.Local)

	cases := []struct {
		input       string
		expectedSub time.Duration
		isTomorrow  bool
	}{
		{"10m", 10 * time.Minute, false},
		{"10分钟", 10 * time.Minute, false},
		{"10分钟后", 10 * time.Minute, false},
		{"半小时", 30 * time.Minute, false},
		{"半小时后", 30 * time.Minute, false},
		{"1小时", 1 * time.Hour, false},
		{"一小时后", 1 * time.Hour, false},
		{"两小时后", 2 * time.Hour, false},
		{"1个半小时", 90 * time.Minute, false},
		{"30秒", 30 * time.Second, false},
		{"30s", 30 * time.Second, false},
		{"10:30", 30 * time.Minute, false}, // 10:00 -> 10:30
	}

	for _, c := range cases {
		tgt, desc, err := ParseRemindTime(c.input, now)
		if err != nil {
			t.Errorf("ParseRemindTime(%q) 出错: %v", c.input, err)
			continue
		}
		if diff := tgt.Sub(now); diff != c.expectedSub {
			t.Errorf("ParseRemindTime(%q) diff = %v, 期望 %v (desc=%s)", c.input, diff, c.expectedSub, desc)
		}
	}
}

func TestRemindPluginExecution(t *testing.T) {
	// 使用临时目录数据库测试
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	defer db.Close()

	p := NewRemindPlugin(db)
	ec := &ExecContext{
		Scope:    store.ScopePrivate,
		OwnerID:  "u123",
		CallerID: "u123",
		Role:     store.RoleOwner,
		DB:       db,
	}

	// 1. 设置一条 10 分钟后的提醒
	res := p.Execute(context.Background(), map[string]string{
		"time":    "10m",
		"content": "喝水",
	}, ec)

	if res.Error != nil {
		t.Fatalf("执行失败: %v", res.Error)
	}
	if !strings.Contains(res.Text, "已经为你设好提醒啦") || !strings.Contains(res.Text, "喝水") {
		t.Fatalf("设置提醒返回文本不符: %s", res.Text)
	}

	// 2. 查看提醒列表
	resList := p.Execute(context.Background(), map[string]string{
		"action": "list",
	}, ec)
	if resList.Error != nil {
		t.Fatalf("查询失败: %v", resList.Error)
	}
	if !strings.Contains(resList.Text, "待提醒任务列表") || !strings.Contains(resList.Text, "喝水") {
		t.Fatalf("列表文本不符: %s", resList.Text)
	}

	// 3. 取消提醒
	resCancel := p.Execute(context.Background(), map[string]string{
		"action": "cancel",
	}, ec)
	if resCancel.Error != nil {
		t.Fatalf("取消失败: %v", resCancel.Error)
	}
	if !strings.Contains(resCancel.Text, "已成功取消") {
		t.Fatalf("取消文本不符: %s", resCancel.Text)
	}

	// 4. 再次查看提醒列表（应为空）
	resListAfter := p.Execute(context.Background(), map[string]string{
		"action": "list",
	}, ec)
	if !strings.Contains(resListAfter.Text, "没有正在等待中的提醒任务") {
		t.Fatalf("取消后列表应为空: %s", resListAfter.Text)
	}
}
