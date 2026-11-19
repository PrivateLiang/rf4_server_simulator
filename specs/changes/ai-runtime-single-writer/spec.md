# AI 运行态单写者与命令契约

Status: done

## Problem Statement

AI 的运行态（`ai_actor_state` / `ai_channel_agent` / `ai_memory` / `ai_agent.actor_enabled`）现在有三个进程直接写：aibridge（读状态时顺手推进/初始化）、gameserver（tick 与上鱼）、admin（开关、换状态、清记忆、删除）。同一行会被「读整行 → 内存改 → 整行 UPSERT」覆盖，AD 的删除与 GS 的 tick 之间存在复活窗口；管理动作同步返回“已生效”，但写失败、进程崩溃或 GS 不在线时用户看不到真实终态。删除 Agent 还藏着运行态级联（`AIDeleteAgent`），`AISetActorEnabled(false)` 里也藏了一条额外的 `DELETE ai_actor_state`，只改显式调用点会漏写者。

官方 `QueuePop`（`internal/store/repos2.go:985`）在取指令的同一事务里 `DELETE FROM admin_queue`，行一旦弹出就没有终态、没有去重键、没有代次；崩溃即丢命令，重试即重复执行。`ai_task` / `ai_result` 是模型任务与回复，`ai_tool_job` 是只读工具往返，都不能表达“一次管理意图的完成语义”。因此不能直接借用现有队列。

## Solution

新增两张专用表：

- `ai_runtime_command`：持久命令（`request_id` 唯一、`source`、`kind`、`agent_key`、`expected_generation`、`expected_revision`、规范化 `payload` + `payload_hash`、`created_ts`、`expires_ts`、`status`、`reason`、`applied_ts`、`result_revision`）。
- `ai_runtime_control`：每 Agent 单调 `generation` / `revision` / `tombstoned`，GS 独占写；删 Actor 行保留 control。

唯一活动 gameserver 是运行态业务写者：选最小 pending 命令 → 同事务读 control/当前配置 → 校验代次与版本 → 更新运行态 → 记终态/revision → 提交。AD 只入队并返回 `202 pending`，BR 只读状态并提交 target 建议；两者都不再直接写运行态。重复相同 `request_id` + 相同规范 payload 返回既有结果，不同 payload 拒绝 conflict。

## User Stories

1. 作为后台管理员，点“开启角色模拟 / 换个状态 / 清空战绩记忆 / 删除 Agent / 拉进频道”后，页面先显示“已受理（等待 GS 应用）”，再轮询到“已应用 / 拒绝 / 过期”，不再把入队当成已生效。
2. 作为后台管理员，GS 停机时我的操作显示“仍待 GS 应用，可刷新”，意图持久保存，不会因为进程重启丢失，也不会在恢复后重复执行。
3. 作为玩家，GS 不在线或后台刚点完删除时，旧 tick、旧模型建议不能把已禁用/已删除的 Agent 状态复活。
4. 作为玩家，`@邀请` / `@移出` 仍按官方公聊广播，合法命令的回执仍只发一次；命令失败不会吞掉广播，也不会把命令原文转送模型。
5. 作为运维，重复的 HTTP 重试、重复的公聊请求、重复的 GS 轮询都不会产生第二次应用（同一次 clear 不会删掉清空后新写入的记忆，同一次 reroll 不会再次掷骰）。
6. 作为架构维护者，扫描全仓可以直接看出：actor / channel_agent / memory 的业务写者只有 GS 的同事务入口；AD 只有命令入队，BR 只有只读与建议。

## Implementation Decisions

### 数据定义

