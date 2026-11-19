# AI 完整工具注册表与写回执

Status: done

## Problem Statement

角色的「能做什么」原先散在三处：`internal/aibridge/tools.go` 的注册表、`runner.go`
里按工具名分发的 `switch`、以及后台可随意填写的 `ai_agent.tools_json`。结果是：

- 新增一个工具要改两个地方，漏改就「注册了但永远不会执行」；未知工具名被静默跳过，
  模型与管理员都看不出到底是「没查到」还是「根本没这个工具」。
- `tools_json` 是一串没有版本、没有类别的名字。它无法表达「这个角色可以看资料，
  但不能提交换目标建议」，也无法表达「显式清空 = 全部拒绝」；旧页面一次整卡保存就能
  悄悄改掉角色权限。
- 唯一的写动作 `actor_target_suggest` 是 `handle` 里的隐式副作用：玩家提到鱼名就按
  概率提交，模型不知情、管理员也关不掉，且与角色能力无关。
- 管理员没有办法安全地并发保存同一角色的能力：两个人同时勾选，后保存的会覆盖前一个，
  没有「你看到的是旧版本」这回事。

## Solution

把「角色能力」变成服务端唯一的目录与投影，把「工具」变成一份声明式注册表：

- 能力目录只有六条有调用点的能力：攻略帮助、回应玩家星蓝、参加圆桌、虚拟垂钓、
  播报自己渔获、接受目标建议。能力 ID 由服务端映射，页面只提交 ID，不能自带工具名。
- 攻略帮助投影为八个只读工具（六个本地 + `fish_query`/`map_vibe` 两个远程读）；
  接受目标建议投影为唯一写工具 `actor_target_suggest`；其余事件能力不新增任何模型可调用
  工具（不发任意言、不发鱼）。
- 每角色一条版本化能力记录（`ai_setting`，不新增表）：`schema_version`、`revision`、
  `generation`、能力集合、可选的旧只读子集。保存按 `base_revision` 比较，冲突返回 409。
- 缺配置 = 只保留基础私聊，没有工具；显式空 = 全部拒绝；记录损坏、旧代次、
  来源不可验证 → 失败关闭，不猜默认值。旧 `tools_json` 只在没有新记录时降级为
  「攻略帮助 + 可证明的旧只读子集」，永不授予写或事件。
- 所有工具调用只走 `Registry.Dispatch`：存在 → 能力 → 参数 → 急停 → 路由。
  未知、未授权、参数不合法、已暂停分别返回可区分错误，不再用空串糊过去。
- 写工具受理返回 `accepted` + 服务端生成的 `request_id` + `applied:false`，模型与玩家
  只看到「已提交，等待游戏服务确认」；GS 在写事务内再验当前能力、配置修订、来源、
  急停原代次、角色代次与地图/鱼范围，撤权或旧修订一律拒绝。
- 受信事件合同（类别、来源、涉及角色、配置修订）与鉴权函数在本票落地；三条事件入口
  和投递前消费由 T-108 接续，页面由 T-105 接续。

## User Stories

1. 作为管理员，我可以给某个角色单独勾选「攻略帮助」而不给「接受目标建议」；保存后
   立刻看到生效的只读工具清单与写工具清单。
2. 作为管理员，两个人同时保存同一角色时只有一人成功，另一人收到「已被其他管理员更新，
   请刷新」，刷新后看到对方保存的那一版。
3. 作为管理员，我可以显式清空一个角色的能力；清空后它仍能私聊，但任何工具都不可用。
4. 作为管理员，我撤销「接受目标建议」后，已经排队但还没执行的旧建议不会再生效。
5. 作为玩家，AI 说「我换去钓鲈鱼了」之前，只会说「已提交，等待游戏服务确认」；
   真正换没换由游戏服的状态决定。
6. 作为玩家，AI 的工具没授权或参数不对时会明确说「这个查不了」，而不是假装查过。
7. 作为服主，升级后老角色仍保留它原先被授予的只读工具子集，不会因为升级突然获得写权限
   或全部资料权限。

