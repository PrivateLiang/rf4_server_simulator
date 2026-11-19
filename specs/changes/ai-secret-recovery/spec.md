# AI 凭据迁出与主密钥恢复

Status: done

## Problem Statement

AI 厂商 API Key 的密文存在玩家共享数据库 `ai_secret` 里，主密钥路径 `ai.master_key_file` 却按进程 CWD 解析，且加载函数 `LoadOrCreateMasterKey` 在文件缺失时**静默新建**一把主密钥。结果是：同一份配置从不同目录启动会指向不同主密钥（旧密文再也解不开），主密钥文件丢失/被换掉时进程会「自动好起来」——新生成的主密钥与既有密文不匹配，旧密钥永久不可恢复，而且在发现之前不会有任何提示。密文与主密钥都留在运行版本地，但没有版本化的备份/恢复流程；新填写的 Key 也继续写回共享库，使凭据与玩家数据绑在一起。

## Solution

凭据与主密钥拆开、按稳定路径解析，并且只在确认没有既有密文时显式初始化：

1. **稳定路径**：`ai.master_key_file` 的相对路径按**配置文件所在目录**解析成绝对路径（不是进程 CWD）；没有配置文件时才保留相对路径（本地开发默认值）。
2. **密文外置**：新凭据只写外部受保护凭据文件 `ai_credentials.json`（与主密钥同目录、分文件保存，权限 0600），不再写入共享数据库；旧写入口 `AISaveSecret` 显式停用。
3. **缺钥不建**：`LoadMasterKey` 缺失即返回 `ErrMasterKeyMissing`，绝不自动生成；`InitMasterKey` 只在显式初始化路径调用，已存在时幂等返回、损坏时保留原文件。
4. **显式初始化**：空安装第一次在后台保存 API Key 时初始化主密钥（该动作写审计）。已有任何密文（旧库 `ai_secret` 行或外部凭据文件）而主密钥缺失时一律锁定：报错、保留文件、不生成新主密钥。
5. **只读导出**：升级形态首次保存 Key 前，把旧库密文逐条验证「可解密 + 厂商关联非空」后原样导出到凭据文件；先写临时文件、整份校验通过再原子替换，旧库一行不删不改。无法用当前主密钥解开的旧密文跳过并在日志中要求重新提供凭据，绝不伪造恢复成功。
6. **锁定语义**：凭据文件带主密钥指纹（`key_id`）。指纹不匹配、JSON/字段损坏、条目解不开时：运行期任务降级为「未配置」提示，后台保存与测试连接返回明确错误，原文件保持不动。
7. **复用加密**：继续使用既有 AES-256-GCM `Encrypt`/`Decrypt`，不自创加密；遮蔽仍用 `Masked`（只留尾四位）。明文 Key 不进日志、回执、仓库。

## User Stories

1. 作为运维，我把同一份 `config.yaml` 从不同目录启动（计划任务、服务包装器、不同盘符），AI 仍使用同一把主密钥，旧密文照常解密。
2. 作为运维，升级到本版本后主密钥文件保留在原位置，第一次在后台保存 Key 时旧库密文被只读导出到外部凭据文件；`ai_secret` 表和数据都还在，没有自动清理。
3. 作为管理员，主密钥文件丢了或换过以后，「测试连接」不会拿旧密文硬发请求，而是明确提示凭据不可用；后台保存也不会悄悄新建主密钥把旧密文「覆盖」掉。
4. 作为安全评审，我能确认新填写的 Key 不出现在 SQLite 的任何表、不出现在日志/审计/HTTP 回显里，只以密文形式存在外部凭据文件。
5. 作为运维，我能把主密钥与凭据文件分别备份；恢复到新目录后解密的仍是原 Key；只恢复其中一个文件时会明确失败而不是假装成功。
6. 边界：没有原主密钥就无法恢复旧密文；系统要求重新提供凭据，不提供「伪造恢复成功」的路径。

## Implementation Decisions

- `internal/aibridge/keyvault.go`：新增 `LoadMasterKey`（只读，缺失/损坏可判定）、`InitMasterKey`（`O_EXCL` 一次性创建，幂等且不覆盖损坏文件）、`CredentialsPath`（主密钥同目录固定文件名）、`MasterKeyID`（SHA-256 前 8 字节指纹）、`CredentialFile`/`CredentialEntry` 与 `LoadCredentialFile`/`SaveCredential`/`APIKey`/`MaskedFor`；`ExportLegacyCredentials`（只读导出 + 原子切换）与 `ResolveProviderAPIKey`（凭据文件优先、旧库只读兼容）。`LoadOrCreateMasterKey` 保留为兼容入口，语义改为只读加载并标注 Deprecated。原子写为「同目录临时文件 → fsync → 校验整份可解密 → rename」，故障注入钩子只用于测试。
- `internal/aibridge/runner.go`：`loadProvider` 先用 `LoadSpecFromDB(..., nil)` 取不含密钥的厂商配置，再由 `ResolveProviderAPIKey` 决定用外部凭据还是旧库密文，最后 `NewProvider`；避免冻结的 loader 在主密钥不匹配时提前失败。
- `internal/admin/ai.go`：保存 Key 走「`ensureMasterKeyForSave`（仅在无任何既有密文时显式初始化）→ `ExportLegacyCredentials` → `SaveCredential`」，不再调用 `AISaveSecret`；「测试连接」只读加载主密钥并统一解析凭据，锁定时返回 `state=credential_locked` / `master_key_unavailable`；状态接口的 `api_key_masked` 优先读外部凭据文件、否则回退旧库。
- `internal/config/config.go`：`Load` 之后按配置文件目录解析 `ai.master_key_file`（绝对路径只 Clean，无配置文件时保持相对）。
- `internal/store/repos_ai.go`：`AISaveSecret` 停用并返回 `ErrAISecretWriteRetired`；新增只读 `AIListSecrets`（导出用）与 `AISecretByName`（兼容读取）。不新增表/列/迁移，不删 `ai_secret`（仍属 RuntimeTables）。
- 兼容与回滚：不新增依赖、表、列、迁移、协议命令与 HTTP 路由；不删任何既有数据。回滚只需恢复上述文件与旧启动方式，但回滚后主密钥路径重新依赖 CWD、缺失会再次静默新建（安全票的回滚只允许回到「拒绝外发」，不得用「恢复外发」当回滚手段）。

