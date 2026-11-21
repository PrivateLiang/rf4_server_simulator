package aibridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func (r *Runner) taskKnowledge(ctx context.Context, task *store.AITask, agent *store.AIAgent,
	caps aiconfig.CapabilityState, native func(context.Context) string) (source, text string) {
	env := r.toolEnv(ctx, task, agent, caps, ctx)
	if turn := shopTurnFrom(ctx); turn != nil {
		env.observeShopQuote = func(q *shopQuote) {
			turn.focus = &shopFocus{itemID: q.itemID, uid: task.UID,
				generation: caps.Generation, revision: caps.Revision, selectedAt: time.Now()}
		}
	}
	if data := r.registry.Prefetch(ctx, caps.ReadTools, task.Text, env); data != "" {
		return "本地资料", data
	}
	if !caps.Has(aiconfig.CapabilityOnlineSearch) {
		return "", ""
	}
	search, err := r.db.AIWebSearchSettings(ctx)
	if err != nil || !search.Enabled || !r.searchAuthorized(ctx, agent, caps) {
		return "", ""
	}
	switch search.EffectiveMode() {
	case aiconfig.SearchNative:
		if native != nil {
			return "厂商联网资料", native(ctx)
		}
	case aiconfig.SearchCustom:
		if r.knowledge != nil {
			return "联网搜索资料", r.knowledge.WebSearch(ctx, task.Text, search.Endpoint)
		}
	}
	return "", ""
}

func (r *Runner) searchAuthorized(ctx context.Context, agent *store.AIAgent, caps aiconfig.CapabilityState) bool {
	current := r.capabilityStateOf(ctx, agent.Key)
	return caps.Has(aiconfig.CapabilityOnlineSearch) && current.Generation == caps.Generation &&
		current.Has(aiconfig.CapabilityOnlineSearch)
}

func (k *Knowledge) localDocument(ctx context.Context) (aiconfig.LocalKnowledge, error) {
	if k == nil || k.local == nil {
		return aiconfig.LocalKnowledge{}, nil
	}
	if read, ok := ctx.Value(localDocumentKey{}).(func() (aiconfig.LocalKnowledge, error)); ok {
		return read()
	}
	return k.local(ctx)
}

type localDocumentKey struct{}

func (k *Knowledge) bindLocalDocument(ctx context.Context) context.Context {
	read := sync.OnceValues(func() (aiconfig.LocalKnowledge, error) { return k.localDocument(ctx) })
	return context.WithValue(ctx, localDocumentKey{}, read)
}

func asciiWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// termPosition matches Chinese terms in sentences and keeps a short Latin
// model code from matching a longer one.
func termPosition(text, term string, offset int) int {
	if term == "" {
		return -1
	}
	for offset < len(text) {
		i := strings.Index(text[offset:], term)
		if i < 0 {
			break
		}
		i += offset
		end := i + len(term)
		if (i == 0 || !asciiWord(term[0]) || !asciiWord(text[i-1])) &&
			(end == len(text) || !asciiWord(term[len(term)-1]) || !asciiWord(text[end])) {
			return i
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		offset = i + size
	}
	return -1
}

func aliasItemIDs(doc aiconfig.LocalKnowledge, query string) []string {
	type match struct {
		at, end int
		term    string
	}
	byTerm := make(map[string][]string)
	for _, alias := range doc.ItemAliases {
		byTerm[alias.Alias] = append(byTerm[alias.Alias], alias.ItemID)
	}
	var matches []match
	for term := range byTerm {
		for at := termPosition(query, term, 0); at >= 0; at = termPosition(query, term, at+len(term)) {
			matches = append(matches, match{at, at + len(term), term})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].at == matches[j].at {
			return matches[i].end > matches[j].end
		}
		return matches[i].at < matches[j].at
	})
	var ids []string
	coveredEnd := 0
	for _, m := range matches {
		// Prefer the more specific nested alias. Keep equal aliases for
		// different items as candidates requiring confirmation.
		if m.end <= coveredEnd {
			continue
		}
		coveredEnd = m.end
		for _, id := range byTerm[m.term] {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
			if len(ids) == 5 {
				return ids
			}
		}
	}
	return ids
}

const itemNameSQL = "COALESCE(NULLIF(name_zh,''),NULLIF(name,''),item_id)"

// exactItems tries a full official name or ID before resolving community aliases.
func exactItems(ctx context.Context, k *Knowledge, query string) (items [][2]string, matched bool, err error) {
	items, err = scanPairs(ctx, k, `SELECT item_id, `+itemNameSQL+`
		FROM item_catalog WHERE item_id=? COLLATE NOCASE ORDER BY item_id LIMIT 5`, []any{query})
	if err != nil || len(items) > 0 {
		return items, len(items) > 0, err
	}
	items, err = scanPairs(ctx, k, `SELECT item_id, `+itemNameSQL+`
		FROM item_catalog WHERE name_zh=? COLLATE NOCASE OR name=? COLLATE NOCASE
		ORDER BY item_id LIMIT 5`, []any{query, query})
	if err != nil || len(items) > 0 {
		return items, len(items) > 0, err
	}
	doc, err := k.localDocument(ctx)
	if err != nil {
		return nil, false, err
	}
	ids := aliasItemIDs(doc, query)
	for _, id := range ids {
		rows, err := scanPairs(ctx, k, `SELECT item_id, `+itemNameSQL+` FROM item_catalog WHERE item_id=?`, []any{id})
		if err != nil {
			return nil, true, err
		}
		items = append(items, rows...)
	}
	// A retired target must not accidentally resolve to a different item.
	if len(items) != len(ids) {
		return nil, true, nil
	}
	return items, len(ids) > 0, nil
}

