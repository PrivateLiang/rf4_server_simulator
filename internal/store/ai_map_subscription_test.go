package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 静态库路径：loc_alias 是静态表，ChannelMapKey 靠它把 nav 前缀翻译成地图 key。
func mapTestStaticPath() string {
	return filepath.Join("..", "..", "server_data", "static.sqlite3")
}

func openMapTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenWithStatic(filepath.Join(t.TempDir(), "map-sub.db"), mapTestStaticPath())
	if err != nil {
		t.Fatalf("OpenWithStatic: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

// TestChannelMapKey 覆盖官服频道名到地图 key 的解析。
// nav 前缀的数字就是地图 level_0NN 的 NN；补零不统一（nav1 与 nav03 都存在）。
func TestChannelMapKey(t *testing.T) {
	d := openMapTestDB(t)
	ctx := context.Background()

	cases := []struct {
		channel string
		want    string
		wantOK  bool
	}{
		// 同一张图的不同区域频道必须落到同一张图 —— 这正是地图级订阅的基础。
		{"nav1_03", "level_001_lake", true},
		{"nav1_04", "level_001_lake", true},
		{"nav1_12", "level_001_lake", true},
		{"nav03_01", "level_003_viiveri_lake", true},
		{"nav06_12", "level_006_ladoga", true},
		{"nav09_05", "level_009_ahtuba", true},
		{"nav10_02", "level_010_belaya", true},
		{"nav11_77", "level_011_ladoga_02", true},
		{"nav12_40", "level_012_north_sea", true},
		{"nav13_09", "level_013_sev_don", true},
		{"nav14_13", "level_014_carp_lake", true},
		{"nav15_20", "level_015_tunguska", true},
		{"nav16_07", "level_016_yama", true},
		{"nav18_03", "level_018_moskitnoe", true},
		{"nav19_11", "level_019_american_pond", true},
		// 补零形式也认：nav01 与 nav1 指同一张图。
		{"nav01_01", "level_001_lake", true},
		// 无区域号也算这张图。
		{"nav1", "level_001_lake", true},
		{"nav1_", "level_001_lake", true},
		// 解析不出地图的一律降级为 false，调用方走「精确订阅 ∪ 全局」。
		{"nav17_01", "", false}, // level_017 不存在
		{"nav", "", false},
		{"nav_01", "", false},
		{"NAV1_03", "", false},
		{"private_room", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := d.ChannelMapKey(ctx, c.channel)
		if ok != c.wantOK || got != c.want {
			t.Errorf("ChannelMapKey(%q) = (%q, %v)，期望 (%q, %v)",
				c.channel, got, ok, c.want, c.wantOK)
		}
	}
}

// TestChannelMapSubscription 验证地图级订阅：订阅一张图后，该图所有区域频道都能发言。
// 这是本次 bug 的核心回归 —— 玩家在 nav1_03 切到 nav1_04 不该再失聪。
func TestChannelMapSubscription(t *testing.T) {
	d := openMapTestDB(t)
	ctx := context.Background()

	// 建三个已接入的 Agent。
	for _, k := range []string{"alpha", "beta", "gamma"} {
		if _, _, err := d.AICreateAgent(ctx, AIAgent{
			Key: k, Display: k, Role: "测试", Prompt: "p",
		}, `["map_vibe"]`); err != nil {
			t.Fatalf("创建 %s: %v", k, err)
		}
		if err := d.AISetAgentJoined(ctx, k, true, 0); err != nil {
			t.Fatalf("接入 %s: %v", k, err)
		}
	}

	// alpha 订阅克马羚诺也湖这张图；beta 订阅白河；gamma 只做精确订阅。
	if err := d.AISetMapAgent(ctx, "level_001_lake", "alpha", true); err != nil {
		t.Fatalf("地图订阅 alpha: %v", err)
	}
	if err := d.AISetMapAgent(ctx, "level_010_belaya", "beta", true); err != nil {
		t.Fatalf("地图订阅 beta: %v", err)
	}
	if err := d.AISetChannelAgent(ctx, "nav1_03", "gamma", true); err != nil {
		t.Fatalf("精确订阅 gamma: %v", err)
	}

	// 地图级订阅：alpha 在该图所有区域频道都应可见。
	for _, ch := range []string{"nav1_01", "nav1_03", "nav1_04", "nav1_12"} {
		keys, err := d.AIChannelSubscribed(ctx, ch)
		if err != nil {
			t.Fatalf("%s 订阅查询: %v", ch, err)
		}
		if !containsKey(keys, "alpha") {
			t.Errorf("%s 应包含地图级订阅的 alpha，实际 %v", ch, keys)
		}
	}

	// 别张图不该看到 alpha。
	keys, err := d.AIChannelSubscribed(ctx, "nav10_02")
	if err != nil {
		t.Fatalf("nav10_02 查询: %v", err)
	}
	if containsKey(keys, "alpha") {
		t.Errorf("白河不该出现克马羚诺也湖的 alpha，实际 %v", keys)
	}
	if !containsKey(keys, "beta") {
		t.Errorf("白河应包含 beta，实际 %v", keys)
	}

	// 精确订阅只在那一个频道生效：gamma 不在 nav1_04。
	keys, _ = d.AIChannelSubscribed(ctx, "nav1_04")
	if containsKey(keys, "gamma") {
		t.Errorf("精确订阅的 gamma 不应出现在 nav1_04，实际 %v", keys)
	}
	keys, _ = d.AIChannelSubscribed(ctx, "nav1_03")
	if !containsKey(keys, "gamma") {
		t.Errorf("精确订阅的 gamma 应出现在 nav1_03，实际 %v", keys)
	}

	// 移出地图订阅后立即失效。
	if err := d.AISetMapAgent(ctx, "level_001_lake", "alpha", false); err != nil {
		t.Fatalf("移出地图订阅: %v", err)
	}
	keys, _ = d.AIChannelSubscribed(ctx, "nav1_04")
	if containsKey(keys, "alpha") {
		t.Errorf("移出后不该再出现 alpha，实际 %v", keys)
	}
}

// TestChannelGlobalSubscribe 验证全局开关：默认关，打开后所有已接入 Agent 在任何频道可见。
func TestChannelGlobalSubscribe(t *testing.T) {
	d := openMapTestDB(t)
	ctx := context.Background()

	for _, k := range []string{"alpha", "silent1"} {
		if _, _, err := d.AICreateAgent(ctx, AIAgent{
			Key: k, Display: k, Role: "测试", Prompt: "p", ReplyMode: "social",
		}, `["map_vibe"]`); err != nil {
			t.Fatalf("创建 %s: %v", k, err)
		}
		if err := d.AISetAgentJoined(ctx, k, true, 0); err != nil {
			t.Fatalf("接入 %s: %v", k, err)
		}
	}
	// 把 silent1 改成只看不说。
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_agent SET reply_mode='silent' WHERE key='silent1'`); err != nil {
		t.Fatalf("设置 silent: %v", err)
	}

	// 默认关：谁都不在。
	if v := d.AISetting(ctx, "ai_global_subscribe", "0"); v != "0" {
		t.Fatalf("全局开关默认值应为 0，实际 %q", v)
	}
	keys, _ := d.AIChannelSubscribed(ctx, "nav1_04")
	if len(keys) != 0 {
		t.Fatalf("全局开关默认关时不应有 Agent，实际 %v", keys)
	}

	// 打开：social 的 alpha 出现，silent 的不出现（默认不含 silent）。
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_setting SET value='1' WHERE key='ai_global_subscribe'`); err != nil {
		t.Fatalf("打开全局开关: %v", err)
	}
	keys, _ = d.AIChannelSubscribed(ctx, "nav1_04")
	if !containsKey(keys, "alpha") {
		t.Errorf("全局开关开启后 alpha 应出现，实际 %v", keys)
	}
	if containsKey(keys, "silent1") {
		t.Errorf("默认不应包含 silent 角色，实际 %v", keys)
	}

	// 显式打开 include_silent 后 silent 也出现。
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_setting SET value='1' WHERE key='ai_global_subscribe_include_silent'`); err != nil {
		t.Fatalf("打开 include_silent: %v", err)
	}
	keys, _ = d.AIChannelSubscribed(ctx, "nav1_04")
	if !containsKey(keys, "silent1") {
		t.Errorf("include_silent 开启后 silent1 应出现，实际 %v", keys)
	}
}

// TestAIListMaps 验证后台地图清单带得出中文名与订阅名单。
func TestAIListMaps(t *testing.T) {
	d := openMapTestDB(t)
	ctx := context.Background()

	maps, err := d.AIListMaps(ctx)
	if err != nil {
		t.Fatalf("AIListMaps: %v", err)
	}
	if len(maps) == 0 {
		t.Fatal("地图清单不应为空")
	}
	var lake *AIMapInfo
	for i := range maps {
		if maps[i].MapKey == "level_001_lake" {
			lake = &maps[i]
		}
	}
	if lake == nil {
		t.Fatal("地图清单应含 level_001_lake")
	}
	if lake.Label != "克马羚诺也湖" {
		t.Errorf("地图中文名 = %q，期望 克马羚诺也湖", lake.Label)
	}
	if lake.Count != 0 {
		t.Errorf("未订阅时 Count 应为 0，实际 %d", lake.Count)
	}

	if _, _, err := d.AICreateAgent(ctx, AIAgent{
		Key: "alpha", Display: "甲", Role: "测试", Prompt: "p",
	}, `[]`); err != nil {
		t.Fatalf("创建: %v", err)
	}
	if err := d.AISetMapAgent(ctx, "level_001_lake", "alpha", true); err != nil {
		t.Fatalf("地图订阅: %v", err)
	}
	maps, _ = d.AIListMaps(ctx)
	for i := range maps {
		if maps[i].MapKey == "level_001_lake" {
			if maps[i].Count != 1 || len(maps[i].Agents) != 1 || maps[i].Agents[0] != "甲" {
				t.Errorf("地图订阅名单 = %+v，期望 1 个「甲」", maps[i])
			}
		}
	}
}

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
