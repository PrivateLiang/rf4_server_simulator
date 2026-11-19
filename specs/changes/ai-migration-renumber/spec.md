# AI 迁移有序让号

Status: done

<!-- done：2026-11-17 T-023 按 B-003 D14 / DEV-011=A 回填。T-014 已把五条 AI 迁移落到 00900–00904，
     T-017 已复用官方 renumberedColumnMigrations；号段口径与日期说明见 docs/adr/0002-ai-migration-numbering.md。 -->

## Problem Statement

运维视角：本分支的 AI 迁移曾与官方 main 用了同一批编号（T-014 前的历史形态）。官方已经占了 `00060`–`00062`，
本分支最早把 `00060_ai_bridge.sql`、`00061_ai_cleanup_legacy.sql`（历史名 → 现 `00900_ai_bridge.sql` /
`00901_ai_cleanup_legacy.sql`）与它们同号。两边合并后
迁移目录里同时出现两个 `00060`、两个 `00061`，而 `schema_migrations` 以编号为主键：
先执行的占住编号，后执行的登记撞 `PRIMARY KEY`，`Migrate()` 返回错误，服务直接起不来。

`retireStaleMigrationRows` 救不了这一例——它只退役「文件名已不在当前目录」的旧行，
而这两个文件仍在目录里，因此永远不会被判为陈旧。更根本的是：只要 AI 继续在
`00001`–`00899` 内取号，官方每前进一次就要重新让号、回归与复验，这是持续成本。

## Solution

把五条 AI 迁移整体移出官方号段，落到临时号段 `00900`–`00904`，并保持原有先后关系：
建表 → 清理 → 地图订阅 → 全局范围 → 加列。官方号段 `00001`–`00899` 全部留给官方，
AI 不再与之抢号；官方给出 mod 迁移号段规范后再一次性对齐（见 ADR 0002）。

合并官方后目录里不再有同号文件，空库、旧库、官方已有库三条升级路径都能一次跑通，
且重复启动幂等。旧库里以旧号 `00060`/`00061`（现 `00900`/`00901`）登记的迁移行由官方既有
`retireStaleMigrationRows` 退役到 `9000060`/`9000061`。

其中「加列」那条是本组唯一不可重跑的语句（`ALTER TABLE ... ADD COLUMN` 没有
`IF NOT EXISTS`）。T-017 起它复用官方 `renumberedColumnMigrations`（`internal/store/store.go`）：
列已存在时只执行同文件的六项 setting 种子，列缺失时整文件执行。官方机制只按**列名**
判断存在、不核对既有列的类型/默认值；`00904` 只登记一列，所以「全部存在」条件退化为
「列存在」，不存在「部分存在」分支，与原 AI 需求等价。

## User Stories

1. 作为运维，我希望合并官方后服务能正常启动，而不是因为迁移登记撞主键起不来。
2. 作为已经跑过旧版 AI 分支的服主，我希望升级后 AI 设置与角色开关的既有值不被重置。
3. 作为维护者，我希望官方继续新增迁移时 AI 分支不再反复让号。
4. 边界：旧库里已经建过 `fishing_talk_enabled` 列时，升级不报 `duplicate column`。
5. 边界：旧库里这一列被改成不兼容的类型时，官方守卫只按列名判存在，升级不报错也不改列，
   只补六项种子（该行为差异见 Further Notes）。
6. 边界：迁移在「执行 SQL」与「写登记行」之间失败时，不留半个迁移，恢复后可重试。

## Implementation Decisions

- **号段规则**：官方号段 `00001`–`00899` 归官方，AI 一律不取；AI 迁移临时独占 `00900`+，
  连续分配并保持依赖序。理由与临时性见 `docs/adr/0002-ai-migration-numbering.md`。
