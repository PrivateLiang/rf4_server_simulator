package aiconfig

import (
	"errors"
	"strings"
	"testing"
)

// 目录只收有调用点的能力；写集合只有 actor_target_suggest。
func TestCapabilityCatalogShape(t *testing.T) {
	caps := CapabilityCatalog()
	if len(caps) != 6 {
		t.Fatalf("能力数量 = %d", len(caps))
	}
	wantOrder := []string{
		CapabilityGuideHelp, CapabilityReplyStar, CapabilityJoinRoundtable,
		CapabilityVirtualFishing, CapabilityBroadcastCatch, CapabilityAcceptTarget,
	}
	for i, id := range wantOrder {
		if caps[i].ID != id {
			t.Fatalf("目录顺序 %d = %s，期望 %s", i, caps[i].ID, id)
		}
	}
	writes := 0
	for _, tool := range CapabilityToolCatalog() {
		if tool.Kind == "write" {
			writes++
			if tool.Name != ToolActorTargetSuggest || tool.Route != "gs_write" {
				t.Fatalf("写工具定义错误: %+v", tool)
			}
		}
	}
	if writes != 1 {
		t.Fatalf("写工具数量 = %d，期望 1", writes)
	}
	// 攻略覆盖八个只读工具，远程两个也在内。
	if len(GuideReadTools()) != 8 {
		t.Fatalf("攻略只读工具 = %v", GuideReadTools())
	}
	for _, id := range []string{CapabilityReplyStar, CapabilityJoinRoundtable,
		CapabilityVirtualFishing, CapabilityBroadcastCatch} {
		c, ok := CapabilityByID(id)
		if !ok || c.Event == "" || len(c.ReadTools) > 0 || len(c.WriteTools) > 0 {
			t.Fatalf("事件能力不应投影工具: %+v", c)
		}
	}
}

// 显式集合投影：只有攻略给只读，只有接受建议给写；其余能力不碰工具。
func TestProjectToolsByCapability(t *testing.T) {
	read, write := ProjectTools([]string{CapabilityGuideHelp}, nil)
	if len(read) != 8 || len(write) != 0 {
		t.Fatalf("仅攻略 = %v/%v", read, write)
	}
	read, write = ProjectTools([]string{CapabilityAcceptTarget}, nil)
	if len(read) != 0 || len(write) != 1 || write[0] != ToolActorTargetSuggest {
		t.Fatalf("仅接受建议 = %v/%v", read, write)
	}
	read, write = ProjectTools([]string{CapabilityReplyStar, CapabilityJoinRoundtable}, nil)
	if len(read) != 0 || len(write) != 0 {
		t.Fatalf("事件能力不应授予工具: %v/%v", read, write)
	}
	read, write = ProjectTools(nil, nil)
	if len(read) != 0 || len(write) != 0 {
		t.Fatalf("空集合 = %v/%v", read, write)
	}
}

// 旧 tools_json 只保留可证明的只读子集，顺序按目录，去重，未知/写工具丢弃。
func TestLegacyReadToolsKeepsOnlyReadableSubset(t *testing.T) {
	got := LegacyReadTools(`["map_vibe","species_lookup","actor_target_suggest","nope","map_vibe"]`)
	want := []string{ToolSpeciesLookup, ToolMapVibe}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("旧子集 = %v，期望 %v", got, want)
	}
	if got := LegacyReadTools(`not json`); got != nil {
		t.Fatalf("坏 JSON 应视为无授权: %v", got)
	}
	if got := LegacyReadTools(`[]`); len(got) != 0 {
		t.Fatalf("空旧值应无工具: %v", got)
	}
}

// 读侧状态：缺配置/显式空/非法/旧代次都不得授予工具。
func TestResolveCapabilityStateFailsClosed(t *testing.T) {
	// 缺记录 + 旧 tools_json：只读子集，无写、无事件。
	st := ResolveCapabilityState("a", nil, false, nil, `["item_lookup"]`, 1)
	if st.State != CapabilityStateConfigured || !st.Legacy || st.Revision != 0 {
		t.Fatalf("旧快照状态 = %+v", st)
	}
	if len(st.ReadTools) != 1 || st.ReadTools[0] != ToolItemLookup || len(st.WriteTools) != 0 {
		t.Fatalf("旧快照投影 = %+v", st)
	}
	// 缺记录且没有旧值：缺配置。
	st = ResolveCapabilityState("a", nil, false, nil, `[]`, 1)
	if st.State != CapabilityStateMissing || len(st.Tools()) != 0 {
		t.Fatalf("缺配置 = %+v", st)
	}
	// 显式空。
	empty := CapabilitySet{SchemaVersion: 1, Revision: 2, Generation: 1, Capabilities: []string{}}
	st = ResolveCapabilityState("a", &empty, true, nil, `["item_lookup"]`, 1)
	if st.State != CapabilityStateEmpty || len(st.Tools()) != 0 {
		t.Fatalf("显式空 = %+v", st)
	}
	// 非法记录。
	st = ResolveCapabilityState("a", nil, true, errors.New("bad"), `["item_lookup"]`, 1)
	if st.State != CapabilityStateInvalid || len(st.Tools()) != 0 {
		t.Fatalf("非法 = %+v", st)
	}
	// 旧代次。
	rec := CapabilitySet{SchemaVersion: 1, Revision: 3, Generation: 1,
		Capabilities: []string{CapabilityGuideHelp, CapabilityAcceptTarget}}
	st = ResolveCapabilityState("a", &rec, true, nil, "", 2)
	if st.State != CapabilityStateInactive || len(st.Tools()) != 0 {
		t.Fatalf("旧代次 = %+v", st)
	}
	// 正常：八个读 + 一个写。
	st = ResolveCapabilityState("a", &rec, true, nil, "", 1)
	if st.State != CapabilityStateConfigured || len(st.ReadTools) != 8 ||
		len(st.WriteTools) != 1 || !st.Has(CapabilityAcceptTarget) {
		t.Fatalf("正常 = %+v", st)
	}
}