- 迁移：执行时现取的下一 AI 号（本树为 `00905_ai_runtime_commands.sql`），只追加两张表、两个索引、一次 control 种子；不改旧迁移、不改官方队列。
- `ai_runtime_command`：`AUTOINCREMENT id`、`request_id TEXT NOT NULL UNIQUE`、索引 `(status,id)` 与 `(agent_key,id)`；`status ∈ pending/applied/rejected/expired`。
- `ai_runtime_control`：主键 `agent_key`，`generation` 从 1 起、`revision` 从 0 起、`tombstoned`。现有 Agent 由迁移种子建立；新建 Agent 在同一次创建里补 control；缺行按 `generation=1 / revision=0` 处理。
- 去重：`request_id` 全局唯一；同 request + 同 `payload_hash` 返回既有行（`duplicate=true`），不同 hash 返回 conflict。`payload_hash` 对校验后的规范字段计算（结构体重新编码，键序无关），并混入 `kind` + `agent_key` 防止空 payload 的不同意图撞 hash。
- 限额：单条命令规范化 payload ≤ 4096 字节；全局 pending ≤ 1024、单 Agent pending ≤ 32，限额检查与 INSERT 同事务（BEGIN IMMEDIATE），满返回 `ErrAIRuntimeQueueFull`（HTTP 429）；BR 遇满放弃建议、不阻塞模型与游戏。
- 寿命：建议 TTL 30 秒、AD 操作 TTL 10 分钟、上限 24 小时；到期未应用的 pending 由 GS 标 `expired` 且不执行。终态保留 7 天；总数达到 100000 时先 GC 过期终态，仍满则拒绝新命令。
- 重试窗口：超过 24 小时的 pending 重试标 expired，不重放；已终态记录仍返回原结果，不承诺无限期 exactly-once。

### 命令表（source × kind）

| kind | 来源 | payload | GS 应用规则 |
|---|---|---|---|
| `actor_set_enabled` | admin | `enabled bool` | 校验 Agent 未删除、`expected_generation`（>0 时）；启用还需本地 consent 与 AI 总闸允许；禁用清 Actor 行并 `generation++`；状态有变化时 `revision++` |
| `actor_reroll` | admin | 无 | 校验 generation/revision 与 Agent 开关；只在 GS 内掷一次骰；`revision++` |
| `actor_target_suggest` | bridge | `fish_key, loc_key` | 校验开关、代次、revision、鱼在当前地图 roster 内、冷却与概率；GS 自己掷概率，不信 BR 的结论；`revision++` |
| `channel_set_member` | admin / gameserver | `channel, enabled` | 校验 Agent 未删除；gameserver 来源还要求目标 `joined=1`；同事务写 `ai_channel_agent`；`revision++` |
| `memory_clear` | admin | 无 | 同事务清 `ai_memory`；幂等；`revision++` |
| `agent_delete` | admin | 无 | 逻辑删除 + 级联（Actor/频道成员/未决任务/未投递结果），`generation++`、`tombstoned=1`；不删聊天历史、不复用 key/UID |

### 顺序与原子性

- GS 单活动消费者是部署前提；SQLite 用 `BEGIN IMMEDIATE` + `status='pending'` 条件复查兜底。两个 GS 并发试取时只有一个能提交，另一个读到已终态后取下一条。禁止“先删除/先 claim 再处理”。
- 应用事务：读最小 pending → （过期则标 expired 继续）→ ensure 并读 control → 校验 → 写运行态 → 更新命令终态与 `result_revision` → 提交。事务内崩溃全部回滚，命令保持 pending；提交后未回显时同 `request_id`/id 查询得到 applied，恢复不会再执行一次。
- tick / 上鱼记忆：同事务读控制与状态、更新后 `revision++`；禁止事务外读整行再 upsert。
- 外部 IO 不进事务；事务预算 ≤ 50 ms/条，每轮 ≤ 32 条。

### 管理 API