- **迁移号映射**（固定）：`60→00900`、`61→00901`、`78→00902`、`79→00903`、`80→00904`。
  只改文件名，SQL 内容不变；`00904_ai_fishing_talk.sql` 的 `ALTER TABLE ADD COLUMN` 由官方
  `renumberedColumnMigrations` 守卫重放（T-017 起）。此前的手写守卫、SQL 分段标记与相应
  常量已随 T-017 删除，分段标记机制废弃；`00904` 文件本身只保留说明性注释。
- **为什么整体移出官方号段**：只把 `60/61` 挪到 `00063`–`00077` 的空档仍会随官方取号再次冲突；
  把五个整体挪到 `00081+` 也会被官方后续占用。`00900`+ 在可预见范围内官方不可达
  （远高于当前官方最大 `00069`，官方需再新增约 831 个迁移）。
- **这是临时方案**：官方一旦给出 mod 迁移号段规范（或等价新解法），一次性对齐；
  在此之前不反复讨论号段，也不为每次官方取号重新让号。
- **守卫位置与语义**：T-017 起复用官方 `renumberedColumnMigrations`（`internal/store/store.go`），
  在既有逐文件事务内、`tx.Exec(body)` 之前按 entry 的 `table` / `columns` 判断；命中时把
  迁移体换成 entry 的 `remains`，迁移仍登记新编号。AI 只在官方 map 里加一条 `00904` entry：
  `table=ai_agent`、`columns=["fishing_talk_enabled"]`、`remains` = 六项 setting 种子。
  排序、判重、登记与 `retireStaleMigrationRows` 的通用算法都由官方维护，AI 自有改动只剩
  这一条 entry（`store.go` 的 AI 自有差异 +10/−0）。
- **不持有列定义期望**：官方 map entry 只声明表名与列名，`remains` 只是六项幂等种子
  （`INSERT OR IGNORE`）的重复，不持有列类型/默认值声明。列定义（type/notnull/default）
  由测试把迁移文件 SQL 真跑一遍后经 `pragma_table_info` 反查，SQL 改了而 entry 的
  `table`/`columns` 没改会红。
- **数据与迁移**：只改 AI 自己的五条迁移，不动任何官方迁移，也不新增表。
  旧库兼容依赖已有的 `retireStaleMigrationRows`（官方同号迁移进入目录时退役旧行），
  不手工改 `schema_migrations`。
- **回滚**：无行为开关。回退 = 改回文件名并从 `renumberedColumnMigrations` 移除 `00904`
  entry（迁移文件本身不改）；已升级的库不要只回退文件名（登记行还是新名），需按
  `schema_migrations` 成对回滚。

## Testing Decisions

- **测哪一层**：存储层，真实 SQLite 临时库跑完整 `Migrate()`；不打桩、不 mock 迁移器。
  用例入口 `internal/store/ai_migration_renumber_test.go` → `TestAIMigrationRenumber`。
- **夹具来源**：
  - 官方 `00060`–`00062` 用官方 main `919b3f0` 的原样副本内联在测试里，不用 T-001 旧快照。
  - 旧 AI 布局（`60/61/78/79/80`）用当前 embed 的同一份 SQL 执行后改登记行构造——
    只改文件名与登记编号，SQL 内容未变，因此等价于旧库。
  - 合成目录（官方同号 + 让号后 AI 五个）用「文件名 + 编号」列表直接驱动
    `retireStaleMigrationRows`：该函数只按名字与编号判重，不含 SQL 内容。
- **期望值来自哪**：迁移文件自己声明的对象（表/列/种子键）+ 官方原文的 DDL，
  不是「用同样代码再算一遍」。列定义期望由实际执行迁移文件 SQL 后的 `pragma_table_info` 反查
  （官方守卫本身不核对定义）。
- **必须覆盖的矩阵**：空库 / 仅旧 60-61 / 到旧 78 / 到旧 79 / 完整旧五迁移 /
  失败中断后重试 / 连续两次启动；官方 62 已应用的库；合成目录退役；已退役一轮
  （`9000060` 已存在，DEV-002 条件二）。
