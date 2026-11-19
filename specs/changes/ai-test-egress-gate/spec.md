# AI 测试连接统一受闸

Status: done

## Problem Statement

T-009 把外发总闸收敛到 `aiconfig.Egress` 之后，后台「测试连接」（`POST /api/ai/test`）仍是唯一一条不受它约束的出口：`handleAITest` 不读 `ai_setting`，且允许页面当前未保存的 `base_url` / `model_id` / 明文 `api_key` 覆盖库中配置，等于在总闸关闭、急停或没有任何 consent 时也能把 Key 发到任意 http(s) 地址（`回执/T-006_回执.md` §6.4 出口 C）。

## Solution

后台「测试连接」与运行期模型调用、联网搜索改用同一套 `aiconfig.Egress` / consent 判定和同一个目的地口径：总闸关闭、紧急停止、没有有效 `outbound` 授权、授权范围已变化时一律不建立外部连接，并返回与运行期相同的拒绝原因。测试连接只使用库里已保存、且与 consent 目的地快照一致的厂商配置和 Key；页面未保存的地址、模型或明文 Key 直接被拒绝。请求只带固定合成提示，失败信息不包含明文 Key、完整厂商地址或玩家内容。

## User Stories

1. 作为管理员，我在没有确认「允许模型调用出网」时点「测试连接」，看到的是「未确认」，而不是一次真实外发。
2. 作为管理员，我撤销授权、触发紧急停止或关闭外发总闸后，再点「测试连接」会被拒绝，且原因与后台安全卡片显示的一致。
3. 作为管理员，我换了厂商地址或模型但还没保存时，测试连接提示先保存；保存后旧授权失效，必须重新确认。
4. 作为管理员，我填了一个尚未保存的 Key 时，测试连接拒绝使用它，不会把页面上的明文 Key 发出去。
5. 作为运维，测试连接成功时只复用已保存的配置；失败时页面上看不到明文 Key、完整厂商地址。
6. 边界：测试连接按钮本身不构成 consent，点它不会写入授权；撤销不取消已经发出的请求（与 T-009 的边界一致）。

## Implementation Decisions

- `internal/aiconfig`：新增 `EgressForDestination`，在 `Egress` 放行后再核对「本次要发送的目的地」是否等于授权冻结的目的地快照；不相等按 `consent_scope_changed` 拒绝。它只读取现有 `ai_setting` 键，不新增表、列或迁移。
- `internal/aibridge`：新增出口前检查类型 `EgressGate` 与共享函数 `CheckEgressBeforeSend`（组合 `EgressForDestination` 与 `ProviderSpec` 的目的地），拒绝时返回带 `state` / `display` 的 `EgressDeniedError`。`OpenAICompatProvider` 在真正 `client.Do` 前调用 gate；`Knowledge.WebSearch` 在真正发请求前调用 gate。
- `internal/aibridge/runner.go`：`loadProvider` 为每个任务 Provider 安装同一检查；`NewRunner` 为联网搜索安装基于 `Runner.egress` 的同一检查。运行期不再只依赖入队/任务开始时的判断。
- `internal/admin/ai.go`：`handleAITest` 先经 `aiconfig.Egress` 判定（拒绝时不读取主密钥），再用 `LoadSpecFromDB` 取已保存配置，核对页面字段只是「已保存值的重复」——未保存的 `base_url` / `model_id` / Key 直接拒绝；随后用 `CheckEgressBeforeSend` 核对目的地，并在 Provider 上安装同一个检查作为发送前最后一道闸。请求固定为一条合成提示。
- `internal/admin/ai.go`：测试连接的失败回显在通用脱敏之外，再移除精确的明文 Key、完整厂商地址与主机；只返回脱敏后的失败原因。
- 兼容与回滚：不新增依赖、表、列、迁移、协议命令；回滚只恢复上述文件。安全票的回滚只允许回到禁止外发的状态，不得用「恢复外发」当回滚手段。

## Testing Decisions

- 测哪一层：纯逻辑（`internal/aiconfig`）、存储 + 真实 handler（`internal/admin`，真实 SQLite）、Provider/Search 出口（`internal/aibridge`，本地 `httptest` 模拟厂商与搜索端点）。
- 期望值从哪来：授权记录用手写 JSON 字面量（独立真值，不由被测构造函数生成）；「是否出网」用模拟服务的请求计数；Key / 地址回显用字符串包含断言。
- 覆盖：无 consent、总闸关闭、紧急停止、已撤销、伪造授权、目的地范围变化、目的地被绕过保存接口直改、页面未保存的 Key / 地址 / 模型；放行时模拟厂商收到 1 次请求且请求体不含明文 Key、使用固定合成提示；撤销后下一次请求不再出网；厂商 500 回显 Key/地址与连接失败两种情况的后台回显都不含 Key、完整地址或主机。
- 回滚后哪些测试会变红：删除 `handleAITest` 的出口判定与 Provider gate → 禁止场景的请求计数不再为 0；把 `CheckEgressBeforeSend` 退化为只判总闸 → 目的地漂移用例重新出网；去掉未保存输入核对 → 未保存 Key 用例会用已保存 Key 成功；去掉 Provider/Search gate → 任务中途撤销与未授权搜索用例重新出网。
- 全部为合成数据与 `127.0.0.1` 上的本地模拟服务；不连接真实厂商、不使用真实凭据、不产生第三方出网。

## Out of Scope

- T-011 三出口主机白名单尚未实施；本票不限制已保存 `base_url` 指向任意主机，也不限制 HTTP 重定向后的主机。
- T-012 凭据迁出与主密钥恢复不在本票范围。
- 后台页面 `index.html` 不在本票白名单，未改前端；未保存输入由后端拒绝并返回可读原因。
- 真实厂商端到端、真实浏览器点选、真实旧库升级：不验证。
- 不修改协议、结算、玩家状态条真源、静态库与 CI。

## Further Notes

- `[事实]` 改动前 `handleAITest` 读 `ai_setting` 零次，并允许页面覆盖厂商地址、模型与明文 Key（`回执/T-006_回执.md` §6.4）。
- `[事实]` 运行期的 `aiconfig.Egress` 与 consent 判定由 T-009 提供；本票只在出口处增加「当前目的地」复核，不改变 consent 的写入方式。
- `[推断]` 如果绕过后台保存接口直接改 `ai_provider`，授权快照不会刷新；本票据此按「目的地与快照不一致」拒绝，而不是信任库里的任意变更。
- `[缺口]` 未做真实厂商出网与真实浏览器点选；本票所有放行验证只到本地模拟厂商为止。
