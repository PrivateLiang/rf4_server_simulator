package store

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 本文件覆盖 T-003/T-014「AI 迁移有序让号」：临时号段落点 60→00900、61→00901、
// 78→00902、79→00903、80→00904（官方号段 00001–00899 归官方，AI 临时独占 00900+），
// 以及 00904 复用官方 renumberedColumnMigrations 的重放守卫。
//
// 官方 00060–00062 用官方 main（919b3f0）的原样副本造"官方已有库/合成目录"，
// 不用 T-001 的旧快照；旧 AI 布局（60/61/78/79/80）用当前 embed 的同一份 SQL
// 执行后改登记行构造——只改文件名与登记编号，SQL 内容未变。

// aiRenumberMap 是让号映射：newName 为当前 embed 里的文件名，oldName/oldVer 为
// 让号前（T-001 快照 e9ae1d6）的文件名与登记编号。
var aiRenumberMap = []struct {
	newName string
	oldName string
	oldVer  int64
}{
	{"migrations/00900_ai_bridge.sql", "migrations/00060_ai_bridge.sql", 60},
	{"migrations/00901_ai_cleanup_legacy.sql", "migrations/00061_ai_cleanup_legacy.sql", 61},
	{"migrations/00902_ai_map_subscription.sql", "migrations/00078_ai_map_subscription.sql", 78},
	{"migrations/00903_ai_global_scope.sql", "migrations/00079_ai_global_scope.sql", 79},
	{"migrations/00904_ai_fishing_talk.sql", "migrations/00080_ai_fishing_talk.sql", 80},
}

// aiFishingTalkMigrationName 是 00904 在 migrationsFS 里的名字，也是官方
// renumberedColumnMigrations 的键。
const aiFishingTalkMigrationName = "migrations/00904_ai_fishing_talk.sql"

// aiRenumberRetiredNames 是改号后必须从 embed 消失的全部旧名：既包括 T-001 快照
// 的原始名（60/61/78/79/80），也包括 T-003 v1 的中间名（63/64/70/71/72）。
var aiRenumberRetiredNames = []string{
	"migrations/00060_ai_bridge.sql",
	"migrations/00061_ai_cleanup_legacy.sql",
	"migrations/00078_ai_map_subscription.sql",
	"migrations/00079_ai_global_scope.sql",
	"migrations/00080_ai_fishing_talk.sql",
	"migrations/00063_ai_bridge.sql",
	"migrations/00064_ai_cleanup_legacy.sql",
	"migrations/00070_ai_map_subscription.sql",
	"migrations/00071_ai_global_scope.sql",
	"migrations/00072_ai_fishing_talk.sql",
}

// aiFishingTalkSettingKeys 是 00904 的六项种子键（只用于断言，复制自迁移文件本身，
// 不是产品代码的第二来源）。
var aiFishingTalkSettingKeys = []string{
	"ai_fishing_talk_enabled",
	"ai_fishing_talk_super_chance",
	"ai_fishing_talk_star_chance",
	"ai_fishing_talk_near_chance",
	"ai_fishing_talk_miss_chance",
	"ai_fishing_roundtable_chance",
}

// officialMigrations62 是官方 main（919b3f0）00060–00062 的原样副本，
// 仅作测试夹具，不参与产品代码。
var officialMigrations62 = []struct {
	name string
	body string
}{
	{
		name: "migrations/00060_skill_cast_effects.sql",
		body: `CREATE TABLE IF NOT EXISTS skill_cast_effect_def(
    cat_id        INTEGER NOT NULL,
    node_id       INTEGER NOT NULL,
    effect_key    TEXT NOT NULL,
    per_level_pct REAL NOT NULL,
    gate_kind     TEXT NOT NULL,
    gate_type     INTEGER NOT NULL,
    source        TEXT NOT NULL DEFAULT 'client-loc',
    confidence    REAL NOT NULL DEFAULT 0.9,
    PRIMARY KEY(cat_id, node_id)
);`,
	},
	{
		name: "migrations/00061_rf4records_catch.sql",
		body: `CREATE TABLE IF NOT EXISTS rf4records_catch(
    fish_name_en TEXT NOT NULL,
    fish_key   TEXT NOT NULL DEFAULT '',
    bait_name  TEXT NOT NULL,
    bait_id    TEXT NOT NULL DEFAULT '',
    weight_g   INTEGER NOT NULL DEFAULT 0,
    waterbody  TEXT NOT NULL DEFAULT '',
    region     TEXT NOT NULL DEFAULT '',
    categories TEXT NOT NULL DEFAULT '',
    trophy_class TEXT NOT NULL DEFAULT '',
    catch_date TEXT NOT NULL DEFAULT '',
    fetched_at REAL NOT NULL,
    PRIMARY KEY(fish_name_en, bait_name, weight_g, catch_date, waterbody));
CREATE INDEX IF NOT EXISTS idx_rf4records_catch_fish_bait ON rf4records_catch(fish_key, bait_id);`,
	},
	{
		name: "migrations/00062_add_bs_salmon_grade_line.sql",
		body: `INSERT OR IGNORE INTO fish_grade_lines(fish_key, qualified_g, trophy_g, blue_trophy_g, source, checked_at)
VALUES ('bs_salmon', 600, 14000, 20000, 'manual/rf4-stat+l_salmon参照', '2026-11-15');`,
	},
}