## Implementation Decisions

- **能力目录与投影**：`internal/aiconfig/capabilities.go`（新）。`CapabilityCatalog`、
  `CapabilityToolCatalog`、`GuideReadTools`、`ProjectTools`、`LegacyReadTools`、
  `ResolveCapabilityState`、`TrustedEvent` / `AuthorizeEvent`。本包只做纯函数与校验，
  不读写数据库。能力 ID：`guide_help`、`reply_star`、`join_roundtable`、
  `virtual_fishing`、`broadcast_catch`、`accept_target_suggest`。
- **存储**：`internal/store/repos_ai.go`。键 `ai_capability:<agent_key>`，值
  `{schema_version, revision, generation, capabilities, readonly_tools?, updated_ts}`。
  `AISaveCapabilities` 在 `BEGIN IMMEDIATE` 内比较 `base_revision`、校验 ID、绑定当前代次；
  `AIGetCapabilities` / `AIReadCapabilityState` 是唯一读入口。**不新增迁移、不新增表字段**
  ——这正是 DEV-111 A 「先提交结构/空号再批准」的前置没有被触发的原因。
- **工具注册表**：`internal/aibridge/tools.go`。`ToolSpec` 增加 `ResultSchema`、`Kind`、
  `Route`、`AgentScoped`、`HandlerKey`、`TimeoutMS`、`MaxResultBytes`（全部 `json:"-"`，
  模型只看到 Name/Description/Parameters）。`NewRegistry` 在启动时拒绝重复名、
  非法 Kind×Route、缺 handler、目录外名称；`Dispatch(ctx,name,rawArgs,ToolEnv)` 是唯一
  执行入口；`Run` 保留改造前预注入的顺序与 1600 字截断。
- **Runner**：`internal/aibridge/runner.go`。每任务读一次能力投影，`Specs` 与预注入都
  只使用投影后的工具；`runToolCalls` 只调 `Dispatch`，删除按名字的 `switch`；
  工具失败以固定中文短句回传，写受理固定为「已提交，等待游戏服务确认」。
- **写工具**：`internal/aibridge/actor.go`。删除隐式 `maybeSwitchActor` 副作用；
  `enqueueTargetSuggest` 先归一化鱼名与地图，再按当前控制代次生成
  `request_id = "ai-" + SHA256(namespace, agent, generation, task_id, tool_call_id, tool_name)[:16]`，
  投递到既有 `ai_runtime_command`，不引入第二命令队列。重复调用复用同一 ID；同 ID
  不同载荷返回冲突；队列满明确失败。
- **GS 单写者**：`internal/gameserver/ai_runtime_commands.go`、`ai_tool_jobs.go`。
  写事务内按当前能力记录复核 `capability` 与 `cap_revision`；只读作业执行前按当前能力
  复核，撤权返回「（工具不可用：授权已撤销）」；两个远程读仍只走 `ai_tool_job`。
- **后台接口**：`internal/admin/ai.go`、`api.go`。`GET /api/ai/capabilities` 返回目录、
  工具公开定义与每个角色的当前集合/修订/状态；`POST /api/ai/capabilities` 按
  `base_revision` 保存，409 冲突、400 非法、404 角色不存在，全部走既有管理鉴权与审计。
  旧 `/api/ai/tools` 与 `/api/ai/agent/update` 保留兼容，但旧 `tools` 只写降级快照，
  不再影响新版真源。
- **兼容与回滚**：不新增迁移；旧 `tools_json` 首次读取即时降级，不写库。回滚 = 恢复
  本票改动文件；`ai_capability:*` 记录保留不删，旧代码不读它，不会造成双写。
- **模型可见字段**：`ToolSpec` 的内部字段一律 `json:"-"`，已用测试断言 JSON 只有三个键。

## Testing Decisions