// 旧粒度保留：记录携带只读子集时不能扩到全部目录。
func TestResolveCapabilityStateKeepsLegacyGranularity(t *testing.T) {
	rec := CapabilitySet{
		SchemaVersion: 1, Revision: 1, Generation: 1,
		Capabilities:  []string{CapabilityGuideHelp},
		ReadOnlyTools: []string{ToolMapVibe, ToolBaitPref},
	}
	st := ResolveCapabilityState("a", &rec, true, nil, "", 1)
	if len(st.ReadTools) != 2 || st.ReadTools[0] != ToolBaitPref || st.ReadTools[1] != ToolMapVibe {
		t.Fatalf("旧粒度 = %v", st.ReadTools)
	}
}

func TestCapabilitySetValidation(t *testing.T) {
	s := CapabilitySet{SchemaVersion: 1, Revision: 1, Capabilities: []string{CapabilityAcceptTarget}}
	if _, err := MarshalCapabilitySet(s); err != nil {
		t.Fatalf("合法集合被拒绝: %v", err)
	}
	// 未知能力。
	if _, err := MarshalCapabilitySet(CapabilitySet{SchemaVersion: 1,
		Capabilities: []string{"nope"}}); !errors.Is(err, ErrCapabilityInvalid) {
		t.Fatalf("未知能力错误 = %v", err)
	}
	// 顺序不对。
	if _, err := MarshalCapabilitySet(CapabilitySet{SchemaVersion: 1,
		Capabilities: []string{CapabilityAcceptTarget, CapabilityGuideHelp}}); err == nil {
		t.Fatal("乱序应被拒绝")
	}
	// null 与显式空无法区分，拒绝落库。
	if _, err := MarshalCapabilitySet(CapabilitySet{SchemaVersion: 1}); !errors.Is(err, ErrCapabilityInvalid) {
		t.Fatalf("null 集合错误 = %v", err)
	}
	// 未知 schema。
	if _, err := MarshalCapabilitySet(CapabilitySet{SchemaVersion: 9,
		Capabilities: []string{}}); err == nil {
		t.Fatal("未知版本应被拒绝")
	}
	// 只读子集必须有攻略能力。
	if _, err := MarshalCapabilitySet(CapabilitySet{SchemaVersion: 1,
		Capabilities:  []string{CapabilityAcceptTarget},
		ReadOnlyTools: []string{ToolItemLookup}}); err == nil {
		t.Fatal("无攻略却带只读子集应被拒绝")
	}
	// 坏记录仍能读出修订，供冲突重试。
	if rev, ok := CapabilityRevisionOf(`{"revision":7,"capabilities":"broken"}`); !ok || rev != 7 {
		t.Fatalf("坏记录修订 = %d/%v", rev, ok)
	}
}

