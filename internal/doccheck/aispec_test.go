package doccheck

import (
	"strings"
	"testing"
)

// aiSpecFixture 造一份最小仓库：一份 AI spec + 合法的 docs 索引。
func aiSpecFixture(t *testing.T, spec string) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"docs/README.md":                  "# 索引\n",
		"specs/changes/ai-sample/spec.md": spec,
	})
}

func aiSpecIssues(t *testing.T, root string) []Issue {
	t.Helper()
	report, err := Run(root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return issuesByRule(report.Issues, RuleAISpecRef)
}

func TestAISpecRefHistoricalNotesPass(t *testing.T) {
	root := aiSpecFixture(t, strings.Join([]string{
		"# 样例",
		"Status: done",
		"",
		"## Problem Statement",
		"",
		"旧实现仍手写 `guardAIFishingTalkColumn`。",
		"",
		"## Solution",
		"",
		"历史名 `00060_ai_bridge.sql` → 现 `00900_ai_bridge.sql`。",
		"已删除 `guardAIFishingTalkColumn` 与 `-- ai-guard` 标记。",
	}, "\n"))
	if got := aiSpecIssues(t, root); len(got) != 0 {
		t.Fatalf("合法历史注记不应报错，实际 %d 条：%+v", len(got), got)
	}
}

func TestAISpecRefBareOldMigrationNameIsReported(t *testing.T) {
	root := aiSpecFixture(t, strings.Join([]string{
		"# 样例",
		"Status: done",
		"",
		"## Solution",
		"",
		"当前迁移是 `00060_ai_bridge.sql`。",
	}, "\n"))
	got := aiSpecIssues(t, root)
	if len(got) != 1 {
		t.Fatalf("裸旧迁移名应报 1 条，实际 %d 条：%+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "00060_ai_bridge.sql") {
		t.Fatalf("报错未指出旧名：%+v", got[0])
	}
	t.Logf("裸旧名命中：%s", got[0].String())
}

func TestAISpecRefBareRetiredSymbolIsReported(t *testing.T) {
	root := aiSpecFixture(t, strings.Join([]string{
		"# 样例",
		"Status: done",
		"",
		"## Solution",
		"",
		"守卫 `guardAIFishingTalkColumn` 仍在当前实现。",
	}, "\n"))
	got := aiSpecIssues(t, root)
	if len(got) != 1 {
		t.Fatalf("裸退役符号应报 1 条，实际 %d 条：%+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "guardAIFishingTalkColumn") {
		t.Fatalf("报错未指出退役符号：%+v", got[0])
	}
	t.Logf("裸退役符号命中：%s", got[0].String())
}

func TestAISpecRefUnmarkedHistoryIsReported(t *testing.T) {
	root := aiSpecFixture(t, strings.Join([]string{
		"# 样例",
		"Status: done",
		"",
		"## Further Notes",
		"",
		"历史实现曾使用 `00078_ai_map_subscription.sql`。",
	}, "\n"))
	got := aiSpecIssues(t, root)
	if len(got) != 1 {
		t.Fatalf("只有「历史」字样、没有现名或「已删除」时应报 1 条，实际 %d 条：%+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "00078_ai_map_subscription.sql") {
		t.Fatalf("报错未指出未标记的历史引用：%+v", got[0])
	}
	t.Logf("无标记历史引用命中：%s", got[0].String())
}

func TestAISpecRefMissingOldRangeMigrationIsReported(t *testing.T) {
	root := aiSpecFixture(t, strings.Join([]string{
		"# 样例",
		"Status: done",
		"",
		"## Solution",
		"",
		"当前迁移是 `00082_ai_missing.sql`。",
	}, "\n"))
	got := aiSpecIssues(t, root)
	if len(got) != 1 || !strings.Contains(got[0].Message, "00082_ai_missing.sql") {
		t.Fatalf("当前段引用不存在的旧号段迁移文件应报错，实际 %+v", got)
	}
	t.Logf("缺失旧号段文件命中：%s", got[0].String())
}

func TestAISpecRefIgnoresNonAISpecs(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/README.md": "# 索引\n",
		"specs/changes/other/spec.md": strings.Join([]string{
			"# 非 AI",
			"Status: done",
			"",
			"## Solution",
			"",
			"迁移是 `00060_ai_bridge.sql`，守卫 `guardAIFishingTalkColumn`。",
		}, "\n"),
	})
	if got := aiSpecIssues(t, root); len(got) != 0 {
		t.Fatalf("非 AI spec 不在规则范围内，实际 %d 条：%+v", len(got), got)
	}
}