- **纯逻辑（aiconfig）**：目录顺序/数量、写工具唯一性、能力投影、旧子集收窄、显式空、
  非法/旧代次失败关闭、坏记录修订可读、受信事件的类别/来源/修订/圆桌升级。
  期望值全部手写字面量，不复用被测函数生成。
- **存储（真实 SQLite + 迁移）**：内置 Agent 的旧 `tools_json` 降级为只读子集且读取不写库；
  首存/冲突/显式空/未知能力/旧代次/坏记录修复/旧粒度保留。
- **注册表（单元）**：启动校验逐项、六工具字面输出等价、query/fish 别名、空查询、
  未知/无权/参数错/暂停、gs_read 参数回退、写工具缺 fish、`ToolSpec` 只暴露三个公开字段。
- **Runner（单元）**：能力投影决定 Specs 与允许集合；`runToolCalls` 对写工具真正入队、
  对未知工具回固定错误；回执文案固定。
- **GS（真实迁移库）**：只读作业按当前能力执行、撤权后拒绝且不产生写命令；写命令撤权
  → `capability_missing`，旧修订 → `stale_capability`，代次/旧建议/幂等回归沿用 T-102。
- **后台（真实 HTTP + auth）**：匿名 401、目录与旧快照读取、保存、409/400/404、
  显式空、旧表单不覆盖新意图。
- **回滚后哪些测试会变红**（在可丢弃副本逐项撤回后复跑，均实测）：
  1. 去掉 `Dispatch` 的能力白名单 → `TestRegistryDispatchLocalReads` 红。
  2. 去掉 GS 事务内能力/修订复核 → 撤权与旧修订两条用例红（`applied/`）。
  3. 让 `request_id` 每次不同 → 目标建议重试用例红（产生第二行）。
  4. 让 `ProjectTools` 忽略旧只读子集 → 旧粒度用例红（扩到八个工具）。
  5. 恢复 runner 的名字分发旁路 → `TestRunnerToolCallsRouteThroughDispatch` 红
     （写工具退回「工具没有返回数据」，不入队）。

## Out of Scope

- 页面与交互（T-105）：本票只给目录与接口，不改 HTML/JS。
- 三条事件入口与投递前消费（T-108）：本票给合同与鉴权函数；任务表没有事件类别/配置
  修订列，若 T-108 需要持久化，按 DEV-111 A 另提迁移结构、编号与回滚。
- 目标建议终态的上下文注入（读取本角色至多 8 条）：属后续任务读取，本票只保证写与受理。
- 报名、退赛、计分、管理 delete/reroll/memory_clear：不注册为模型工具。
- 新增迁移、比赛接入、真实客户端验证。

## Further Notes

[假设] 旧 `tools_json` 的首次读取降级不落库，避免读路径成为第二写者；管理员第一次保存
新版能力后，旧值只作历史快照。
[缺口] 三条事件运行入口未接、页面未消费；目标 Linux 与真实客户端仍按实机阶段。

## 返工修订（R-139 · 2026-11-19）

R-138 独立复验退回三组票内缺口，本节记录修订后的合同；§1–§6 的既有决定不撤销。

### 玩家写回执与下一轮终态

- 只要本任务提交过 `actor_target_suggest`（模型工具或旧 `maybeSwitchActor` 旁路），
  `ai_result.reply_text` 一律固定为「已提交，等待游戏服务确认。」，不再采用模型
  关于“已经换好”的正文；命令处于 pending、applied 或 rejected 都按“已受理”处理。
- 下一任务开始时读本 Agent 最近至多 8 条目标建议终态（applied/rejected/expired）；
  SQL 按 `agent_key` 过滤，pending 不进入上下文；原因、状态、鱼/图为空时写 `unknown`；
  读失败或具备写能力但没有任何可读终态时同样写 `unknown`，不能当作没提交过。
  只在任务开始时读一次，不主动推送、不轮询。

### 能力保存与 GS 运行变更

