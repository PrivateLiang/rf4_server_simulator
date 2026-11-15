package aibridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/store"
)

// TestCatchBriefLine 验证常驻渔获简报：AI 虚拟上鱼只写库不发频道消息，
// 模型平时不知道自己上过鱼，容易顺着「蹲了很久」编出「一条没影」。
// 这句简报就是用来压住这种自相矛盾的。
func TestCatchBriefLine(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "catch-brief.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Runner{db: d}

	// 没有任何渔获时不注入，避免反而引导出「我没钓到鱼」。
	if got := r.catchBriefLine(ctx, "hutao"); got != "" {
		t.Fatalf("无渔获应返回空: %q", got)
	}

	// 写两条，其中一条明显更大。
	for _, c := range []store.AICatch{
		{AgentKey: "hutao", FishKey: "a", FishName: "圆腹雅罗鱼", WeightG: 2793, Loc: "level_011_ladoga_02", LocName: "拉多加湖群岛", Event: "ok"},
		{AgentKey: "hutao", FishKey: "b", FishName: "茴鱼", WeightG: 5100, Loc: "level_011_ladoga_02", LocName: "拉多加湖群岛", Event: "star"},
	} {
		if err := d.AIAddCatch(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	line := r.catchBriefLine(ctx, "hutao")
	// 常驻简报只给定性事实，不报条数/重量：陪聊不是报账。
	for _, want := range []string{"确实上过鱼", "不是空军", "别再说自己一条没上"} {
		if !strings.Contains(line, want) {
			t.Fatalf("简报缺 %q: %s", want, line)
		}
	}
	for _, unwanted := range []string{"累计", "kg", "2 条", "5.10"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("简报不该出现精确数字 %q: %s", unwanted, line)
		}
	}
	if len([]rune(line)) > 70 {
		t.Fatalf("简报超长: %d 字 %q", len([]rune(line)), line)
	}

	// 按 Agent 隔离：别人的渔获不会串到本角色。
	if got := r.catchBriefLine(ctx, "klee"); got != "" {
		t.Fatalf("不应看到别人的渔获: %q", got)
	}

	// 开关关闭时不注入，且不影响别的功能。
	if _, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES('ai_catch_brief_inject','0',0)
		ON CONFLICT(key) DO UPDATE SET value='0'`); err != nil {
		t.Fatal(err)
	}
	if got := r.catchBriefLine(ctx, "hutao"); got != "" {
		t.Fatalf("开关关闭应返回空: %q", got)
	}
}

// TestCatchLineDetailed 验证玩家明确问渔获时仍能拿到完整记录。
func TestCatchLineDetailed(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "catch-detail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Runner{db: d}
	if err := d.AIAddCatch(ctx, store.AICatch{
		AgentKey: "klee", FishKey: "a", FishName: "欧洲无须鳕", WeightG: 4412,
		Loc: "level_012_north_sea", LocName: "挪威海", Event: "ok",
	}); err != nil {
		t.Fatal(err)
	}

	detail := r.catchLine(ctx, "klee")
	for _, want := range []string{"累计 1 条", "4.41kg", "欧洲无须鳕", "挪威海"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("完整记录缺 %q: %s", want, detail)
		}
	}
	brief := r.catchBriefLine(ctx, "klee")
	if len([]rune(brief)) >= len([]rune(detail)) {
		t.Fatalf("简报应比完整记录短: brief=%q detail=%q", brief, detail)
	}
}