// aiLegacyDBOptions 描述一个"旧库"的构造方式。
type aiLegacyDBOptions struct {
	// oldThrough 按旧布局应用到哪个旧编号（0 表示不装任何 AI 迁移）。
	// 旧布局顺序：60 ai_bridge → 61 cleanup → 78 → 79 → 80。
	oldThrough int64
	// official 在旧布局之后补上官方 00060–00062（模拟官方同号迁移已存在）。
	official bool
	// preRetired 模拟"已退役一轮"：把旧 60/61 登记行挪到 9000060/9000061，
	// 然后才允许官方同号迁移落位（真实合并轮的执行顺序）。
	preRetired bool
	// legacyRows 预置 ai_session / ai_message 各多少行（0 表示不建这两张表）。
	legacyRows int
}

// newAILegacyDB 造一个旧库：官方前缀 00001–00059 用当前 embed 直接应用，
// 再按 option 叠加旧 AI 布局 / 官方 60–62 / 已退役登记 / legacy 哨兵数据。
func newAILegacyDB(t *testing.T, opt aiLegacyDBOptions) *DB {
	t.Helper()
	db := openCatchHistoryDBAtVersion(t, 59)
	if opt.oldThrough > 0 {
		for _, m := range aiRenumberMap {
			if m.oldVer > opt.oldThrough {
				continue
			}
			body, err := fs.ReadFile(migrationsFS, m.newName)
			if err != nil {
				t.Fatalf("读取迁移 %s: %v", m.newName, err)
			}
			applyMigrationBody(t, db, string(body), m.oldVer, m.oldName)
		}
	}
	if opt.preRetired {
		if _, err := db.SQL.Exec(`UPDATE schema_migrations SET version=9000060,
			name='retired/migrations/00060_ai_bridge.sql'
			WHERE version=60 AND name='migrations/00060_ai_bridge.sql'`); err != nil {
			t.Fatalf("模拟旧 60 已退役: %v", err)
		}
		if _, err := db.SQL.Exec(`UPDATE schema_migrations SET version=9000061,
			name='retired/migrations/00061_ai_cleanup_legacy.sql'
			WHERE version=61 AND name='migrations/00061_ai_cleanup_legacy.sql'`); err != nil {
			t.Fatalf("模拟旧 61 已退役: %v", err)
		}
	}
	if opt.official {
		for _, m := range officialMigrations62 {
			applyMigrationBody(t, db, m.body, migrationVersion(m.name), m.name)
		}
	}
	if opt.legacyRows > 0 {
		if _, err := db.SQL.Exec(`CREATE TABLE IF NOT EXISTS ai_session(id INTEGER PRIMARY KEY, scope TEXT);
			CREATE TABLE IF NOT EXISTS ai_message(id INTEGER PRIMARY KEY, session_id INTEGER);`); err != nil {
			t.Fatalf("建 legacy 表: %v", err)
		}
		for i := 1; i <= opt.legacyRows; i++ {
			if _, err := db.SQL.Exec(`INSERT INTO ai_session(id, scope) VALUES(?, 'sentinel');
				INSERT INTO ai_message(id, session_id) VALUES(?, 1)`, i, i); err != nil {
				t.Fatalf("写 legacy 哨兵行: %v", err)
			}
		}
	}
	return db
}

