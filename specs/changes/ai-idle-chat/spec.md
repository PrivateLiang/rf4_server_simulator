# 闲时搭话：频道安静时 AI 自己找话头

Status: done

## Problem Statement

AI 的主动说话只有两个来源：玩家上星/上蓝后的反应，以及自己上鱼后的播报。虚拟钓鱼的大部分时间是等待，一小时实际约 7 次上鱼、经档位与性格门后只剩 1–2 句；玩家无聊时最想有人搭话，AI 却恰好最安静。

同时，「每小时发言上限」在实现上计的是上鱼尝试而不是真正说出口的话，且上鱼播报与未来可能的新说话通道没有统一预算。

## Solution

新增一条独立的主动事件通道「闲时搭话」：角色正在钓鱼、频道里真人安静一段时间、且距上次上鱼超过 5 分钟时，由服务端挑一个愿意开口的角色，让它按固定事实与轮换话头说一两句。

- 新能力 `idle_chat`（显示名「闲时搭话」），与「虚拟垂钓」「播报自己渔获」分开授权；升级时不自动授予。
- 新事件类别 `EventIdleChat`，复用既有受信事件、证据、能力复核与 24/27 投递链路。
- 静默计时只由真人公聊重置；AI 自己的消息不重置，避免自我续命。
- 节奏：静默 ≥ `ai_idle_silence_min`（默认 3 分钟）、每小时 ≤ `ai_idle_hourly_limit`（默认 4 句）、最小间隔 `max(2min, 60/limit min)`、全局 ≤ 30 句/小时。
- 内容接地：只注入地点与已钓时长，话头由服务端轮换（安静/耐心/自嘲/询问/钓点/闲聊），提示词禁止价格、坐标、天气、鱼口与重复。

## User Stories

1. 作为玩家，我在频道里安静钓鱼时，AI 会偶尔自己找个话头，而不是只有上鱼才出声。
2. 作为玩家，我刚钓到鱼或正在聊天时，AI 不会抢话。
3. 作为服主，我可以只给某个角色开「闲时搭话」，也可以完全不开；关闭后零行为、零模型调用。
4. 作为服主，我调「安静多久」和「每小时几句」时，页面直接告诉我换算后的节奏。
5. 作为维护者，撤掉 `idle_chat` 能力后，已排队但未投递的旧任务不会再生效。

## Implementation Decisions

- **能力与事件**：`internal/aiconfig/capabilities.go` 新增 `CapabilityIdleChat` / `EventIdleChat` 与目录项；无工具、不要求圆桌。`CapabilityForEvent` 自动映射，`AuthorizeEvent`、入队证据与投递前复核复用既有逻辑，不新增分支。
- **触发**：`internal/gameserver/ai_idle.go`。`aiFishingForChannel` 在筛出「此刻在钓鱼」的角色后调用 `maybeAIIdleChat`；返回 true 时跳过本 tick 的上鱼，保证同一时刻只有一条消息。
- **静默**：`hSocialPublic` 调 `noteAIHumanMessage` 记录真人消息时间；状态在 `aiIdleStates`（频道级、内存），进程重启清零。
- **候选**：`aiEventCandidates(EventIdleChat)` ∩ 当前钓鱼名单；按 Key 稳定排序，逐个跑 `activityWants`（`activityIdleFeature`：社交 0.8 / 尝新 0.45 / 投入 0 / 作息 0.35），第一个愿意的人说。
- **预算**：频道级滚动小时 + 最小间隔；全局 30 句/小时兜底多频道并发。本批与上鱼播报分开计，不共享 B。
- **提示词**：只注入 `actorLocName` 与 `SinceTS` 推算的分钟数；话头由 `aiIdleTopics` 轮换，同话头 30 分钟冷却、最近 3 条不复用。
- **证据**：`AIEnqueueSystemChannel` 带 `AITaskEvidence{Category: idle_chat, Capability: idle_chat, GSPersonalityRevision}`，与 T-108 既有合同一致。
- **设置**：`internal/admin/ai_settings.go` 新增 `ai_idle_silence_min` / `ai_idle_hourly_limit`（`Advanced`，通用首屏预算不变）。
- **前端**：`web/app/pages/ai.js` 的「虚拟钓鱼」页新增「闲时搭话」区块：状态（哪些角色已开启）、两个数字输入、按当前值换算的效果预览；能力勾选由服务端目录自动渲染。旧单页不新增入口。
- **无迁移、无协议、无新路由、BR 不按类别分支。**

## Testing Decisions

- **纯逻辑**（`ai_idle_test.go`）：静默未到 / 间隔太近 / 额度用尽 / 滚动小时翻篇；话头排除最近与冷却、同种子可复现；提示词只含地点与时长；特征项投入为 0；最近上鱼时间只读。
- **链路**（`ai_idle_chain_test.go`，真实 DB + 真实入队）：能力 + 订阅 + 四维满足 → 入队且证据绑定 `idle_chat`；同小时第二次被间隔挡住。
- **能力**（`capabilities_test.go`）：`idle_chat` 存在、事件映射、无工具、非 legacy、不要求圆桌；目录数量 10 → 11。
- **回滚后变红**：撤掉 `idle_chat` 目录项 → 入队报未知类别；把 `lastHumanAt` 改成由 AI 消息重置 → 链路用例的第二次调用会入队；去掉间隔判定 → 同小时第二次入队。
- 门禁：`go build ./...`、`go vet ./...`、`go test ./internal/gameserver ./internal/admin ./internal/aiconfig ./internal/aibridge ./internal/store -count=1`。

## Out of Scope

- 上鱼播报与闲时搭话共享同一份预算、以及「冷清/正常/热闹/自定义 + 小时内分布」：需要改上鱼节拍，单独一批。
- 旧单页（`legacy.html`）的设置入口。
- 真实客户端里的观感、烦人阈值与实机调参。
- 玩家侧开关（静音/拉黑）与关系系统。

## Further Notes

- `[事实]` 默认关闭：能力未授予时 `aiEventCandidates` 为空，不产生任何任务与模型调用。
- `[事实]` 闲时搭话不写 `ai_catch`，不改变渔获、经济或玩家存档；只写 `ai_task`/`ai_result` 与既有投递。
- `[事实]` 本轮同时把能力「参加圆桌」的用户可见名统一为「参加多人对话」，ID 与事件类别不变。
- `[缺口]` 未做真实客户端验证；默认值（3 分钟 / 4 句 / 5 分钟没鱼）需要一周体感后再调。
