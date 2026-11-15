package aibridge

import "testing"

func TestCatchQuestion(t *testing.T) {
	for _, s := range []string{
		"你钓了多少", "今天钓了几条", "最近渔获怎么样", "你最大的一条多大", "你钓到过什么鱼",
		// 实测漏掉的自然语言问法（截图里玩家问的是「你们钓了哪些鱼了」）。
		"你们钓了哪些鱼了", "钓了啥", "上鱼了吗", "有鱼吗", "钓到鱼没", "今天空军了吗",
	} {
		if !catchQuestion(s) {
			t.Fatalf("%q 应该识别为渔获问题", s)
		}
	}
	for _, s := range []string{"今天天气怎么样", "河鲈在哪钓", "推荐个饵", "你好"} {
		if catchQuestion(s) {
			t.Fatalf("%q 不应触发渔获查询", s)
		}
	}
}
