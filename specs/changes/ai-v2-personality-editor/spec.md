# AI 后台四维性格与评分草稿

Status: done

## Problem Statement

T-103 已经把四维性格做成唯一真源（`aiconfig` 定义 + 共享派生 + 同版表达编译），
但管理员没有任何入口：`internal/admin/ai_settings.go` 只有内部只读快照，
`/api/ai` 下没有任何性格路由；「性格与交互」页只固定当前角色并显示能力区，
留着一行「性格编辑由 T-107 接线」的占位说明。结果是四维只能靠手工写
`ai_setting` 才能改，改完也看不出保存的是哪一版、是否已被运行读过。

同时，「按角色描述给四个旋钮一个建议」这条路完全没有：既没有把角色描述变成
建议值的模型调用，也没有把建议值与真实配置分开的草稿层。若直接把模型输出写进
配置，就会出现「管理员没点保存，性格已经变了」「模型乱答也能覆盖人工微调」这类
无法追责的结果。

## Solution

在现有管理鉴权下补三个路由，并把「建议」与「配置」彻底分开：

- `GET /api/ai/personality`：四维定义、三段区间、每个角色的当前值/修订/
  描述/派生预览、保存与生效证据，以及评分要用的模型去向与急停状态。
- `POST /api/ai/personality`：按 `base_revision` 保存四维；同值不跳修订；
  冲突 409 且不覆盖别人的保存。
- `POST /api/ai/personality/score`：用该角色的现有描述请求一次模型评分，
  只返回草稿（建议值、建议区间、短理由），不落库、不改配置。

页面上「性格与交互 → 当前角色 → 四维性格」读同一份定义：拖动滑块或方向键微调、
「按描述建议旋钮」把合法建议填进草稿、逐项或全部撤销，只有「保存性格」才进库。
保存后显示「已保存 · 修订 N」；「已生效」只在服务端给出同版运行消费证据时显示。

## User Stories

1. 作为管理员，我从「性格与交互」当前角色进入就能读到这个角色的四维，拖动或用
   键盘改完点保存，刷新后回读到同一个修订与同一组值。
2. 作为管理员，我切换角色时如果草稿没保存，会被问「保存并切换 / 放弃并切换 /
   取消」，不会把 A 角色的草稿带到 B 角色或静默丢掉。
3. 作为管理员，另一个管理员先保存后，我再保存会看到「已被其他管理员更新」，
   我的草稿还在，可以按最新修订重新载入。
4. 作为管理员，我点「按描述建议旋钮」后，四个旋钮按建议值移动，并显示
   「原 → 新」、浅色建议区间和一条短理由；我逐项撤销或全部撤销都行。
5. 作为管理员，描述写得不够或自相矛盾时，页面只显示「请审查提示词：描述不足或
   互相矛盾」，不会把「无法评估」当成「建议都调成 0」。
6. 作为管理员，我改完描述再回来，旧建议已过期（区间消失），但我手调的值还在。
7. 作为管理员，急停开启时评分按钮直接被禁用并说明原因；已保存的连接没有配置时
   评分也会明确失败，页面保留草稿。
8. 作为服主，评分失败、超时、取消、迟到、模型乱答都不会改动配置，也不会把
   自然语言当成成功结果。

## Implementation Decisions

- **路由与鉴权**：三个路由全部挂在既有 `withAuth` 下（匿名/普通玩家 401），
  形状只限 DEV-111 A 已批的「管理鉴权 + 按 base_revision 保存 + 评分只回草稿」。
- **定义与快照**：`GET` 直接投影 `aiconfig.PersonalityDimensions()`、
  `aiconfig.PersonalityBands()`（新增：0..32 / 33..67 / 68..100 三段）与 T-103 的
  `aiPersonalitySnapshot`；每角色附带 `description`（= `ai_agent.prompt`）、
  `saved_at`、`save_display`、`effect` 与 `derived` 预览。页面不再另抄标签或阈值。
