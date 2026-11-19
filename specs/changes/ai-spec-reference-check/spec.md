# AI spec 正文引用检查（doccheck 窄规则）

Status: done

<!-- 2026-11-17 维护者已评审确认（按现状接受 T-023 规则范围与正反夹具）；同日由 draft 转 done。 -->

## Problem Statement

`doccheck` 只校验文档索引、spec 状态、链接与 `docs/...` / `specs/...` 路径引用，不检查正文里的迁移文件名与符号。T-014 让号、T-017 复用官方守卫后，AI spec 曾继续引用已改号的迁移名与已删除的手写守卫；T-018 跨三张票才清掉，且同类漂移没有门禁能自动发现。运维按 spec 找迁移文件、或按符号排查守卫时，会读到不存在的机制。

## Solution

在 `internal/doccheck` 增加一条窄规则 `ai-spec-ref`，只扫描 `specs/changes/ai-*/spec.md`，只检查三件事：

1. 当前描述里的旧号段 AI 迁移文件名（`000[6-8][0-9]_ai_*.sql`）必须存在于 `internal/store/migrations/`；不存在的旧名若未标注为历史，报错。
2. 已退役符号（T-017 删除的手写守卫标识符与分段标记）不得出现在「当前实现描述」段（`## Problem Statement` 之外）；历史引用必须与「已删除」/「退役」等标记同行。
3. 历史引用必须带明确标记：迁移名用「历史名 → 现 `00900_…`」或「已删除」，符号用「已删除」/「退役」/「不再」等；不以整文件豁免消误报。

## User Stories

1. 作为运维，我希望 spec 里出现的迁移文件名都真实存在，按名找文件不会落空。
2. 作为维护者，我希望 spec 不再把已删除的守卫当成现行机制，评审时能直接看到漂移。
3. 作为 Agent，我希望历史说明仍可保留，但必须写明当前名或「已删除」，不靠整文件豁免。
4. 边界：`Problem Statement` 按问题描述段处理，允许叙述旧机制；其他段必须显式标注。

## Implementation Decisions

- 规则常量 `RuleAISpecRef = "ai-spec-ref"`，实现放 `internal/doccheck/aispec.go`，由 `Run(root)` 调用；不新增统计字段，CLI 输出格式保持不变。
- 扫描范围仅 `specs/changes/ai-*/spec.md`；非 AI spec、`docs/`、`specs/capabilities/` 不扫。
- 迁移名正则匹配 `000[6-8][0-9]_ai_*.sql`（`.sql` 可省略），到 `internal/store/migrations/` 查存在性；官方迁移号（不含 `_ai_`）与现行 `00900`+ 号段不在存在性检查范围内。
- 已退役符号固定为 `guardAIFishingTalkColumn` 与 `-- ai-guard`（覆盖 `-- ai-guard:begin/end`，两者均已删除）；符号出现在 `## Problem Statement` 之外且同行没有删除/退役标记时报错。
- 历史标记按行判定：迁移名接受「已删除」「→ 现」「→现」「现名」「当前名」「现为」「当前为」；符号接受「已删除」「删除」「移除」「退役」「废弃」「弃用」「不再」「此前」「原」「旧实现」「旧版」。不按文件跳过。
- 三反引号围栏内不参与判定（与既有 `link` / `ref` 规则一致）。
- 不引入第三方依赖，不改 CI、官方 spec、迁移 SQL 与生产代码。

## Testing Decisions

- 测哪一层：`internal/doccheck` 包内单元测试，`t.TempDir()` 造最小仓库；`Run(root)` 是唯一入口。
- 正例：AI spec 同时包含当前迁移名、带「历史名 → 现」的旧名、带「已删除」的退役符号，以及 `Problem Statement` 里的旧符号 → `ai-spec-ref` 零 issue。
- 反例（各自必须报错）：① 当前段裸旧迁移名（如旧号 `0006x_ai_*.sql` 的具体文件名）；② 当前段裸退役符号；③ 只有「历史」字样、没有当前名或「已删除」的历史引用；④ 当前段引用不存在的旧号段 AI 迁移文件。
- 仓库本体：`TestRepositoryDocs` 仍要求全仓 `issues=0`；另以夹具断言 `ai-spec-ref` 为 0，防止规则误报既有合法历史叙述。
- 撤回反证：在 AI spec 注入一处裸旧名 → `go run ./tools/doccheck` 非零；移除后恢复 0。

## Out of Scope

- 不写通用 Go 符号解析器、不做全仓符号分析、不校验官方 spec / `specs/capabilities/`。
- 不新增 CI job、不改 `.github/`、不引入第三方依赖。
- 不重写迁移历史结论，不修改既有 AI spec 正文。

## Further Notes

- `[事实]` 规则改动已于 2026-11-17 经维护者评审确认（按现状接受）；本 spec 同日由 draft 转 done。
- `[缺口]` 规则不覆盖未来新增的退役符号；新增退役项需同步规则常量与夹具。
