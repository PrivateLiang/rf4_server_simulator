# AI 主密钥 CLI 初始化

Status: done

## Problem Statement

空安装（没有主密钥、没有旧库密文、没有外部凭据文件）时，`aibridge` 按 fail-closed 启动失败是正确行为，但此前唯一的显式初始化入口是「先在管理后台保存一次 API Key」。这要求 launcher / gameserver / 管理后台先起来、数据库已经迁移、并且运维已经在页面上填好厂商与模型；在只想先把密钥文件备好、或后台尚未配好时无法完成初始化。另一条老路「缺失即自动创建」会让既有密文永久无法解密，不能恢复。

## Solution

提供一次性、显式的运维入口 `aibridge -init-key -config <path>`：

1. 与后台保存共用同一个判定 helper：只有「主密钥缺失、旧库 `ai_secret` 无任何密文行、外部凭据文件不存在」三条**同时成立**才创建主密钥。
2. 创建后打印「已创建」并立即退出；已存在且合法时打印「已存在，未改动」并退出；损坏时明确报错、保留原文件。
3. 任一拒绝条件成立（旧库有密文 / 外部凭据文件存在 / 主密钥损坏）都以非零码退出，不覆盖任何文件，提示恢复原主密钥或重新提供凭据。
4. 该入口不启动 Runner、不建 Provider、不加载静态资料库、不监听观测端口、不产生网络活动；不打印、不落日志任何 Key 材料。

## User Stories

1. 作为运维，我可以在启动任何服务之前运行 `aibridge -init-key -config config.yaml`，看到「主密钥已创建」后退出，随后正常启动 launcher/gameserver/aibridge。
2. 作为运维，重复执行 `-init-key` 或与后台保存并发执行，都只会得到同一把主密钥，文件字节不被改写。
3. 作为安全评审，我可以确认：主密钥损坏、旧库已有密文、外部凭据文件已存在时，命令拒绝创建且不改动任何既有文件；输出里没有 Key 材料。
4. 作为安全评审，我可以确认：CLI 与后台保存没有两套「是否允许创建」的判定，任何一侧放松都会同时影响另一侧并被测试抓住。

## Implementation Decisions

- `internal/aibridge/masterkey_init.go`（新增）：唯一判定点 `EnsureMasterKeyForExplicitInit(ctx, db, keyPath)`，返回 `MasterKeyInitResult{Key, Created}`；拒绝错误为 `ErrMasterKeyInitLegacyCiphertext` / `ErrMasterKeyInitCredentialFile`，二者同时满足 `errors.Is(err, ErrMasterKeyMissing)`，后台既有「缺主密钥」提示与 fail-closed 分支不需要分叉。`db == nil` 表示主库尚未创建，等价于「旧库没有密文行」；旧库存在但 `ai_secret` 表不存在时同样按无密文处理。
- `internal/aibridge/keyvault.go`：`InitMasterKey` 保持「幂等、损坏不覆盖」语义；创建改为「同目录写完整临时文件 → fsync → 硬链接到目标」，硬链接失败（目标已存在）时读取既有文件，文件系统不支持硬链接时退回 `O_EXCL`。内部 `initMasterKeyOnce` 额外报告本次是否真的新建，供 CLI 区分「已创建 / 已存在」。不改变主密钥格式、长度、权限与路径语义。
- `internal/admin/ai.go`：`ensureMasterKeyForSave` 改为调用共享 helper；只有 `Created` 时才写 `ai_master_key` 审计，其余行为与 T-012 一致（HTTP 状态、提示、保留原文件）。删除只服务于旧判定的 `fileExists`。
- `cmd/aibridge/main.go`：新增 `-init-key` 布尔 flag；命中后在任何日志、主库静态挂载、`OpenKnowledge`、`NewRunner`、观测端口之前完成判定并返回。检查旧库密文时只在主库文件已存在时打开它，不为了检查而创建空库；结果打印到 stdout（只说明是否新建），错误打印到 stderr 并以非零码退出。正常启动路径不变。
- `docs/deploy.md`、`config.example.yaml`：首次部署段落补充 CLI 路径，并明确三条件与「不打印 Key」。
- 兼容与回滚：不新增 HTTP 路由、不改表/迁移、不改加密算法、不改配置键。回滚只需删除 `-init-key` 分支；主密钥文件格式与后台保存路径不变。禁止回滚成「缺失即自动创建」。

## Testing Decisions

- 测哪一层：`internal/aibridge` 纯文件 + 真实临时 SQLite（不 mock 文件系统）；`internal/admin` 真实 handler + 真实 SQLite；`cmd/aibridge` 直接调用 `runInitKey`；另以两个真实进程手工冒烟并发。
- 期望值来源：32 字节长度、`ErrMasterKeyInvalid` / `ErrMasterKeyMissing` 字面量、旧库密文为测试手写合成行、外部凭据文件为手写 JSON；全部为合成数据，不连接真实厂商、不使用真实 Key。
- 覆盖：空安装（无主库 / 空库 / 已迁移空库）、幂等且字节不变、损坏/长度异常不覆盖、旧库有密文拒绝、外部凭据文件存在拒绝、并发两次只产生一把密钥、CLI 不创建空主库/不依赖静态库、后台 `ensureMasterKeyForSave` 走同一判定（T-012 既有用例全绿 + 仅凭据文件存在的锁定用例）。
- 回滚后哪些测试会变红：把三条件退回「缺失即创建」→ 旧库密文与外部凭据文件两组用例红；把 `InitMasterKey` 退回「建空文件再写」→ 并发用例可能读到半写文件；把后台判定改回本地实现 → CLI 用例不会覆盖后台、共享性失去约束。

## Out of Scope

- 不做主密钥轮换、迁移、删除或重新生成；不改变主密钥格式/位置语义。
- 不恢复「缺失即自动创建」，不做交互式输入，不新增 HTTP 路由或后台按钮。
- 不执行生产库密文迁出、不删除 `ai_secret`、不触碰真实主密钥/凭据文件。
- 不修改 `internal/store/**`（含迁移）、`internal/gameserver/**`、`internal/aiconfig/**`、`runner.go`、`actor.go`、`specs/capabilities/`、CI。

## Further Notes

- `[事实]` 空安装的既有入口是「后台首次保存 Key」，需要 launcher / 后台先可用；本票补的是同一判定的命令行入口。
- `[事实]` `ai_secret` 表不存在（未迁移/空库）等价于「没有密文行」，不会因此拒绝创建。
- `[事实]` 并发创建依赖文件系统原子「仅当不存在才出现」；本机 Windows NTFS 与 CI Linux 均支持硬链接，异常文件系统走 `O_EXCL` 兜底。
- `[缺口]` 未在真实 Windows ACL / 网络共享目录、真实旧库与真实运维部署上验证；真实库密文迁出仍需人批准。
