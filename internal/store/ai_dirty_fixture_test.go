package store

// 脏数据夹具：所有涉及 ai_* 表的新增/修改用例都从真实库出现过的形态起步。
// 这里只写测试，不参与生产构建。
//
// 覆盖五类（T-016 §3.3）：
//   1. ai_model 同 provider 多行（有效行 + 空 model_id 种子遗留）；
//   2. ai_model 空值行不被当成有效模型；
//   3. ai_agent 已逻辑删除但 joined/enabled/actor_enabled 仍为 1；
//   4. schema_migrations 登记行指向已不存在的文件（孤儿）；
//   5. ai_setting 历史种子行 updated_ts=0 与显式保存过的键并存。
//
// 另外附带孤儿登记（ai_channel_agent / ai_map_agent / ai_actor_state 指向
// 已删除 Agent），用于验证「已接入」口径不把脏登记算进去。

import (
	"context"
	"testing"
)

type dirtyAISeed struct {
	ProviderID      int64
	ModelRowID      int64 // 有效模型行（model_id 非空）
	BlankModelRowID int64 // 历史种子遗留的空 model_id 行
	GhostAgentID    int64 // 已逻辑删除但 joined/enabled 仍为 1 的 Agent
}

const (
	dirtyGhostAgentKey   = "ghost"
	dirtyOrphanChannel   = "dirty_orphan_room"
	dirtyOrphanMapKey    = "level_999_orphan"
	dirtyExplicitSetting = "dirty_explicit_marker"
	dirtyOrphanMigration = int64(900099)
)

func seedAIDirtyData(t *testing.T, d *DB) dirtyAISeed {
	t.Helper()
	ctx := context.Background()
	var seed dirtyAISeed

	if err := d.SQL.QueryRow(
		`SELECT id FROM ai_provider WHERE enabled=1 ORDER BY id LIMIT 1`).Scan(&seed.ProviderID); err != nil {
		t.Fatalf("读取 provider: %v", err)
	}

	// 1) 真实库形态：先把迁移种子的空行填成有效模型，再残留一行空 model_id
	// （00900_ai_bridge.sql 的 INSERT OR IGNORE 种子在有效行存在时会再插一行空行）。
	if _, err := d.SQL.Exec(`
		UPDATE ai_model SET model_id='deepseek-flash'
		WHERE id=(
			SELECT MIN(id) FROM ai_model WHERE provider_id=? AND model_id=''
		)`, seed.ProviderID); err != nil {
		t.Fatalf("构造有效模型行: %v", err)
	}
	res, err := d.SQL.Exec(`
		INSERT INTO ai_model(provider_id, model_id, display, context_limit, supports_tools, supports_thinking, enabled)
		VALUES(?, '', '历史种子遗留（空模型）', 0, 1, 1, 1)`, seed.ProviderID)
	if err != nil {
		t.Fatalf("构造空模型行: %v", err)
	}
	if seed.BlankModelRowID, err = res.LastInsertId(); err != nil {
		t.Fatalf("读取空模型行 id: %v", err)
	}
	if err := d.SQL.QueryRow(`
		SELECT id FROM ai_model WHERE provider_id=? AND model_id='deepseek-flash'
		ORDER BY id LIMIT 1`, seed.ProviderID).Scan(&seed.ModelRowID); err != nil {
		t.Fatalf("读取有效模型行 id: %v", err)
	}

	// 3) 已逻辑删除、但 joined/enabled/actor_enabled 都还留着 1 的 Agent。
	res, err = d.SQL.Exec(`
		INSERT INTO ai_agent(key, display, role, contact_uid, prompt, tools_json,
		                     thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		                     reply_mode, talkativeness, deleted_ts)
		VALUES(?, '幽灵助手', '测试', 2147483639, '', '[]', 0, 'off', 1, 1, 1, 'social', 50, ?)`,
		dirtyGhostAgentKey, nowTS())
	if err != nil {
		t.Fatalf("构造已删除 Agent: %v", err)
	}
	if seed.GhostAgentID, err = res.LastInsertId(); err != nil {
		t.Fatalf("读取已删除 Agent id: %v", err)
	}
	// 孤儿登记：已删除 Agent 仍留在频道订阅、地图订阅与运行态里。
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_channel_agent(channel, agent_id, enabled, priority, cooldown_ms, added_by, added_ts)
		VALUES(?, ?, 1, 100, 8000, 'test', 0)`, dirtyOrphanChannel, seed.GhostAgentID); err != nil {
		t.Fatalf("构造孤儿频道登记: %v", err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_map_agent(map_key, agent_id, enabled, priority, cooldown_ms, added_by, added_ts)
		VALUES(?, ?, 1, 100, 8000, 'test', 0)`, dirtyOrphanMapKey, seed.GhostAgentID); err != nil {
		t.Fatalf("构造孤儿地图登记: %v", err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_actor_state(agent_key, loc, loc_name, activity, updated_ts)
		VALUES(?, ?, '孤儿湖', 'fishing', 0)`, dirtyGhostAgentKey, dirtyOrphanMapKey); err != nil {
		t.Fatalf("构造孤儿运行态: %v", err)
	}

	// 4) 孤儿迁移登记行：名字不在当前迁移目录，编号也不与任何当前文件冲突。
	if _, err := d.SQL.Exec(`
		INSERT INTO schema_migrations(version, name, applied_at)
		VALUES(?, 'migrations/99999_orphan_ai.sql', 0)`, dirtyOrphanMigration); err != nil {
		t.Fatalf("构造孤儿迁移登记: %v", err)
	}

	// 5) 历史种子行（outbound_enabled 的 updated_ts=0）由迁移本身写入；
	// 这里再写一个显式保存过的普通键作为对照。
	if err := d.AISetSettings(ctx, map[string]string{dirtyExplicitSetting: "1"}); err != nil {
		t.Fatalf("构造显式保存过的键: %v", err)
	}
	return seed
}