// 受信事件：类别/来源/修订都可验证才放行；播报升级需要圆桌。
func TestAuthorizeEvent(t *testing.T) {
	states := map[string]CapabilityState{
		"a": {AgentKey: "a", State: CapabilityStateConfigured, Revision: 4,
			Capabilities: []string{CapabilityReplyStar, CapabilityBroadcastCatch},
			ReadTools:    nil},
		"b": {AgentKey: "b", State: CapabilityStateConfigured, Revision: 4,
			Capabilities: []string{CapabilityReplyStar, CapabilityBroadcastCatch, CapabilityJoinRoundtable}},
	}
	// 缺类别不可验证。
	if err := AuthorizeEvent(TrustedEvent{Source: "system", AgentKeys: []string{"a"}, ConfigRevision: 4}, states); !errors.Is(err, ErrEventUnverifiable) {
		t.Fatalf("缺类别错误 = %v", err)
	}
	// 未知类别。
	if err := AuthorizeEvent(TrustedEvent{Source: "system", Category: "made_up",
		AgentKeys: []string{"a"}, ConfigRevision: 4}, states); !errors.Is(err, ErrEventUnverifiable) {
		t.Fatalf("未知类别错误 = %v", err)
	}
	// 有授权且修订一致。
	if err := AuthorizeEvent(TrustedEvent{Source: "system", Category: EventReplyStar,
		AgentKeys: []string{"a"}, ConfigRevision: 4}, states); err != nil {
		t.Fatalf("合法事件被拒绝: %v", err)
	}
	// 修订不一致：管理员已保存新意图，旧事件不能消费。
	if err := AuthorizeEvent(TrustedEvent{Source: "system", Category: EventReplyStar,
		AgentKeys: []string{"a"}, ConfigRevision: 3}, states); !errors.Is(err, ErrEventNotGranted) {
		t.Fatalf("修订不一致错误 = %v", err)
	}
	// 播报升级为圆桌需要「参加圆桌」。
	if err := AuthorizeEvent(TrustedEvent{Source: "system", Category: EventBroadcastCatch,
		AgentKeys: []string{"a"}, ConfigRevision: 4}, states); !errors.Is(err, ErrEventNotGranted) {
		t.Fatalf("缺圆桌错误 = %v", err)
	}
	if err := AuthorizeEvent(TrustedEvent{Source: "system", Category: EventBroadcastCatch,
		AgentKeys: []string{"b"}, ConfigRevision: 4}, states); err != nil {
		t.Fatalf("有圆桌仍被拒绝: %v", err)
	}
	// 真人对话不属于事件能力。
	if err := AuthorizeEvent(TrustedEvent{Source: "human", AgentKeys: []string{"nobody"}}, states); err != nil {
		t.Fatalf("真人对话被事件规则拦截: %v", err)
	}
	// 来源伪造。
	if err := AuthorizeEvent(TrustedEvent{Source: "model", Category: EventReplyStar,
		AgentKeys: []string{"a"}}, states); !errors.Is(err, ErrEventUnverifiable) {
		t.Fatalf("伪造来源错误 = %v", err)
	}
}

// 只有虚拟垂钓直接对应 GS 原模拟；事件能力与攻略都不擅自打开模拟。
func TestCapabilityActorSimulationMapping(t *testing.T) {
	if !CapabilityRequiresActorSimulation(CapabilityVirtualFishing) {
		t.Fatal("虚拟垂钓应要求 GS 模拟开关")
	}
	for _, id := range []string{CapabilityGuideHelp, CapabilityReplyStar,
		CapabilityJoinRoundtable, CapabilityBroadcastCatch, CapabilityAcceptTarget} {
		if CapabilityRequiresActorSimulation(id) {
			t.Fatalf("%s 不应直接改 GS 模拟开关", id)
		}
	}
	if !RequiresActorSimulation([]string{CapabilityGuideHelp, CapabilityVirtualFishing}) {
		t.Fatal("含虚拟垂钓的集合应要求模拟")
	}
	if RequiresActorSimulation([]string{CapabilityGuideHelp, CapabilityAcceptTarget}) {
		t.Fatal("攻略/写能力不应要求模拟")
	}
}

// 能力运行命令的 ID 由角色与修订决定：同输入稳定，跨角色/修订/attempt 不同。
func TestCapabilityRuntimeRequestIDs(t *testing.T) {
	a := CapabilityRuntimeRequestID("companion", 3, true, 1)
	if again := CapabilityRuntimeRequestID("companion", 3, true, 1); again != a {
		t.Fatalf("同输入 ID 不稳定: %s / %s", a, again)
	}
	if !IsCapabilityRuntimeRequest(a) {
		t.Fatalf("未识别为能力命令: %s", a)
	}
	prefix := CapabilityRuntimeRequestPrefix("companion", 3)
	if !strings.HasPrefix(a, prefix) || !strings.HasPrefix(
		CapabilityRuntimeRequestID("companion", 3, true, 2), prefix) {
		t.Fatalf("ID 未带修订前缀: %s / %s", a, prefix)
	}
	if CapabilityRuntimeRequestID("companion", 3, false, 1) == a {
		t.Fatal("启用/禁用应得到不同 ID")
	}
	if CapabilityRuntimeRequestID("companion", 4, true, 1) == a {
		t.Fatal("不同修订应得到不同 ID")
	}
	if CapabilityRuntimeRequestID("other", 3, true, 1) == a {
		t.Fatal("不同角色应得到不同 ID")
	}
	if CapabilityRuntimeRequestID("companion", 3, true, 2) == a {
		t.Fatal("补派 attempt 应得到不同 ID")
	}
	if IsCapabilityRuntimeRequest("ad-20260101") {
		t.Fatal("旧后台请求不应被误认成能力命令")
	}
}