## Testing Decisions

- 测哪一层：纯文件/密钥逻辑（`internal/aibridge` 临时目录）、旧库导出与运行期解析（`internal/aibridge` 真实 SQLite + 手写密文）、配置加载（`internal/config` 临时 YAML + `t.Chdir`）、后台真实 handler（`internal/admin` 真实 SQLite + 本地 fixture）、存储层只读接口（`internal/store`）。
- 期望值从哪来：主密钥、明文 Key、密文、遮蔽值全部是测试手写字面量，不由被测构造函数生成；「旧库是否被改」直接比对导入前后的密文字节；「外部凭据是否可用」用同一把主密钥独立解密验证。
- 覆盖：首次显式初始化与重复初始化幂等；主密钥缺失不建文件/目录、损坏不覆盖；CWD 变化后同一配置仍指向同一把主密钥；凭据文件不含明文、可解、遮蔽尾四位；主密钥不匹配/凭据文件损坏时读写都锁定且文件字节不变；部分写失败（临时文件已写、rename 前失败）后旧文件完好、临时文件不残留、重试成功；备份到新目录恢复可解，只恢复密文时锁定不建钥；旧库只读导出（原样密文、厂商关联、旧库零改动、重试幂等）；无原主密钥的旧密文跳过且不写空文件、重新提供凭据可写入；缺密文返回空而非伪造；错误与跳过原因不含明文/密文；运行期凭据文件优先、无文件回退旧库、不匹配锁定；后台首次保存不写 `ai_secret`、写外部凭据、审计只记遮蔽值、状态展示遮蔽值、主密钥缺失/损坏/轮换时保存与测试连接均锁定且零外发。
- 回滚后哪些测试会变红：把 `LoadMasterKey` 改回「缺失即新建」→ 缺钥不建、备份恢复、缺钥锁定用例红；删掉 `config.Load` 的目录解析 → CWD 用例红；后台保存改回 `AISaveSecret` → 不落库、外部凭据、状态遮蔽用例红；去掉指纹校验 → 主密钥不匹配锁定用例红；把原子写换成直接写 → 部分写失败用例红；导出不验证可解密/不原样复制 → 只读导出、无原主密钥跳过用例红；`loadProvider` 改回只用旧库 → 凭据文件优先用例红。
- 全部为合成数据与本地 SQLite；不连接真实厂商、不使用真实凭据、不产生第三方出网。

## Out of Scope

- 不删除 `ai_secret` 表、不自动清理旧密文、不执行生产库迁移：本票只提供只读导出与备份/恢复流程，生产迁出与旧密文删除仍需人批准。
- 不新增后台页面/路由/前端改动：`index.html` 只读，`api_key_masked` 展示由后端返回。
- 不提供独立 CLI 初始化命令（`cmd/*` 本票只读）：空安装的显式初始化入口是后台保存 Key。
- 不验证真实厂商、真实旧库升级、真实备份介质与多进程并发初始化。
- 不改协议、结算、玩家状态条真源、静态库、迁移与 CI。

## Further Notes

- `[事实]` 改动前 `LoadOrCreateMasterKey` 在文件缺失时生成密钥并写盘（`keyvault.go` 旧实现），`cmd/aibridge`、`cmd/admin`、`internal/admin/ai.go` 三处调用；`ai.master_key_file` 相对 CWD 解析。
- `[事实]` `ai_secret` 属 RuntimeTables（`internal/store/repos_integrity.go`），本票只停写、只读、不删表。
- `[推断]` 升级部署若曾从不同 CWD 启动并各生成过主密钥，旧的 `data/ai_master.key` 只能解开其中一部分密文；导出会逐条报告「无法解密、需重新提供凭据」，不会伪造成成功。
- `[缺口]` 未在真实旧库、真实备份介质、真实厂商与真实浏览器上验证；`aibridge` 在未初始化主密钥时按 fail-closed 启动失败（`cmd/aibridge` 只读，无法在启动时给出更细提示），需按部署文档先完成一次显式初始化。
