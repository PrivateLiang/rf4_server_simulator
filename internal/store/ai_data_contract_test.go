package store

// T-016 数据契约回归：保存厂商配置必须只改后台选中的 ai_model 行；
// 同组多行、空值行、已删除行、孤儿登记与历史种子都必须被容忍。
// 期望值全部是手写字面量，不复用被测 SQL 生成。

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"rf4server/internal/aiconfig"
)

type aiModelRow struct {
	ID      int64
	Model   string
	Enabled int
}

func aiModelRows(t *testing.T, d *DB, providerID int64) []aiModelRow {
	t.Helper()
	rows, err := d.SQL.Query(`
		SELECT id, model_id, enabled FROM ai_model
		WHERE provider_id=? ORDER BY id`, providerID)
	if err != nil {
		t.Fatalf("读取模型行: %v", err)
	}
	defer rows.Close()
	var out []aiModelRow
	for rows.Next() {
		var r aiModelRow
		if err := rows.Scan(&r.ID, &r.Model, &r.Enabled); err != nil {
			t.Fatalf("扫描模型行: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历模型行: %v", err)
	}
	return out
}

func openDirtyAIDB(t *testing.T, name string) *DB {
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

// TestAISaveProviderDestinationUpdatesOnlySelectedRow 是本票的核心回归：
// 两行 ai_model（有效行 + 空行）的库上保存必须成功，且只改选中的那一行。
func TestAISaveProviderDestinationUpdatesOnlySelectedRow(t *testing.T) {
	d := openDirtyAIDB(t, "dirty-save.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()

	dest, err := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://vendor.invalid/v1", ModelID: "model-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AISaveProviderDestination(ctx, seed.ProviderID, seed.ModelRowID,
		"https://vendor.invalid/v1", "model-new", dest); err != nil {
		t.Fatalf("两行模型上保存失败: %v", err)
	}

	got := aiModelRows(t, d, seed.ProviderID)
	want := []aiModelRow{
		{ID: seed.ModelRowID, Model: "model-new", Enabled: 1},
		{ID: seed.BlankModelRowID, Model: "", Enabled: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("模型行数 = %d，期望 %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行 = %+v，期望 %+v（空行必须保持原样）", i, got[i], want[i])
		}
	}

	var base, snapshot string
	if err := d.SQL.QueryRow(`SELECT base_url FROM ai_provider WHERE id=?`, seed.ProviderID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != "https://vendor.invalid/v1" {
		t.Fatalf("base_url = %q", base)
	}
	snapshot = d.AISetting(ctx, aiconfig.KeyEgressDestination, "")
	if snapshot != dest {
		t.Fatalf("目的地快照 = %q，期望 %q", snapshot, dest)
	}

	// 后台状态页的读取口径（最低 id 的 enabled 行）必须看到新模型，
	// 而不是空值行。
	var visible string
	if err := d.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(m.model_id,'')
		FROM ai_provider p LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.enabled=1 ORDER BY p.id, m.id LIMIT 1`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != "model-new" {
		t.Fatalf("后台读到的模型 = %q，期望 model-new", visible)
	}
}

// TestAISaveProviderDestinationUniqueConflictNotSwallowed 目标行改成另一行
// 已占用的模型 ID 时必须报错并整体回滚，不能吞掉 UNIQUE 冲突假装成功。
func TestAISaveProviderDestinationUniqueConflictNotSwallowed(t *testing.T) {
	d := openDirtyAIDB(t, "dirty-conflict.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()

	first, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://vendor.invalid/v1", ModelID: "initial-model",
	})
	if err := d.AISaveProviderDestination(ctx, seed.ProviderID, seed.ModelRowID,
		"https://vendor.invalid/v1", "initial-model", first); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	// 第三个非空模型行占住 "taken-model"。
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_model(provider_id, model_id, display, context_limit, supports_tools, supports_thinking, enabled)
		VALUES(?, 'taken-model', '已占用', 0, 0, 0, 1)`, seed.ProviderID); err != nil {
		t.Fatal(err)
	}

	second, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://vendor.invalid/v2", ModelID: "taken-model",
	})
	err := d.AISaveProviderDestination(ctx, seed.ProviderID, seed.ModelRowID,
		"https://vendor.invalid/v2", "taken-model", second)
	if err == nil {
		t.Fatal("撞 UNIQUE(provider_id, model_id) 时必须返回错误")
	}
	if errors.Is(err, ErrAIModelRowNotFound) {
		t.Fatalf("期望 UNIQUE 冲突，实际是模型行缺失: %v", err)
	}

	var base string
	if err := d.SQL.QueryRow(`SELECT base_url FROM ai_provider WHERE id=?`, seed.ProviderID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != "https://vendor.invalid/v1" {
		t.Fatalf("失败后 base_url 被改: %q", base)
	}
	if got := d.AISetting(ctx, aiconfig.KeyEgressDestination, ""); got != first {
		t.Fatalf("失败后目的地快照被改: %q", got)
	}
	rows := aiModelRows(t, d, seed.ProviderID)
	if len(rows) != 3 || rows[0].Model != "initial-model" || rows[1].Model != "" || rows[2].Model != "taken-model" {
		t.Fatalf("失败后模型行被改: %+v", rows)
	}
}

// TestAISaveProviderDestinationRejectsMissingRow 没有模型行或传了失效的
// 行 id 时，必须显式报错，不能影响 base_url 与快照。
func TestAISaveProviderDestinationRejectsMissingRow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		modelRowID int64
		clearRows  bool
	}{
		{name: "没有模型行", modelRowID: 0, clearRows: true},
		{name: "行 id 已失效", modelRowID: 999999, clearRows: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDirtyAIDB(t, "dirty-missing.db")
			seed := seedAIDirtyData(t, d)
			ctx := context.Background()
			if tc.clearRows {
				if _, err := d.SQL.Exec(`DELETE FROM ai_model WHERE provider_id=?`, seed.ProviderID); err != nil {
					t.Fatal(err)
				}
			}
			dest, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
				BaseURL: "https://vendor.invalid/v9", ModelID: "model-x",
			})
			err := d.AISaveProviderDestination(ctx, seed.ProviderID, tc.modelRowID,
				"https://vendor.invalid/v9", "model-x", dest)
			if !errors.Is(err, ErrAIModelRowNotFound) {
				t.Fatalf("期望 ErrAIModelRowNotFound，实际: %v", err)
			}
			var base string
			if err := d.SQL.QueryRow(`SELECT base_url FROM ai_provider WHERE id=?`, seed.ProviderID).Scan(&base); err != nil {
				t.Fatal(err)
			}
			if base != "https://api.deepseek.com" {
				t.Fatalf("失败后 base_url 被改: %q", base)
			}
			if got := d.AISetting(ctx, aiconfig.KeyEgressDestination, ""); got != "" {
				t.Fatalf("失败后写入了目的地快照: %q", got)
			}
		})
	}
}

