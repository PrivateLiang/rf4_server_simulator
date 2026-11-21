package aibridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestShopFollowUpCurrency(t *testing.T) {
	for input, want := range map[string]string{
		"银币多少": "银币", "那金币呢？": "金币", "能用银币买吗？": "银币", "这根多少银币呀": "银币",
		"银币50道具": "", "另一根蓝竿银币多少": "", "金币怎么换银币": "", "有啥参数": "",
	} {
		if got := shopFollowUpCurrency(input); got != want {
			t.Errorf("%q = %q，期望 %q", input, got, want)
		}
	}
	now := time.Now()
	caps := aiconfig.CapabilityState{Generation: 2, Revision: 4}
	focus := shopFocus{itemID: "rod_1", uid: 1, generation: 2, revision: 4, selectedAt: now}
	if !focus.valid(1, caps, now) || focus.valid(2, caps, now) ||
		focus.valid(1, caps, now.Add(shopFocusLifetime)) {
		t.Fatal("会话身份或时效没有正确绑定")
	}
	caps.Revision++
	if focus.valid(1, caps, now) {
		t.Fatal("能力修订变化后仍可沿用旧指代")
	}
}

func TestShopPriceFollowUpInPrivateQueue(t *testing.T) {
	var modelCalls atomic.Int32
	var sawInitialQuote atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		modelCalls.Add(1)
		var body ChatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		for _, m := range body.Messages {
			if strings.Contains(m.Content, "196.96 金币") {
				sawInitialQuote.Store(true)
			}
		}
		// The model intentionally never repeats the model name. The
		// follow-up must have trusted tool context independently.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"查到了，参考价请以商店为准。"}}]}`)
	}))
	defer server.Close()
	db, key := newGateTestDB(t, server.URL)
	k := toolTestKnowledge(t)
	if _, err := k.static.Exec(`
		UPDATE item_catalog SET name='Poseidon Ultra 80-100', name_zh='Poseidon Ultra 80-100' WHERE item_id='rod_1';
		INSERT INTO item_catalog VALUES('rod_2','Another Rod','另一根竿','rod',3),('silver_50','Coins 50','50 银币','currency',4);
		CREATE TABLE item_price(item_id TEXT, silver INTEGER, gold INTEGER);
		INSERT INTO item_price VALUES('rod_1',0,19696),('rod_2',8850,0),('silver_50',5000,0);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO item_catalog(item_id,name_zh) VALUES('rod_1','Poseidon Ultra 80-100'),('rod_2','另一根竿')`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := db.AISaveLocalKnowledge(ctx, 0, aiconfig.LocalKnowledge{ItemAliases: []aiconfig.ItemAlias{
		{Alias: "重技杆", ItemID: "rod_1"}, {Alias: "另一个型号", ItemID: "rod_2"},
		{Alias: "赛竿", ItemID: "rod_1"}, {Alias: "赛竿", ItemID: "rod_2"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AISaveCapabilities(ctx, "companion", 0, []string{aiconfig.CapabilityShopKnowledge}, nil); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db, k, config.AI{MaxConcurrent: 4, RequestTimeoutSec: 4}, key, logging.Discard())
	r.providerTransport = server.Client().Transport
	queue := func(uid int64, text string) int64 {
		t.Helper()
		id, err := db.AIEnqueue(ctx, &store.AITask{Kind: "dm", Scope: "dm", ScopeKey: strconv.FormatInt(uid, 10), UID: uid,
			Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	response := func(id int64, want string) {
		t.Helper()
		state, text, detail := aiResultOf(t, db, id)
		if state != "done" || !strings.Contains(text, want) {
			t.Fatalf("task %d: %s %q (%q)，期望包含 %q", id, state, text, detail, want)
		}
	}

	initial := queue(1, "重技杆子多少钱啊，有哪些参数")
	silver := queue(1, "银币多少")
	otherPlayer := queue(2, "银币多少")
	r.pump(ctx)
	response(initial, "查到了")
	response(silver, "Poseidon Ultra 80-100的静态价目里没有银币报价，已记录的参考价是 196.96 金币")
	response(otherPlayer, "你想查哪件物品")
	if modelCalls.Load() != 1 || !sawInitialQuote.Load() {
		t.Fatal("应只为原问题调一次模型并带原价目")
	}

	changed := queue(1, "另一个型号多少钱")
	changedSilver := queue(1, "银币多少")
	r.pump(ctx)
	response(changed, "查到了")
	response(changedSilver, "另一根竿的静态参考价是 88.50 银币")
	if _, err := k.static.Exec(`UPDATE item_price SET silver=9300 WHERE item_id='rod_2'`); err != nil {
		t.Fatal(err)
	}
	refreshed := queue(1, "多少银币")
	r.pump(ctx)
	response(refreshed, "93.00 银币")

	queue(1, "赛竿多少钱")
	ambiguous := queue(1, "银币多少")
	r.pump(ctx)
	response(ambiguous, "你想查哪件物品")

	if err := db.AISetSetting(ctx, "ai_profile_dm_history_messages", "0"); err != nil {
		t.Fatal(err)
	}
	queue(1, "重技杆多少钱")
	disabledHistory := queue(1, "银币多少")
	r.pump(ctx)
	response(disabledHistory, "你想查哪件物品")
}