- **保存**：请求体 `{agent_key, base_revision, schema_version, values}`；
  `schema_version` 必须显式等于当前版本；`values` 用
  `aiconfig.DecodePersonalityScoreValues` 严格解析（缺维度、非整数、超域、额外键
  一律 400，不把「没填」当成合法的 0）。同值且配置可用时不写库、不跳修订，
  与 T-104 能力保存同一口径。落库仍只走 T-103 的 `AISavePersonality`
  （单事务比较 `base_revision`），冲突 409、已删除/旧代次 404。
- **评分只回草稿**：`POST /api/ai/personality/score` 的输入固定六字段
  `request_id / agent_key / base_revision / description / dimension_schema_version /
  current_values`；输出固定 `request_id / base_revision / schema_version /
  status / values / ranges / reasons`（`unassessable` 只多一个固定文案 `reason`）。
  顺序是：请求形状 → 角色与描述（以库为准，改了就 409 `description_changed`）
  → 配置修订（不一致 409 `stale_personality`）→ 急停（拒绝时连主密钥都不读）
  → 已保存连接 → 外发一次（30 秒截止，发送前再用 `aibridge.Gate` 重验急停与修订）。
  不重试、不递归、不落库。
- **严格解析模型输出**（`aibridge/personality_score.go`）：回复必须是单个 JSON
  对象（只容忍「整段就是一个代码块」的包装）；顶层与四维对象都
  `DisallowUnknownFields`，多出 `capabilities`/`tools`/`events`/`emergency_stop`/
  `chance`/`quota` 等键直接拒绝；四维必须齐全且都是整数；区间必须是两个整数并
  满足 `0 ≤ min ≤ value ≤ max ≤ 100`（`[45,70,80]` 这种三元也会被拒）；理由非空、
  ≤40 字、不含换行；`unassessable` 不得携带 `values/ranges/reasons`。
  自然语言、尾随内容、第二个对象一律拒绝。
- **提示词**：系统提示词由同一份四维定义生成（ID、中文标签、两端），写死输出形状
  与「不要解释/不要代码块/不给能力、工具、事件、权限、概率、配额」；用户提示词带
  描述（超长按 4000 字截断）与当前四维。`request_id` 由页面生成，只用于丢弃迟到
  响应。
- **前端**：`internal/admin/web/ai/ai.js` 动态挂载「四维性格」卡片到
  `#aiPane-personality`（片段 `index.html` 不在本票白名单，卡片的角色选择与能力区
  保持原样，只把片段里两行 T-107 占位说明替换成管理员可读的四步说明）。
  草稿按 `agent_key` 分开；评分回包只在「最新请求序号 + 草稿版本 + 描述未变 +
  修订未变」同时成立时回填；建议区间用浅色范围带 + 位置标记，域内越过区间给黄色
  警告但允许保存；超域/非整数在字段旁报错并阻止提交。
- **保存与生效分离**：`save_display` 只由库里的修订与保存审计时间给出；
  `effect.effective` 只在 T-108 的 `store.AIAgentEvidenceReport` 给出同版真实消费
  证据时为真：同一份只读快照里，角色当前代次/已保存修订与一条真实 task/result 的
  GS 选择修订、BR 表达修订逐项相等，且结果未处于回收/作废终态。生效文案带被消费的
  修订号与任务/结果 ID；`tasks_since_save` 只是诊断，绝不参与生效判定。保存 r+1 后
  立即回到等待，直到新任务按 r+1 真正运行。页面按查询时的角色/代次/修订应用回读，
  迟到的旧生效响应不得覆盖新版本或另一个角色。
- **边界**：能力集合（T-104）、工具注册表、事件能力、配置发布（T-102）、
  玩家页面/归属/令牌与迁移都不动；不新增表、列、迁移或依赖；不写 V1 归档与主检出。
- **L0 自决**：① 片段 `index.html` 不在白名单 → 用 JS 动态挂载并替换两行占位说明；
  ② `unassessable` 的固定文案放在 `reason`（票面列的是 ok 的 `values/ranges/reasons`）；
  ③ `request_id` 由页面生成、服务端只校验格式并回显。

