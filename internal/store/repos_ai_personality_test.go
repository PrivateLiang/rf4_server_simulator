package store

// T-103 存储层回归：每 Agent 一条四维配置，原子修订、非法拒绝、
// 软删停用、同 key 恢复与代次重新确认。
// 期望值全部手写，不复用被测 SQL 生成。

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"rf4server/internal/aiconfig"
)

func openPersonalityDB(t *testing.T, name string) *DB {
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

// newPersonalityAgent 建一个正常 Agent，返回联系人 UID。
func newPersonalityAgent(t *testing.T, d *DB, key string) int64 {
	t.Helper()
	uid, restored, err := d.AICreateAgent(context.Background(), AIAgent{
		Key: key, Display: "钓友" + key, Role: "测试", ThinkingEffort: "off", ReplyMode: "social",
	}, "[]")
	if err != nil {
		t.Fatalf("建 Agent %s: %v", key, err)
	}
	if restored {
		t.Fatalf("新 key %s 不应走恢复路径", key)
	}
	return uid
}

// setGeneration 直接改运行态代次，模拟 GS 删除/恢复时推进代次。
func setGeneration(t *testing.T, d *DB, key string, generation int64) {
	t.Helper()
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_runtime_control(agent_key,generation,revision,tombstoned)
		VALUES(?,?,0,0)
		ON CONFLICT(agent_key) DO UPDATE SET generation=excluded.generation`, key, generation); err != nil {
		t.Fatalf("改代次: %v", err)
	}
}

func mustPersonality(t *testing.T, s, e, p, r int) aiconfig.PersonalityConfig {
	t.Helper()
	cfg := aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Sociability:   s, Exploration: e, Persistence: p, Regularity: r,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置本身非法: %v", err)
	}
	return cfg
}

// TestAIGetPersonalityMissingIsNeutral 缺值不是错误：四维 50，可参与派生。
func TestAIGetPersonalityMissingIsNeutral(t *testing.T) {
	d := openPersonalityDB(t, "personality-missing.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if got.Present || got.NeedsRepair || got.Inactive != "" {
		t.Fatalf("缺值状态不对: %+v", got)
	}
	want := aiconfig.DefaultPersonality()
	if got.Config != want {
		t.Fatalf("缺值应为中性默认 %+v，实际 %+v", want, got.Config)
	}
	if !got.Derivable() {
		t.Fatal("缺值应按中性默认参与派生")
	}
	if got.CurrentGeneration != 1 {
		t.Fatalf("新 Agent 代次应为 1，实际 %d", got.CurrentGeneration)
	}
}

// TestAISavePersonalityWritesRevisionAndGeneration 首次保存 = 修订 1、绑定当前代次。
func TestAISavePersonalityWritesRevisionAndGeneration(t *testing.T) {
	d := openPersonalityDB(t, "personality-save.db")
	newPersonalityAgent(t, d, "klee")
	ctx := context.Background()

	rev, err := d.AISavePersonality(ctx, "klee", 0, mustPersonality(t, 80, 20, 65, 40))
	if err != nil {
		t.Fatalf("首存失败: %v", err)
	}
	if rev != 1 {
		t.Fatalf("首次保存修订应为 1，实际 %d", rev)
	}
	got, err := d.AIGetPersonality(ctx, "klee")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || !got.Derivable() || got.Inactive != "" || got.NeedsRepair {
		t.Fatalf("读取状态不对: %+v", got)
	}
	if got.Config.Revision != 1 || got.Config.Generation != 1 || got.Config.SchemaVersion != 1 {
		t.Fatalf("落库版本/修订/代次不对: %+v", got.Config)
	}
	if got.Config.Sociability != 80 || got.Config.Exploration != 20 ||
		got.Config.Persistence != 65 || got.Config.Regularity != 40 {
		t.Fatalf("四维值不对: %+v", got.Config)
	}
	// 库里真的只有一条该 Agent 的值。
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_setting WHERE key=?`,
		aiPersonalityKey("klee")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("按 Agent 应只有一条配置，实际 %d 条", n)
	}
	// 按 Agent 隔离：另一个人读不到。
	newPersonalityAgent(t, d, "furina")
	other, err := d.AIGetPersonality(ctx, "furina")
	if err != nil {
		t.Fatal(err)
	}
	if other.Present || other.Config != aiconfig.DefaultPersonality() {
		t.Fatalf("别的 Agent 不应读到这条配置: %+v", other)
	}
}

