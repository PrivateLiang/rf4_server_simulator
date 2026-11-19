# AI 前端改动的门禁

Status: done

## Problem Statement

AI 分支相对合并基线给 `internal/admin/web/index.html` 增加了约 590 行前端代码：安全前端（`applyAISecurity` / `AI_SETTING_STATES` / `sec.egress`）、consent 前端（`security.consent` + 确认/撤销按钮）与 AI 页样式类。这部分代码**没有任何 UI 回归保护**：仓库无 `package.json`，官方 11 个 `internal/admin/*_ui_test.cjs` 依赖未纳管的 `playwright` + 浏览器，CI 与 Go 代码都零引用它们——等于不在任何门禁内。

由此存在三类结构性回归：① 前端重新自抄安全默认值（把「缺省」解释成开/关）；② 前端自己从 `ai_setting` 原始值推断授权、绕过独立确认/撤销端点；③ AI 页覆盖官方页面挂载点或官方同名 CSS 选择器，改坏官方后台页面。

## Solution

不引入浏览器或前端工具链，用 Go 测试对 `internal/admin/web/index.html` 做静态断言，把上述三类风险变成 `go test` 门禁的一部分（`internal/admin` 在 CI 的 `go test -p 1 ./internal/... ./cmd/gameserver/... ./cmd/gateway/... ./cmd/seed/...` 子集内）。六类断言分别对应：安全默认值、后端描述消费、授权推断、独立端点、页面挂载点、CSS 作用域。

## User Stories

1. 作为后续改 AI 后台前端的开发者，如果我把出网开关改回 `st.outbound_enabled!=='0'?'开':'关'` 这类本地解释，`go test ./internal/admin` 会立即变红，而不是等到线上显示错误。
2. 作为安全链（T-008/T-009）的维护者，前端只能消费后端 `security.keys` / `security.egress.display` / `security.consent` 的描述，不能自己拿 `ai_setting` 原始值算授权。
3. 作为运维，确认/撤销只能走 `POST /api/ai/consent/confirm` 与 `POST /api/ai/consent/revoke`，不会被「整卡保存」顺带提交授权键。
4. 作为官方后台其他页面的使用者，AI 页只新增 `#view-ai`，且 AI 样式只作用在 AI 自己的类上，官方 `#view-*` 页面与选择器不被改写。
5. 作为评审者，每条断言的真值都是手写字面量（官方基线取自官方 `2b906d4` 的同一文件），不由被测代码生成期望。

## Implementation Decisions

- 新增 `internal/admin/ai_frontend_contract_test.go`：`TestAIFrontendContract` 六个子用例，分别覆盖六类断言；CSS 解析只做选择器/类名/裸类规则计数，不执行 JS、不渲染页面。
- 扩展 `internal/admin/ai_security_test.go` 的 `TestAdminUINoLocalSecurityDefault`：保留原有禁用串与必需串，追加 `outbound_enabled!==`、`emergency_stop!=='0'`、`ai_actor_enabled!==` 及空格变体，并调用新文件里的统一检查函数；不删除、不放宽原断言。
- 官方基线（挂载点、CSS 类名、裸类规则数）以手写常量固定在测试文件内，注释写明提取方法与来源版本；AI 新增 CSS 块的定位用文件内既有注释标记，仅用于取作用域，真值仍是官方基线。
- 不新增依赖：无 `package.json` / `playwright` / `node`，`go.mod` / `go.sum` 不变。

## Testing Decisions

- 测哪一层：`internal/admin` 包内静态测试，直接读磁盘上的 `web/index.html`（`os.ReadFile`），不经过浏览器。
- 期望值来自哪：官方 `2b906d4` 的 `internal/admin/web/index.html`——`#view-*` 11 个挂载点、171 个 CSS class、108 个类的裸类选择器规则数（148 条）；以及票面点名的禁用/必需字面量。
- 覆盖：① 缺省即开/即关的本地解释（含去空白变体）；② `applyAISecurity` / `AI_SETTING_STATES` / `sec.consent` / `sec.egress` 消费点 + 出网显示唯一写入点；③ 原始 `outbound_enabled` / `ai_consent` 键不出现；④ 独立端点 + 整卡保存不含授权键；⑤ 官方 `#view-*` 集合原样保留、`view-ai` 唯一；⑥ AI 新增类与官方类无交集、AI 规则至少绑定一个 AI 新类、官方裸类规则数不变。
- 撤回反证：把出网显示改回 `st.outbound_enabled!=='0'?'开':'关'`，断言 1/2 变红；按原字节还原后全绿并核对 SHA-256（证据见回执）。
- 全部为合成数据（只读静态文件），无凭据、无第三方出网、无浏览器。

## Out of Scope

- 不引入 playwright / node / `package.json`，不改官方 11 个 `*_ui_test.cjs`。
- 不改 `internal/admin/web/index.html` 生产代码、不改 `ai.go` / `aiconfig/*`、不改 CI / `.github/`。
- 不做真实浏览器交互与视觉回归（留给 B-003 之后的前端拆分方案）。
- 不覆盖运行期 HTTP 行为（由既有 `internal/admin` 测试与 T-010/T-011 用例负责）。

## Further Notes

- [事实] 官方 11 个 `*_ui_test.cjs` 依赖 `node:test` + `playwright` + chromium/Edge，CI 零引用、Go 零引用；本票不把它们接进 CI。
- [事实] 现有 AI 前端新增的 40 个 CSS 类中，仅 10 个以 `ai-` 开头（`ai-status`/`ai-stat`/`ai-tabs`/`ai-pane`/`ai-note`/`ai-subhead`/`ai-adv`/`ai-create-card` 等）；其余（`agent-*`、`actor-pill`、`channel-*`、`toggle-*`、`multi-select*`、`tool-item`、`switch-label` 等）无 `ai-` 前缀，但与官方 171 个类名**零交集**——断言按「交集为空」而不是「前缀必须为 ai-」执行，避免把现状判成失败。
- [事实] CI 中 `internal/admin` 由 `.github/workflows/ci.yml:74` 的 `go test -p 1 ./internal/... ./cmd/gameserver/... ./cmd/gateway/... ./cmd/seed/...` 覆盖（子集，不是 `./...`）；本票断言无需改 CI 即进入门禁。
