# AI 安全默认值单点化

Status: done

## Problem Statement

`outbound_enabled` 的「缺省即开」在 9 处独立出现：迁移种子、后台注册表、桥接进程两处闸门、游戏服三处闸门、`/healthz` 展示、后台页面。任何一处改动都不会带动其它处；更关键的是，历史迁移把该键种子写成 `"1"`，而后台一次整卡保存会把该模块全部键重写一遍，导致「迁移默认 1」与「用户显式开启 1」在数据上不可区分（`回执/T-006_回执.md` §6.3）。运维在后台看到「开」，无法判断这是不是用户的选择。

## Solution

新增低层包 `internal/aiconfig` 作为唯一的键名、安全默认与判定声明点。出网总闸改为 fail-closed：缺键、非法值、历史值 `"1"`、整卡保存后的 `"1"` 一律按关闭处理；`emergency_stop` 只有明确的 `"0"` 才算未停止，无法解析的非空值保守停止。后台不再自己解释默认值，而是直接输出 `aiconfig` 的判定描述；前端只消费描述。四个历史默认开启键在后台逐个显示「未确认」，不再显示「开」。

## User Stories

1. 作为管理员，我希望在后台看到「出网：未确认」而不是「开」，以便知道当前配置不能证明是我主动开启的。
2. 作为管理员，我希望保存整张「连接与总开关」卡片后外发仍然关闭，而不是被默认值悄悄打开。
3. 作为运维，我希望紧急停止的值写坏时按停止处理，而不是继续外发。
4. 边界：历史迁移种子保持 `"1"` 不变；新库/旧库升级后，只要没有显式授权记录，外发一律关闭。
5. 边界：`AI.Enabled=true` 只表示启动 aibridge 进程，不构成任何外发授权。

## Implementation Decisions

- 新增 `internal/aiconfig/settings.go`：键名常量、安全默认（`outbound_enabled="0"`、`emergency_stop="0"`）、`Lookup` 读取类型、`Egress`/`EmergencyStop`/`Security` 判定。包内不导入 admin / aibridge / gameserver。
- BR（`internal/aibridge/runner.go`）与 GS（`ai_catch_react.go`、`ai_fishing.go`、`ai_fishing_talk.go`）的全部外发闸门改走 `aiconfig`；GS 三处共用一个 `aiEgress` 辅助函数。BR/GS 不导入 admin。
- 后台（`internal/admin/ai_settings.go`、`ai.go`）的默认值改引用 `aiconfig` 常量；`/api/ai/status` 新增 `security` 字段（外发判定 + 四个历史默认开启键的展示状态）。前端 `web/index.html` 只读取 `security.egress.display` 与 `security.keys`，删除本地「缺省即开」判断。
- `cmd/aibridge/main.go` 的 `/healthz` 保留原始值，但新增 `outbound_allowed`/`outbound_state`/`outbound_display`，默认值改引用 `aiconfig`。
- 历史 SQL 种子（`00900_ai_bridge.sql`，T-014 由旧号 `00060` 改号）不动；不改表结构、不加新键、不新增迁移。
- 本地 actor 意图与外发授权分开：`ai_actor_enabled` / `ai_catch_talk` / `ai_memory_inject` 的运行语义不变，只把声明默认收敛到 `aiconfig` 并在后台标「未确认」。
- T-009 的接入点已存在：所有运行期读取点都经过 `aiconfig.Egress`；后续在该函数内增加「显式授权/撤销」读取即可，不需要再造空接口或无调用点的注册表。

## Testing Decisions

- 纯逻辑（`internal/aiconfig`）：用手写字面量验证缺键、非法值、历史值 `"1"`、显式 `"0"`、急停优先、急停解析失败保守停止；期望值来自人工定义的状态与中文描述，不用被测代码反向生成。
- 桥接进程（`internal/aibridge`）：用本地 `httptest` 模拟厂商并统计请求数；缺键/非法值/历史 `"1"`/整卡保存四种情况下 `Runner.handle` 必须写 `fallback` 且请求数为 0；`config.AI.Enabled=true` 时同样不放行。
- 后台（`internal/admin`）：真实 SQLite + 真实 handler。整卡保存请求后断言库值确为 `"1"`、`aiconfig` 仍不放行；`/api/ai/status` 的 `security` 四个键显示「未确认」；静态检查 `index.html` 不再包含 `outbound_enabled!=='0'`。
- 游戏服（`internal/gameserver`）：三条链路共用的 `aiEgress` 在历史 `"1"`、非法值、显式 `"0"`、急停、缺键下都不放行。
- 观测口（`cmd/aibridge`）：`/healthz` 快照在历史 `"1"` 下 `outbound_allowed=false`，缺键回落到安全默认。
- 回滚后哪些测试会变红：把 `aiconfig.Egress` 的 `"1"` 分支改回放行 → aiconfig/aibridge/admin/gameserver 用例变红；把前端默认判断改回 `outbound_enabled!=='0'` → 后台 UI 静态用例变红；把后台默认值改回字面量 `"1"` → 单点声明一致性用例变红。

## Out of Scope

- 不实现 T-009 的显式授权/撤销存储与交互，只保留真实读取路径。
- 不修 T-010 的后台「测试连接」绕过总闸，不修 T-011 的主机白名单。
- 不改历史 SQL 种子、不改表结构、不新增迁移、不改本地行为的运行语义。
- 不改协议、结算、状态条真源、静态库与 CI。

## Further Notes

- `[事实]` 九处默认值副本见 `回执/T-006_回执.md` §6.1；`"1"` 与显式开启不可区分见 §6.3。
- `[事实]` `internal/gameserver/ai_channel.go:132` 仍是 `emergency_stop` 的独立兜底 `"0"`；该文件不在本票白名单，未改，默认值方向安全。
- `[推断]` 本票完成后外发默认关闭属于预期行为；恢复外发不是回滚手段，安全票回滚只能回到禁止外发的隔离环境。
- 未验证：真实厂商端到端出网（票禁第三方）；T-009 显式授权落地后的放行路径。
