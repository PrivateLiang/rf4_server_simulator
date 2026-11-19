# AI 三出口主机白名单

Status: done

## Problem Statement

T-010 让后台「测试连接」、运行期模型调用与联网搜索共用同一套 `aiconfig.Egress` / consent 判定，但三个出口仍可被任意地址控制：厂商地址（`ai_provider.base_url`）与联网检索地址（`ai_setting.search_endpoint`）由后台填写，授权只冻结「当前填的那个 URL」，不限制它指向哪台主机。于是只要有过一次有效授权，任何被填进去的主机（包括内网地址、云元数据地址、被 DNS 解析到内网的主机名）都能收到携带真实 API Key 的请求；重定向、环境变量代理与 DNS 重绑定还会把已校验的连接改道到未授权目标。部署侧没有任何「只允许这些目的主机」的开关。

## Solution

部署配置新增一份外部出口主机白名单（`ai.egress_allowed_hosts`，默认空）。模型调用、联网搜索与后台「测试连接」三个出口共用这份白名单：默认空列表意味着所有外发被拒绝；后台保存或填写 URL 不会把该主机加入白名单。判定分两层，任何一层不放行都不会建立连接：

1. **发送前 URL 级检查**：必须 HTTPS、不得含 userinfo，`host:port` 精确命中白名单（大小写与结尾点规范化、缺省端口 443，不做通配/子域/后缀匹配）；
2. **连接阶段地址检查**：解析主机后逐条检查实际地址，拒绝环回、私网、链路本地、CGNAT、保留/文档段等非公网目标；只拨校验过的 IP（解析一次，防 DNS 重绑定）；HTTP 客户端显式禁用环境变量代理。

重定向每一跳重新核对白名单；跨主机/跨端口重定向不转发 `Authorization` / `x-api-key`。后台「测试连接」在总闸/consent/目的地范围之后再过主机白名单，两道闸的拒绝原因用不同 `state` 返回，并可在后台状态里看到部署白名单「配了几条」。

## User Stories

1. 作为运维，我在部署配置里只列公司允许的厂商与检索主机；名单为空时系统拒绝一切外部调用，而不是「先跑起来再说」。
2. 作为管理员，我把厂商地址或检索地址改成任意 URL 并保存，不会因此获得对该主机的授权；点「测试连接」会被主机白名单拒绝，并看到独立于授权闸的原因。
3. 作为管理员，厂商返回 302 到另一台主机时，系统不会带着 API Key 跟过去；目标主机不在白名单时连连接都不会建立。
4. 作为安全评审，我能看到「被授权的域名被 DNS 解析到 10.x / 127.x / 169.254.x」时请求在拨号前就被拒绝。
5. 作为运维，我设置 `HTTPS_PROXY` 不会让已校验的外发被改道到代理主机。
6. 边界：白名单只来自部署配置文件，重启后生效；后台没有编辑入口，也不会有任何「临时放行」开关。

## Implementation Decisions

- `internal/aiconfig`：新增部署级 `EgressHostPolicy`（规范化、精确匹配、`CheckURL` / `CheckEndpoint`）、地址分类 `IsPublicEgressAddr`、拒绝原因 `HostPolicyError`，以及后台状态描述 `EgressHostsStatus`。使用现有 `SecurityView` 增加 `egress_hosts` 字段；不新增表、列或迁移。
- `internal/config`：`ai.egress_allowed_hosts` 是唯一配置源，默认空；`Load` 时校验条目（通配、userinfo、非 HTTPS、带路径、非法端口直接启动失败），并把同一份列表复制给后台进程读到的 `config.Admin`（后台与 aibridge 是两个进程，各自只读自己那部分配置；不在 YAML 的 admin 段另配一份，避免漂移）。
- `internal/aibridge/provider.go`：出口 HTTP 客户端统一由 `newEgressHTTPClient` 构造——`egressPolicyTransport` 对每个请求做 URL 级检查；`newSafeTransport` 在拨号前解析并校验实际地址、只拨校验过的 IP、`Proxy=nil`；`CheckRedirect` 逐跳重验并跨主机剥离鉴权头。`OpenAICompatProvider` 默认空白名单（拒绝一切），由调用方 `SetEgressPolicy` 安装；`NewProviderWithTransport` 只用于测试注入本地 fixture Transport。
- `internal/aibridge/knowledge.go`：`Knowledge` 持有同一份 `EgressHostPolicy`，`WebSearch` 在总闸之后、构造请求之前检查检索地址；未安装策略时默认拒绝。
- `internal/aibridge/runner.go`：`NewRunner` 从 `config.AI` 构造唯一一份策略，同时装给模型 Provider 与联网搜索；非法配置降级为空白名单（拒绝），不会退回「不限制」。
- `internal/admin/ai.go`：`handleAITest` 在 T-010 的 Egress/consent/目的地判定之后检查部署白名单，拒绝时返回 `state=egress_host_*` 与独立文案；`/api/ai/status` 的 `security.egress_hosts` 展示配置条数；普通保存接口不写白名单。
- 兼容与回滚：无新依赖、表、列、迁移、协议命令。回滚只恢复上述文件与配置项；安全票的回滚只允许回到「拒绝外发」，不得用「恢复外发」当回滚手段。

