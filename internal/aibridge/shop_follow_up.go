package aibridge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

type shopQuote struct {
	itemID, name string
	silver, gold int64
}

func currencyPrice(cents int64, currency string) string {
	return fmt.Sprintf("%d.%02d %s", cents/100, cents%100, currency)
}

func (q *shopQuote) prices() string {
	var prices []string
	if q.silver > 0 {
		prices = append(prices, currencyPrice(q.silver, "银币"))
	}
	if q.gold > 0 {
		prices = append(prices, currencyPrice(q.gold, "金币"))
	}
	return strings.Join(prices, " / ")
}

func (q *shopQuote) answer(currency string) string {
	amount := q.silver
	if currency == "金币" {
		amount = q.gold
	}
	if amount > 0 {
		return fmt.Sprintf("%s的静态参考价是 %s，实际售价以游戏商店为准。", q.name, currencyPrice(amount, currency))
	}
	if other := q.prices(); other != "" {
		return fmt.Sprintf("%s的静态价目里没有%s报价，已记录的参考价是 %s。", q.name, currency, other)
	}
	return fmt.Sprintf("%s的静态价目里没有%s报价。", q.name, currency)
}

// Only currency-and-price follow-ups are inferred. A sentence naming a new
// item, a denomination, or asking about exchange rates goes to normal lookup.
var shopCurrencyQuestion = regexp.MustCompile(`^(?:请问)?(?:那么|那|这根|这件|这款|这个|它)?(?:(?:用)?(银币|金币)(?:要?多少(?:钱)?|价格呢?|价呢?|呢|能买吗|可以买(?:吗)?|有没有)|(?:要?多少|能用|可以用|有没有)(银币|金币)(?:买(?:吗)?|购买(?:吗)?)?)(?:吗|呢|啊|呀)?$`)

func shopFollowUpCurrency(raw string) string {
	text := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("？?！!。，,、", r) {
			return -1
		}
		return r
	}, raw)
	match := shopCurrencyQuestion.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1] + match[2]
}

// shopFocus lives with the last completed private reply in the existing
// bounded in-memory history. It is never serialized to a model provider.
type shopFocus struct {
	itemID                    string
	uid, generation, revision int64
	selectedAt                time.Time
}

const shopFocusLifetime = 5 * time.Minute

func (f *shopFocus) valid(uid int64, caps aiconfig.CapabilityState, now time.Time) bool {
	return f != nil && f.itemID != "" && f.uid == uid && f.generation == caps.Generation &&
		f.revision == caps.Revision && now.Sub(f.selectedAt) < shopFocusLifetime
}

type shopTurn struct{ focus *shopFocus }
type shopTurnKey struct{}

func shopTurnFrom(ctx context.Context) *shopTurn {
	turn, _ := ctx.Value(shopTurnKey{}).(*shopTurn)
	return turn
}

func (r *Runner) priceFollowUp(ctx context.Context, t *store.AITask, agent *store.AIAgent,
	caps aiconfig.CapabilityState, turn *shopTurn, currency string) string {
	recent := r.sessions.History(sessionKey(t), 1)
	var focus *shopFocus
	if len(recent) > 0 {
		focus = recent[0].shopFocus
	}
	if !focus.valid(t.UID, caps, time.Now()) {
		return "你想查哪件物品的" + currency + "参考价？发一下名称或型号。"
	}
	args, _ := json.Marshal(toolArgs{"query": focus.itemID})
	env := r.toolEnv(ctx, t, agent, caps, ctx)
	env.Recheck = func(ctx context.Context, name string) (bool, error) {
		view, err := r.db.AIGetCapabilities(ctx, agent.Key)
		if err != nil {
			return false, err
		}
		return focus.valid(t.UID, view.State, time.Now()) && view.State.ToolAllowed(name), nil
	}
	result, err := r.registry.Dispatch(ctx, aiconfig.ToolShopPrice, string(args), env)
	if err != nil {
		return "这件物品的报价暂时查不到，稍后再问我一次吧。"
	}
	if result.shopQuote == nil || result.shopQuote.itemID != focus.itemID {
		return "刚才那件物品在当前目录里找不到了，请再发一次名称或型号。"
	}
	turn.focus = focus
	return result.shopQuote.answer(currency)
}
