# AI 前端拆分为独立资源文件

Status: done

## Problem Statement

官方后台单页 `internal/admin/web/index.html` 持续膨胀（合并基线 1,401 行 → 官方现约 2,942 行），AI 的 CSS、markup 与 JS（约 543 行，另含 T-020 的待应用命令 UI）夹在官方页面中间。每次官方改这个文件都要人工避让 AI 段落；T-019 的六类静态断言也只能对「一个 index」检查，无法覆盖拆分后的完整资源集合与 CSS 作用域边界（T-019 已知：块外非裸选择器可漏检）。

## Solution

把 AI 的 UI 从官方单页迁出为同源静态资源：

- `web/ai/index.html`：可信静态片段（无脚本、无行内事件/样式），由模块注入 `#view-ai` 挂载点。
- `web/ai/ai.css`：全部 AI 样式；每条规则的每个逗号分支都以 `#view-ai` 为根并落在其子树。
- `web/ai/ai.js`：唯一启动模块（浏览器原生 ES module），经窄桥 `window.RF4Admin` 使用官方 `api` / `toast` / 路由注册，不读取 TOKEN、不 monkey patch 全局 `go`。

官方 `index.html` 只保留**空的** `#view-ai` 挂载点、导航标记、一个 `<script type="module" src="ai/ai.js">`，以及不含 AI 业务的最小宿主接线（外部视图注册、窄桥、`#ai` 首次直达等待注册完成）。`internal/admin/embedded.go` 继续使用 `//go:embed all:web`；`api.go` 的静态文件入口改为 `WebHandler`，仅对 `/ai/` 前缀加 `Cache-Control: no-cache`，官方页面缓存语义不变。

## User Stories

1. 作为后续改 AI 后台前端的开发者，我只改 `web/ai/**`，不再与官方 `index.html` 的改动互相避让。
2. 作为官方后台其他页面的维护者，`index.html` 里不再有 AI 的 CSS/markup/JS；AI 样式只在 `ai.css`，且全部限定在 `#view-ai` 子树内。
3. 作为安全链（T-008/T-009/T-020）的维护者，迁出后的前端仍然只消费后端 `security.keys` / `security.egress.display` / `security.egress_hosts.display` / `security.consent`，确认/撤销仍走独立端点，运行态命令仍轮询 `GET /api/ai/commands/{id}` 并准确回显 applied/rejected/expired。
4. 作为运维，首次直达 `#ai` 不会被官方默认分支改成 dashboard；片段/样式加载失败时显示「AI 页面加载失败/重试」，官方页面仍可用；AI 源文件 `no-cache`，避免旧 JS 配新 HTML。
5. 作为评审者，六类断言从「读一个 index」迁为「读实际 embed 资源集合」，语义保留或更强；CSS 四负例与资源可服务/MIME/404 都有 Go 证据。

## Implementation Decisions

- 官方路由只增加一个**通用**外部视图注册点：`registerExternalView(name, def)` / `waitExternalView(name)` / `enterExternalView` / `leaveExternalView`。模块在求值时注册 `ai`；`boot()` 在 `#ai` 时等待注册完成后再进入，超时仍回到 dashboard（模块脚本本身 404 时官方页面可用）。
- 窄桥 `window.RF4Admin` 固定暴露 `api` / `toast` / `registerExternalView` / `onLogout`，不暴露 TOKEN；`logout()`（含 401 触发的注销）调用外部视图的 `leave` 与注销钩子，取消轮询计时器。
- 片段与样式经 `new URL(..., import.meta.url)` 同源加载，兼容 `/`（cmd/admin）与 `/admin/`（launcher）两种挂载；加载用单次 Promise 去重，失败渲染错误卡与「重试」，404 不会被当作片段。
- DOM 安全：片段是静态可信 HTML；动态数据一律 `textContent` / 属性 / `value` 属性 API，不用字符串拼 HTML；行内 `onclick/onchange/oninput` 改为挂载根上的事件委托；Agent 编辑弹窗也在 `#view-ai` 子树内自建（固定定位），保证样式边界与离开时关闭。
- AI 专用 CSS 类统一 `ai-` 前缀（原 `agent-*` / `actor-pill` / `channel-*` / `toggle-*` / `multi-select*` / `tool-item` 等一并改名），keyframes 为 `ai-fade`；只引用官方 `:root` 变量或新增 `--ai-*`。
- `security.egress_hosts` 以只读 `<b id="aiEgressHosts">` 展示后端 `display`，不提供编辑入口。
- 缓存：`WebHandler(webFS)` 包装现有 `http.FileServer`，仅 `/ai/` 前缀设置 `Cache-Control: no-cache`；其余路径（含官方 `index.html`）不设置。
- 不引入 Node/npm/打包器/构建产物/新依赖；`go.mod` / `go.sum` 不变，仓库仍无 `package.json`。