// TestAIJoinedViewsIgnoreDeletedAgentAndOrphanRegistrations 已删除 Agent 即使
// joined=1、仍有频道/地图登记，也不得被算成「已接入」。
func TestAIJoinedViewsIgnoreDeletedAgentAndOrphanRegistrations(t *testing.T) {
	d := openDirtyAIDB(t, "dirty-joined.db")
	seedAIDirtyData(t, d)
	ctx := context.Background()

	if err := d.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
		t.Fatalf("接入正常 Agent: %v", err)
	}
	joined, err := d.AIJoinedAgents(ctx)
	if err != nil {
		t.Fatalf("AIJoinedAgents: %v", err)
	}
	if len(joined) != 1 || joined[0].Key != "companion" {
		t.Fatalf("已接入名单 = %+v，期望只有 companion", joined)
	}

	subscribed, err := d.AIChannelSubscribed(ctx, dirtyOrphanChannel)
	if err != nil {
		t.Fatalf("AIChannelSubscribed: %v", err)
	}
	if len(subscribed) != 0 {
		t.Fatalf("已删除 Agent 仍出现在频道订阅: %v", subscribed)
	}
	maps, err := d.AIMapAgents(ctx, dirtyOrphanMapKey)
	if err != nil {
		t.Fatalf("AIMapAgents: %v", err)
	}
	if len(maps) != 0 {
		t.Fatalf("已删除 Agent 仍出现在地图订阅: %v", maps)
	}

	channels, err := d.AIListChannels(ctx)
	if err != nil {
		t.Fatalf("AIListChannels: %v", err)
	}
	for _, c := range channels {
		if c.Name == dirtyOrphanChannel && (c.Count != 0 || len(c.Agents) != 0) {
			t.Fatalf("孤儿频道登记被计入已接入: %+v", c)
		}
	}
}