func searchCatalogItems(ctx context.Context, k *Knowledge, query string) ([][2]string, error) {
	items, err := scanPairs(ctx, k, `SELECT item_id, `+itemNameSQL+`
		FROM item_catalog WHERE instr(lower(item_id),?)>0 OR instr(lower(name_zh),?)>0 OR instr(lower(name),?)>0
		ORDER BY item_id LIMIT 5`, []any{query, query, query})
	if err != nil || len(items) > 0 {
		return items, err
	}
	for _, kw := range keywordsOf(query) {
		rows, err := scanPairs(ctx, k, `SELECT item_id, `+itemNameSQL+`
			FROM item_catalog WHERE name_zh LIKE ? ORDER BY item_id LIMIT 5`, []any{kw})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !slices.Contains(items, row) && len(items) < 5 {
				items = append(items, row)
			}
		}
	}
	return items, nil
}

func itemCandidates(items [][2]string) string {
	if len(items) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("请确认物品型号。候选：")
	for _, item := range items {
		fmt.Fprintf(&out, "\n%s（%s）", item[1], item[0])
	}
	return out.String()
}

func toolItem(ctx context.Context, k *Knowledge, raw string) (string, error) {
	query := aiconfig.NormalizeKnowledgeTerm(raw)
	items, matched, err := exactItems(ctx, k, query)
	if err != nil {
		return "", err
	}
	if !matched {
		items, err = searchCatalogItems(ctx, k, query)
		return itemCandidates(items), err
	}
	if len(items) != 1 {
		return itemCandidates(items), nil
	}
	return fmt.Sprintf("物品：%s（ID：%s）", items[0][1], items[0][0]), nil
}

func toolShopPrice(ctx context.Context, k *Knowledge, raw string) (ToolResult, error) {
	query := aiconfig.NormalizeKnowledgeTerm(raw)
	items, matched, err := exactItems(ctx, k, query)
	if err != nil {
		return ToolResult{}, err
	}
	if !matched {
		items, err = searchCatalogItems(ctx, k, query)
		return ToolResult{Data: itemCandidates(items)}, err
	}
	if len(items) != 1 {
		return ToolResult{Data: itemCandidates(items)}, nil
	}
	item := items[0]
	quote := &shopQuote{itemID: item[0], name: item[1]}
	err = k.static.QueryRowContext(ctx, `SELECT silver, gold FROM item_price
		WHERE item_id=? AND (silver>0 OR gold>0)`, item[0]).Scan(&quote.silver, &quote.gold)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolResult{shopQuote: quote}, nil
	}
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Data: fmt.Sprintf("%s 静态参考价：%s。实际售价与库存以游戏商店为准。", quote.name, quote.prices()), shopQuote: quote}, nil
}

func toolRecipe(ctx context.Context, k *Knowledge, raw string) (string, error) {
	query := aiconfig.NormalizeKnowledgeTerm(raw)
	items, matched, err := exactItems(ctx, k, query)
	if err != nil {
		return "", err
	}
	if matched && len(items) > 1 {
		return "简称对应多个物品，请提供完整配方名称或产物型号。", nil
	}
	if matched && len(items) == 0 {
		return "", nil
	}
	id := ""
	if len(items) == 1 {
		id = items[0][0]
	}
	rows, err := scanPairs(ctx, k, `SELECT COALESCE(NULLIF(r.name,''),r.recipe_id), COALESCE(NULLIF(c.name_zh,''), r.output_id, '')
		FROM recipe r LEFT JOIN item_catalog c ON c.item_id=r.output_id
		WHERE (?<>'' AND r.output_id=?) OR (?='' AND instr(lower(r.name),?)>0)
		ORDER BY r.recipe_id LIMIT 4`, []any{id, id, id, query})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, pair := range rows {
		fmt.Fprintf(&out, "配方：%s，产物：%s\n", pair[0], pair[1])
	}
	return out.String(), nil
}

func localNotesEntry(name, topic, description string, params json.RawMessage) registryEntry {
	return registryEntry{
		spec: ToolSpec{Name: name, Description: description, Parameters: params, Kind: ToolKindRead, Route: ToolRouteLocal,
			AgentScoped: true, HandlerKey: name, TimeoutMS: 2000, MaxResultBytes: 650},
		handler: textTool(func(ctx context.Context, k *Knowledge, query string) (string, error) {
			doc, err := k.localDocument(ctx)
			if err != nil {
				return "", err
			}
			query = aiconfig.NormalizeKnowledgeTerm(query)
			var out strings.Builder
			count := 0
			for _, note := range doc.Notes {
				if note.Topic != topic || !slices.ContainsFunc(note.Keywords, func(term string) bool { return termPosition(query, term, 0) >= 0 }) {
					continue
				}
				fmt.Fprintf(&out, "本地心得《%s》（来源：%s）：%s\n", note.Title, note.Source, trimRunes(note.Text, 220))
				count++
				if count == 2 {
					break
				}
			}
			return out.String(), nil
		}),
	}
}