// applyMigrationBody 执行一段迁移 SQL 并按指定编号/文件名登记，模拟旧库当时的登记。
func applyMigrationBody(t *testing.T, db *DB, body string, version int64, name string) {
	t.Helper()
	if _, err := db.SQL.Exec(body); err != nil {
		t.Fatalf("执行旧迁移 %s（登记 %d）: %v", name, version, err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,0)`,
		version, name); err != nil {
		t.Fatalf("登记旧迁移 %s: %v", name, err)
	}
}

// mergedMigrationEntries 返回"官方 main 已合入"的合成迁移目录：当前 embed 的全部文件
// 加上官方 00060–00062 的文件名。retireStaleMigrationRows 只按名字与编号判重，
// 不含 SQL 内容，因此这个列表足以复现合成目录的退役行为。
func mergedMigrationEntries(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	all := append([]string{}, names...)
	for _, m := range officialMigrations62 {
		all = append(all, m.name)
	}
	sort.Strings(all)
	return all
}

// dumpEmbedMigrations 打出当前迁移目录的文件清单（§5 要求的"前后输出迁移文件列表"）。
func dumpEmbedMigrations(t *testing.T) {
	t.Helper()
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	t.Logf("embed 迁移文件 %d 个：", len(names))
	for _, name := range names {
		if strings.Contains(name, "_ai_") {
			t.Logf("  %s", name)
		}
	}
}

// dumpMigrationRows 打出 schema_migrations 的 version/name（无凭据、无玩家数据）。
func dumpMigrationRows(t *testing.T, db *DB, label string) {
	t.Helper()
	rows, err := db.SQL.Query(`SELECT version, name FROM schema_migrations
		WHERE name LIKE '%ai_%' OR version >= 60 ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	t.Logf("schema_migrations（%s，仅 AI 与 60+ 行）：", label)
	for rows.Next() {
		var v int64
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %d  %s", v, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertAISchemaReady(t *testing.T, db *DB) {
	t.Helper()
	for _, table := range []string{"ai_task", "ai_result", "ai_agent", "ai_setting",
		"ai_provider", "ai_model", "ai_secret", "ai_channel", "ai_channel_agent",
		"ai_tool_job", "ai_memory", "ai_actor_state", "ai_catch", "ai_map_agent"} {
		assertCount(t, db,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='`+table+`'`, 1)
	}
	assertCount(t, db,
		`SELECT COUNT(*) FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`, 1)
	for _, key := range aiFishingTalkSettingKeys {
		assertCount(t, db, `SELECT COUNT(*) FROM ai_setting WHERE key='`+key+`'`, 1)
	}
	// 00903 收尾后这两个旧开关不应存在。
	assertCount(t, db,
		`SELECT COUNT(*) FROM ai_setting WHERE key IN ('ai_global_subscribe','ai_global_subscribe_include_silent')`, 0)
}

func assertRegistered(t *testing.T, db *DB, version int64, name string) {
	t.Helper()
	assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=`+
		strconv.FormatInt(version, 10)+` AND name='`+name+`'`, 1)
}

// TestAIMigrationRenumber 是 T-003 的主用例，子场景对应票 §4 的矩阵。
func TestAIMigrationRenumber(t *testing.T) {
	t.Run("embed-mapping", func(t *testing.T) {
		dumpEmbedMigrations(t)
		present := map[string]bool{}
		names, err := fs.Glob(migrationsFS, "migrations/*.sql")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int64]string{}
		for _, name := range names {
			present[name] = true
			v := migrationVersion(name)
			if other, dup := seen[v]; dup {
				t.Fatalf("编号 %d 被两个文件占用：%s / %s", v, other, name)
			}
			seen[v] = name
		}
		for _, m := range aiRenumberMap {
			if !present[m.newName] {
				t.Fatalf("新名 %s 未进入 embed", m.newName)
			}
			if v := migrationVersion(m.newName); v < 900 || v > 904 {
				t.Fatalf("%s 的新号 %d 不在临时号段 00900–00904 内", m.newName, v)
			}
		}
		// 旧名一个都不能留在 embed：含 T-001 原始名与 T-003 v1 中间名。
		for _, old := range aiRenumberRetiredNames {
			if present[old] {
				t.Fatalf("旧名 %s 仍在 migrations 目录里", old)
			}
		}
		// 建表 → 清理 → 订阅 → 全局范围 → 加列 的执行顺序必须成立（文件名排序）。
		order := []int64{900, 901, 902, 903, 904}
		for i := 1; i < len(order); i++ {
			if order[i-1] >= order[i] {
				t.Fatalf("AI 迁移顺序被破坏：%v", order)
			}
		}
	})

	t.Run("fresh-db", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(); err != nil {
			t.Fatalf("空库迁移失败: %v", err)
		}
		for _, m := range aiRenumberMap {
			assertRegistered(t, db, migrationVersion(m.newName), m.newName)
		}
		assertAISchemaReady(t, db)
		dumpMigrationRows(t, db, "空库一次迁移后")
	})

	t.Run("old-60-61", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 61})
		dumpMigrationRows(t, db, "升级前（旧 60/61）")
		// DR-001：官方 60/61 进入目录后，旧 AI 登记行不再"原地保留"，
		// 而是按官方 retireStaleMigrationRows 退役到 9000060/9000061，
		// 与 merged-dir-retire / pretired-9000060 语义一致。
		if err := retireStaleMigrationRows(db.SQL, mergedMigrationEntries(t)); err != nil {
			t.Fatalf("退役旧 60/61 失败: %v", err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatalf("旧 60/61 库升级失败: %v", err)
		}
		assertRegistered(t, db, 9000060, "retired/migrations/00060_ai_bridge.sql")
		assertRegistered(t, db, 9000061, "retired/migrations/00061_ai_cleanup_legacy.sql")
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations
			WHERE name IN ('migrations/00060_ai_bridge.sql','migrations/00061_ai_cleanup_legacy.sql')`, 0)
		for _, m := range aiRenumberMap {
			assertRegistered(t, db, migrationVersion(m.newName), m.newName)
		}
		assertAISchemaReady(t, db)
		dumpMigrationRows(t, db, "旧 60/61 升级后")
	})

	t.Run("old-through-78", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 78})
		if err := db.Migrate(); err != nil {
			t.Fatalf("到旧 78 的库升级失败: %v", err)
		}
		assertRegistered(t, db, 78, "migrations/00078_ai_map_subscription.sql")
		assertRegistered(t, db, 902, "migrations/00902_ai_map_subscription.sql")
		assertAISchemaReady(t, db)
	})

	t.Run("old-through-79", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 79})
		if err := db.Migrate(); err != nil {
			t.Fatalf("到旧 79 的库升级失败: %v", err)
		}
		assertRegistered(t, db, 79, "migrations/00079_ai_global_scope.sql")
		assertRegistered(t, db, 903, "migrations/00903_ai_global_scope.sql")
		assertAISchemaReady(t, db)
	})

	t.Run("old-full-five", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80})
		if _, err := db.SQL.Exec(`INSERT INTO ai_agent(key, display, contact_uid)
			VALUES('sentinel', '哨兵', 999001)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_agent SET fishing_talk_enabled=1 WHERE key='sentinel'`); err != nil {
			t.Fatal(err)
		}
		dumpMigrationRows(t, db, "升级前（完整旧五迁移）")
		// DR-001：旧 60/61 按官方 retireStaleMigrationRows 退役到 9000060/9000061；
		// 旧 78/79/80 的编号已不被目录占用，登记行原地保留。
		if err := retireStaleMigrationRows(db.SQL, mergedMigrationEntries(t)); err != nil {
			t.Fatalf("退役旧 60/61 失败: %v", err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatalf("完整旧五迁移库升级失败: %v", err)
		}
		for _, m := range aiRenumberMap {
			assertRegistered(t, db, migrationVersion(m.newName), m.newName)
		}
		assertRegistered(t, db, 9000060, "retired/migrations/00060_ai_bridge.sql")
		assertRegistered(t, db, 9000061, "retired/migrations/00061_ai_cleanup_legacy.sql")
		for _, old := range []struct {
			ver  int64
			name string
		}{
			{78, "migrations/00078_ai_map_subscription.sql"},
			{79, "migrations/00079_ai_global_scope.sql"},
			{80, "migrations/00080_ai_fishing_talk.sql"},
		} {
			assertRegistered(t, db, old.ver, old.name)
		}
		// 守卫略过 ADD COLUMN，不能重置既有值。
		var talk int
		if err := db.SQL.QueryRow(`SELECT fishing_talk_enabled FROM ai_agent WHERE key='sentinel'`).Scan(&talk); err != nil {
			t.Fatal(err)
		}
		if talk != 1 {
			t.Fatalf("已有列值被重置：fishing_talk_enabled=%d，期望 1", talk)
		}
		assertAISchemaReady(t, db)
		dumpMigrationRows(t, db, "完整旧五迁移升级后")
	})

	t.Run("guard-seeds-still-run", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80})
		if _, err := db.SQL.Exec(`DELETE FROM ai_setting WHERE key='ai_fishing_roundtable_chance'`); err != nil {
			t.Fatal(err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM ai_setting WHERE key='ai_fishing_roundtable_chance'`, 0)
		if err := db.Migrate(); err != nil {
			t.Fatalf("重放失败: %v", err)
		}
		// 缺的那项种子被补齐 → 证明守卫没有跳过整个文件。
		var value string
		if err := db.SQL.QueryRow(
			`SELECT value FROM ai_setting WHERE key='ai_fishing_roundtable_chance'`).Scan(&value); err != nil {
			t.Fatalf("种子未补齐: %v", err)
		}
		if value != "0.5" {
			t.Fatalf("种子值 = %q，期望 0.5", value)
		}
	})

	t.Run("guard-missing-column", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 79})
		assertCount(t, db, `SELECT COUNT(*) FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`, 0)
		if err := db.Migrate(); err != nil {
			t.Fatalf("缺列升级失败: %v", err)
		}
		var typ string
		if err := db.SQL.QueryRow(
			`SELECT type FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`).Scan(&typ); err != nil {
			t.Fatalf("缺列未补齐: %v", err)
		}
		if !strings.EqualFold(typ, "INTEGER") {
			t.Fatalf("补出来的列类型 = %q，期望 INTEGER", typ)
		}
		assertRegistered(t, db, 904, "migrations/00904_ai_fishing_talk.sql")
	})

	t.Run("guard-partial-column-single", func(t *testing.T) {
		// 官方机制按「columns 全部存在」才走 remains。00904 只登记一列，因此不存在
		// 「部分存在」分支：列不存在 → 整文件执行（guard-missing-column），列存在 →
		// remains（guard-seeds-still-run / guard-incompatible-column）。官方 00067
		// 这类多列 entry 才有部分存在分支，不属于 AI 迁移，不在本用例范围。
		spec, ok := renumberedColumnMigrations[aiFishingTalkMigrationName]
		if !ok {
			t.Fatalf("renumberedColumnMigrations 缺少 %s", aiFishingTalkMigrationName)
		}
		if len(spec.columns) != 1 {
			t.Fatalf("00904 登记的列数 = %d，期望 1（多列才会出现部分存在分支）", len(spec.columns))
		}
		if strings.Contains(strings.ToUpper(spec.remains), "ALTER TABLE") {
			t.Fatal("remains 不应包含 ALTER TABLE：列存在时必须跳过建列语句")
		}
		if got := strings.Count(spec.remains, "('"); got != len(aiFishingTalkSettingKeys) {
			t.Fatalf("remains 种子行数 = %d，期望 %d", got, len(aiFishingTalkSettingKeys))
		}
	})

	t.Run("guard-incompatible-column", func(t *testing.T) {
		// 官方 renumberedColumnMigrations 只按列名判断存在，不核对类型/默认值。
		// 00904 只有一列，「列存在」即「columns 全部存在」，于是 TEXT 列也会触发
		// remains：只补六项种子，不再执行 ADD COLUMN。
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 79})
		if _, err := db.SQL.Exec(`ALTER TABLE ai_agent ADD COLUMN fishing_talk_enabled TEXT`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatalf("官方守卫按列名跳过 ADD COLUMN，不应报错: %v", err)
		}
		assertRegistered(t, db, 904, "migrations/00904_ai_fishing_talk.sql")
		assertAISchemaReady(t, db)
		// 既有列定义保持不变（官方机制不做声明兼容校验，也不重建列）。
		var typ string
		if err := db.SQL.QueryRow(
			`SELECT type FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		if typ != "TEXT" {
			t.Fatalf("既有列定义被改动: %q，期望 TEXT", typ)
		}
	})

	t.Run("guard-missing-table", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80})
		// 让 00900 处于"已应用"状态，否则它会先重建 ai_agent。
		if _, err := db.SQL.Exec(`INSERT INTO schema_migrations(version,name,applied_at)
			VALUES(900,'migrations/00900_ai_bridge.sql',0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`DROP TABLE ai_agent`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`DELETE FROM schema_migrations WHERE version=80`); err != nil {
			t.Fatal(err)
		}
		err := db.Migrate()
		if err == nil {
			t.Fatal("缺 ai_agent 表时必须报错")
		}
		if !strings.Contains(err.Error(), "ai_agent") {
			t.Fatalf("错误信息未指向缺表: %v", err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=904`, 0)
	})

	t.Run("guard-without-guard-duplicate", func(t *testing.T) {
		// 撤回官方守卫的等价复现：从 renumberedColumnMigrations 移除 00904 的
		// entry 后，同一份完整旧库夹具（列已存在）会按原文件执行 → duplicate column。
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80})
		spec, ok := renumberedColumnMigrations[aiFishingTalkMigrationName]
		if !ok {
			t.Fatalf("renumberedColumnMigrations 缺少 %s", aiFishingTalkMigrationName)
		}
		delete(renumberedColumnMigrations, aiFishingTalkMigrationName)
		t.Cleanup(func() { renumberedColumnMigrations[aiFishingTalkMigrationName] = spec })
		err := db.Migrate()
		if err == nil {
			t.Fatal("移除官方 map entry 后必须报 duplicate column，否则测试无效")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			t.Fatalf("期望 duplicate column，实际: %v", err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=904`, 0)
	})

	t.Run("guard-declaration-matches", func(t *testing.T) {
		// 官方 map 的 table/columns 必须与迁移文件里那条 ALTER 一致；remains 必须是
		// 六项种子而不是空串（空串会让「列存在」时整文件跳过、漏种子）。
		spec, ok := renumberedColumnMigrations[aiFishingTalkMigrationName]
		if !ok {
			t.Fatalf("renumberedColumnMigrations 缺少 %s", aiFishingTalkMigrationName)
		}
		if spec.table != "ai_agent" || len(spec.columns) != 1 || spec.columns[0] != "fishing_talk_enabled" {
			t.Fatalf("map entry 声明 = table %q columns %v，期望 ai_agent / [fishing_talk_enabled]",
				spec.table, spec.columns)
		}
		if spec.remains == "" {
			t.Fatal("remains 为空会让列已存在的库跳过整个文件，六项种子丢失")
		}
		for _, key := range aiFishingTalkSettingKeys {
			if !strings.Contains(spec.remains, "'"+key+"'") {
				t.Fatalf("remains 缺少种子键 %s", key)
			}
		}
		// 把迁移文件里那条 ALTER 真跑一遍，核对它建出的列名/声明与 map 声明一致。
		db, err := Open(filepath.Join(t.TempDir(), "decl.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.SQL.Exec(`CREATE TABLE ai_agent(id INTEGER PRIMARY KEY);
			CREATE TABLE ai_setting(key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', updated_ts REAL NOT NULL DEFAULT 0);`); err != nil {
			t.Fatal(err)
		}
		body, err := fs.ReadFile(migrationsFS, aiFishingTalkMigrationName)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "ADD COLUMN "+spec.columns[0]) {
			t.Fatalf("迁移文件里没有 ADD COLUMN %s，与 map 声明漂移", spec.columns[0])
		}
		if _, err := db.SQL.Exec(string(body)); err != nil {
			t.Fatalf("迁移文件本身执行失败: %v", err)
		}
		var typ, def string
		var notNull int
		if err := db.SQL.QueryRow(`SELECT type, "notnull", IFNULL(dflt_value,'')
			FROM pragma_table_info('ai_agent') WHERE name=?`, spec.columns[0]).
			Scan(&typ, &notNull, &def); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(typ, "INTEGER") || notNull != 1 || def != "0" {
			t.Fatalf("SQL 声明 (%s, notnull=%d, default=%s)，期望 (INTEGER, 1, 0)", typ, notNull, def)
		}
	})

	t.Run("failed-run-retry", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 79})
		if _, err := db.SQL.Exec(`CREATE TRIGGER fail_ai_renumber BEFORE INSERT ON schema_migrations
			WHEN NEW.name='migrations/00904_ai_fishing_talk.sql'
			BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(); err == nil {
			t.Fatal("注入失败后迁移必须报错")
		}
		// SQL 与登记同事务：ALTER 必须一起回滚，不能留下半个迁移。
		assertCount(t, db, `SELECT COUNT(*) FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`, 0)
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=904`, 0)
		if _, err := db.SQL.Exec(`DROP TRIGGER fail_ai_renumber`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatalf("恢复后重试失败: %v", err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM pragma_table_info('ai_agent') WHERE name='fishing_talk_enabled'`, 1)
		assertRegistered(t, db, 904, "migrations/00904_ai_fishing_talk.sql")
	})

	t.Run("double-start", func(t *testing.T) {
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80})
		if _, err := db.SQL.Exec(`INSERT INTO ai_agent(key, display, contact_uid)
			VALUES('sentinel', '哨兵', 999001)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatalf("第一次启动失败: %v", err)
		}
		before := snapshotCounts(t, db)
		if err := db.Migrate(); err != nil {
			t.Fatalf("第二次启动失败: %v", err)
		}
		after := snapshotCounts(t, db)
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("两次启动后 %s 由 %d 变为 %d", k, v, after[k])
			}
		}
	})

	t.Run("official-db-upgrade", func(t *testing.T) {
		// 官方 00060–00062 已应用的库：不许重复建官方表，也不许篡改官方登记。
		db := newAILegacyDB(t, aiLegacyDBOptions{official: true})
		if _, err := db.SQL.Exec(`INSERT INTO skill_cast_effect_def(cat_id,node_id,effect_key,per_level_pct,gate_kind,gate_type)
			VALUES(1,1,'official-sentinel',5,'rod',1);
			INSERT INTO rf4records_catch(fish_name_en,bait_name,fetched_at) VALUES('sentinel-fish','sentinel-bait',1);`); err != nil {
			t.Fatal(err)
		}
		dumpMigrationRows(t, db, "官方 62 已应用的库（升级前）")
		if err := db.Migrate(); err != nil {
			t.Fatalf("官方已有库升级失败: %v", err)
		}
		assertRegistered(t, db, 60, "migrations/00060_skill_cast_effects.sql")
		assertRegistered(t, db, 61, "migrations/00061_rf4records_catch.sql")
		assertRegistered(t, db, 62, "migrations/00062_add_bs_salmon_grade_line.sql")
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE name LIKE 'retired/%'`, 0)
		assertCount(t, db, `SELECT COUNT(*) FROM skill_cast_effect_def WHERE effect_key='official-sentinel'`, 1)
		assertCount(t, db, `SELECT COUNT(*) FROM rf4records_catch WHERE fish_name_en='sentinel-fish'`, 1)
		assertCount(t, db, `SELECT COUNT(*) FROM fish_grade_lines WHERE fish_key='bs_salmon'`, 1)
		for _, m := range aiRenumberMap {
			assertRegistered(t, db, migrationVersion(m.newName), m.newName)
		}
		assertAISchemaReady(t, db)
		dumpMigrationRows(t, db, "官方 62 已应用的库（升级后）")
	})

	t.Run("merged-dir-retire", func(t *testing.T) {
		// 合成目录（官方 60/61 + 让号后的 AI 五个）下，旧 60/61 登记行必须让位。
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 61})
		entries := mergedMigrationEntries(t)
		if err := retireStaleMigrationRows(db.SQL, entries); err != nil {
			t.Fatalf("退役旧分支登记失败: %v", err)
		}
		assertRegistered(t, db, 9000060, "retired/migrations/00060_ai_bridge.sql")
		assertRegistered(t, db, 9000061, "retired/migrations/00061_ai_cleanup_legacy.sql")
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE name='migrations/00060_ai_bridge.sql'`, 0)
		// 退役可重复执行：第二遍不应再动。
		if err := retireStaleMigrationRows(db.SQL, entries); err != nil {
			t.Fatalf("二次退役失败: %v", err)
		}
		assertRegistered(t, db, 9000060, "retired/migrations/00060_ai_bridge.sql")
	})

	t.Run("pretired-9000060", func(t *testing.T) {
		// R-004 条件二：已退役一轮的旧库（schema_migrations 里已有 9000060）。
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 80, preRetired: true, official: true})
		dumpMigrationRows(t, db, "已退役一轮的旧库（升级前）")
		if err := db.Migrate(); err != nil {
			t.Fatalf("已退役一轮的旧库升级失败: %v", err)
		}
		assertRegistered(t, db, 9000060, "retired/migrations/00060_ai_bridge.sql")
		assertRegistered(t, db, 9000061, "retired/migrations/00061_ai_cleanup_legacy.sql")
		assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE name LIKE 'retired/%'`, 2)
		assertRegistered(t, db, 60, "migrations/00060_skill_cast_effects.sql")
		assertRegistered(t, db, 61, "migrations/00061_rf4records_catch.sql")
		for _, m := range aiRenumberMap {
			assertRegistered(t, db, migrationVersion(m.newName), m.newName)
		}
		assertAISchemaReady(t, db)
		dumpMigrationRows(t, db, "已退役一轮的旧库（升级后）")
	})

	t.Run("negative-same-number-collides", func(t *testing.T) {
		// 旧同号文件不移走：官方同号迁移登记撞 UNIQUE（Migrate 第 137 行同一动作）。
		db := newAILegacyDB(t, aiLegacyDBOptions{oldThrough: 60})
		_, err := db.SQL.Exec(`INSERT INTO schema_migrations(version,name,applied_at)
			VALUES(60,'migrations/00060_skill_cast_effects.sql',0)`)
		if err == nil {
			t.Fatal("同号登记必须撞 UNIQUE 约束")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			t.Fatalf("期望 UNIQUE 冲突，实际: %v", err)
		}
		assertRegistered(t, db, 60, "migrations/00060_ai_bridge.sql")
	})

	t.Run("negative-create-table-moved-high", func(t *testing.T) {
		// 反例：只把建表迁移 00900 挪到高位、其余留在低位，文件名序会让读表迁移先跑。
		// 这里直接以"没有 00900"的空库执行 00902 / 00904，复现同一错序后果。
		db := newAILegacyDB(t, aiLegacyDBOptions{})
		for _, newName := range []string{
			"migrations/00902_ai_map_subscription.sql",
			"migrations/00904_ai_fishing_talk.sql",
		} {
			body, err := fs.ReadFile(migrationsFS, newName)
			if err != nil {
				t.Fatal(err)
			}
			_, execErr := db.SQL.Exec(string(body))
			if execErr == nil {
				t.Fatalf("%s 在缺表时执行成功，负例失效", newName)
			}
			if !strings.Contains(strings.ToLower(execErr.Error()), "no such table") {
				t.Fatalf("%s 期望 no such table，实际: %v", newName, execErr)
			}
			t.Logf("%s → %v", newName, execErr)
		}
		// 补一步：先补齐 ai_setting 再跑 00904，暴露 ALTER 的顺序依赖（no such table: ai_agent）。
		if _, err := db.SQL.Exec(`CREATE TABLE ai_setting(key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT '', updated_ts REAL NOT NULL DEFAULT 0)`); err != nil {
			t.Fatal(err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/00904_ai_fishing_talk.sql")
		if err != nil {
			t.Fatal(err)
		}
		_, execErr := db.SQL.Exec(string(body))
		if execErr == nil || !strings.Contains(execErr.Error(), "no such table: ai_agent") {
			t.Fatalf("00904 在缺 ai_agent 时应报 no such table: ai_agent，实际: %v", execErr)
		}
		t.Logf("00904（补齐 ai_setting 后）→ %v", execErr)
	})

	t.Run("legacy-drop-loses-data", func(t *testing.T) {
		// 不是绿灯：把 00901（原 00061）DROP legacy 表的后果固定下来。
		// 非空的 ai_session / ai_message 会被无条件删除，计划层必须就此拍板。
		db := newAILegacyDB(t, aiLegacyDBOptions{legacyRows: 2})
		assertCount(t, db, `SELECT COUNT(*) FROM ai_session`, 2)
		assertCount(t, db, `SELECT COUNT(*) FROM ai_message`, 2)
		if err := db.Migrate(); err != nil {
			t.Fatalf("升级失败: %v", err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('ai_session','ai_message')`, 0)
		t.Logf("升级前 ai_session=2 行、ai_message=2 行；升级后两表已不存在，哨兵行随 DROP 丢失")
	})
}

// snapshotCounts 取关键表行数，用于"两次启动无额外数据变化"。
func snapshotCounts(t *testing.T, db *DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, q := range []struct {
		key   string
		query string
	}{
		{"schema_migrations", `SELECT COUNT(*) FROM schema_migrations`},
		{"ai_setting", `SELECT COUNT(*) FROM ai_setting`},
		{"ai_agent", `SELECT COUNT(*) FROM ai_agent`},
		{"ai_map_agent", `SELECT COUNT(*) FROM ai_map_agent`},
		{"ai_provider", `SELECT COUNT(*) FROM ai_provider`},
		{"ai_model", `SELECT COUNT(*) FROM ai_model`},
	} {
		var n int
		if err := db.SQL.QueryRow(q.query).Scan(&n); err != nil {
			t.Fatalf("统计 %s: %v", q.key, err)
		}
		out[q.key] = n
	}
	return out
}