## Testing Decisions

- 测哪一层：`internal/admin` 包内 Go 测试，通过实际 `WebFS()` 读取完整资源集合（`index.html` + `ai/index.html` + `ai/ai.css` + `ai/ai.js`），不读磁盘单页；另用 `httptest` + `WebHandler(WebFS())` 验证 HTTP 200 / MIME / `Cache-Control` / 404。
- 期望值来自哪：官方 `2b906d4` 的 `internal/admin/web/index.html`——`#view-*` 11 个挂载点、171 个 CSS class、108 个类的裸类选择器规则数（148 条）；以及票面点名的禁用/必需字面量与 AI 资源标记。全部为手写字面量，不由被测代码生成。
- 六类断言的迁移口径：
  1. 安全默认值：扫描完整资源集合（含去空白变体与 `st.*` 变体）。
  2. 后端安全描述消费：`ai.js` 的 `applyAISecurity` / `settingStates` / `consent` 消费点；出网显示唯一写入点来自 `(sec.egress||{}).display`、出口白名单唯一写入点来自 `(sec.egress_hosts||{}).display`，回退仅 `'—'`。
  3. 授权推断：完整集合中不得出现 `outbound_enabled` / `ai_consent` 原始键；只消费 `it.status` 的 `valid` / `subject`。
  4. 独立端点：确认/撤销各出现 1 次且各在自己的函数内 POST；整卡保存只按 `spec.key` 组装，不含授权键。
  5. 挂载点：官方 11 个 `#view-*` + 唯一的空 `#view-ai`；`ai.js` module 引用恰好 1 个；片段内不得出现任何 `#view-*`（重复挂载）。
  6. CSS 边界：保守 parser 只支持注释 / `@media` / `@keyframes` / 普通规则；每个逗号分支以 `#view-ai` 为根、禁兄弟逃逸、禁 `body/html/:root`、禁 `@import`/外部 URL、新类与 keyframes 必须 `ai-` 前缀、未知嵌套语法拒绝；官方 `<style>` 的官方类集合与裸类规则数不变。
- 负例：`.card .section-head`（块外）、`#view-ai .ai-x, .ai-y`（逗号第二分支逃逸）、`@media(...){.ai-x}`（媒体内逃逸）、重复 `id="view-ai"`（重复挂载）都必须返回 issue。
- 撤回反证：把 `ai.js` 的出网显示改回 `outbound_enabled!=='0'` 一类前端自算，断言 1/2 变红；按原字节还原后全绿并核对 SHA-256。
- 浏览器验收（人做）：首开/刷新 `#ai`、导航往返、慢网/404、401 注销、确认/撤销/保存、恶意 HTML 名称、多行模型、官方 11 个页面基本操作与样式不变。静态断言不冒充浏览器覆盖。

## Out of Scope

- 不重写官方页面、不建第二套 SPA、不改官方 11 个 `*_ui_test.cjs`。
- 不改 `internal/admin/ai.go`、`api.go` 的后端契约（静态入口改为 `WebHandler` 不改变任何 API 路由）；不改 `internal/aiconfig/**`、`internal/aibridge/**`、`internal/store/**`、迁移。
- 不引入 Node / npm / playwright / 打包器 / CDN / 构建产物；不改 CI / `.github/**`；不改 `specs/capabilities/`。
- 不做真实浏览器自动化门禁；不把静态断言包装成浏览器已覆盖。

## Further Notes

- [事实] T-019 的六类断言读取口径（单页 `os.ReadFile`）由本票取代；T-019 的官方基线常量（挂载点 / 类集合 / 裸类计数）继续沿用。
- [事实] T-020 的 pending UI（`pollAICommand` 的 1 秒轮询、30 秒停止、applied/rejected/expired 回显；`aiAgentMeta` 的 generation/revision；角色开关三处按钮改造）已一并迁入 `web/ai/ai.js`，官方页面不再保留。
- [事实] `tool-item` 是 AI 工具选择器专用类，原先寄生在官方 `<style>` 中；本票将其规则迁入 `ai.css` 并改名为 `ai-tool-item`，官方样式表不再含 AI 专用规则。
- [事实] 真实浏览器交互、样式隔离与 401 注销仍由人工验收；Go 断言只覆盖静态结构与 HTTP 资源语义。
