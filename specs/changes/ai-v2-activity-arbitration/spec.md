# AI 活动仲裁与事件运行

Status: done

## Problem Statement

T-103 已经把四维性格做成唯一真源，并给出一个共享派生纯函数；T-104 已经把角色能力、
受信事件合同与写回执交出来。但两者都还没有真实消费点：

- 三条主动事件链（真人频道圆桌、玩家上星/上蓝反应、AI 自己上鱼播报）仍在读
  `reply_mode`、`talkativeness` 与一批 `*_chance`，没有任何一处按能力集合决定
  「这个角色此刻能不能参与」，也没有一处把事件的来源与配置修订说出来。
- 虚拟钓鱼的播报分支由全局开关 `ai_fishing_talk_enabled` 决定，与「频道里到底有没有
  能播报的角色」无关：关掉全局开关时，静默上鱼节拍与配额时点一起改变；开着时，
  没有报鱼能力的角色仍然参与节拍，看不出配额被谁占掉。
- 角色活动的推进（休息/继续、换目标）仍在读 `ai_actor_rest_chance`、
  `ai_actor_switch_chance`，与四维无关；换目标既没有硬驻留，也没有可解释的接受阈值。
- 比赛/活动事实没有任何只读出口，模型只能凭上下文猜自己是不是在比赛。

后果：同一个角色「能不能主动说话」在后台配置与运行行为之间没有可核对的路径；
撤权之后旧任务仍可能投递；配额与播报失败后的渔获条数无法逐段核对。

## Solution

把「要不要动、要不要说」收敛到 T-103 的同一个纯函数，把「能不能参与」收敛到 T-104
的受信事件合同，并在三条既有事件入口上落地：

- `internal/gameserver/ai_activity_policy.go`（新增）：活动仲裁策略与 T-103 适配层。
  类型级 tunables 从既有活动配置继承（Bhard/Bextra/Tbase），提供硬驻留底线
  `Dmin=Bhard+Bextra×p̂×(0.5+0.5r̂)`、休整节拍 `Trest=Tbase×(1+0.5r̂)`、第一闸
  （证据/锁/硬驻留）、第二闸 `W=σ(2s+e−p̂−1+d×u+min(0.6,0.2n))`、同玩家同邀请去重，
  以及每个行为动作的「事实特征」映射。所有一次性行为（起桌、接话、星蓝反应、
  自己报鱼、休息/继续）都走 `aiconfig.DecidePersonality`。
- `internal/gameserver/{ai_channel.go,ai_contacts.go,ai_catch_react.go,ai_fishing.go,
  ai_fishing_talk.go}`：三条事件链统一经 `aiEventCandidates`（范围 = 精确 ∪ 地图 ∪
  全局，按角色去重）与 `aiEventAuthorized`（`TrustedEvent` + `AuthorizeEvent`）筛选；
  点名先核对频道范围；DM 不受频道范围限制；投递前复核发送者仍存在且启用。
- `internal/gameserver/{ai_actor.go,ai_runtime_commands.go}` 与
  `internal/store/repos_ai_actor.go`：actor tick 的休息/继续与建议换目标改吃 T-103
  结果；`AdvanceWith` 接收调用方给出的时长与休息决定，存储层不再掷行为骰子；
  换目标加第一闸（硬驻留/锁）与第二闸（W≥0.6）+ 同邀请去重。
- `internal/aibridge/{runner.go,actor.go}`：消费前按原任务绑定复核（类别、逐角色
  能力修订、代次），圆桌发言权改由 T-103 派生，并按任务注入一份只读活动/比赛事实
  快照（带来源、采样时间、陈旧标志与 `registered=false`）。
- `internal/store/migrations/00907_ai_task_evidence.sql` 与
  `internal/store/repos_ai.go`（T-108 §6 / DEV-121 A 已签认）：一张迁移建两张从属于
  `ai_task`/`ai_result` 的凭据表。`ai_task_evidence(task_id, binding_json)` 由 GS 在
  入队事务里写受信入口类别（或直接对话标识）、发起者与有界参与者，每位参与者带角色
  代次、本次被要求的事件能力及其修订，只有真的用四维做过本次选择才带
  `gs_personality_revision`。`ai_result_expression(result_id, agent_key, generation,
  br_personality_revision)` 由 BR 在实际把同一份已校验配置编译进提示词并产生正常
  非空结果时与结果同事务写。旧公共入口保留（直接对话按 `Direct=true` 绑证据），
  事件的类别与能力必须由入口显式给出；无凭据的旧在途项按不可证终止。

## User Stories

1. 作为服主，我把某个角色的「回应玩家星蓝」撤掉之后，它不会再因为玩家上星而冒出来；
   已经排队的旧回复也不会在撤权后投递。
