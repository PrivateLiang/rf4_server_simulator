package aiconfig

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	KnowledgeFishing       = "fishing"
	KnowledgeShop          = "shop"
	KnowledgeCraft         = "craft"
	MaxLocalKnowledgeBytes = 256 << 10
)

var ErrLocalKnowledgeInvalid = errors.New("本地资料不合法")

type LocalKnowledge struct {
	ItemAliases []ItemAlias     `json:"item_aliases"`
	Notes       []KnowledgeNote `json:"notes"`
}

type ItemAlias struct {
	Alias  string `json:"alias"`
	ItemID string `json:"item_id"`
}

type KnowledgeNote struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Keywords []string `json:"keywords"`
	Text     string   `json:"text"`
	Source   string   `json:"source"`
}

// NormalizeKnowledgeTerm matches case and full-width Latin model numbers without
// changing Chinese names or punctuation such as the dash in a product model.
func NormalizeKnowledgeTerm(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= '！' && r <= '～' {
			r -= '！' - '!'
		}
		return unicode.ToLower(r)
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func (d *LocalKnowledge) Normalize() error {
	if len(d.ItemAliases) > 1000 || len(d.Notes) > 250 {
		return fmt.Errorf("%w：最多 1000 个别名和 250 条心得", ErrLocalKnowledgeInvalid)
	}
	if d.ItemAliases == nil {
		d.ItemAliases = []ItemAlias{}
	}
	if d.Notes == nil {
		d.Notes = []KnowledgeNote{}
	}
	seen := make(map[ItemAlias]bool, len(d.ItemAliases))
	for i := range d.ItemAliases {
		a := &d.ItemAliases[i]
		a.Alias, a.ItemID = NormalizeKnowledgeTerm(a.Alias), strings.TrimSpace(a.ItemID)
		if !knowledgeTermValid(a.Alias) || !knowledgeTextValid(a.ItemID, 128) {
			return fmt.Errorf("%w：第 %d 个别名需要有效别名和物品 ID", ErrLocalKnowledgeInvalid, i+1)
		}
		if seen[*a] {
			return fmt.Errorf("%w：第 %d 个别名与已有映射重复", ErrLocalKnowledgeInvalid, i+1)
		}
		seen[*a] = true
	}
	for i := range d.Notes {
		n := &d.Notes[i]
		n.Title, n.Text, n.Source = strings.TrimSpace(n.Title), strings.TrimSpace(n.Text), strings.TrimSpace(n.Source)
		if n.Topic != KnowledgeFishing && n.Topic != KnowledgeShop && n.Topic != KnowledgeCraft {
			return fmt.Errorf("%w：第 %d 条心得的主题必须是 fishing、shop 或 craft", ErrLocalKnowledgeInvalid, i+1)
		}
		if !knowledgeTextValid(n.Title, 80) || !knowledgeTextValid(n.Text, 800) ||
			!knowledgeTextValid(n.Source, 200) || len(n.Keywords) == 0 || len(n.Keywords) > 12 {
			return fmt.Errorf("%w：第 %d 条心得需要标题、正文、来源和 1–12 个关键词", ErrLocalKnowledgeInvalid, i+1)
		}
		for j := range n.Keywords {
			n.Keywords[j] = NormalizeKnowledgeTerm(n.Keywords[j])
			if !knowledgeTermValid(n.Keywords[j]) {
				return fmt.Errorf("%w：第 %d 条心得的关键词须为 2–64 字符的文字或型号", ErrLocalKnowledgeInvalid, i+1)
			}
		}
	}
	return nil
}

func knowledgeTermValid(s string) bool {
	return utf8.RuneCountInString(s) >= 2 && knowledgeTextValid(s, 64) &&
		strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

func knowledgeTextValid(s string, maxLen int) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxLen && !strings.ContainsRune(s, 0)
}
