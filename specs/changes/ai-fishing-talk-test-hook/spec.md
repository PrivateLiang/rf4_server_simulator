# AI 发鱼测试按钮（临时辅助 · v2 删除）

Status: done

## Problem Statement

「AI 自己上鱼后主动报一句」由 `maybeAIFishingTalk` 驱动，前置是
`ai_fishing_talk_enabled=1` + Agent 的 `fishing_talk_enabled=1` + 概率门槛
（`ai_fishing_talk_star_chance` / `_near_chance` / `_miss_chance`）+ Agent 处于
「在钓鱼」角色态 + 频道节拍。概率与 actor 状态都是随机的，人无法稳定复现；
T-005 实机里这一项最难验。人 2026-11-17 21:42 要求加一个「发鱼测试按钮」，
把「后续开票删除」写进要求。

**本能力是测试脚手架，不是产品功能**；代码内全部标
`// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）`，
v2 收工时按本文件末尾的删除清单整段摘除。

## Solution

后台「AI 虚拟钓鱼」页新增一张临时卡片：选择 Agent → 点「触发一次上鱼播报」。
服务端先做强制前置校验（任一不满足返回明确 4xx），通过后写一条 pending 触发
请求；唯一活动的 gameserver 在 1 秒内消费：重跑同一套校验，挑一个「该 Agent
可见且有在线玩家」的频道，复用既有上鱼播报的文案与入队路径生成一条
`ai_task`。之后 `aibridge` 调模型写 `ai_result`、gameserver 投递 24/27 的
过程完全是真实链路。页面轮询同路径的查询接口，显示「已触发 · 任务号 / 频道 /
鱼」或拒绝原因。

只跳过两件事：**概率门槛**与 **actor 冷却**；其余（文案选择、`ai_task` 入队、
`ai_result`、24/27 投递）一律复用现有代码，不复制第二套。

## User Stories

1. 作为后台管理员，我点一下按钮就能让指定 AI 立刻在客户端频道报一句，不用等随机上鱼。
2. 作为后台管理员，开关没开 / 急停 / 未授权 / Agent 不存在时，我看到明确原因，而不是「点了没反应」。
3. 作为玩家，测试触发的消息与正常随机播报没有区别：同一段提示词、同一个任务队列、同一条频道投递。
4. 作为维护者，`grep "T-027 测试脚手架"` 能定位全部脚手架；按删除清单可以机械摘除、不留调用点。

## Implementation Decisions

### 入口与鉴权

- `POST /api/ai/test/fishing-talk`，body `{agent_key}`，与既有后台接口同级 JWT 保护。
- `GET /api/ai/test/fishing-talk?request_id=...` 查询同一次触发的结果；路由与 POST 同路径。
- 响应：POST 成功返回 `202 {ok, status:"pending", request_id, agent_key}`；GET 返回
  `pending` / `enqueued`（含 `task_id`、`channel`、`fish_name`、`event`）/ `rejected`（含 `reason`、`message`）。

### 强制前置校验（admin 同步 4xx，gameserver 消费时重跑）

| 条件 | 不满足时的结果 |
|---|---|
| Agent 存在且未删除 | 404 `Agent 不存在或已删除` |
| Agent 已停用 / 尚未接入游戏 | 409（`agent_disabled` / `agent_not_joined`） |
| Agent `fishing_talk_enabled=1` | 409「该 Agent 的『上鱼报话』开关未开启」 |
| 全局 `ai_fishing_talk_enabled=1` | 409「全局『AI 上鱼播报』开关未开启」 |
| `emergency_stop=0` | 409「紧急停止已开启」 |
| `aiconfig.Egress` 放行（`outbound_enabled=1` + 有效 outbound 授权） | 409「外发未放行：<状态>」 |
| gameserver 侧：存在「有在线玩家且该 Agent 可见」的频道 | 结果 `rejected`（`no_online_channel`），不产生任务 |
| gameserver 侧：频道所在地图有真实鱼群数据 | 结果 `rejected`（`no_fish_available` 等），不产生任务 |

`catch_talk` 是「玩家上鱼后 AI 接话」的独立授权项，不是本链路的前置；现有播报
路径真正要求的是 `aiconfig.Egress`（outbound 授权 + 出网开关），这里按现有路径执行。
所有拒绝都带可读 `message`，不静默降级。

### 跨进程方式

- admin 与 gameserver 是两个进程，且本票文件白名单不包含 store 的运行态命令表，
  因此不扩展 T-020 的 `ai_runtime_command` 有限命令契约：用既有共享 KV 表放一行
  `t027:fishing-talk-test` 作为请求/结果（与 `stats:sessions` 同一种跨进程共享键），
  不新增表、不改迁移、不碰运行态写入口。
- gameserver 在 `StartAIFishing` 生命周期上挂 1 秒消费者：读 pending → 重校验 →
  选频道 → 入队 → 写回结果。单活动 gameserver 是部署前提（与 T-020 相同）。
- 结果行带 `request_id`：旧 request_id 不会误读后来覆盖的新请求。

### 复用与边界

- 把 `maybeAIFishingTalk` 中的「文案 + 上蓝圆桌 + `AIEnqueueSystemChannel`」抽成
  `aiFishingTalkEnqueue`；随机播报与测试钩子共用，概率与 actor 冷却只留在
  `maybeAIFishingTalk` 内。测试钩子走 `evStar`（不触发圆桌分支），不写 `ai_catch`、
  不占频道节拍/配额、不改任何 `ai_setting`、不直接写 `mail` 或 24/27。
- 因为跳过概率与冷却，正常随机播报的概率、节拍与配额行为不变；既有用例全绿。

### 兼容 / 回滚