2. 作为服主，频道里没有任何「播报自己渔获」的角色时，AI 上鱼只入库不播报；
   我有意只给某个角色「虚拟垂钓」而不给「播报自己渔获」，它就只钓鱼不说话。
3. 作为玩家，我在频道里直接叫某个角色的名字求助，它即使只有攻略能力也会回答；
   但我只是闲聊时，只有攻略能力的角色不会突然插话。
4. 作为玩家，我安利一条鱼时，AI 不会立刻放下手里钓了很久的目标；同一句安利重复三次
   以内它可能被说动，超过三次就不再重复受理。
5. 作为管理员，AI 的休息与继续不再由一条「进入休息概率」决定，而由这个角色的四维
   与当前体力解释；提高持续投入只会让它多留一会，不会缩短硬驻留。
6. 作为玩家，我问 AI「你是不是在比赛」时，它没有参赛身份就只说没有；本地赛事表的
   截止时间过了也不会被说成「官方已结束」。

## Implementation Decisions

- **能力是唯一准入**：事件类别由服务端入口写死（`aiconfig.EventReplyStar` /
  `EventRoundtable` / `EventVirtualFishing` / `EventBroadcastCatch`），禁止从自然语言或
  HTTP body 解析。准入口径复用 `aiconfig.AuthorizeEvent`；目录里 `broadcast_catch`
  自带 `RequiresRoundtable`，因此「播报自己渔获」在单人场景也要求圆桌资格——这是
  T-104 已签收的目录语义，本票不另立一套。
- **范围 = 精确 ∪ 地图 ∪ 全局**：沿用 `AIChannelSubscribed` 的三源并集并去重；
  点名候选也必须落在该范围里，否则视为 `out_of_scope` 并记日志。DM 完全不过频道范围。
- **主动 vs 直接求助**：被点名的单人回复属于直接求助，不受事件能力限制（仅攻略也能答）；
  没人点名的单人接话属于主动事件，候选必须已获准「参加圆桌」。
- **唯一随机源**：所有「要不要」都由 `DecidePersonality` 决定，沉默/不动作为零特征候选
  （四项全 0，得分为 0），因此比较的是「这件事对这个角色的吸引力」而非「沉默也带偏好」。
  旧的 `ai_actor_rest_chance`、`ai_actor_switch_chance`、`ai_catch_*_chance`、
  `roundtable_on_speech_chance`、`ai_fishing_talk_*_chance` 与 `talkativeness` 不再参与
  行为决定；`ai_fishing_talk_enabled` 由 DEV-113 A 的「频道内是否有生效的播报角色」替代。
  `reply_mode` 只保留 `silent` 作为玩家静音意图的硬静音（不参与概率与排序，与单频道
  路径一致），其余取值不再读取。
- **硬驻留是下限不是替代**：`Dmin` 只由类型级 tunables 与 p̂/r̂ 决定，重复邀请只抬高 W，
  不降低 `Dmin`；一段钓鱼的时长取「区间随机」与 `Dmin` 的较大者；休息段不受硬驻留约束。
- **第二闸的 d 取值**：普通消遣 `d=+1`（等待越久越愿意动），已有目标投入 `d=−1`
  （投入越久越不愿换）。换目标建议在「当前就在同一张图钓同一条鱼」时按投入处理。
- **DEV-113 A**：虚拟钓鱼的节拍分支按「频道内是否有已配置、生效的播报角色」选择；
  有播报角色时保留原预占时点（先占额、再抽样），并分四段诊断
  （`attempt`/`quota`/`enqueue`/`delivery`）。没有播报角色时沿用旧静默节拍：
  每次抽鱼都计一次配额、只写渔获不说话。
- **渔获与播报解耦**：入队失败不影响已提交的渔获，一条鱼只写一行；发言失败后不得补写
  第二条。
- **只读事实快照**：活动事实只经 `store.AIActivityFacts` 读一次同库 `competition` 投影
  （单次 SELECT、≤800ms、只读事务、不结算、不写任何表），`aibridge` 内不再有官方表裸
  SQL；一个任务只取一份快照，圆桌全部发言者共用。输出保留 `source=official-db(本地投影)`、
  `sampled_at`、`stale`、`registered=false`、`capability=unavailable`、资格 `unknown`，
  并带最多 5 条实际投影行的 `id`/状态/开始/预计结束（不含玩家数据）；`event_snapshot`
  固定 `unknown(source_unavailable)` 并给负证据（不调用懒结算、不注册事件、不写官方表、
  不宣称官方已结束）；报名中/未开始不计入「进行中」，本地预计结束已过只标 `stale=true`
  与阶段 unknown。