// TestAISavePersonalityRejectsInvalidValues 显式非法拒绝保存，不静默钳制。
func TestAISavePersonalityRejectsInvalidValues(t *testing.T) {
	d := openPersonalityDB(t, "personality-invalid.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	for name, cfg := range map[string]aiconfig.PersonalityConfig{
		"上越界": {SchemaVersion: 1, Sociability: 101, Exploration: 50, Persistence: 50, Regularity: 50},
		"下越界": {SchemaVersion: 1, Sociability: 50, Exploration: -1, Persistence: 50, Regularity: 50},
	} {
		if _, err := d.AISavePersonality(ctx, "hutao", 0, cfg); !errors.Is(err, ErrAIPersonalityInvalid) {
			t.Fatalf("%s 应拒绝保存: %v", name, err)
		}
	}
	// 一次都没写进去（不是被钳制成 100/0 后落库）。
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_setting WHERE key=?`,
		aiPersonalityKey("hutao")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("非法值不得落库，实际写了 %d 条", n)
	}
}

// TestAISavePersonalityRevisionConflict 并发版本冲突：后到的提交被拒，库里保持先到的值。
func TestAISavePersonalityRevisionConflict(t *testing.T) {
	d := openPersonalityDB(t, "personality-conflict.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustPersonality(t, 10, 20, 30, 40)); err != nil {
		t.Fatal(err)
	}
	// 两个管理员都读到修订 1：甲先存成功，乙用同一个 base 提交必须失败。
	if _, err := d.AISavePersonality(ctx, "hutao", 1, mustPersonality(t, 60, 70, 80, 90)); err != nil {
		t.Fatalf("甲保存应成功: %v", err)
	}
	_, err := d.AISavePersonality(ctx, "hutao", 1, mustPersonality(t, 0, 0, 0, 0))
	if !errors.Is(err, ErrAIPersonalityConflict) {
		t.Fatalf("乙应收到修订冲突: %v", err)
	}
	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Sociability != 60 || got.Config.Revision != 2 {
		t.Fatalf("冲突提交不应覆盖甲的值: %+v", got.Config)
	}
	// 乙重新读取后再提交（base=2）即可成功。
	if _, err := d.AISavePersonality(ctx, "hutao", 2, mustPersonality(t, 5, 5, 5, 5)); err != nil {
		t.Fatalf("读新修订后应可保存: %v", err)
	}
}

// TestAIPersonalityCorruptValueNeedsRepair 坏值原样保留并标需修复，不参与派生。
func TestAIPersonalityCorruptValueNeedsRepair(t *testing.T) {
	d := openPersonalityDB(t, "personality-corrupt.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	const raw = `{"schema_version":1,"revision":9,"generation":1,"sociability":120,"exploration":50,"persistence":50,"regularity":50}`
	if _, err := d.SQL.Exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
		aiPersonalityKey("hutao"), raw); err != nil {
		t.Fatal(err)
	}
	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || !got.NeedsRepair || got.RepairReason == "" {
		t.Fatalf("坏值应标记需修复: %+v", got)
	}
	if got.Derivable() {
		t.Fatal("需修复的配置不得参与派生")
	}
	if got.Config.Sociability != 120 {
		t.Fatalf("越界值应原样带出供修复: %+v", got.Config)
	}
	// 存值没被偷偷改掉。
	var stored string
	if err := d.SQL.QueryRow(`SELECT value FROM ai_setting WHERE key=?`,
		aiPersonalityKey("hutao")).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != raw {
		t.Fatalf("坏值不得被静默改写: %s", stored)
	}
	// 读侧必须给出可提交修订：R-113 之前这里只判合法配置，坏记录一律按 0，
	// 导致按读到的修订修复反而报「当前修订 0」。
	if got.Config.Revision != 9 {
		t.Fatalf("坏记录的可提交修订应为 9，实际 %d", got.Config.Revision)
	}
	// 修复：用读侧给出的修订重写整条记录。
	rev, err := d.AISavePersonality(ctx, "hutao", got.Config.Revision, mustPersonality(t, 55, 55, 55, 55))
	if err != nil {
		t.Fatalf("按读侧修订修复失败: %v", err)
	}
	if rev != 10 {
		t.Fatalf("修复后修订应为 10，实际 %d", rev)
	}
	fixed, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if fixed.NeedsRepair || !fixed.Derivable() || fixed.Config.Sociability != 55 {
		t.Fatalf("修复后应可派生: %+v", fixed)
	}
}

// softDeleteAgent 复刻 GS 的删除应用事务：先逻辑删除，再推进代次并留墓碑。
func softDeleteAgent(t *testing.T, d *DB, key string) {
	t.Helper()
	if err := d.AIWithRuntimeTx(context.Background(), func(tx *AIRuntimeTx) error {
		if _, err := tx.DeleteAgent(key); err != nil {
			return err
		}
		_, err := tx.BumpControl(key, 1, true)
		return err
	}); err != nil {
		t.Fatalf("删除 %s: %v", key, err)
	}
}

// TestAIGetPersonalityInactiveAfterSoftDelete 软删后停用该代次配置，但保留存值。
func TestAIGetPersonalityInactiveAfterSoftDelete(t *testing.T) {
	d := openPersonalityDB(t, "personality-delete.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustPersonality(t, 70, 30, 60, 20)); err != nil {
		t.Fatal(err)
	}
	softDeleteAgent(t, d, "hutao")

	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if got.Inactive != AIPersonalityInactiveDeleted || got.Derivable() {
		t.Fatalf("软删后应停用: %+v", got)
	}
	// 存值仍在：恢复后还能看到管理员上次调的四维。
	if !got.Present || got.Config.Sociability != 70 {
		t.Fatalf("软删不应删掉四维存值: %+v", got)
	}
	// 已删除的 Agent 不接受新保存。
	if _, err := d.AISavePersonality(ctx, "hutao", 1, mustPersonality(t, 1, 2, 3, 4)); !errors.Is(err, ErrAIPersonalityInactive) {
		t.Fatalf("已删除 Agent 应拒绝保存: %v", err)
	}
}

// TestAISameKeyRestoreKeepsContactAndHistory 同 key 恢复沿用联系人与历史，
// 但旧代次四维不自动生效；保存一次即重新确认到当前代次。
func TestAISameKeyRestoreKeepsContactAndHistory(t *testing.T) {
	d := openPersonalityDB(t, "personality-restore.db")
	ctx := context.Background()
	uid := newPersonalityAgent(t, d, "ghostkey")

	// 旧联系人、旧回复与战绩历史。
	if err := d.AIAddMemory(ctx, "ghostkey", "star", "fish_a", "圆腹雅罗鱼", 2793, "level_011_ladoga_02"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_result(task_id,status,reply_text,raw_usage,err,created_ts,contact_uid)
		VALUES(0,'done','旧回复','','',?,?)`, nowTS(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AISavePersonality(ctx, "ghostkey", 0, mustPersonality(t, 88, 22, 66, 44)); err != nil {
		t.Fatal(err)
	}

	// 删除：同一事务里逻辑删除 + 推进代次 + 留墓碑（GS 生产路径）。
	softDeleteAgent(t, d, "ghostkey")

	// 恢复：同 key 建回，联系人 UID 必须复用。
	againUID, restored, err := d.AICreateAgent(ctx, AIAgent{
		Key: "ghostkey", Display: "回来的钓友", Role: "测试", ThinkingEffort: "off", ReplyMode: "social",
	}, "[]")
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if !restored {
		t.Fatal("同 key 应走恢复路径")
	}
	if againUID != uid {
		t.Fatalf("恢复后联系人 UID = %d，期望沿用 %d", againUID, uid)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM ai_memory WHERE agent_key='ghostkey'`); n != 1 {
		t.Fatalf("战绩历史应保留，实际 %d 条", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM ai_result WHERE contact_uid=`+strconv.FormatInt(uid, 10)); n != 1 {
		t.Fatalf("旧回复记录应保留，实际 %d 条", n)
	}

	// 旧代次四维不自动生效。
	got, err := d.AIGetPersonality(ctx, "ghostkey")
	if err != nil {
		t.Fatal(err)
	}
	if got.Inactive != AIPersonalityInactiveGeneration || got.Derivable() {
		t.Fatalf("恢复后旧代次配置应停用: %+v", got)
	}
	if got.CurrentGeneration != 2 {
		t.Fatalf("恢复后代次应为 2，实际 %d", got.CurrentGeneration)
	}
	if got.Config.Sociability != 88 {
		t.Fatalf("停用期间仍应能读到原值供确认: %+v", got.Config)
	}

	// 重新确认当前代次：带原修订保存一次即可生效。
	rev, err := d.AISavePersonality(ctx, "ghostkey", got.Config.Revision, mustPersonality(t, 88, 22, 66, 44))
	if err != nil {
		t.Fatalf("重新确认失败: %v", err)
	}
	active, err := d.AIGetPersonality(ctx, "ghostkey")
	if err != nil {
		t.Fatal(err)
	}
	if !active.Derivable() || active.Inactive != "" {
		t.Fatalf("重新确认后应生效: %+v", active)
	}
	if active.Config.Generation != 2 || active.Config.Revision != rev {
		t.Fatalf("应绑定当前代次并落新修订: %+v", active.Config)
	}
}

// TestAIPersonalityIgnoresLegacyBehaviourKeys 旧行为字段与旧概率不是四维输入。
func TestAIPersonalityIgnoresLegacyBehaviourKeys(t *testing.T) {
	d := openPersonalityDB(t, "personality-legacy.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustPersonality(t, 40, 60, 50, 70)); err != nil {
		t.Fatal(err)
	}
	before, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	// 把旧字段与旧概率全部改成极端值。
	if err := d.AISetAgentBehavior(ctx, "hutao", "mention", 100); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{
		"roundtable_chime_chance":        "0.99",
		"roundtable_on_speech_chance":    "0",
		"ai_fishing_talk_super_chance":   "0",
		"ai_catch_star_chance":           "1",
		"ai_actor_switch_chance":         "1",
		"ai_actor_rest_chance":           "0",
		"ai_fishing_roundtable_chance":   "1",
		"ai_actor_fishing_session_min_s": "300",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if after.Config != before.Config {
		t.Fatalf("旧字段变化不应影响四维真源:\nbefore=%+v\nafter =%+v", before.Config, after.Config)
	}
}

// TestAIPersonalityKeyPerAgent 键按 Agent 区分，且不与全局开关撞键。
func TestAIPersonalityKeyPerAgent(t *testing.T) {
	if aiPersonalityKey("a") == aiPersonalityKey("ab") {
		t.Fatal("不同 Agent 的键不能相同")
	}
	if aiPersonalityKey("a") == aiconfig.KeyEmergencyStop || aiPersonalityKey("a") == aiconfig.KeyOutboundEnabled {
		t.Fatal("性格键不能与全局开关撞键")
	}
}

// countRows 是一个手写期望值的最小查询助手。
func countRows(t *testing.T, d *DB, query string) int {
	t.Helper()
	var n int
	if err := d.SQL.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("查询失败 %s: %v", query, err)
	}
	return n
}

// ---- R-113 反证回归：未知 schema、缺维度与修复修订口径 ----

// TestAIGetPersonalityMissingDimensionNeedsRepair 缺必需维度按需修复处理，不得悄悄变 0 后参与派生。
func TestAIGetPersonalityMissingDimensionNeedsRepair(t *testing.T) {
	d := openPersonalityDB(t, "personality-missing-dim.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	const raw = `{"schema_version":1,"revision":7,"generation":1,"sociability":60,"exploration":50,"persistence":50}`
	if _, err := d.SQL.Exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
		aiPersonalityKey("hutao"), raw); err != nil {
		t.Fatal(err)
	}
	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present {
		t.Fatal("存值在场时 Present 应为 true")
	}
	if !got.NeedsRepair || got.RepairReason == "" {
		t.Fatalf("缺 regularity 应标需修复: %+v", got)
	}
	if got.Derivable() {
		t.Fatal("需修复的配置不得参与派生（不能把缺失当合法 0）")
	}
	if got.Raw != raw {
		t.Fatalf("原值应可查: %q", got.Raw)
	}
	if got.Config.Revision != 7 {
		t.Fatalf("读侧应给出可提交修订 7，实际 %d", got.Config.Revision)
	}
}

// TestAISavePersonalityRejectsUnknownSchema 显式未知版本必须拒绝，且不得写入任何数据。
func TestAISavePersonalityRejectsUnknownSchema(t *testing.T) {
	d := openPersonalityDB(t, "personality-schema.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	unknown := aiconfig.PersonalityConfig{
		SchemaVersion: 2,
		Sociability:   50, Exploration: 50, Persistence: 50, Regularity: 50,
	}
	if _, err := d.AISavePersonality(ctx, "hutao", 0, unknown); !errors.Is(err, ErrAIPersonalityInvalid) {
		t.Fatalf("未知 schema_version=2 应拒绝保存: %v", err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM ai_setting WHERE key='ai_personality:hutao'`); n != 0 {
		t.Fatalf("拒绝保存后不得留下数据，实际 %d 条", n)
	}
	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if got.Present || got.Config != aiconfig.DefaultPersonality() {
		t.Fatalf("拒绝保存后应仍是缺值: %+v", got)
	}

	// 当前支持的合法形状仍可保存。
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustPersonality(t, 50, 50, 50, 50)); err != nil {
		t.Fatalf("当前版本应可保存: %v", err)
	}
	ok, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Derivable() || ok.Config.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		t.Fatalf("合法保存后应生效: %+v", ok)
	}
}

// TestAIPersonalityRepairRevisionContract 读出的修订必须能直接用于修复：
// 修复成功、陈旧修订被拒绝、保存失败原值不变。
func TestAIPersonalityRepairRevisionContract(t *testing.T) {
	d := openPersonalityDB(t, "personality-repair.db")
	newPersonalityAgent(t, d, "hutao")
	ctx := context.Background()

	const raw = `{"schema_version":1,"revision":7,"generation":1,"sociability":120,"exploration":50,"persistence":50,"regularity":50}`
	if _, err := d.SQL.Exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
		aiPersonalityKey("hutao"), raw); err != nil {
		t.Fatal(err)
	}
	got, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsRepair || got.Derivable() {
		t.Fatalf("越界值应需修复且不可派生: %+v", got)
	}
	if got.Config.Revision != 7 {
		t.Fatalf("读侧应给出可提交修订 7，实际 %d", got.Config.Revision)
	}

	// 陈旧修订被拒绝，原值一个字不改。
	if _, err := d.AISavePersonality(ctx, "hutao", 6, mustPersonality(t, 55, 55, 55, 55)); !errors.Is(err, ErrAIPersonalityConflict) {
		t.Fatalf("陈旧修订应报冲突: %v", err)
	}
	var stored string
	if err := d.SQL.QueryRow(`SELECT value FROM ai_setting WHERE key=?`,
		aiPersonalityKey("hutao")).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != raw {
		t.Fatalf("冲突后原值不得改动: %s", stored)
	}

	// 用读侧给出的修订修复成功。
	rev, err := d.AISavePersonality(ctx, "hutao", got.Config.Revision, mustPersonality(t, 55, 55, 55, 55))
	if err != nil {
		t.Fatalf("按读侧修订 7 修复应成功: %v", err)
	}
	if rev != 8 {
		t.Fatalf("修复后修订应为 8，实际 %d", rev)
	}
	fixed, err := d.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if fixed.NeedsRepair || !fixed.Derivable() || fixed.Config.Revision != 8 {
		t.Fatalf("修复后应可派生且修订 8: %+v", fixed)
	}

	// 修复后再用旧修订提交仍被拒绝。
	if _, err := d.AISavePersonality(ctx, "hutao", 7, mustPersonality(t, 1, 2, 3, 4)); !errors.Is(err, ErrAIPersonalityConflict) {
		t.Fatalf("修复后的旧修订应报冲突: %v", err)
	}
}