## Testing Decisions

- **管理 HTTP（真实 SQLite + 真实 handler）**：`TestAIPersonalityHTTPAuthAndSave`
  覆盖匿名 401（读/存/评分）、四维定义与三段区间、未保存时的中性视图、
  首存修订 1、刷新后回读同一修订与同组值、同值不跳修订、陈旧修订 409 且不覆盖、
  `saved_at` 证据与「不显示已生效」。`TestAIPersonalitySaveValidation` 覆盖超域、
  负值、未知版本、缺 `schema_version`/`base_revision`/`values`、额外键、缺维度、
  四维额外键、负修订全部 400 且一个都没落库；不存在角色 404。
  `TestAIPersonalityTwoRolesIndependent` 覆盖两角色修订/取值/描述互不影响。
- **评分（本地 TLS 模拟厂商 + 真实 Provider 构造）**：`TestAIPersonalityScoreDraftOnly`
  断言一次调用、请求体带描述与当前四维、回执恰好七个键、不回显描述、评分不改修订
  与值；`TestAIPersonalityScoreGuards` 断言描述已改/修订过期/未知版本/超域/缺
  `request_id`/额外键/空描述全部拒绝且零外发，急停拒绝零外发、解除后可评分；
  `TestAIPersonalityScoreIllegalAndUnassessable` 断言自然语言、额外安全键、
  区间不含建议值、`unassessable` 带值都 502 且不改配置，`unassessable` 只回固定文案。
  `TestAIPersonalityEvidenceNoFakeEffective` 造一条真实任务与结果，断言只统计
  「保存后结束的任务」作诊断、`effective=false`、`reason=br_expression_missing`、
  文案含「等待同版运行证据」且不含「已生效」。
  `TestAIPersonalityScoreErrorMapping` 断言超时 504、急停/配置过期 409、
  输出不合约 502、调用失败 502，且回执不含密钥与厂商地址。
- **P4 同版运行（真实管理 Handler + 真实 GS/BR 子进程）**：
  `TestT107EffectiveFromRuntime` 在同一份临时库上先经真实管理 JWT/Handler 保存，
  再以 `go test -c` 构建的 gameserver 测试子进程跑真实 `maybeAIPlayerCatch` 上星链与
  真实 BR 表达，屏障文件保证两次真实管理 GET 分别落在「GS 已入队、BR 未表达」与
  「表达落库」之后。覆盖未保存（`not_saved`）、保存后等待、入队后仍等待、
  已生效（带 task/result 与修订号）、刷新同文案、保存 r+1 立即等待、r+1 新任务
  才点亮；负例覆盖跨版旧任务、其他角色同号修订、同名角色重建（旧代次）、
  只有 fallback、证据不可读/已回收/已清理/表达已清理。R-158 夹具库在 env 门控下
  复制到临时目录用真实 GET 复用。测试只用子进程夹具，不让 admin 与 gameserver
  测试包互相导入。
- **P6 旧键与既有流程**：`TestAIPersonalitySaveKeepsLegacySettingsCapsAndLimits`
  先把旧开关/旧概率/硬限写成非默认值，连续两次真实管理保存后逐键比对 `ai_setting`
  （除 `ai_personality:*` 外零变化），并点名 8 项圆桌/播报硬限与能力集合不变；
  页面侧由 `TestAIPersonalityFrontendContract` 固化：确认停读的旧概率/播报键不得再
  出现在控件、渲染或提交里，硬限、只读提示与 T-105 首句流程保留。
- **契约层**（`aiconfig`）：三段区间首尾相接覆盖 0..100 且与 `PersonalityBandOf`
  同源；取值/区间/理由逐项校验（含 40 字边界与换行）；严格解析四维缺一维、
  小数、字符串、`null`、超域、额外维度、额外概率键全部拒绝；提示词的 JSON 键名
  由结构体 tag 反射比对，防止提示词与解析漂移；超长描述按上限截断。
