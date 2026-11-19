# AI 数据契约与脏数据容忍

Status: done

## Problem Statement

真实环境的玩家库在后台「保存厂商与密钥」时返回 500 `厂商配置保存失败`。该库的
`ai_model` 同一厂商有 2 行：一行是后台填过的有效模型，另一行是 `00900_ai_bridge.sql`
迁移种子遗留的空 `model_id` 行。旧实现按 `provider_id` 一次性更新该厂商的全部模型行，
两行被刷成同一个值，撞 `UNIQUE(provider_id, model_id)`，事务回滚，保存失败。

同类风险不只在一条 SQL：如果测试夹具永远只有一行干净数据，代码就会继续假设
「应当唯一的键在真实库里一定唯一」。本改动把保存路径改为精确定位，并把脏数据
（重复行、空值行、已删除行、孤儿登记行、历史种子行）变成 AI 测试夹具的默认起点。

## Solution

后台保存厂商配置时，先读出实际展示给管理员的 `ai_model` 行 id，更新时只改这一行；
同一厂商的其它模型行保持原样，不会被清空、合并或覆盖。模型行不存在或目标行已失效时
明确报错，不再把「一行都没改」当成保存成功。目的地快照仍与厂商地址、模型在同一事务内
提交。

AI 相关测试夹具统一叠加真实库出现过的脏数据形态；新增回归用例在「有效行 + 空行」
「已删除 Agent + 孤儿频道/地图登记」「历史种子行 + 显式保存行」「孤儿迁移登记」上运行。
另外提供只读预览清理脚本，扫描真实库并列出建议，不自动执行写库。

## User Stories

1. 作为管理员，我在有两行 `ai_model` 的库上保存厂商地址与模型时能成功，不再看到 500。
2. 作为管理员，保存后只有当前展示的那一行模型被修改，另一行空模型保持原样。
3. 作为管理员，模型 ID 与同厂商另一行重复时看到失败提示，而不是被静默覆盖。
4. 作为运维，升级后历史迁移种子行（`updated_ts=0`）仍被判定为「未确认」，不会被当成授权。
5. 作为运维，已逻辑删除的 Agent 即使 `joined=1`、仍有频道/地图登记，也不会出现在「已接入」名单。
6. 作为运维，`schema_migrations` 里指向已不存在文件的登记行不会让下一次 `Migrate()` 失败。
7. 作为运维，我可以用只读脚本预览重复模型行、孤儿登记与状态不一致，脚本默认不写库。

## Implementation Decisions

- `internal/store.AISaveProviderDestination` 增加 `modelRowID` 参数，模型更新语句为
  `UPDATE ai_model SET model_id=? WHERE id=? AND provider_id=?`；目标行 0 行受影响时返回
  `ErrAIModelRowNotFound`。厂商地址仍按 `ai_provider.id` 精确定位，目的地快照仍用
  `ai_setting` 的 `ai_egress_destination` 键 upsert，三步同事务。
- `internal/admin/ai.go` 的保存入口在读取当前厂商时一并读取实际选中的 `ai_model.id`，
  传给存储层；状态页、授权页与保存入口的模型行读取统一按 `p.id, m.id` 排序，保证
  「展示的那一行」就是「被更新的那一行」。模型行缺失时错误文案区分于通用 500。
- 不新增表、列或迁移；不删除任何模型行。同一厂商的其它行按「不动」处理。
- 逐一审查 `internal/store/repos_ai*.go` 与 `internal/admin/ai.go` 的全部
  `INSERT` / `UPDATE` / `DELETE`：按业务键（`key`、`id`、`UNIQUE` 约束）定位的保持；
  按状态/时间/父级批量处理且批量本身就是设计语义的保持；只有 `ai_model` 的
  `provider_id` 全量覆盖被修正。
- 测试夹具：`internal/store` 新增统一的脏数据种子，覆盖同组多行、空值行、已删除行、
  孤儿登记行、历史种子行五类；所有涉及 `ai_*` 表的存储层测试从该种子起步。
- 只读维护脚本放在工作区 `维护脚本/`，不进产品仓库：默认只读；写模式必须显式
  `--apply` 且先做 SQLite 备份；只允许「禁用」可逆动作，不删除行；不输出密钥或玩家内容。

## Testing Decisions

- 存储层（真实 SQLite）：在「有效模型行 + 空 `model_id` 行」的夹具上调用
  `AISaveProviderDestination`，手写期望字面量断言两行的 id/model/enabled，确认空行原样保留。
- 存储层（真实 SQLite）：目标行改成另一行已占用的模型 ID 时断言返回错误，且
  `base_url`、目的地快照与两行模型都回滚；模型行缺失/失效时断言 `ErrAIModelRowNotFound`。
- 后台入口（真实 HTTP 路由 + 真实 SQLite）：`internal/store` 外部测试包经登录后
  `POST /api/ai/config`，在两行 `ai_model` 的库上断言 200，且只有目标行变化。
- 存储层：已删除 Agent（`joined=1`）不计入 `AIJoinedAgents`、频道/地图订阅与已接入计数；
  历史种子 `updated_ts=0` 经 `aiconfig.Egress` / `Security` 判定为未确认；孤儿迁移登记
  在不冲突时保留、同号时由退役算法让位。
- 期望值全部是手写常量（行 id、模型字符串、状态字符串），不用被测 SQL 再算一遍。
- 回滚测试会变红：把模型更新改回 `WHERE provider_id=?` 后，
  `TestAISaveProviderDestinationUpdatesOnlySelectedRow`、
  `TestAISaveProviderDestinationAtomic` 与真实后台路由用例都会因
  `UNIQUE constraint failed: ai_model.provider_id, ai_model.model_id` 失败（已实测）。
- 清理脚本：用合成 SQLite 夹具验证默认只读输出；真实库只跑 `--dry-run`，跑前跑后对库文件
  做哈希比对，确认零写入。

## Out of Scope

- 不清理、不删除、不禁用任何真实库数据；是否清理由人决定。
- 不修改 `retireStaleMigrationRows` 及其排序/判重/登记算法，不修改迁移文件内容。
- 不新增表/列/迁移/依赖，不改协议、结算、玩家状态条真源、静态库与 CI。
- 不承诺把已存在的重复模型行合并成一行；脚本只给建议。
- `internal/aibridge`、`internal/gameserver` 与 `internal/admin` 既有测试文件不在本票
  文件白名单内，未统一叠加脏数据种子；其数据容忍由存储层契约用例覆盖。

## Further Notes

- `[事实]` `ai_model` 的约束是 `UNIQUE(provider_id, model_id)`；空 `model_id` 行来自迁移种子。
- `[推断]` 其它 AI 表的业务键都有 `PRIMARY KEY` / `UNIQUE` 约束保护，按父级批量更新
  只出现在状态机与清理路径，属于明确设计语义，本票不改。
- `[缺口]` 未在真实浏览器点选保存；后台入口由真实 HTTP 路由用例覆盖。
- `[缺口]` 未执行清理脚本的写模式；脚本默认只读，写模式只在合成夹具上验证结构。
