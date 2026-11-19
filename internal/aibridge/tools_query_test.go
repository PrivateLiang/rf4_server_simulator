package aibridge

import "testing"

func TestFirstFishKeyword(t *testing.T) {
	cases := map[string]string{
		"河鲈在哪钓":       "河鲈",
		"请问鲤鱼用什么饵":    "请问", // 2 字滑窗取首片段，交给查鱼助手解析，解析不了会回帮助文本
		"下通古斯卡河 常见拟鲤": "下通",
		"hello":       "",
		"":            "",
	}
	for in, want := range cases {
		if got := firstFishKeyword(in); got != want {
			t.Errorf("firstFishKeyword(%q) = %q, want %q", in, got, want)
		}
	}
	if got := firstFishKeyword("河鲈 白河"); got == "" {
		t.Fatal("带空格的鱼名不应为空")
	}
}
