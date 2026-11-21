# 圆桌人数跟随氛围

Status: done

## Problem Statement

「只有 3 个人说话」不是配置没找到，而是被四层硬上限锁死：

- 后台 `roundtable_max_speakers` 隐藏，范围 2–3；
- gameserver `aiIntSetting(..., 3, 2, 3)` 夹到 3；
- aibridge 消费时同样夹到 2–3；
- `ai_task_evidence` 的参与者上限常量是 3，超过直接拒绝入队；
- 上鱼圆桌写死 `len(keys)==3`，闲时升级固定 2 人。

同时 `roundtable_max_turns` 上限 6、`roundtable_max_ms` 上限 30 秒；10 个人每人一次模型调用，光轮次间隔就超过时限，只放开人数也没用。

## Solution

圆桌规模也跟随「整体氛围」，不再给预设单独的人数旋钮；只有自定义档保留一个 2–12 的人数输入。

| 氛围 | 最多接话 | 最多条数 | 冷却 | 每小时 | 单次时限 |
|---|---|---|---|---|---|
| 冷清 | 3 | 4 | 5 分钟 | 1 | 30 秒 |
| 正常 | 5 | 6 | 2 分钟 | 3 | 45 秒 |
| 热闹 | 10 | 12 | 45 秒 | 6 | 90 秒 |
| 不限 | 12（实际取频道可用数） | 16 | 20 秒 | 12 | 120 秒 |
| 自定义 | 2–12（新字段） | 人数 +2（≤16） | 2 分钟 | 3 | 90 秒 |

- 实际参与人数 = min(档位, 频道内已授权且当前可接话的角色数, 12)；闲时搭话另要求角色正在钓鱼。
- 条数恒 ≥ 人数，保证排在后面的角色也能轮到。
- 闲时随机升级仍只拉 2 人：避免一条「今天真安静」触发 10 人围观。
- 圆桌仍按一次事件计 1 格说话预算；Tip 明示，否则 10 条只花 1 格会让「每小时 N 句」对圆桌不严格。

## Implementation Decisions

- `internal/aiconfig/talk_mood.go`：`TalkMood` 新增 `RoundtableSpeakers/Turns/CooldownSec/Hourly/MaxMS`；`RoundtableProfile`、`TalkMoodRoundtable`、`CustomRoundtableProfile`；`RoundtableMaxParticipants = 12`。
- `internal/store/repos_ai.go`：`AITaskEvidenceMaxParticipants = aiconfig.RoundtableMaxParticipants`（3 → 12）；圆桌任务 TTL 60 → 180 秒，容纳 120 秒大桌。
- `internal/gameserver/ai_talk_budget.go`：`aiRoundtableProfile(ctx)` 解析氛围/自定义；GS 与 BR 共用同一份 `aiconfig` 数据。
- `internal/gameserver/ai_channel.go`：点名上限、起桌人数、冷却、每小时、时限全部改用 profile。
- `internal/gameserver/ai_fishing_talk.go`：上鱼圆桌去掉写死的 3，按 profile 收 peer；冷却/每小时改为 profile。
- `internal/aibridge/runner.go`：`roundtableProfile` 同源解析；`maxSpeakers/maxTurns/maxMS` 改用 profile；`maxStreak`/`gap` 仍读旧隐藏参数。
- `internal/admin/ai_settings.go`：新增 `ai_talk_roundtable_speakers`（2–12，Advanced）；旧 `roundtable_max_*` 保持 Hidden。
- `internal/admin/web/app/pages/ai.js`：预览增加「一次圆桌最多 X 人、Y 条」；自定义新增人数输入；Tip 说明 Actual 人数、预算口径与成本。

## Testing Decisions

- `TestAITalkRoundtableProfile`（真实 DB）：quiet/normal/lively/unlimited/custom/未知值与越界夹取。
- `TestTalkMoodPresets`：人数 2–12、条数 ≥ 人数且 ≤16、冷却/配额/时限合法。
- 既有 `TestT108*` 圆桌与证据用例继续通过（2–3 人场景不变）。
- 浏览器冒烟：四档切换、预览出现「一次圆桌最多 N 人」、自定义人数输入、无 console error。

## Out of Scope

- 单桌超过 12 人（证据表有界，频道也不该一次拉满）。
- 圆桌按实际条数计预算（当前 1 次事件 1 格）。
- 每个氛围再单独暴露条数/冷却/时限旋钮。

## Further Notes

- `[事实]` 这次改的是「结构性上限」，不是 UI 开关；只改设置不生效是因为四处都夹到 3。
- `[事实]` 一次 10 人圆桌 = 10 次模型调用；热闹档一小时最多 6 次，成本会明显上升。
- `[缺口]` 10 人桌的实际观感（是否太吵、是否接得上）需实机验证；若太散，优先降到 6–8 人。