- **模型往返**（`aibridge`）：票面形状与代码块包装可解析且结果一致；24 类非法输出
  逐条拒绝；每请求只调用一次厂商；`unassessable` 不带 values/ranges/reasons；
  请求校验失败时不调用厂商；草稿 schema 与表达式编译的 schema 同源。
- **前端静态契约**：`TestAIPersonalityFrontendContract` 固化路由、六字段请求、
  键盘控件、逐项/全部撤销、三选一切换、说明卡四步、无 T-107 占位、维度与分段只
  来自服务端（页面不写死四维 ID）、性格卡不出现能力/工具/旧概率入口、
  `已生效` 只在 `effect.effective` 分支出现。
- **浏览器夹具**（`internal/admin/ai_personality_ui_test.cjs`，Playwright + Chromium）：
  ①手调/方向键/超域拒绝/保存提交 `base_revision` 与四维/刷新回读同一修订/
  窄屏单列无横滚；②评分请求六字段、建议回填、范围带、理由、「原 → 新」、
  越区间警告、逐项与全部撤销、全程不落库；③两角色草稿隔离、切换三选一（取消/
  放弃/保存并切换）、409 冲突保留草稿与重新载入、描述变更后建议过期而手调值保留；
  ④非法输出/无法评估/超时/迟到（草稿版本变化）/取消都保留草稿，急停时评分入口禁用。
- **回滚后变红**（隔离副本用 `go test -overlay` / 临时页面副本实测，见回执）：
  删掉服务端管理鉴权 → 匿名读写评分用例红；删掉 `base_revision` 比较 →
  冲突/迟到用例红；评分直接落库 → 「评分不改配置」与「刷新回读」用例红；
  把 effect 改成按完成结果推定 → P4 跨版反例红；摘掉页面迟到响应守卫 →
  浏览器夹具的新版本/另一角色用例红。还原后各组回绿。
- 门禁：`go test ./internal/admin ./internal/aibridge ./internal/aiconfig -count=1`、
  `node internal/admin/ai_personality_ui_test.cjs`、`go test ./... -count=1`、
  `go build ./...`、`go vet ./...`、`go run ./tools/doccheck`。

## Out of Scope

- 真实游戏客户端与真实厂商的实机验收、5 分钟盲测（T-110）。
- 旧概率键的退役删除：T-108 运行审计已确认停读的键只在本票页面移除控件、
  渲染与提交，`AISettingSpecs` 的读写兼容与迁移种子保留；仍被运行读取的键
  （圆桌/播报硬限、`ai_actor_*` 时长与休眠保护）继续保留并登记为「无页面入口」。
- 能力表单与首句流程（T-105 已交付，保持原样）；工具注册表与写集合（T-104）。
- 玩家侧任何入口（DEV-109：玩家只游玩）。
- 迁移、表结构、CI、官方协议、结算、玩家状态条与静态库。

## Further Notes

- `[事实]` 依据原票 T-107 §1–§5、DEV-111 A（管理鉴权、按修订保存、评分只回草稿）、
  DEV-109（旋钮只由管理员调）与 T-103 的四维定义与公式。
- `[缺口]` 没有真实客户端/真实厂商材料；评分准确度与「旋钮像不像性格」未做陌生人盲测。
- `[事实]` 生效证据已按 T-108 的按角色只读投影接入（P4）：真实 GS 选择 + BR 表达
  落库后管理 GET 显示「已生效 · 修订 r（任务/结果）」，保存 r+1 立即回到等待；
  跨版、异角色同号、重建、只有 fallback、证据不可读/已清理均不点亮。
- `[事实]` 旧键按 T-108 运行读取与扰动证据逐键核对：页面只清除确认停读的
  概率/播报控件（`ai_fishing_talk_enabled`、`ai_fishing_talk_*_chance`、
  `ai_fishing_roundtable_chance`）；圆桌/播报硬限、`ai_actor_*` 时长与休眠保护、
  `reply_mode` 硬门、`talkativeness` 字段继续保留并登记为「运行读取但无页面入口」。
  逐键结论见回执。
