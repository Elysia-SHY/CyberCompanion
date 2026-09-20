package scheduler

import (
	"testing"
	"time"
)

// TestNextRunExpressions 覆盖全部支持的时间表达式。
func TestNextRunExpressions(t *testing.T) {
	base := time.Date(2026, 3, 10, 7, 30, 0, 0, time.Local)

	cases := []struct {
		expr string
		want time.Time
	}{
		// 空表达式是一次性任务
		{"", time.Time{}},
		{"every 15m", base.Add(15 * time.Minute)},
		{"every 2h", base.Add(2 * time.Hour)},
		{"every 30s", base.Add(time.Minute)}, // 下限被抬到 1 分钟，避免刷屏
		{"hourly", time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)},
		// 今天 08:00 还没到，应落在今天
		{"daily 08:00", time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)},
		// 今天 06:00 已经过了，应落在明天
		{"daily 06:00", time.Date(2026, 3, 11, 6, 0, 0, 0, time.Local)},
		{"daily 7:30", time.Date(2026, 3, 11, 7, 30, 0, 0, time.Local)}, // 恰好等于当前时刻 → 顺延到明天
	}

	for _, c := range cases {
		got := NextRun(c.expr, base)
		if !got.Equal(c.want) {
			t.Errorf("NextRun(%q) = %v, 期望 %v", c.expr, got, c.want)
		}
	}
}

// TestNextRunInvalid 验证非法表达式返回零值而不是 panic 或乱算。
func TestNextRunInvalid(t *testing.T) {
	base := time.Now()
	for _, expr := range []string{"daily 25:00", "daily 8", "every", "every abc", "cron * * * *", "随意写的"} {
		if got := NextRun(expr, base); !got.IsZero() {
			t.Errorf("非法表达式 %q 返回了 %v, 期望零值", expr, got)
		}
	}
}

// TestQuietHoursAcrossMidnight 是这套逻辑里最容易写错的地方。
//
// 22:00 - 08:00 这样的跨零点时段，若用 start <= hour < end 直接比较，
// 23 点会被判定为「不在免打扰内」—— 恰恰是最需要静音的时刻。
func TestQuietHoursAcrossMidnight(t *testing.T) {
	at := func(hour int) time.Time {
		return time.Date(2026, 3, 10, hour, 0, 0, 0, time.Local)
	}

	// 22:00 - 08:00
	for _, h := range []int{22, 23, 0, 3, 7} {
		if !inQuietHours(at(h), 22, 8) {
			t.Errorf("%d 点应处于免打扰时段（22:00-08:00）", h)
		}
	}
	for _, h := range []int{8, 12, 18, 21} {
		if inQuietHours(at(h), 22, 8) {
			t.Errorf("%d 点不应处于免打扰时段（22:00-08:00）", h)
		}
	}

	// 同日时段 13:00 - 15:00
	if !inQuietHours(at(14), 13, 15) {
		t.Error("14 点应处于免打扰时段（13:00-15:00）")
	}
	if inQuietHours(at(16), 13, 15) {
		t.Error("16 点不应处于免打扰时段")
	}

	// 起止相同表示未设置
	if inQuietHours(at(3), 0, 0) {
		t.Error("起止相同时不应判定为免打扰")
	}
}

// TestQuietEnd 验证免打扰结束后能算出正确的恢复时刻。
func TestQuietEnd(t *testing.T) {
	// 深夜 23:30，免打扰 22:00-08:00 → 应恢复到次日 08:00
	now := time.Date(2026, 3, 10, 23, 30, 0, 0, time.Local)
	got := quietEnd(now, 22, 8)
	want := time.Date(2026, 3, 11, 8, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("quietEnd = %v, 期望 %v", got, want)
	}

	// 凌晨 03:00，应恢复到当天 08:00
	now = time.Date(2026, 3, 10, 3, 0, 0, 0, time.Local)
	got = quietEnd(now, 22, 8)
	want = time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("quietEnd = %v, 期望 %v", got, want)
	}
}

// TestExpandPlaceholders 验证时间占位符替换。
func TestExpandPlaceholders(t *testing.T) {
	now := time.Date(2026, 3, 10, 8, 5, 0, 0, time.Local) // 2026-03-10 是周二

	got := expandPlaceholders("今天 {{date}}（{{weekday}}）现在是 {{time}}", now)
	want := "今天 2026-03-10（周二）现在是 08:05"
	if got != want {
		t.Fatalf("展开结果 = %q, 期望 %q", got, want)
	}

	// 没有占位符时原样返回
	if got := expandPlaceholders("该喝水了", now); got != "该喝水了" {
		t.Fatalf("无占位符时被改动: %q", got)
	}
}

// TestParseClock 验证时间点解析的边界。
func TestParseClock(t *testing.T) {
	ok := map[string][2]int{
		"08:30": {8, 30},
		"8:30":  {8, 30},
		"00:00": {0, 0},
		"23:59": {23, 59},
	}
	for expr, want := range ok {
		h, m, valid := parseClock(expr)
		if !valid || h != want[0] || m != want[1] {
			t.Errorf("parseClock(%q) = %d:%d valid=%v", expr, h, m, valid)
		}
	}

	for _, expr := range []string{"24:00", "08:60", "-1:00", "8", "abc", "8:30:00"} {
		if _, _, valid := parseClock(expr); valid {
			t.Errorf("parseClock(%q) 应判为非法", expr)
		}
	}
}