## Testing Decisions

- 测哪一层：纯逻辑（`internal/aiconfig` 的规范化/匹配/地址分类）、配置加载（`internal/config` 临时 YAML）、出口传输（`internal/aibridge` 本地 TLS fixture + 注入 Transport/解析器/拨号器）、真实 handler（`internal/admin` 真实 SQLite + 本地 fixture）。
- 期望值从哪来：白名单条目、授权 JSON 与 IP 分类表全部手写为测试字面量，不由被测构造函数生成；「是否建立连接」用底层 fixture Transport / 拨号器的调用计数断言；「鉴权是否泄漏」直接读 fixture 收到的请求头。
- 覆盖：空白名单拒绝、大小写/结尾点/缺省端口/显式端口、端口不符、恶意前后缀、HTTP、userinfo、通配条目启动失败；跨主机重定向逐跳重验、跨主机不转发 Bearer 与 x-api-key、同主机保留鉴权、重定向降级到 HTTP 被拒；DNS 解析到私网/环回/链路本地/IPv6 私网及混合解析在拨号前拒绝、IP 字面量环回拒绝、只拨校验过的 IP、环境变量代理不生效；搜索出口与模型出口共用同一策略、总闸优先；后台「测试连接」在空白名单/主机不符/HTTP 地址时零请求、两道闸 state 不同、状态接口展示配置条数、保存 URL 不扩大白名单；运行期空白名单时任务降级且零请求。
- 回滚后哪些测试会变红：去掉 URL 级策略 → 主机不符/恶意后缀/HTTP/userinfo 用例请求计数不再为 0；去掉连接阶段地址检查 → 内网解析与环回字面量用例会尝试拨号；去掉 `Proxy=nil` → 代理用例拨到代理地址；去掉 `CheckRedirect` 的白名单检查 → 跨主机重定向目标收到请求；去掉跨主机鉴权剥离 → 目标主机收到 `Authorization`/`x-api-key`；去掉后台第二道闸 → 后台测试连接在空白名单下仍会请求 fixture；把 `config.Load` 的白名单校验删掉 → 通配条目不再启动失败。
- 全部为合成数据与 `127.0.0.1` 上的本地模拟服务；不连接真实厂商、不使用真实凭据、不产生第三方出网。

## Out of Scope

- 不改后台页面 `index.html`：`security.egress_hosts` 由后端返回，前端未渲染属已知缺口。
- 不新增后台编辑白名单的接口；白名单只能改部署配置并重启。
- 不为联网搜索增加新的 consent 目的地快照；搜索沿用 T-010 的总闸/授权判定 + 本票的主机白名单。
- 不验证真实厂商、真实 DNS、真实浏览器点选与真实旧库升级。
- 不改协议、结算、玩家状态条真源、静态库、迁移与 CI。

## Further Notes

- `[事实]` 改动前三个出口都不限制主机：`base_url` / `search_endpoint` 由后台任意填写，`handleAITest` 在 T-010 后虽受 consent 约束，但仍会连接已保存的任意主机（`回执/T-006_回执.md` §6.4、`回执/T-010_回执.md` §4）。
- `[事实]` `net/http` 默认只对 `Authorization`/`Cookie` 做跨域重定向保护，不覆盖 `x-api-key`（Go 标准库 `shouldCopyHeaderOnRedirect`），因此本票在 `CheckRedirect` 里显式剥离。
- `[推断]` 部署侧若把允许的主机名解析到内网（例如内网 DNS 劫持），连接阶段地址检查会拒绝；这是有意的 fail-closed，需要运维同时修正 DNS 或改用公网可达地址。
- `[缺口]` 未在真实厂商、真实代理环境或真实浏览器中验证；后台前端未展示 `egress_hosts`。