- 保存返回 `revision`、`changed`、`availability`、`runtime{desired,actual}` 与
  `commands[]`（命令 ID、request_id、状态、原因、能力修订）；重复保存同一集合
  不跳修订、不重复补派；补派失败在 `availability=failed` 报告，下一次保存按当前
  修订加 attempt 后缀重新补派。
- 只有集合（当前或历史）声明「虚拟垂钓」或本 Agent 已有能力运行命令时，才由管理
  保存接管 GS 模拟开关；保存攻略等无关集合不会顺手关闭旧后台打开的模拟。
- 所需 `actor_set_enabled` 复用现有队列与 GS 应用路径。request_id 为
  `cap1-<agent 摘要>-r<能力修订>-<enabled>[-a<attempt>]`，`expected_revision` 写能力
  修订，禁用命令带当前代次。GS 对能力命令在同一写事务里复核当前能力状态与修订，
  旧修订返回 `stale_capability`，配置缺失/失效返回 `capability_missing`。
- 撤销「虚拟垂钓」触发的 GS 禁用会推进一次代次。读侧只有在“记录代次 +1 恰好等于
  当前代次”且“该能力修订存在已应用命令、其开关意图与集合一致”时才解释这次推进；
  GS 已保证只有当前修订的能力命令能应用，因此外部禁用、额外代次推进、旧修订命令
  都不会被解释成生效。这样「保留攻略、撤销垂钓」在代次推进后仍保持攻略可用，
  同时不复活旧权限。

### 同任务撤权

- `Registry.Dispatch` 在存在检查之后、参数/路由之前调用 `ToolEnv.Recheck` 重读
  当前能力；任务开始时的允许集合只决定模型看到哪些工具，不再决定能否执行。
  撤权后本地处理器不被调用，返回固定的未授权错误；GS 远程读、写在各自执行点
  继续复核，原有撤权与急停用例保留。

### 升级与回撤

- 换版按现有部署边界：先停整组进程，新组排空 `ai_runtime_command`；升级前入队、
  没有 `capability`/`cap_revision` 的旧建议由新 GS 以 `capability_missing` 终结，
  新格式建议正常应用；确认无 pending 后再切换。
- 回撤沿用同一份库：能力记录只是惰性的 `ai_setting` 行，旧代码不读它；旧
  `tools_json`、能力快照与命令终态保留，不重放已终结命令。真实二进制整组替换与
  目标平台演练仍按实机阶段，本票只交可复跑的测试级演练。

### Testing Decisions（R-139 增补）

- 写回执：工具路径与旧旁路各一条，用本地 TLS 假厂商验证 `ai_result.reply_text`
  固定、命令仍 pending、request_id 由服务端生成。
- 下一轮：9 条终态 + 最新缺原因终态、1 条 pending、1 条他角终态，断言只出现最新 8 条、
  缺字段为 unknown、pending/他角不混入；另用真实 GS 各应用/拒绝一条并断言原因进入
  下一轮上下文；无下一轮时等待后不再发请求。
- 能力保存：HTTP + 真实 GS 消费者覆盖授权、撤销、同集幂等、409、失败后 attempt 2
  补派、旧后台 actor 写入；GS 单元覆盖旧修订命令 `stale_capability`、代次解释与
  「保留攻略」、换版排空与回撤快照。
- 撤权：假厂商屏障固定“任务开始 → 管理员清空 → 下一次 Dispatch”顺序；本地处理器
  钩子计数 0，对照用例计数 1；同包保留 GS 读/写撤权与 T-102 急停回归。

### Out of Scope（R-139 调整）

- 三条事件运行入口与投递前消费仍归 T-108；页面归 T-105；本票不注册新的模型工具。
- 能力保存只在集合声明过虚拟垂钓后接管 GS 模拟开关。旧后台直接打开、但从未保存
  虚拟垂钓的角色，其模拟开关仍由旧后台/后续 T-108 事件交集处理；本票不把它解释成
  授权，也不在保存攻略时强制关闭。
- 真实整组二进制换版、目标 Linux、真实客户端/厂商与 `-race` 仍按实机阶段。
