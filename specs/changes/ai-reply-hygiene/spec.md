# 回复卫生与渔获重量统计

Status: done

## Problem Statement

真实频道里出现两类明显错误：

1. **AI 反复自我介绍**。玩家看到「我叫小渔，萌新一枚，请多关照~」，但角色的展示名是「快乐小钓手」，人设里也没有「小渔」这个名字。
2. **渔获重量永远是 0.00 kg**。角色卡「钓鱼战绩」显示累计渔获 35 条、今天 35 条，但两处重量都是 `0.00 kg`，而 `ai_catch` 里实际有 80.8 kg。

## Solution

### 自我介绍

根因是 `runner.go` 的 `firstTalk`：它用**进程内会话历史是否为空**判断「第一次对话」，而会话只在内存、进程重启或急停代次变化就会清空；于是每次重启后的第一条频道消息都会命中：

```
这是你和这个玩家的第一次对话，可以顺口带一句你叫什么，一句话即可。
```

模型被要求报名字，但没有人设名字可用，就编了「小渔」。

处理：

- 删除 `firstTalk` 判定与「第一次对话报名字」指令；
- 在全局约束里加一条硬规则：**不要主动自我介绍、不要报名字或人设、不要用「我是谁」开场；玩家明确问「你是谁」时才用一句话回答。**
- 不做「持久化是否已介绍过」的状态：玩家并不需要每次重启后被重新介绍一次，直接不主动介绍更简单。

### 重量统计

后台证据 `aiOnboardingAgent` 已经算了 `CatchTotalG / CatchTodayG`，但 `agentView`（`/api/ai/status` 的 agents 数组）只暴露了 `catch_total / catch_today` 两个计数，页面读 `catch_total_g / catch_today_g` 得到 `undefined`，除 1000 后显示 0.00。

处理：`agentView` 增加 `catch_total_g`、`catch_today_g` 两个字段并赋值。页面不需要改。

## User Stories

1. 作为玩家，频道里的 AI 不会每次重启后第一句就自我介绍，也不会编一个不存在的名字。
2. 作为玩家，我问「你是谁」时它仍会正常回答。
3. 作为服主，角色卡上的累计/今日重量与实际渔获一致（例如 35 条 / 80.84 kg）。

## Implementation Decisions

- `internal/aibridge/runner.go`：删除 `firstTalk` 与对应 system 指令；在通用约束中追加「不主动自我介绍」规则（对所有频道/私聊生效）。
- `internal/admin/ai.go`：`agentView` 新增 `CatchTotalG int64 json:"catch_total_g"`、`CatchTodayG int64 json:"catch_today_g"`，从 `aiOnboardingAgent` 复制。
- 不改数据库、不改页面、不改协议。

## Testing Decisions

- `go test ./internal/aibridge ./internal/admin ./internal/store` 全绿；`go vet ./...` 通过。
- 浏览器冒烟：真实库副本打开「老李 → 钓鱼战绩」，显示 `35 / 80.84 kg` 与 `35 / 80.84 kg`（修复前均为 0.00）。
- 自我介绍属于模型行为，无法用离线单测覆盖；本次以「删除触发指令 + 反向硬约束」处理，需真实对话观察。

## Out of Scope

- 持久化「是否已经自我介绍过」；既然不主动介绍，就不需要这个状态。
- 对模型输出做「我叫…」正则后处理；先用提示词约束，若仍偶发再考虑。
- 重量单位换算规则保持不变（克存储、页面除 1000）。

## Further Notes

- `[事实]` 会话历史只在 aibridge 内存里，进程重启即清空；`sessionKey` 还包含急停代次，因此「第一次对话」在运行期并不可靠。
- `[事实]` 页面读取的字段名是 `catch_total_g` / `catch_today_g`，与后端新增字段一致。
- `[缺口]` 真实模型是否完全不自我介绍，需要重启后跑一段频道对话确认。
