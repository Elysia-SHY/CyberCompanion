package stickers

import "testing"

func TestDetectScene(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"我好喜欢你呀", "love"},
		{"抱抱~", "love"},
		{"早上好", "greeting"},
		{"晚安", "greeting"},
		{"好饿啊想吃饭", "hungry"},
		{"中午吃什么", "hungry"},
		{"笨蛋主人", "tsundere"},
		{"救命怎么办", "panic"},
		{"今天天气不错", ""},
		{"", ""},
	}

	for _, c := range cases {
		if got := DetectScene(c.input); got != c.want {
			t.Errorf("DetectScene(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestGetRandomSceneSticker(t *testing.T) {
	for _, scene := range []string{"love", "greeting", "hungry", "tsundere", "panic"} {
		if got := GetRandomSceneSticker(scene); got == "" {
			t.Errorf("场景 %q 应至少有一张可用表情", scene)
		}
	}
	if got := GetRandomSceneSticker("不存在的场景"); got != "" {
		t.Errorf("未知场景应返回空，实际 %d 字节", len(got))
	}
}
