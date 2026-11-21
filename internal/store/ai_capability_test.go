package store

// T-104 存储层回归：角色能力配置是一条按 agent_key 的版本化 ai_setting 记录，
// 保存必须是 compare-and-set，旧 tools_json 只降级为只读子集。
// 期望值全部手写，不复用被测 SQL 生成。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
)

func openCapabilityDB(t *testing.T, name string) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

// 缺记录时，内置 Agent 的旧 tools_json 只映射成可证明的只读子集。
func TestAICapabilitiesLegacyFallback(t *testing.T) {
	d := openCapabilityDB(t, "cap-legacy.db")
	ctx := context.Background()
	view, err := d.AIGetCapabilities(ctx, "companion")
	if err != nil {
		t.Fatalf("读能力: %v", err)
	}
	if view.Present {
		t.Fatal("迁移后不应自动写入能力记录")
	}
	if !view.State.Legacy || view.State.State != aiconfig.CapabilityStateConfigured {
		t.Fatalf("旧快照状态 = %+v", view.State)
	}
	// companion 的内置 tools_json 是 ["map_vibe"]。
	if len(view.State.ReadTools) != 1 || view.State.ReadTools[0] != aiconfig.ToolMapVibe {
		t.Fatalf("旧只读子集 = %v", view.State.ReadTools)
	}
	if len(view.State.WriteTools) != 0 {
		t.Fatalf("旧快照不得授予写: %v", view.State.WriteTools)
	}
	var rows int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_setting WHERE key=?`,
		AICapabilityKey("companion")).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("读取路径擅自写了能力记录: %d 行", rows)
	}
}

// 保存 → 修订 +1；同一 base 再保存冲突；显式空可保存并拒绝全部工具。
func TestAICapabilitiesSaveAndConflict(t *testing.T) {
	d := openCapabilityDB(t, "cap-save.db")
	ctx := context.Background()
	st, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil)
	if err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	if st.Revision != 1 || strings.Join(st.ReadTools, ",") != aiconfig.ToolMapVibe || len(st.WriteTools) != 1 {
		t.Fatalf("首次保存投影 = %+v", st)
	}
	// 旧表单即使还写着 ["map_vibe"]，新记录仍然生效。
	view, err := d.AIGetCapabilities(ctx, "companion")
	if err != nil || !view.Present || view.Revision != 1 {
		t.Fatalf("回读 = %+v/%v", view, err)
	}
	if strings.Join(view.State.ReadTools, ",") != aiconfig.ToolMapVibe || view.State.Legacy {
		t.Fatalf("旧 tools_json 覆盖了新意图: %+v", view.State)
	}

	// 过期 base → 冲突，库里的修订不动。
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp}, nil); !errors.Is(err, ErrAICapabilityConflict) {
		t.Fatalf("过期 base 错误 = %v", err)
	}
	view, _ = d.AIGetCapabilities(ctx, "companion")
	if view.Revision != 1 {
		t.Fatalf("冲突后修订 = %d", view.Revision)
	}

	// 显式空：保存成功，工具全拒绝。
	st, err = d.AISaveCapabilities(ctx, "companion", 1, []string{}, nil)
	if err != nil {
		t.Fatalf("保存显式空: %v", err)
	}
	if st.Revision != 2 || st.State != aiconfig.CapabilityStateEmpty || len(st.Tools()) != 0 {
		t.Fatalf("显式空 = %+v", st)
	}

	// 未知能力拒绝，且不推进修订。
	if _, err := d.AISaveCapabilities(ctx, "companion", 2, []string{"nope"}, nil); !errors.Is(err, ErrAICapabilityInvalid) {
		t.Fatalf("未知能力错误 = %v", err)
	}
	view, _ = d.AIGetCapabilities(ctx, "companion")
	if view.Revision != 2 {
		t.Fatalf("非法保存推进了修订: %d", view.Revision)
	}
}

// 旧代次记录不生效；重新保存一次即绑定当前代次。
func TestAICapabilitiesGenerationBinding(t *testing.T) {
	d := openCapabilityDB(t, "cap-gen.db")
	ctx := context.Background()
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_runtime_control(agent_key,generation,revision,tombstoned)
		VALUES('companion',2,0,0)
		ON CONFLICT(agent_key) DO UPDATE SET generation=2`); err != nil {
		t.Fatal(err)
	}
	view, err := d.AIGetCapabilities(ctx, "companion")
	if err != nil {
		t.Fatal(err)
	}
	if view.State.State != aiconfig.CapabilityStateInactive || len(view.State.Tools()) != 0 {
		t.Fatalf("旧代次仍生效: %+v", view.State)
	}
	// 保存时提交读到的修订，写回当前代次。
	if _, err := d.AISaveCapabilities(ctx, "companion", view.Revision,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatalf("重新确认: %v", err)
	}
	view, _ = d.AIGetCapabilities(ctx, "companion")
	if view.State.State != aiconfig.CapabilityStateConfigured || view.State.Generation != 2 {
		t.Fatalf("重新确认后 = %+v", view.State)
	}
}