- 原路径与 JWT 不变；所有运行态动作返回 HTTP 202 + `command_id` / `request_id` / `status=pending`，不带伪造 actor 结果。
- 新增 `GET /api/ai/commands/{id}`（JWT）：只返回 `id/status/reason/kind/agent_key/result_revision/applied_ts`，不回显 payload；pending 可 202，终态 200。
- 地图/全局订阅、频道显示名、Agent 基础配置、provider/model 仍归 AD 直接写（非运行态）；普通整卡保存不得提交 runtime 命令。
- 前端只对受影响按钮显示 pending/已应用/拒绝/过期，并每秒轮询、30 秒停。

### 旧写者 → 新入口（开工扫描）

| 表 | 旧写点 | 旧调用方 | 本票处置 |
|---|---|---|---|
| `ai_actor_state` | `store.AISaveActor`（`repos_ai_actor.go:307`） | GS `ai_actor.go:41/55/71/178`、BR `aibridge/actor.go:42/61/173`、AD `ai.go:1218` | 删除该导出入口；GS tick/上鱼走同事务；BR 只读；AD 入队 |
| `ai_actor_state` | `store.AIDeleteActor`（`repos_ai_actor.go:331`） | AD `ai.go:1191` | 删除导出入口；AD 入队，GS 事务删 |
| `ai_actor_state` | `AIDeleteAgent` 级联（`repos_ai.go:603`） | AD `ai.go:1060` | AD 入队；GS 同事务级联 |
| `ai_actor_state` | `AISetActorEnabled` 隐藏删除（`repos_ai.go:1348`） | AD `ai.go:1186` | 删除该 setter；代次与运行态只在 GS |
| `ai_channel_agent` | `AISetChannelAgent`（`repos_ai.go:1123`） | GS `ai_channel.go:37`、AD `ai.go:660` | 函数保留给 GS 事务；AD 入队；公聊入队后立即同事务应用 |
| `ai_channel_agent` | `AIDeleteAgent` 级联（`repos_ai.go:606`） | AD | 同上 |
| `ai_memory` | `AIAddMemory`（`repos_ai.go:686`） | GS `ai_fishing.go:235` | 并入 GS 渔获事务 |
| `ai_memory` | `AIPruneMemories`（`repos_ai.go:733`） | GS `ai_fishing.go:238` | 并入同一事务 |
| `ai_memory` | `AIClearMemories`（`repos_ai.go:745`） | AD `ai.go:1105` | AD 入队；函数保留给 GS 应用 |
| `ai_agent.actor_enabled` | `AISetActorEnabled`（`repos_ai.go:1342`） | AD | AD 入队；GS 事务写 |
| `ai_catch` | `AIAddCatch`（`repos_ai_catch.go:28`） | GS `ai_fishing.go:216` | 并入 GS 渔获事务（与 actor/memory 同锁） |
| `ai_agent`（配置） | `AICreateAgent` / `AIUpdateAgent` / `AISetAgentBehavior` / `AISetAgentJoined` / `AISetAgentFishingTalk` / `admin/ai.go:543` | AD | 不变：非运行态配置仍归 AD |

### 兼容 / 回滚

- 协议不变：24/10 仍 fire-and-forget、24/27 广播与回执形状不变；不新增游戏协议对象。
- 回滚 = 所有 AI 进程停止、外发关闭后恢复匹配程序 + DB 快照；不能运行旧程序消费新命令，不删除已应用记录来重试。运行期降级保持新单写者并暂停 AI 行为。
- 不新增权限模型（DR-002 已定本阶段不设权限门）。

## 实现落点（执行证据）

