package hal

import (
	"strconv"
	"strings"
)

// 本文件放置所有平台共用的纯计算helper，避免被 build tag 挡住。
// （之前 round1 / formatDuration 放在 probe_unix.go 里，导致 Windows 构建失败。）

// round1 保留一位小数。
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// formatDuration 把秒数格式化为 "3d 4h 12m" / "4h 12m" / "12m"。
func formatDuration(secs float64) string {
	d := int64(secs)
	days := d / 86400
	hours := (d % 86400) / 3600
	mins := (d % 3600) / 60
	switch {
	case days > 0:
		return strconv.FormatInt(days, 10) + "d " + strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(mins, 10) + "m"
	case hours > 0:
		return strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(mins, 10) + "m"
	default:
		return strconv.FormatInt(mins, 10) + "m"
	}
}

// parseMilliCelsius 把内核温度读数归一化为摄氏度。
// 不同平台单位不统一：常见为毫摄氏度，少数为摄氏度或华氏度的误读数。
func parseMilliCelsius(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	if v > 1000 {
		v /= 1000
	}
	if v <= 0 || v > 150 {
		return 0, false
	}
	return v, true
}
