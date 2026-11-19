# 部署

本指南介绍 `rf4server` 的本地部署方式。协议格式面向 RF4 客户端 **4.0.25034**。

## 使用 Windows 发布包

只想本地游玩时，无需安装 Go 或自行编译：

1. 下载[正式版](https://github.com/OpenFisherCommunity/rf4_server_simulator/releases/latest)中的 Windows x64 ZIP 并完整解压，不要选择 `Source code`。若需体验最新合入的功能，可在[全部发布](https://github.com/OpenFisherCommunity/rf4_server_simulator/releases)中选择 Preview；预览版的功能和风险以该版发布说明为准。
2. 阅读包内 `快速开始.txt`。发布包提供本地开发证书 `certs/server.crt`，将公钥证书安装到本机「受信任的根证书颁发机构」。它只用于本地测试，不要分享私钥。
3. 以管理员权限编辑 Windows 的 `%SystemRoot%\System32\drivers\etc\hosts`，将 `api.rf4game.ru` 和 `api.rf4game.com` 指向 `127.0.0.1`；也可在 launcher 控制中心切换 hosts 指向，写入时同样需要管理员权限。客户端在其他电脑运行时，应改为网关的可信局域网地址，详见[客户端接入](#客户端接入)。
4. 双击 `启动服务器.cmd` 或运行 `bin\launcher.exe`，等待自动初始化与健康检查完成。打开 `http://127.0.0.1:9500/`，在「运行控制」中保存 `rf4_x64.exe` 的绝对路径，再点击「启动游戏」。
5. 在同一控制中心进入管理后台。Windows 发布包默认管理员为 `admin` / `admin123`，应在配置中修改；自行从模板部署时以 `admin.username` 和 `admin.password` 为准。

结束本地游玩、需要返回官方服务器时，将 hosts 中上述域名恢复为原先解析。launcher 面板支持切换回官方指向；使用自动切换时，需设置 `launcher.hosts_switch: true` 并以管理员权限启动，launcher 退出会还原它管理的 hosts 条目。手动添加的条目需手动恢复，不要删除其他程序维护的条目。

launcher 的托盘用绿、黄、红分别表示全部运行、部分运行、全部停止，可打开控制中心、进入管理后台、启停服务或退出。控制中心还提供单进程启停与重启、实时日志和构建版本信息；服务崩溃后会按指数退避自动重启。

## 容器部署

Windows、macOS 和 Linux 可使用 [Docker Compose](../docker/README.md) 多服务方式部署，也可使用根目录 Dockerfile 的 [Docker 单容器部署](../DOCKER.md)。两种方式二选一，不要同时占用相同宿主机端口。

## 组件与端口

| 进程 | 默认端口 | 用途 |
| --- | ---: | --- |
| `gateway` | 443/TCP | HTTPS 登录端点。 |
| `gateway` | 9341/TCP | 游戏客户端实时连接。 |
| `gameserver` | 9401/TCP | 网关到游戏服务器的内部隧道。仅绑定到可信网络接口。 |
| `launcher` | 9500/TCP | 本地控制中心，同时提供内嵌管理后台 `/admin/`。 |
| `gateway` | 9501/TCP | 健康检查、就绪检查、指标和性能分析。 |
| `gameserver` | 9502/TCP | 健康检查、就绪检查、指标和性能分析。 |

观测端点提供 `/healthz`、`/readyz`、`/metrics` 和 `/debug/pprof`。

## 前置条件

- 从源码构建时，需要 Go 1.27.3 或更高版本。
- `launcher` 需要 Windows；其他服务器命令可在 Go 支持的其他平台上构建。
- 具备绑定 443 端口的权限。
- 配置的端口没有被其他进程占用。

## 构建与配置

```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1
Copy-Item config.example.yaml config.yaml
```

也可使用 `make build`，构建产物默认位于 `bin/`。依赖为纯 Go 实现，SQLite 驱动使用 `modernc.org/sqlite`，无需 CGO。开发验证命令见[贡献指南](../CONTRIBUTING.md)；协议语料回环可运行 `make corpus`，本地没有语料文件时会自动跳过。

启动前编辑 `config.yaml`：

- 将 `gateway.advertise_host` 设置为客户端可用于实时连接的地址。
- 将 `gateway.login_domain` 设置为主登录主机名。生成的证书会覆盖该名称、`localhost` 和 `127.0.0.1`。
- 为 `gateway.tunnel.psk` 和 `game.tunnel.psk` 设置相同的非空值。
- 在将服务开放到任何网络之前，替换 `admin.jwt_secret`、`admin.password` 和隧道 PSK。
- 保持 `sqlite.static_path` 指向随附的 `server_data/static.sqlite3`。

`config.yaml` 已被 `.gitignore` 排除。配置加载器支持 `config.example.yaml` 中说明的 `RF4_*` 覆盖项。

常用配置入口：

- `config.example.yaml` 是入库模板；实际使用 `config.yaml`，不得提交运行配置。
- `sqlite.path` 指向玩家库；`sqlite.static_path` 指向运行期只读的静态目录。
- `launcher.game_path` 保存客户端路径，`launcher.game_region` 默认为 `CN`。
- 环境变量如 `RF4_SQLITE_PATH`、`RF4_SQLITE_STATIC_PATH`、`RF4_LOG_LEVEL`、`RF4_GAME_CLIENT_PATH` 可覆盖对应设置，完整列表见[配置模板](../config.example.yaml)。

YAML 文件中的管理员用户名和密码会在启动时应用。账户不存在时会创建账户；已有账户的密码不同时会重置为配置中的密码。

## 初始化数据

在启动服务器进程前，先运行一次 `seed`。重复运行是安全的。

```powershell
bin\seed.exe -config config.yaml
```

初始化会执行数据库迁移、导入静态目录、创建配置的默认玩家档案，并创建或对齐管理员账户。重复运行不会删除玩家数据。

## 启动

### 启动器

```text
bin\launcher.exe
```

首次使用时，launcher 会初始化数据库，启动 `gameserver` 和 `gateway`，执行健康检查，并重启已退出的进程。管理后台 handler 直接内嵌在 launcher 中，统一入口为 `http://127.0.0.1:9500`，不再监听独立的 8080 管理端口。

配置 `ai.enabled: true` 时，launcher 还会拉起可选的 `aibridge` 进程（观测入口 `http://127.0.0.1:9504/healthz`），控制中心会显示一张与 gameserver/gateway 同级的 aibridge 状态卡，可单独启停/重启并查看健康状态。它挂掉不会影响游戏进程，launcher 会按指数退避自动重启。AI 的厂商、模型、API Key 与出声开关都在管理后台「AI 助手」页配置。

### 从控制中心启动游戏

在统一控制中心的“运行控制”页填写 `rf4_x64.exe` 的绝对路径并保存。路径会写入当前 `config.yaml` 的：

```yaml
launcher:
  game_path: 'D:\Games\RF4EN\Game\rf4_x64.exe'
  game_region: "CN"
```

点击“启动游戏”后，launcher 直接执行：

```text
D:\Games\RF4EN\Game\rf4_x64.exe -region CN
```

只接受本机同源页面调用，路径必须是存在的绝对路径，且文件名为 `rf4_x64.exe`。

### 手动启动

在独立终端中分别启动以下命令：

```powershell
bin\gameserver.exe -config config.yaml
bin\gateway.exe -config config.yaml
bin\aibridge.exe  -config config.yaml   # 可选：AI 助手
```

手动运行这些进程不会启动管理控制台；管理控制台随 `launcher` 的 9500 统一入口提供。`aibridge` 依赖 gameserver/seed 先完成数据库迁移。

Windows 发布包默认本地开发凭据为 `admin` / `admin123`；源码部署使用配置中的管理员凭据。对于任何未与本地计算机隔离的环境，请在 `config.yaml` 中替换凭据和密钥。

## 部署后自证版本

`bin/` 下的可执行文件会被任何一次 `build.ps1` / `tools/build.ps1` / `make build` 覆盖，而这几个脚本都按**执行时的检出目录**编译。本仓常同时存在多个 worktree，覆盖之后进程还在跑旧代码，从 `/healthz`、托盘、启动日志都看不出来，于是「明明部署了却没生效」。

部署完必须核对「运行中的进程」而不是「磁盘上的文件」：

```powershell
# 直接问进程：这是哪个构建、从哪个目录构建的
curl.exe -s http://127.0.0.1:9502/metrics | Select-String rf4_build_info

# 经 launcher 看：running_build 是子进程自报，exe_changed 表示磁盘已被替换
curl.exe -s http://127.0.0.1:9500/api/status
```

正常输出形如：

```text
rf4_build_info{process="gameserver",version="20260101090000",buildts="20260101010000",commit="abc1234",source="<检出目录>"} 1
```

要点：

- `source` 是构建时的**检出目录**（正斜杠），`commit` 是对应提交的短 SHA，`buildts` 是 UTC 构建时间。`bin/gameserver.exe` 的 `source` 必须是你期望的那个 worktree——不一致说明被别的检出覆盖过。五个构建入口（`build.ps1` / `tools/build.ps1` / `make build` / `.github/scripts/build-preview.sh` / `build-release.sh`）都会自动注入这四个标签，不需要手填；CI 制品的 `commit` 取触发构建的 `$GITHUB_SHA` 短码。
- `procs[].running_build` 缺失或面板显示「未知」表示观测端口不通或跑的是不带这些标签的老构建，不能当成「版本正常」。
- `procs[].exe_changed` 为真表示磁盘上的 exe 已换但进程没重启（面板同时显示提示条，launcher 日志出现 `WARN ... 已被替换`）。此时新构建**尚未生效**，重启对应进程才会切换。
- `/api/status` 的 `current_version` 是 **launcher 自己**的版本；子进程版本看 `procs[].running_build.version`，两者混读会得出错误结论。

手工做验证性部署时请显式带语义化版本号，便于事后追溯：

```powershell
go build -trimpath -ldflags "-s -w -X 'rf4server/internal/buildinfo.Version=<语义化标签>' -X 'rf4server/internal/buildinfo.BuildTs=<UTC 时间戳>' -X 'rf4server/internal/buildinfo.GitCommit=<提交短 SHA>' -X 'rf4server/internal/buildinfo.SourceDir=<检出目录>'".Replace('\','/') -o .deploy/gameserver.exe ./cmd/gameserver
```

设计与取舍见 [deploy-self-verify](../specs/changes/deploy-self-verify/spec.md)。

## 客户端接入

客户端必须将登录主机名解析到网关，并信任生成的 HTTPS 证书。对于使用默认登录主机的客户端，请在 hosts 文件中添加以下条目：

```text
127.0.0.1 api.rf4game.ru
```

如果客户端还会请求其他登录主机名，也需要将其映射到网关，并提供覆盖该主机名的证书。

先启动一次网关以生成 `certs/server.crt` 和 `certs/server.key`，然后仅在本地测试环境中信任 `certs/server.crt`。该证书为自签名证书；不得重新分发私钥。

`gateway.advertise_host` 和 `gateway.advertise_port` 会包含在登录响应中。客户端使用这两个值建立实时连接。

## 数据

主数据库是一个 SQLite 文件，通常为 `data/rf4server.db`，由 gameserver、gateway 和内嵌管理后台共享使用，并启用 WAL 模式。

静态目录存储在 `server_data/static.sqlite3` 中，运行时以只读方式挂载。隔离模型详见 [static_runtime_isolation.md](static_runtime_isolation.md)。

更新发布包前，先退出 launcher 并确认所有服务已经停止，再备份 `data/`、`config.yaml` 和 `certs/`。保留原发布包，以便需要时配合升级前的数据备份恢复；不要用新版本初始化出的空数据库覆盖现有存档。不要上传或分享这些运行数据和密钥。

AI 的 API Key 密文与主密钥不随 `data/` 一起打包才安全：主密钥文件默认 `data/ai_master.key`，密文文件是同目录的 `data/ai_credentials.json`，两者必须**分别备份、分别保护**，详见 [AI 凭据与主密钥](#ai-凭据与主密钥)。

`seed` 会将静态表导入主数据库，作为迁移和兼容副本。运行时会将对存储层所列静态表的读取重定向到只读的 `static` schema。

## AI 凭据与主密钥

AI 厂商的 API Key 只以 AES-256-GCM 密文保存在运行版本地，明文不落库、不进日志、不进配置。存储分成两类文件，权限均为 `0600`：

| 文件 | 默认位置 | 内容 |
| --- | --- | --- |
| 主密钥 | `data/ai_master.key` | 32 字节随机密钥；解密密文只靠它 |
| 不可变密文对象 | `data/ai_credentials.d/<ref>.json` | 每次保存/迁移生成一个对象：密文、主密钥指纹、厂商 ID、遮蔽值；创建后不再改写 |
| 旧凭据文件（只读兼容） | `data/ai_credentials.json` | 升级前的多厂商密文文件；V2 只读，不再写入 |

凭据目录固定与主密钥同目录，不需要单独配置。`ai.master_key_file` 的相对路径按**配置文件所在目录**解析（不是启动目录），所以同一份配置从哪个目录启动都指向同一把主密钥。

**首次部署（空安装）**：主密钥不会在进程启动时自动生成。有两条等价的显式初始化路径：

1. **命令行（可在启动任何服务之前做）**：`bin\aibridge.exe -init-key -config config.yaml`。只有「主密钥缺失 + `ai_secret` 无密文 + 旧凭据文件不存在 + 凭据目录里没有对象/准备件」四条同时成立才会创建；成功时打印「主密钥已创建」或「主密钥已存在，未改动」后立即退出，不启动 Runner、不出网，也不打印任何 Key 材料。任一条件不成立都会报错退出并保留原文件。
2. **管理后台**：在「AI 助手」页填写厂商、模型 ID 和 API Key 并保存——这次显式保存会创建主密钥、先落一个不可变密文对象，再在一个 SQLite 事务里发布活动修订/引用与自动记录，并写一条 `ai_master_key` 审计；审计写不进去时不会报保存成功（主密钥已落盘则保留）。

在此之前 `aibridge` 会按 fail-closed 启动失败（日志「主密钥加载失败」），这是预期行为。

**保存与发布（DEV-114 A）**：

- 新 Key 先写到凭据版本目录里的随机引用对象（临时写 → fsync → 排他安装 → 读回可解密），成功后才进入数据库发布；
- 发布在一个写事务内同时更新厂商/模型行、活动修订（`ai_setting.ai_config_active`）、自动记录与审计；任一步失败整笔回滚，读者继续用上一套完整配置；
- 留空 Key 只允许在同 scheme/主机/有效端口下沿用现有密钥；换源必须重新填写，跨源留空整笔拒绝；
- Runner、后台复测与状态回读只按活动修订读取同一份快照；活动引用缺失、指纹不符或解密失败即锁定，不回退同名文件或旧库；
- 页面显示「已保存 · 域 / 模型 / 修订，待复测」；复测结果按被复测的修订记录，迟到的旧响应不会把新配置标成已测。

**从旧版本升级（`ai_secret` / 旧凭据文件里已有密文）**：

1. 保留原主密钥文件（不要删除、不要重新生成）。升级后第一次在后台保存（即使留空 Key）时，按「原外部凭据文件优先、旧库 `ai_secret` 只读后备」解析旧套；确认地址与模型能和旧密钥配对后，生成新的不可变对象并以 `migration` 来源幂等登记。旧文件与旧库行逐字节保留，不删、不改。
2. 没有活动修订、也没有发布记录时，Runner 仍按旧安装只读兼容运行；一旦发布过修订，只认数据库里的活动指针。
3. 发布历史与活动指针不一致、旧文件损坏、主密钥不符或读库失败都不会被当成「未初始化」：系统锁定并保留文件，需要人工核对后重新提供凭据。

**平台要求（安装持久性）**。凭据对象写入后会同步文件，按引用读回验证，再同步目录才发布配置。Windows 启动器支持本机固定 NTFS 卷上的实例目录，使用运行账户可写的目录句柄确认安装，不要求以管理员身份运行。请使用安装时已建立的实例根目录及其数据子目录，确保密钥、密文和备份只能由授权账户访问。UNC 路径、无法确认的挂载或重解析路径、未验证的卷类型和目录写权限故障会拒绝新 Key，旧可读配置可继续使用。换盘时先关闭实例，把数据库、主密钥及对应密文对象配对迁移，保持主密钥和备份分别保护，再启动并复测。非 Windows 继续使用目录 fsync。

**备份**：退出 launcher、确认所有服务停止后，分别复制主密钥、凭据版本目录与旧凭据文件；主密钥与密文不要放在同一份介质/同一个备份包里，也不要点到点上传。主密钥丢失等于所有密文作废。

**恢复**：把主密钥与凭据目录放回运行目录（主密钥路径以 `config.yaml` 的 `ai.master_key_file` 为准），或改配置指向备份位置后重启。验证方式：管理后台 AI 状态显示凭据已就绪与活动修订、`POST /api/ai/test` 返回成功（显示「此连接已通过复测」）、`aibridge` 健康检查正常。只恢复其中一个文件时系统会明确锁定并保留文件，不会自动新建主密钥。

**成组降级**见下一节。设备上同时保留与两套程序各自匹配的配置与分开保护的凭据。

**主密钥不匹配/凭据损坏**：保存与「复测已保存连接」都会明确报错（`master_key_unavailable` / `credential_locked` / `config_locked`），文件保持原样。不要删除或重写主密钥、凭据目录或旧凭据文件来「绕过」错误——那会丢失其他厂商的可用密文。


## AI 助手四步开聊

管理后台「AI 助手」页的「总览」是日常入口。管理员按四步把 AI 用起来，目标 5 分钟：

1. **保存并复测连接（90 秒）**：在「连接」填写厂商地址、模型 ID 与 API Key，保存后点「复测已保存连接」。复测只使用已保存的活动配置；失败时只重测，不把页面草稿发出去。
2. **创建 Agent（45 秒）**：在「角色与订阅」填 Key、展示名、角色、思考档与系统提示词。联系人 UID 由服务端自动分配；用回已删除的 Key 会复用原 UID，原私聊与渔获历史保留，但需要重新接入。
3. **接入并找到联系人（45 秒）**：对角色点「接入游戏」。这只是保存管理配置；玩家是否真的能在游戏里看到联系人，以游戏端证据为准。让玩家在游戏联系人列表里找到角色并打开私聊，或确认它已收到/发出过个人消息。
4. **发首句并收到回复（60 秒）**：让玩家在游戏私聊里发送「你好」。页面的证据依次为：DM 已入队（`ai_task`）→ AI 节点已回复（`ai_result`）→ 游戏服已投递（`delivered_ts` 或个人消息）。没有真实客户端时，页面只显示「待确认」，后台不会代发消息或伪造回复。

总览的状态优先级固定为：紧急停止 → 需登录 → 缺 Key → 连接失败 → 命令等待 → 可试说。缺数据一律显示「待确认」。急停可在总览提示下直达「连接」页的同一控制；解除后需重新复测。范围订阅（地图/全局/精确频道）与能力保存分别归「角色与订阅」「性格与交互」，接入状态、玩家可见与能力生效是三个不同的事实，不要互相替代。

## AI 运行态单写者与 cutover

AI 的角色状态、频道成员与战绩记忆只有**一个活动 gameserver** 可以写。后台的角色开关、删除、清记忆、频道勾选，以及 aibridge 的「换目标鱼」都只提交持久命令（`ai_runtime_command`），由 gameserver 在同一事务里校验代次/版本后应用；aibridge 只读状态。

**部署前提**：同一数据库同一时间只运行一个 gameserver。多开会让两个进程都尝试消费命令（SQLite 写事务会把它们串行化，但轮询/推送等游戏侧行为仍不保证）。需要扩容时先拆分数据库，不要共享同一个 `data/rf4server.db`。

### T-102 成组升级（DEV-120 A）

1. 关闭外部玩家与后台操作入口，在当前一组后台开启急停。升级后的后台须用最新状态返回的
   `emergency_stop.epoch` 作为 `emergency_stop_epoch` 提交实际改值。急停生效前
   已获最后放行的请求可能返回，部署窗口应等待这些请求和消费者退出。
2. 停止 AD、BR、GS 及能够自动拉起它们的 launcher、服务和监控。部署主管在编排层停用旧组件
   的自动重启，隔离旧执行路径及共享数据库权限。确认数据库和端口只有本次窗口的进程能够使用。
   在此状态备份 SQLite、配置、旧凭据、加密对象和独立保管的主密钥。
3. 在隔离入口内由新版 GS 对共享玩家库运行迁移。核对
   `schema_migrations` 的 `00906_ai_emergency_epoch.sql` 只登记一次，
   `ai_setting.emergency_epoch_record` 格式版本为 1，首迁代次为 1。
   确认待消费的旧任务、结果、工具作业、命令和发鱼测试请求已终结，玩家数据、已完成历史、
   活动引用及审计可读。发现部分进程仍在运行或迁移失败时维持隔离并检查一致性。
4. 保持玩家入口关闭，启动同一发布组的 AD 和 BR。检查后台急停与活动修订、BR 健康及旧队列
   终态。按部署验收解除急停，确认新意图能工作；完成后开放入口。目标 Linux 使用实际凭据
   目录经 HTTP 保存合成 Key，重启 AD/BR 后分别验证使用同一活动套，记录挂载、修订和脱敏计数。

### T-102 成组回撤与再次升级

1. 隔离入口，开启急停。等已放行的外发和消费者退出，停止并隔离整组 AD/BR/GS 及其自动拉起
   路径。核实共享库只有维护连接，留存一份本次窗口的配套备份。
2. 在同一玩家库事务中收尾所有旧组可能领取的条目。将 pending/processing 任务设为 fallback
   并清正文，仍有正文的待投递结果设 `delivered_ts=-1` 并清正文，未完成工具作业设为 done 并清参数和
   结果，pending 命令及待处理发鱼测试记拒绝与原因。检查各可消费集合计数为零。
3. 保留 00906 的追加字段、代次记录和迁移登记，保留玩家写入、历史终态、审计和新密文对象。
   在维护事务内恢复与旧程序及旧凭据配对的 provider/model、目的地、搜索与旧闸配置。回到采用
   DEV-114 A 的发布时还须核对兼容的活动修订和引用。回到更早组时，将新活动发布资料与对象保存在
   独立备份中，并用旧组原有来源恢复密钥。核实旧地址、模型和密钥的一致性。
4. 启动同一版本的旧组，验健康、旧闸、普通玩家公聊和无旧积压重放。恢复入口前核对玩家数据、
   审计和新密文对象仍在。再次升级仍需按第 1、2 步隔离和清理旧版新产生的无可信来源工作，
   并在验算版本与代次后按本节升级步骤切回。保留原代次记录，重新开放前经新版后台做一次有审计的
   急停切换，使历史绑定与新工作隔离。

运行期命令返回 `202` 与 `command_id`，页面通过 `GET /api/ai/commands/{id}` 查询
终态。GS 停机时仍有效的意图持久等待，超过 TTL 标记 expired。队列满返回 429。

## 安全与协议约束

- 网关登录流程不会认证游戏账户。它会为配置的用户 ID 签发 token，而实时连接会接受结构有效、但未在当前 token 存储中注册的 token。请将网络访问边界视为信任边界，不要将服务暴露到公网。
- 完整的登录 token 就是 RC4 密钥。输入和输出 RC4 流会跨帧连续。帧分配、加密和套接字写入必须通过会话写入器保持有序。
- 在处理客户端帧之前先发送 ACK。改变这一顺序会破坏客户端协议。
- 服务器到客户端的推送必须使用预期的顶层 value 结构，并通过响应契约检查。无效推送会被拒绝，而不会发送。
- 世界时钟 keepalive 间隔必须小于客户端连接超时时间。默认为 5 秒；客户端在没有下行数据约 10 秒后断开连接。世界时钟使用 UTC。
- 将 launcher、隧道和观测端口绑定到可信网络接口。替换所有默认凭据和密钥。

## 智能体辅助部署 prompt

以下 prompt 可按本机实际路径调整后，直接交给 Codex、Dsh、豆包、WorkBuddy 等智能体：

```text
请帮我在这台 Windows 电脑上部署并启动 <项目目录> 里的 rf4_server_simulator。

请先检查：
- Windows 版本，以及当前是否以管理员权限运行；
- Go 是否已安装，版本是否不低于 1.27.3；
- 443、9341、9401、9500-9504 端口是否可用。

然后完成以下工作：
1. 阅读 README.md、config.yaml 和 docs/deploy.md，说明你将执行的操作。
2. 构建项目，确认 bin 下生成 gateway.exe、gameserver.exe、seed.exe、aibridge.exe 和 launcher.exe。
3. 检查 config.yaml 中 gateway.tunnel.psk 与 game.tunnel.psk 一致，sqlite.static_path 指向 server_data/static.sqlite3。
4. 备份 C:\Windows\System32\drivers\etc\hosts，然后把 api.rf4game.ru 和 api.rf4game.com 指向 127.0.0.1。
5. 启动一次 gateway 生成 certs/server.crt 和 certs/server.key，并指导我把 server.crt 安装到“受信任的根证书颁发机构”。不要上传或分享私钥。
6. 启动 bin\launcher.exe，检查 http://127.0.0.1:9500、http://127.0.0.1:9500/#admin 以及各服务健康检查。
7. 只做构建和运行配置修复，不改变业务逻辑。每一步告诉我执行内容、结果和失败原因；需要管理员权限时先停下让我确认。
```