- **旧 60/61 的退役期望（DR-001）**：`old-60-61` 与 `old-full-five` 按真合并目录语义，
  断言旧号登记行（历史名 `00060_ai_bridge` / `00061_ai_cleanup_legacy` → 现 `00900`/`00901`）
  退役到 `9000060`/`9000061`；旧 `78/79/80` 的编号不再被占用，登记行原地保留。
- **两个负例**：旧同号文件不移走 → 登记撞 `UNIQUE`；只把建表迁移 `00900` 挪到高位 →
  `no such table: ai_setting` / `ai_agent`。
- **守卫有效性反证**：同一份「完整旧五迁移」夹具，从 `renumberedColumnMigrations` 移除
  `00904` entry 后按原文件执行，必须报 `duplicate column`（`guard-without-guard-duplicate`）；
  否则说明用例没有真的触及官方守卫。
- **回滚后哪些测试会变红**：移除 `00904` entry → `old-full-five` / `guard-seeds-still-run` /
  `pretired-9000060` 立刻 `duplicate column`；把映射改回旧号 → `embed-mapping` 报旧名仍在目录；
  entry 与迁移文件漂移 → `guard-declaration-matches` 红。

## Out of Scope

- 不合并官方 main、不改官方迁移、不改迁移器通用算法、不操作真实库、不删真实数据。
- 不为 `ai_session` / `ai_message` 的去留新增设计：这两张表的 `DROP` 语义原样保留，
  风险单列在 Further Notes。
- 不把 `00900`+ 写成永久官方契约；官方 mod 号段规范落地前不预设计适配。
- 不改 `specs/capabilities/`、`CLAUDE.md`、`CONTRIBUTING.md`、CI。

## Further Notes

- `[事实]` 官方 main `919b3f0` 迁移最大号 62 是 T-003/T-014 当时的基线；T-017 再对齐后官方
  `94e7b5b` 迁移 66 个、最大 `00069`，官方 #480 的 `00065`–`00069` 已合入；AI `00900`–`00904`
  与官方号段无重号（本树迁移合计 71 个 = 官方 66 + AI 5）。
- `[事实]` 官方 #480 之后上游 `internal/store/store.go` 已有 `renumberedColumnMigrations`；
  AI 侧自 T-017 起复用该 map 的 `00904` entry，不再保留独立 helper、守卫常量与 SQL 分段标记。
- `[事实]` `00901_ai_cleanup_legacy.sql`（原 `00061`）会 `DROP TABLE IF EXISTS ai_message` /
  `ai_session`，目的是满足 `ValidateTableOwnership` 的「未登记表」校验。该语句本轮未改语义。
- `[事实]` 仓库历史里这两张表从没有被任何代码写过（`git log --all -S ai_session` 只命中
  引入它们的那个提交，且该提交只增注释与 DROP），但**不能**据此推定线上旧库为空。
- `[事实]` `specs/changes/ai-bridge/spec.md` 的旧迁移名引用已由 T-018 订正为当前名
  （`00900_ai_bridge.sql` / `00901_ai_cleanup_legacy.sql`）；本文件当时记的「留给 T-004 一并订正」
  遗留已消。
- `[事实]` 台账 DEV-005 已决（R-006 实测 `ai_session` / `ai_message` 两表均不存在，接受 DROP）；
  真实库升级前仍须先备份；DEV-006=B 的人工迁就已随官方 #508（退役落点向上探测）撤销，
  部署无需再挪 `9000061`。
- `[缺口]` 真实旧库的在线升级未在本环境执行（禁读玩家库），`[事实]` 级的旧库安全结论仍缺。
- `[缺口]` 官方守卫的 `pragma_table_info` 查询失败分支未单独构造用例（正常连接上不会失败）；
  缺表（`guard-missing-table`）与既有列定义不兼容（`guard-incompatible-column`，只按列名判存在、
  不报错）两条有真实用例。