// 坏记录：读取失败关闭，但修订号仍可提交修复。
func TestAICapabilitiesCorruptRecord(t *testing.T) {
	d := openCapabilityDB(t, "cap-corrupt.db")
	ctx := context.Background()
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSetting(ctx, AICapabilityKey("companion"),
		`{"schema_version":1,"revision":5,"generation":1,"capabilities":"broken"}`); err != nil {
		t.Fatal(err)
	}
	view, err := d.AIGetCapabilities(ctx, "companion")
	if err != nil {
		t.Fatal(err)
	}
	if view.State.State != aiconfig.CapabilityStateInvalid || len(view.State.Tools()) != 0 {
		t.Fatalf("坏记录未失败关闭: %+v", view.State)
	}
	if view.Revision != 5 {
		t.Fatalf("坏记录修订 = %d，期望 5", view.Revision)
	}
	if _, err := d.AISaveCapabilities(ctx, "companion", 5,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatalf("按读到的修订修复失败: %v", err)
	}
}

// 旧粒度：显式保存时携带只读子集，投影不能扩到全部目录。
func TestAICapabilitiesLegacyGranularityPreserved(t *testing.T) {
	d := openCapabilityDB(t, "cap-granularity.db")
	ctx := context.Background()
	st, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp},
		[]string{aiconfig.ToolMapVibe, aiconfig.ToolItemLookup})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.ReadTools, ",") != aiconfig.ToolItemLookup+","+aiconfig.ToolMapVibe {
		t.Fatalf("旧粒度投影 = %v", st.ReadTools)
	}
	st, err = d.AISaveCapabilities(ctx, "companion", 1,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityJoinRoundtable}, nil)
	if err != nil || strings.Join(st.ReadTools, ",") != aiconfig.ToolItemLookup+","+aiconfig.ToolMapVibe {
		t.Fatalf("编辑其它能力扩大了旧授权 = %v / %v", st.ReadTools, err)
	}
	if _, err := d.AISaveCapabilities(ctx, "host", 0,
		[]string{aiconfig.CapabilityGuideHelp}, nil); !errors.Is(err, ErrAICapabilityInvalid) {
		t.Fatalf("无历史授权的角色不能新建旧攻略授权：%v", err)
	}
}