- **撤订阅的逐层复核**：范围口径统一为「精确 ∪ 地图 ∪ 全局 ∧ enabled ∧ 未删除 ∧ 已接入」。
  BR 认领后仍按原绑定复核，并在每次真正出站前（单频道与圆桌每轮 `provider.Chat` 前）与
  每次写结果前按任务频道复核当前范围；GS 在 `AIDeliveryAuthorization` 里复核本句发言者与
  必要发起者，仍在真正发 24/27 前。失效一律按 `out_of_scope` 落不可投递终态：不调用厂商、
  不写可投递结果、零新帧、不补发；圆桌剔除失效发言者，不足两人即终止。DM 与明确直接
  询答不受此限（被点名的频道直答仍须先在范围里，才能进频道）。
- **重领与迟到**：`AIPopTask` 返回本次认领后的 `attempt`；写结果、追加圆桌发言与结束任务
  一律以 `state='processing' AND attempt=?` 限定，旧尝试的迟到写入返回 `ErrAITaskNotActive`
  且不写结果；`AIExpireTasks` 把 processing 退回 pending 的同一事务里，把该任务未投递的
  旧结果置终态（`delivered_ts=-1`、`err=attempt_reclaimed`、清正文）。不新增 `ai_result.attempt`
  列；旧入口保留给测试，内部按当前尝试绑定，生产 runner 只用 attempt 绑定入口。
- **派生选人**：`aiPickSpeaker` 不再用「10 + talkativeness」加权随机；候选按稳定顺序逐个
  跑 T-103 共享派生（`activityWants`，种子由 `aiEventSeed(channel, 原话)` 与角色键派生），
  首个愿意者发言，无人愿意即合法沉默；`aiEventSeed` 不掺时间。last-speaker 去重、冷却、
  配额与既有硬限保留。`reply_mode` 只作硬门：`silent` 频道内一律静默、`mention` 不参与
  频道主动事件，DM 与直接询答维持既有行为。
- **回退与保留历史**：回退仅限 `NeedsSeed=false`；旧版三进程可启动并读取既有 DM、已终结
  结果、配置与配额，旧版 `cmd/seed` 拒绝含 00907 附表的库属预期安全边界。新版 `AIPrune`
  在同一事务里清扫「无父行孤儿」（`ai_task_evidence` 无父任务、`ai_result_expression`
  无父结果），只删无父行，不删有父行历史，也不回灌旧库或删表降级。
- **来源与版本由同一事务承载**：事件的类别、逐角色能力修订与「这一次真的用了哪版
  四维」不再只写日志。GS 在入队事务里写 `ai_task_evidence`，BR 在结果事务里写
  `ai_result_expression`；两个写者与原边界一致（GS 独占运行态、AD 独占配置、BR 只写
  自己的结果与消费凭据）。BR 认领后与 GS 每次出站前都用「此刻」的能力/代次对照原绑定，
  严格等值比较修订，因此撤权与「撤后重授」都表现为旧意图终止；没有凭据的旧在途项
  按不可证终止，不给历史行补造类别或版本。
- **旧全局开关退出决定链**：`roundtable_enabled` 不再被 GS 读取。新库默认值下，
  管理员只按能力与订阅开通就能起桌；该键保留旧写兼容与页面展示，翻转它不改变任何
  运行结论。三个读取点（`ai_channel.go`、`ai_catch_react.go`、`ai_fishing_talk.go`）
  一并移除。

## Testing Decisions

- **纯函数层**（`ai_activity_policy_test.go`）：`Dmin` 只加不减且不越 `Bhard+Bextra`；
  `Trest` 夹在配置区间内；第一闸三条互相独立；W 随 u/n 单调、高交往高尝新更容易接受；
  邀请去重窗口与上限；非法配置不产出决定；`ai_personality:` 前缀与 store 侧一致
  （用真实写入反证，不靠注释）。
- **链路层**（`ai_event_chains_test.go`）：三条链各一条「确实发生」+ 一条「合法沉默」，
  逐条记录任务 ID、候选与参与者；越界点名、无资格、急停三种零入队；
  DEV-113 A 的四段诊断、只开垂钓不播报（有鱼无声）、不开垂钓（无鱼无任务）、
  入队失败后仍只有一条鱼。
- **仲裁层**：硬驻留拒绝、高交往高尝新接受、高持续投入拒绝、同邀请第四次拒绝，
  全部走真实的 GS 命令应用路径。
- **桥接层**（`event_consume_test.go`）：系统单频道事件在撤掉原事件能力后不写可投递
  结果（终态原因 = `event_capability_revoked`）；系统圆桌撤权后按原修订识别撤权、
  不足两人落终态且不调用模型；只读快照必须写出 `registered=false`、`stale=true`
  与 `unknown`。
