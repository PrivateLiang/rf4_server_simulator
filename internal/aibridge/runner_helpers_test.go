package aibridge

import "testing"

// TestToolResultText 守住「失败不静默」：未知/无权/参数错/暂停必须给出
// 可区分的一行，空结果与失败不能混成同一个空串。
func TestToolResultText(t *testing.T) {
	cases := []struct {
		name string
		res  ToolResult
		err  error
		want string
	}{
		{"ok", ToolResult{Status: "ok", Data: "鱼种:河鲈"}, nil, "鱼种:河鲈"},
		{"empty", ToolResult{Status: "ok"}, nil, "（工具没有返回数据）"},
		{"accepted", ToolResult{Status: "accepted", Data: "已被模型改写"}, nil, "已提交，等待游戏服务确认。"},
		{"unknown", ToolResult{}, ErrToolUnknown, "（工具不存在）"},
		{"denied", ToolResult{}, ErrToolNotAllowed, "（该工具未获授权）"},
		{"stopped", ToolResult{}, ErrToolStopped, "（助手已暂停，工具不可用）"},
	}
	for _, c := range cases {
		if got := toolResultText(c.res, c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// 参数错要带具体原因，便于模型自行纠正。
	got := toolResultText(ToolResult{}, &ToolError{Code: "invalid_args", Msg: "工具参数不合法: 缺少 fish"})
	if got == "" || got == "（工具参数不合法）" {
		t.Fatalf("参数错缺少原因: %q", got)
	}
}