// 保存「虚拟垂钓」时按当前修订补派 actor_set_enabled：启用只入队不应用，
// 同一集合重复保存不跳修订、不重复补派；撤销垂钓产生禁用命令并由代次解释
// 保证「攻略」能力不被这次禁用误伤。
func TestAICapabilitiesRuntimeDispatchAndIdempotency(t *testing.T) {
	d := openCapabilityDB(t, "cap-runtime.db")
	ctx := context.Background()
	virtual := []string{aiconfig.CapabilityVirtualFishing}

	res, err := d.AISaveCapabilitiesWithRuntime(ctx, "companion", 0, virtual, nil)
	if err != nil {
		t.Fatalf("保存虚拟垂钓: %v", err)
	}
	if res.Revision != 1 || !res.Changed || res.Runtime.Availability != AICapabilityAvailabilityPending ||
		!res.Runtime.Desired || res.Runtime.Actual || res.Runtime.CommandID == 0 {
		t.Fatalf("首次保存运行差量 = %+v", res)
	}
	cmd, err := d.AIGetRuntimeCommand(ctx, res.Runtime.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != AIRuntimeKindActorSetEnabled || cmd.Source != AIRuntimeSourceAdmin ||
		cmd.ExpectedRevision != 1 || cmd.ExpectedGeneration != 0 ||
		!strings.Contains(cmd.Payload, `"enabled":true`) ||
		!strings.HasPrefix(cmd.RequestID, aiconfig.CapabilityRuntimeRequestPrefix("companion", 1)) {
		t.Fatalf("启用命令 = %+v", cmd)
	}

	// 同集合重复保存：修订不跳，复用同一条命令。
	res2, err := d.AISaveCapabilitiesWithRuntime(ctx, "companion", 1, virtual, nil)
	if err != nil {
		t.Fatalf("重复保存: %v", err)
	}
	if res2.Revision != 1 || res2.Changed || res2.Runtime.CommandID != res.Runtime.CommandID {
		t.Fatalf("重复保存 = %+v", res2)
	}
	var count int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command
		WHERE kind=?`, AIRuntimeKindActorSetEnabled).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("重复保存补派了 %d 条命令", count)
	}

	// 模拟 GS 应用启用：actor_enabled=1；启用不推进代次，只推进 revision。
	if _, err := d.SQL.Exec(`UPDATE ai_runtime_command SET status='applied', applied_ts=?, result_revision=1
		WHERE id=?`, float64(time.Now().Unix()), res.Runtime.CommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`UPDATE ai_agent SET actor_enabled=1 WHERE key='companion'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_runtime_control(agent_key,generation,revision,tombstoned)
		VALUES('companion',1,1,0)
		ON CONFLICT(agent_key) DO UPDATE SET revision=1`); err != nil {
		t.Fatal(err)
	}

	// 撤销垂钓（保留攻略）：产生禁用命令，运行态仍是开启。
	res3, err := d.AISaveCapabilitiesWithRuntime(ctx, "companion", 1,
		[]string{aiconfig.CapabilityGuideHelp}, nil)
	if err != nil {
		t.Fatalf("撤销垂钓: %v", err)
	}
	if res3.Revision != 2 || !res3.Changed || res3.Runtime.Availability != AICapabilityAvailabilityPending ||
		res3.Runtime.Desired || res3.Runtime.CommandID == 0 {
		t.Fatalf("撤权运行差量 = %+v", res3)
	}
	disable, err := d.AIGetRuntimeCommand(ctx, res3.Runtime.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disable.Payload, `"enabled":false`) || disable.ExpectedGeneration != 1 ||
		disable.ExpectedRevision != 2 {
		t.Fatalf("禁用命令 = %+v", disable)
	}

	// 模拟 GS 应用：关开关、推进代次、写终态。之后攻略能力仍须生效
	// （记录代次 1、当前代次 2，由这条能力命令解释）。
	now := float64(time.Now().Unix())
	if _, err := d.SQL.Exec(`UPDATE ai_runtime_command SET status='applied', applied_ts=?, result_revision=1 WHERE id=?`,
		now, disable.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`UPDATE ai_agent SET actor_enabled=0 WHERE key='companion'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_runtime_control(agent_key,generation,revision,tombstoned)
		VALUES('companion',2,1,0)
		ON CONFLICT(agent_key) DO UPDATE SET generation=2, revision=1`); err != nil {
		t.Fatal(err)
	}
	view, err := d.AIGetCapabilities(ctx, "companion")
	if err != nil {
		t.Fatal(err)
	}
	if view.State.State != aiconfig.CapabilityStateConfigured || strings.Join(view.State.ReadTools, ",") != aiconfig.ToolMapVibe ||
		view.State.Generation != 2 || view.State.Has(aiconfig.CapabilityVirtualFishing) {
		t.Fatalf("禁用后代次解释 = %+v", view.State)
	}
	rt, err := d.AICapabilityRuntimeStatus(ctx, "companion", view.State)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Availability != AICapabilityAvailabilityAvailable || rt.Desired || rt.Actual {
		t.Fatalf("禁用后可用性 = %+v", rt)
	}

	// 失败后按当前修订补派：重新授权 → 命令被标 rejected → 同集合保存补 attempt 2。
	res4, err := d.AISaveCapabilitiesWithRuntime(ctx, "companion", 2,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityVirtualFishing}, nil)
	if err != nil {
		t.Fatalf("重新授权: %v", err)
	}
	if res4.Revision != 3 || res4.Runtime.Availability != AICapabilityAvailabilityPending {
		t.Fatalf("重新授权 = %+v", res4)
	}
	if _, err := d.SQL.Exec(`UPDATE ai_runtime_command SET status='rejected', reason='simulated_failure', applied_ts=?
		WHERE id=?`, now, res4.Runtime.CommandID); err != nil {
		t.Fatal(err)
	}
	res5, err := d.AISaveCapabilitiesWithRuntime(ctx, "companion", 3,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityVirtualFishing}, nil)
	if err != nil {
		t.Fatalf("失败后补派: %v", err)
	}
	if res5.Revision != 3 || res5.Changed || res5.Runtime.CommandID == res4.Runtime.CommandID ||
		res5.Runtime.Availability != AICapabilityAvailabilityPending {
		t.Fatalf("失败后补派 = %+v", res5)
	}
	retry, err := d.AIGetRuntimeCommand(ctx, res5.Runtime.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(retry.RequestID, "-a2") || retry.ExpectedRevision != 3 {
		t.Fatalf("补派命令 = %+v", retry)
	}
}