// TestHistoricalSeedSettingStaysUnconfirmed 历史种子行（updated_ts=0）与
// 显式保存过的普通键都不再构成闸门：V2 只留急停。
func TestHistoricalSeedSettingStaysUnconfirmed(t *testing.T) {
	d := openDirtyAIDB(t, "dirty-settings.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()

	var seedTS, explicitTS float64
	if err := d.SQL.QueryRow(`SELECT updated_ts FROM ai_setting WHERE key=?`,
		aiconfig.KeyOutboundEnabled).Scan(&seedTS); err != nil {
		t.Fatal(err)
	}
	if seedTS != 0 {
		t.Fatalf("历史种子 updated_ts = %v，期望 0（夹具前提不成立）", seedTS)
	}
	if err := d.SQL.QueryRow(`SELECT updated_ts FROM ai_setting WHERE key=?`,
		dirtyExplicitSetting).Scan(&explicitTS); err != nil {
		t.Fatal(err)
	}
	if explicitTS <= 0 {
		t.Fatalf("显式保存键 updated_ts = %v，期望 >0", explicitTS)
	}

	lookup := func(key string) string { return d.AISetting(ctx, key, "") }
	if got := aiconfig.Egress(lookup); !got.Allowed {
		t.Fatalf("历史种子不得阻止发送，得到 (%v,%q)", got.Allowed, got.State)
	}
	view := aiconfig.Security(lookup)
	if !view.Egress.Allowed {
		t.Fatal("Security 视图把历史种子当成拦截条件")
	}
	if _, ok := view.Keys[aiconfig.KeyOutboundEnabled]; ok {
		t.Fatal("Security 视图不得再暴露 outbound_enabled")
	}
	_ = seed
}

// TestOrphanMigrationRegistrationTolerated 指向已不存在文件的登记行：
// 编号与当前目录不冲突时保留不动；与当前文件同号时由退役算法改名让位。
func TestOrphanMigrationRegistrationTolerated(t *testing.T) {
	t.Run("编号不冲突保留", func(t *testing.T) {
		d := openDirtyAIDB(t, "dirty-orphan-keep.db")
		seed := seedAIDirtyData(t, d)
		if err := d.Migrate(); err != nil {
			t.Fatalf("孤儿登记行导致 Migrate 失败: %v", err)
		}
		var name string
		if err := d.SQL.QueryRow(`SELECT name FROM schema_migrations WHERE version=?`,
			dirtyOrphanMigration).Scan(&name); err != nil {
			t.Fatalf("孤儿登记行被删除: %v", err)
		}
		if name != "migrations/99999_orphan_ai.sql" {
			t.Fatalf("孤儿登记行被改写: %q", name)
		}
		_ = seed
	})

	t.Run("同号孤儿由退役让位", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{})
		if _, err := db.SQL.Exec(`INSERT INTO schema_migrations(version, name, applied_at)
			VALUES(60, 'migrations/00060_missing_branch.sql', 0)`); err != nil {
			t.Fatalf("构造同号孤儿: %v", err)
		}
		entries := mergedMigrationEntries(t)
		if err := retireStaleMigrationRows(db.SQL, entries); err != nil {
			t.Fatalf("退役同号孤儿失败: %v", err)
		}
		var name string
		if err := db.SQL.QueryRow(`SELECT name FROM schema_migrations WHERE name LIKE 'retired/%'`).Scan(&name); err != nil {
			t.Fatalf("没有退役记录: %v", err)
		}
		if name != "retired/migrations/00060_missing_branch.sql" {
			t.Fatalf("退役名 = %q", name)
		}
	})
}

