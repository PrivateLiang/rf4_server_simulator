# 说话体感修复：节拍、闲时上限、不限档与多人接话

Status: done

## Problem Statement

2026-11-21 18:20–19:05（约 45 分钟）的真实运行诊断：

- `ai_catch` 写入 **109 条**，但频道只听到 **4 条上鱼播报**、**2 条闲时搭话**、**0 次 AI 自发多人对话**；
- 「热闹」档设了 12 句/小时，实际远低于此；
- 玩家体感是「只有上鱼才说话、说话单调、各说各的」。

根因有三类：

1. **静默上鱼绕过节拍与配额**：`attempt=false`（普通达标 / 不可播报）的分支直接写 `ai_catch` 并 return，既不看 `interval` 也不占配额。每个 10 秒 tick 都有约一半概率白捡一条鱼，一小时可写上百条；而真正能开口的档位又被 `ai_fishing_hourly_limit` 按「尝试」而不是「说出口」计数。
2. **闲时搭话被自己的小上限锁死**：`ai_idle_hourly_limit` 默认 4、间隔 `60/4=15 分钟`，与「热闹」档的 12 句/3 分钟无关；再叠加「距上次上鱼 5 分钟」和频繁上鱼，实际只剩 2 条/45 分钟。
3. **多人对话几乎没有触发源**：AI 自发圆桌只在上蓝（0.2%）时尝试；闲时搭话永远是单人独白。

## Solution

- **节拍先行**：每一次上鱼（无论档位、是否可播报）都先过频道 `interval`；节拍通过后推进 `interval` 并写渔获。配额只决定「这一次能不能开口」，不再抑制写库。
- **闲时并入总预算**：删除闲时自己的小时上限与 15 分钟间隔，只保留「真人静默」与「距上次上鱼 1 分钟」两个触发条件；句数与间隔全部交给频道级「说话节奏」。
- **新增「不限」档**：不设每小时上限，只保留 60 秒最小间隔（上鱼播报仍可插队）；防止退化成每 10 秒一条。
- **闲时升级多人**：安静时的一段搭话有概率升级成两人对话（冷清 0、正常 10%、热闹 25%、不限 35%、自定义 15%），两人都需具备「闲时搭话」，仍按一次事件计 1 格预算。
- **角色卡显示运行态**：有 `ai_actor_state` 时显示「钓鱼中 · 湖名 · 目标鱼 · 已 N 分钟」。
- **日志降噪**：`attempt` 阶段改为 Debug，避免每 10 秒刷一行 INFO。
- **清理死旋钮**：`ai_fishing_talk_enabled`、四个 `ai_fishing_talk_*_chance`、`ai_fishing_roundtable_chance`、`ai_fishing_hourly_limit` 隐藏（保留旧库兼容，不再上页面）；其中 hourly_limit 仍作为隐藏兜底读取。

## Implementation Decisions

- `internal/gameserver/ai_fishing.go`：`interval` 检查移到 `attempt` 分支之前；`canTalk = attempt && fishCount < hourLimit`，只影响是否调用 `maybeAIFishingTalk`，渔获始终写入。
- `internal/gameserver/ai_idle.go`：
  - `aiIdleState` 去掉 `lastIdleAt/hourStart/hourCount` 与嵌入的预算状态；`aiIdleDue(now,lastHuman,silence)` 只判静默。
  - 删除 `ai_idle_hourly_limit` 与 `60/limit` 间隔；保留 30 秒探测节流与进程级 60 条/小时安全阀。
  - `aiIdleMinSinceCatch` 从 5 分钟降为 1 分钟。
  - 新增 `aiIdleRoundtableChance(mood)` 与两人圆桌入队；`aiIdleRoundtableRollForTest` 仅测试注入。
  - 话头从 6 个扩到 10 个（加入天气、装备、疲惫、渔获故事）。
- `internal/aiconfig/talk_mood.go`：新增 `unlimited`（Budget 0、MinGap 60s、Burst 1）。
- `internal/gameserver/ai_talk_budget.go`：`Budget<=0` 走不限分支，仅校验非优先通道的最小间隔并更新 `lastAt`。
- `internal/gameserver/ai_activity_policy.go`：`attempt` 阶段日志降为 Debug。
- `internal/admin/ai_settings.go`：4 个旧参数标 `Hidden`；`ai_idle_hourly_limit` 标 `Hidden` 并注明已并入整体氛围。
- `internal/admin/web/app/pages/ai.js`：
  - 「基本」只留虚拟上鱼总开关；「说话节奏」支持四档（含不限）与自定义；「闲时搭话」只留安静阈值；「高级」移除已失效的开口概率。
  - 角色卡新增运行态行 `actorLine(a)`。
- 无迁移、无协议、无新 HTTP 路由。

## Testing Decisions

- `TestT108FishingChainDev113A/静默档也受节拍约束`：预置 `lastAt=now, interval=1h`，调用后 `ai_catch` 必须为 0（回归：修复前普通达标会无视节拍写库）。
- `TestAITalkTakeUnlimited`：不限档无小时上限、仍需 60 秒间隔、上鱼可插队。
- `TestAIIdleChatRoundtable`（真实 DB）：两个闲时角色 + 固定抽样 0 → 一条 `roundtable` 任务，证据含双方 `idle_chat`。
- `TestAIIdleDue` / `TestAIIdleRoundtableChance` / `TestTalkMoodPresets` 更新。
- 既有四段诊断仍通过（`attempt/quota/enqueue/delivery`）；`quota` 语义改为「获准开口」。
- 浏览器冒烟：五页 + 氛围四档 + 自定义展开，console 0 error。

## Out of Scope

- 按角色分配预算、滚动窗口、圆桌按条数计费、预算持久化。
- 真实客户端上的最终体感与默认值微调（需再跑一小时）。
- 旧单页（`legacy.html`）的新设置入口。

## Further Notes

- `[事实]` 修复前的 109 条/45 分钟不是「AI 很勤快」，而是静默档绕过节拍造成的假渔获；修复后应按 base 3min ×1.2 节拍约 8 条/小时。
- `[事实]` 不限档的 60 秒间隔是产品底线，不是费用护栏；模型调用会明显增加。
- `[缺口]` 多人对话的观感、两个 AI 是否真的「接得上」，仍需实机看一周。
