# AI 显式授权与撤销

Status: done

## Problem Statement

T-008 把「缺省即开」收敛成了 fail-closed，但外发总闸仍然永远无法放行：`ai_setting` 里的 `outbound_enabled=1` 既可能是历史迁移种子，也可能是后台一次整卡保存重写出来的值，`updated_ts` 与「审计里出现过键名」同样不能证明用户同意（`回执/T-006_回执.md` §6.3）。因此需要一个可判定的事实：用户在后台逐项确认过「允许把哪类内容发到哪个目的地」。

## Solution

在现有 `ai_setting` 上用专用有类型元数据键记录显式授权（consent），不新增表、列或迁移。授权只能通过独立的确认/撤销接口事务写入；普通整卡保存拒收这些键。四项分别记录：`outbound`、`actor`、`catch_talk`、`memory_inject`。只有 `outbound` 授权参与外发总闸判定；`actor` 是本地意图，单独确认它也绝不授权传输玩家内容。

`aiconfig.Egress` 在原有 `emergency_stop` / `outbound_enabled` 判定之后增加「已证明同意」分支：只有 schema、内容版本、内容清单与目的地快照全部匹配且状态为 `granted` 时才可能 `Allowed=true`。缺记录、非法记录、已撤销、范围变化、伪造键一律关闭。撤销写入后，同一进程内下一次出口检查立即读到 `revoked`；已在途请求不会被取消，也不承诺撤回已经发出的数据。

## User Stories

1. 作为管理员，我在后台「连接」页能看到四项授权各自的当前状态与范围，并逐项确认或撤销。
2. 作为管理员，确认页明确列出会发送的内容类别（聊天原文、昵称/UID、钓鱼记录、记忆摘要）与目的地（厂商 base_url / 模型 ID）。
3. 作为管理员，我换厂商地址或模型后，旧授权自动失效，必须重新确认。
4. 作为管理员，我只打开角色状态模拟（actor）时，玩家内容仍然不会外发。
5. 作为运维，旧数据库升级后即使 `outbound_enabled` 仍是历史 `1`，也不会被推定为已同意。
6. 作为运维，紧急停止仍然优先于任何授权。
7. 边界：授权审计只记录项名、状态、内容版本与目的地摘要，不含原始密钥、完整地址或玩家内容。

## Implementation Decisions

- consent 键：`ai_consent_outbound`、`ai_consent_actor`、`ai_consent_catch_talk`、`ai_consent_memory_inject`；值为 JSON（schema / item / state / subject / ts / scope），不是 `"0"/"1"`。
- 目的地快照键：`ai_egress_destination`，值 JSON（base_url / model_id）。由保存厂商配置或确认授权时与 consent 同事务写入；Egress 用它比对授权范围。
- `internal/store` 新增 `AISaveProviderDestination`：厂商地址/模型与目的地快照在同一事务内提交；该函数不写 consent。
- 确认接口 `POST /api/ai/consent/confirm`：校验项名与管理员主体；外发类项要求厂商地址与模型非空；同主体、同范围重复确认幂等（不重写记录）。确认外发类项时 consent 与目的地快照同事务写入。
- 撤销接口 `POST /api/ai/consent/revoke`：撤销是安全动作，不依赖厂商配置完整；重复撤销幂等。
- `handleAISettingsSave` 对 consent/目的地元数据键直接返回 400，整卡保存不得携带或重写它们。
- `handleAIConfigSave` 只刷新目的地快照（与厂商地址/模型同事务），不写 consent。快照变化后，旧授权在读取时因范围不匹配而失效，必须重新确认；本地 actor 授权不受目的地变化影响。
- `aiconfig.Security` 的 `consent` 字段输出四项标签、内容范围、状态、主体、时间与目的地；前端只渲染，不推断默认同意。
- `aiconfig.Egress` 的放行条件：急停未触发 + `outbound_enabled="1"` + 有效 `outbound` consent。历史 `1`、整卡保存 `1`、审计键名仍只到「未确认」。

## Testing Decisions

- `internal/aiconfig`：用手写 JSON 字面量验证已授权放行、缺记录/裸 `1`/非法 JSON/schema 或 item 不符/主体为空/时间非法/内容版本或清单变化/目的地快照缺失或变化/撤销/急停/显式关；另有独立用例证明 actor 或 catch_talk 单独确认不放行外发。
- `internal/admin`：真实 SQLite + 真实 handler。覆盖三种旧库形态（从未保存、整卡保存过、审计只有 keys）均未授权；确认后才放行；重复确认幂等；撤销立即拒绝；目的地变化后旧授权失效并可重新确认；伪造键全关；整卡保存携带 consent/目的地键返回 400；缺目的地时外发确认返回 409；授权审计不含密钥或完整地址。
- 在途边界（`internal/admin`）：本地 `httptest` 模拟厂商，请求已到达模拟厂商后执行撤销；断言下一次 `Egress` 拒绝，且该在途请求正常完成——撤销不取消已发出的请求，也不承诺撤回已发送数据。
- `internal/store`：用 SQLite 触发器让事务中途失败，断言厂商地址/模型、目的地快照与 consent 均整体回滚，不留下半套授权。
- 全部使用合成数据与本地 `httptest`；不连接真实厂商、不使用真实凭据、不产生第三方出网。
- 回滚后哪些测试会变红：把 `Egress` 的 consent 分支改回「只看开关」→ aiconfig/admin/store 相关用例变红；去掉整卡保存的 consent 键拒收 → admin 用例红；去掉事务 → store 用例红。

## Out of Scope

- T-010 后台「测试连接」统一受闸尚未实施；该接口仍可绕过本票的 consent 判定，本票不修。
- T-011 三出口主机白名单尚未实施；本票不限制 base_url 指向。
- 不在运行期对 `catch_talk` / `memory_inject` 的每次请求逐条校验内容类别（`aibridge` 不在本票白名单）；本票只保证这四项分别有记录、外发总闸只认 `outbound` 授权。
- 不修改协议、结算、玩家状态条真源、静态库与 CI，不新增表/列/迁移/依赖。

## Further Notes

- `[事实]` 整卡保存会重写整个模块的键、审计只记键名（`回执/T-006_回执.md` §6.3），因此 consent 必须是独立的、有类型的记录。
- `[推断]` 直接改数据库 `ai_provider` 而不经过后台保存接口，会绕过目的地快照刷新；本票只覆盖应用内路径，数据库直改不在信任边界内。
- `[缺口]` 未在真实浏览器点选确认/撤销；前端由 Go 静态断言与 Node 语法核验覆盖。
- `[缺口]` 未做真实厂商出网；本票所有“允许外发”的验证只到本地模拟厂商为止。
- 安全票的回滚只允许回到禁止外发的状态，不得用「恢复外发」当回滚手段。