// TestAIEmergencyStateAndLiftContract 固定急停持久化口径：
//   - emergency_stop 与最近解除点在同一条 SQL 里读（同一快照）；
//   - 只有「非 0 → 0」才写解除点，重复保存同值不移动解除点；
//   - 缺键/空值与解除记录损坏必须可区分。
func TestAIEmergencyStateAndLiftContract(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "emergency-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dec := aiconfig.EvaluateEmergency(d.AIEmergencyState(ctx))
	if dec.Stopped || dec.Fault != "" || dec.LiftedTS != 0 {
		t.Fatalf("初始急停状态 = %+v，期望未停且无解除历史", dec)
	}

	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	reading := d.AIEmergencyState(ctx)
	if reading.StopRaw != "1" || reading.LiftedRaw != "" {
		t.Fatalf("保存停止后读取 = %+v，期望 stop=1 且不写解除点", reading)
	}

	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	reading = d.AIEmergencyState(ctx)
	lift1, err := strconv.ParseFloat(reading.LiftedRaw, 64)
	if err != nil || lift1 <= 0 {
		t.Fatalf("首次解除未写解除点: %q (%v)", reading.LiftedRaw, err)
	}

	// 重复保存同值：解除点不得移动（同值保存不是一次新的解除）。
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if got := d.AIEmergencyState(ctx).LiftedRaw; got != reading.LiftedRaw {
		t.Fatalf("重复保存同值移动了解除点: %q → %q", reading.LiftedRaw, got)
	}

	// 带空白/非法的停止值在读取端同样算停止：从这里写成 0 必须记解除点，
	// 否则停前工作会被当成「从未有解除历史」而重放。
	if _, err := d.SQL.ExecContext(ctx,
		`DELETE FROM ai_setting WHERE key=?`, aiconfig.KeyEmergencyStopLiftedTS); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_setting SET value='0 ' WHERE key=?`, aiconfig.KeyEmergencyStop); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	reading = d.AIEmergencyState(ctx)
	if reading.StopRaw != "0" {
		t.Fatalf("保存后急停值 = %q，期望规范化为 0", reading.StopRaw)
	}
	if v, err := strconv.ParseFloat(reading.LiftedRaw, 64); err != nil || v <= 0 {
		t.Fatalf("从带空白的停止值解除未写解除点: %q (%v)", reading.LiftedRaw, err)
	}

	if _, err := d.SQL.ExecContext(ctx, `UPDATE ai_setting SET value='broken' WHERE key=?`,
		aiconfig.KeyEmergencyStopLiftedTS); err != nil {
		t.Fatal(err)
	}
	dec = aiconfig.EvaluateEmergency(d.AIEmergencyState(ctx))
	if dec.Stopped || dec.Fault != "" || dec.Epoch != 4 {
		t.Fatalf("audit timestamp changed emergency decision: %+v", dec)
	}

	// 真实缺历史：两个键都缺失时按默认未停，不伪造解除点。
	if _, err := d.SQL.ExecContext(ctx,
		`DELETE FROM ai_setting WHERE key IN (?,?)`,
		aiconfig.KeyEmergencyStop, aiconfig.KeyEmergencyStopLiftedTS); err != nil {
		t.Fatal(err)
	}
	dec = aiconfig.EvaluateEmergency(d.AIEmergencyState(ctx))
	if dec.Stopped || dec.Fault != "" || dec.LiftedTS != 0 {
		t.Fatalf("缺键状态 = %+v，期望默认未停且缺历史", dec)
	}
}

// TestAIPendingResultsCarriesParentIntentEpoch 待投递结果必须同时带
// 父任务代次与结果生成时点。
func TestAIPendingResultsCarriesParentIntentEpoch(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "pending-intent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid, source_kind,
		                    text, state, deadline_ts, attempt, created_ts, participants, loc, intent_epoch)
		VALUES('channel','channel','t102','1','companion',2147483645,'system',
		       '','done',0,0,111,'[]','',1)`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_result(task_id,status,reply_text,contact_uid,created_ts)
		VALUES(?,'done','reply',2147483645,222)`, taskID); err != nil {
		t.Fatal(err)
	}
	list, err := d.AIPendingResults(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("待投递结果 = %d 条，期望 1", len(list))
	}
	if list[0].IntentEpoch != 1 || list[0].CreatedTS != 222 {
		t.Fatalf("时点 = task:%v result:%v，期望 1/222",
			list[0].IntentEpoch, list[0].CreatedTS)
	}
}
