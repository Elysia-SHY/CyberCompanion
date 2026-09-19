package config

import "crypto/subtle"

// ConstantTimeEqual 以常量时间比较两个字符串。
//
// 普通 `==` 比较会在第一个不同的字节处提前返回，攻击者可以通过
// 响应时间差逐字符推断出口令。这里用 crypto/subtle 消除这个侧信道。
//
// 注意：长度不同时依然会立即返回，因此还应配合尝试次数限制使用。
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// MaskSecret 对密钥类字符串做脱敏，用于在 WebUI / 日志中展示。
// 只保留前 4 后 4 位，短于 12 位则全部打码。
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) < 12 {
		return "••••••••"
	}
	return string(r[:4]) + "••••••••" + string(r[len(r)-4:])
}

// IsMasked 判断字符串是否为脱敏占位（用于识别「前端回传了掩码而不是新值」的情况）
func IsMasked(s string) bool {
	return len(s) >= 8 && (contains(s, "•••••") || s == "••••••••")
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