- 回滚 = 按删除清单摘除路由 / handler / gameserver 脚手架 / 按钮与查询；数据只多一行
  `kv` 键，删除即可，无迁移、无表结构变化。
- 删除后 `aiFishingTalkEnqueue` 仍被 `maybeAIFishingTalk` 使用，是产品代码，不回退重构。

## v2 删除清单（机械可执行）

1. `internal/admin/api.go`：删除两条路由注册
   `POST /api/ai/test/fishing-talk` 与 `GET /api/ai/test/fishing-talk`
   （含其上的 `T-027` 注释）。
2. `internal/admin/ai.go`：删除 `handleAIFishingTalkTest`、`handleAIFishingTalkTestGet`、
   `aiFishingTalkTestHTTPStatus` 三个函数及 `gameserver` import（若删除后不再被本文件使用）。
3. `internal/gameserver/ai_fishing_talk.go`：删除 `AIFishingTalkTestKVKey`、`aiFishingTalkTest*`
   常量、`AIFishingTalkTestReject`、`AIFishingTalkTestState`、`AIFishingTalkTestPrecheck`、
   `RequestAIFishingTalkTest`、`GetAIFishingTalkTest`、`startAIFishingTalkTest`、
   `consumeAIFishingTalkTest`、`aiFishingTalkTestChannel`、`finishAIFishingTalkTest`
   及该文件末尾的 T-027 脚手架段（保留 `aiFishingTalkPrompt` / `aiFishingTalkEnqueue` /
   `maybeAIFishingTalk` / `tryAIFishingRoundtable`）。同时移除只为脚手架引入的
   `database/sql`、`encoding/json`、`errors`、`sort`、`strings`、`aiconfig` 等 import
   （以 gofmt/编译器为准）。
4. `internal/gameserver/ai_fishing.go`：删除 `StartAIFishing` 里的
   `s.startAIFishingTalkTest(db, stop)` 一行及其注释。
5. `internal/admin/web/ai/index.html`：删除 `#aiPane-fishing` 内的
   `#aiFishingTalkTestCard` 整张卡片（含 `#aiTestFishingAgent`、
   `data-action="test-fishing-talk"`、`#aiTestFishingResult`）。
6. `internal/admin/web/ai/ai.js`：删除 `renderAIFishingTestAgents`、
   `triggerAIFishingTalkTest` 两个函数、`onMountedClick` 里的
   `case 'test-fishing-talk'`、`renderAgentList` 里的
   `renderAIFishingTestAgents()` 调用，以及全部 `T-027` 注释。
7. 测试：整文件删除 `internal/gameserver/ai_fishing_talk_test_hook_test.go`、
   `internal/admin/ai_fishing_talk_test_hook_test.go`。
8. 数据清理（一次性 SQL，不走迁移）：`DELETE FROM kv WHERE k='t027:fishing-talk-test'`。
9. 删除后验收：`grep -rn "T-027 测试脚手架" internal` 无命中；
   `grep -rn "fishing-talk-test\|AIFishingTalkTest" internal` 无命中；
   `go build ./...`、`go vet ./...`、`go test ./internal/admin ./internal/gameserver -count=1` 全绿。

## Testing Decisions

- gameserver（真实迁移 SQLite + 只读静态库 + 真实隧道探测）：
  `TestAIFishingTalkTestHookUsesRealChain` 断言概率门槛设为 0 时钩子仍触发、只新增
  1 行 `ai_task`（kind/scope/agent/文案与现有提示词一致）、正常随机播报仍被概率挡住；
  再用 `AIComplete` 走 `aibridge` 的真实写入口生成 1 行 `ai_result`，由既有
  `pollAIResults` 投递 24/27。
  `TestAIFishingTalkTestHookRevalidates` 断言消费时开关/consent/Agent 状态被改、
  无在线频道、超时五种情况都 `rejected` 且零 `ai_task`。
- admin：前置六种不满足各返回 4xx、写请求前零 `ai_task`；全满足时 202 且只写
  pending 请求（不直接写 `ai_task`）、GET 能查到结果；两条路由经 mux 注册且需要 JWT。
- 真实模型调用不在单测内（票禁连第三方）；`ai_result` 由 `AIComplete` 模拟
  `aibridge` 的写入口，GS 投递为真实代码。真实客户端可见性仍由人证（T-005）。

## Out of Scope

- 不做批量/定时触发；不用于生产。
- 不改迁移、不改 CI、不改官方协议/结算/经济。
- 不绕过 `fishing_talk_enabled` / `ai_fishing_talk_enabled` / `emergency_stop` /
  consent / 频道可见性；不直接往 `mail` 或 24/27 塞假消息。
- 不写 `ai_catch`（本钩子只验证播报链路；DoD 只要求 `ai_task` / `ai_result` 各 1 行）。
- 不改变正常随机播报的概率、节拍与配额；不修改 consent 语义。

## Further Notes

- `[事实]` 本票改动只在票面白名单内：admin 两个源文件 + 两个前端资源 + gameserver
  两个源文件 + 两个包的测试 + 本 spec。
- `[事实]` T-020 的 `ai_runtime_command` 是运行态单写者契约（有限 kind 表），
  本票不扩展它；脚手架用 KV 请求/结果行，删除时一行 SQL 即可清理。
- `[假设]` 单活动 gameserver 是部署前提；多 GS 并发消费同一请求不在本脚手架保证范围内。
- `[事实]` 部署级出口主机白名单不在本钩子的前置校验里，仍由 `aibridge` 既有的每请求
  出口闸执行（不绕过）；未配置时任务按既有失败路径落 `ai_result`，后台「最近错误」
  可见，本钩子行为不变。
- `[缺口]` 真实客户端上「点按钮 → 频道出现 AI 播报」仍需人证；真实模型调用未在单测内跑。