- 迁移 `internal/store/migrations/00905_ai_runtime_commands.sql`（号执行时现取：官方 66 个最大 `00069`、AI 段最大 `00904`）；表归属登记 `internal/store/repos_integrity.go`。
- 存储层 `internal/store/repos_ai_runtime_command.go`：去重/规范化 payload hash/限额/寿命/GC、`BEGIN IMMEDIATE` 应用事务、control 代次与 tombstone；运行态表 SQL 助手收口到 `airRandomMap` / `saveAIActor` / `deleteAIActor` / `aiSoftDeleteAgent` / `setChannelAgent` / `addAIMemory` / `pruneAIMemories` / `clearAIMemories` / `aiAddCatch`（均由事务连接或既有测试调用）。
- GS 消费者 `internal/gameserver/ai_runtime_commands.go`：500ms、每轮 ≤32 条、单条 ≤50ms；tick/上鱼改走 `AIWithRuntimeTx`；公聊命令入队后立即同事务应用。消费者挂在 `StartAIFishing` 生命周期上，`cmd/*` 零改动。
- BR `internal/aibridge/actor.go`：`actorLine` 只读，状态缺失时省略位置句；`maybeSwitchActor` 只提交 `actor_target_suggest`（request_id=`bridge:<task>:<agent>:1`）。
- AD `internal/admin/ai_runtime_commands.go` + `ai.go/api.go`：202 + `GET /api/ai/commands/{id}`；`index.html` 只对受影响按钮加 pending/轮询。
- 门禁：`TestAIRuntimeCommand*` / `TestAISingleWriter*` 两个前缀在四包均有命中且全绿；迁移矩阵 21/21；`vet` / `doccheck` / `go build ./...` / `go test ./...` 全绿。撤回反证四组（BR Save / AD 清 memory / 去代次检查 / 分离状态与 ack 事务）各使对应测试变红，恢复后 SHA-256 一致。详见 `回执/T-020_回执.md`。

## Testing Decisions

- 真库（合成 SQLite 文件）而不是 mock：两个独立 `store.DB` 连接/两个消费者抢同一行，独立期望是“一次应用”；副作用与命令终态要么全有要么全无。
- 必有用例：重复相同 request、同键不同 payload、重复 clear 后新记忆不丢、重复 reroll 不再随机、旧建议晚到、禁用/删除与 tick 竞争、重启三崩溃点（应用前/事务回滚/提交后未回显）、双消费者、GS 停机/恢复、TTL/队列满、坏命令不堵队、旧行缺 control、软删除 + 五类脏数据、多模型既有保存不回归。
- 安全链回归：T-008～T-012 的既有用例、T-013 广播与主鱼/额外鱼边界全部重跑；AI 关闭后官方主链路等价。
- 撤回反证四组：恢复 BR Save、恢复 AD 清 memory、去代次检查、分离状态与 ack 事务；对应测试应红，恢复后核对哈希。

## Out of Scope

- 不替换/修改官方 `QueuePop`、`admin_queue` 与官方通信原则。
- 不造通用消息中间件、不做跨进程长租约或新协调服务。
- 不新增权限模型、不改官方协议/经济/结算/状态条真源。
- 不删除 AI 聊天历史、不复用 key/UID、不真实删除生产数据。

## Further Notes

- [事实] 迁移号执行时现取：官方段 66 个最大 `00069`，AI 段最大 `00904` → 本票落 `00905`（`00900+` 临时号段口径不变）。
- [事实] 现有官方 `QueuePop` 取指令即删除，崩溃丢命令、重试重复执行；AI 的 `ai_task`/`ai_result`/`ai_tool_job` 分别是模型任务、回复与只读工具往返，完成语义不同。
- [假设] 单活动 GS 是部署前提；多 GS 并发只靠 SQLite 写事务兜底“恰好一个提交”，不做分布式租约。
- [事实] 同键重试口径：pending 超 24 小时或已过 `expires_ts` 标 `expired`；已终态的行保留原终态返回、绝不重放。两条路径都不会二次应用。
- [缺口] `go test -race` 本机不可用：默认 `CGO_ENABLED=0`，PATH 无 gcc/cc/clang/cl；设 `CGO_ENABLED=1` 报 `cgo: C compiler "gcc" not found`。并发证据改用双 DB 连接/两消费者、tick 竞争的普通构建用例并重复 10 次，未取得 race 报告。
- [缺口] 真实客户端上的 pending/应用显示与公聊可见性仍需 T-005 实机补证；真实库升级需另获人批准。