- **跨包运行层**（`ai_event_evidence_test.go`）：`TestT108EvidenceRuntime` 的固定子例
  `DefaultRoundtable`（两种旧键取值下有同一条圆桌任务与真实 24/27）、`RevokedBeforeBR`
  （0 次厂商调用、0 帧、终态原因、撤后重授仍终止、新授权圆桌成功）、`RevokedBeforeGS`
  （1 次调用、0 帧、终态、再轮询不补发）、`SameRevision`（GS/BR 同修订、厂商请求含
  独立编译的预期片段、改四维不改权限、换四维约束随之变化）、`ChangedBeforeBR`
  （跨版旧任务 0 调用且不标双端成功、新任务按 r+1 生效）、`ChangedAfterBR`
  （快照后保存按已绑定旧版完成，不点亮新版本）、`SubscriptionRevokedBeforeBR` /
  `SubscriptionRevokedBeforeGS`（只撤订阅：0 次厂商 / 1 次调用 + 0 帧，终态
  `out_of_scope`）、`SubscriptionRevokedMidRoundtable`（本轮调用保留、本句不写、不足两人
  终止）、`ScopeChangedToOtherMap`（地图级订阅换到别图 → 越界）、
  `SubscriptionRegrantedBeforeEgress`（撤后重授只放行新任务）、`DirectDialogUnaffected`
  （直接对话与 DM 不受范围限制）、`ActivityFactsPath`（一任务一份快照、两名发言者共用
  同一 `sampled_at`、实际赛事行与诚实 unknown）。跨包证据用临时库、真实入口、
  可计数 TLS 厂商与真实隧道客户端，不直接塞队列代替正向链。
- **重领迟到**（`ai_event_evidence_test.go`）：`TestT108LateResultAfterReclaim` 的
  `LateSingle`（旧尝试写入不改状态、不写结果）、`LateRoundtableSpeaker`（回收时未投递
  旧结果置 `attempt_reclaimed` 终态且不进入待投递集合）、`SameRevisionOtherAgent`
  （版本不变时仍按 attempt 拒绝）、`RebuiltAgentGeneration`（角色重建后旧尝试仍被拒）、
  `ReclaimTwice`（连续两轮回收，两次旧写入都被拒）。
- **选人硬门**（`ai_speaker_pick_test.go`）：`TestT108SpeakerPickDerivedStable`（固定种子
  两次同人；真实种子连续 16 次不换人；`aiEventSeed` 与时间无关）、
  `…IgnoresTalkativeness`（0/100 与 100/0 同一个人，运行期不读 talkativeness）、
  `…MentionHardGate` / `…SilentHardGate`（发言类事件候选排除，虚拟钓鱼不受影响，
  点名组只剔除 silent）、`…AllDeclineIsSilence`（无人愿意即沉默）。
- **迁移、清理与回退**（`ai_task_evidence_migration_test.go`、`rollback_compat_test.go`）：
  `TestAITaskEvidenceMigration` 覆盖空库、既有 00906 库、重复启动、登记失败注入后旧库
  可读可重试、证据写失败整条回滚、父行清理不遗留孤儿；`TestAIPruneSweepsRollbackOrphans`
  覆盖旧版清理留下的无父行孤儿（清扫归零、有父行历史与正文不动、重复清扫幂等）；
  `TestAIRollbackWindowKeepsOldRowsReadable` 覆盖旧版无证据行只读与投递拒绝；
  `TestAIRollbackWindowNeedsSeedBoundary` 记录回退前置的库形态。
- **回归**：旧 `RestChance` 被忽略（即使配置为 1）；`gameserver`/`aibridge`/`store`/
  `admin`/`aiconfig` 五包 `go test` 与 `go vet ./...`、`go run ./tools/doccheck` 全绿；
  全量 `go test ./...` 与 `go build ./...` 见回执。
- **撤回反证**：撤掉 GS 的任务绑定、BR 的消费前复核、BR 的表达等值或 GS 的发前复核，
  对应子例各自先红后绿；恢复旧全局开关读取使 `DefaultRoundtable` 变红。DoD-v3 追加
  G9–G16：撤 BR 出站前范围复核、撤 GS 投递前范围复核、撤结果写入的 `AND attempt=?`、
  撤回收时旧结果终态化、恢复 `10+talkativeness` 加权随机、把 `aiEventSeed` 改回时间种子、
  塞回官方表裸 SQL / 撤 stale 判定、撤 `AIPrune` 孤儿清扫，分别使对应子例变红；去掉
  overlay（G15a 为临时改真实源文件并核对恢复后 SHA256）后同一子例回绿。每组撤回用
  `go test -overlay` 指向该组临时 JSON，原始输出与退出码见回执。

## Out of Scope

- 比赛报名/退出/ready 与计分：按 DEV-108/DEV-117 独立推进，本票只读事实。
- 管理页面的旧概率旋钮下线与四旋钮交汇：归 T-107（本票让这些键不再被读取，
  页面清理须与 T-107 一起验收）。
- 真实整组二进制换版、目标 Linux、真实客户端/厂商：后置。
