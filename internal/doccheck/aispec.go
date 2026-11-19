package doccheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RuleAISpecRef 检查 AI spec 的「当前实现描述」是否仍引用已退役的迁移文件名或符号。
// 规则范围与判定口径见 specs/changes/ai-spec-reference-check/spec.md。
const RuleAISpecRef = "ai-spec-ref"

// aiSpecPattern 只匹配 AI 变更 spec；非 AI spec 不在本规则范围内。
const aiSpecPattern = "specs/changes/ai-*/spec.md"

// aiMigrationRefPattern 匹配旧号段的 AI 迁移文件名（00060–00089，.sql 可省略）。
// 这些是 T-014 让号前的旧名；现行 00900+ 号段不在本规则的存在性检查范围内。
var aiMigrationRefPattern = regexp.MustCompile(`\b(000[6-8][0-9]_ai_[a-z0-9_]+)(?:\.sql)?\b`)

// retiredAISymbols 是 T-017 已删除的 AI 手写守卫标识符。
// 出现在当前实现描述里时必须由同一行明确说明已删除/退役。
var retiredAISymbols = []string{
	"guardAIFishingTalkColumn",
	"-- ai-guard",
}

// checkAISpecRefs 扫描 AI spec 正文，按显式标记区分历史引用与当前实现描述。
func checkAISpecRefs(root string) ([]Issue, error) {
	paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(aiSpecPattern)))
	if err != nil {
		return nil, err
	}
	var issues []Issue
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", rel, err)
		}
		issues = append(issues, checkAISpecFile(root, rel, string(data))...)
	}
	return issues, nil
}

// checkAISpecFile 逐行检查一份 AI spec；围栏内的示例不参与判定。
func checkAISpecFile(root, rel, content string) []Issue {
	var issues []Issue
	section := ""
	inFence := false
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = line
		}
		if line == "" {
			continue
		}
		issues = append(issues, checkAIMigrationRefs(root, rel, i+1, raw)...)
		issues = append(issues, checkAIRetiredSymbols(rel, i+1, section, raw)...)
	}
	return issues
}

func checkAIMigrationRefs(root, rel string, line int, text string) []Issue {
	var issues []Issue
	seen := map[string]bool{}
	for _, m := range aiMigrationRefPattern.FindAllStringSubmatch(text, -1) {
		name := m[1] + ".sql"
		if seen[name] {
			continue
		}
		seen[name] = true
		if aiMigrationExists(root, name) || hasMigrationHistoryMarker(text) {
			continue
		}
		issues = append(issues, Issue{
			Rule: RuleAISpecRef,
			Path: rel,
			Line: line,
			Message: fmt.Sprintf(
				"引用了不存在的旧 AI 迁移文件 %s；历史引用须标注「历史名 → 现 …」或「已删除」", name),
		})
	}
	return issues
}

func aiMigrationExists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, "internal", "store", "migrations", name))
	return err == nil
}

// hasMigrationHistoryMarker 判断迁移名是否带明确的历史标记。
// 只写「历史」不够：必须给出当前名或「已删除」，避免读者把旧名当现行文件名。
func hasMigrationHistoryMarker(line string) bool {
	if strings.Contains(line, "已删除") {
		return true
	}
	if strings.Contains(line, "→ 现") || strings.Contains(line, "→现") {
		return true
	}
	if strings.Contains(line, "现名") || strings.Contains(line, "当前名") ||
		strings.Contains(line, "现为") || strings.Contains(line, "当前为") {
		return true
	}
	return false
}

func checkAIRetiredSymbols(rel string, line int, section, text string) []Issue {
	var issues []Issue
	for _, symbol := range retiredAISymbols {
		if !strings.Contains(text, symbol) {
			continue
		}
		// Problem Statement 是显式的「改动前」段落，允许叙述旧机制。
		if section == "## Problem Statement" {
			continue
		}
		if hasSymbolHistoryMarker(text) {
			continue
		}
		issues = append(issues, Issue{
			Rule: RuleAISpecRef,
			Path: rel,
			Line: line,
			Message: fmt.Sprintf(
				"当前实现描述引用了已退役符号 %s；历史引用须标注「已删除」/「退役」等", symbol),
		})
	}
	return issues
}

func hasSymbolHistoryMarker(line string) bool {
	for _, marker := range []string{
		"已删除", "删除", "移除", "退役", "废弃", "弃用", "不再", "此前", "原 ", "旧实现", "旧版",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}
