package aibridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestLocalKnowledgeAliasesAndQueryOutcomes(t *testing.T) {
	k := toolTestKnowledge(t)
	if _, err := k.static.Exec(`
		INSERT INTO item_catalog VALUES('rod_2','RX-20','长鲤竿','rod',3);
		CREATE TABLE item_price(item_id TEXT, silver INTEGER, gold INTEGER);
		INSERT INTO item_price VALUES('rod_1',1234,250),('rod_2',999,0);
		CREATE TABLE recipe(recipe_id TEXT, name TEXT, output_id TEXT);
		INSERT INTO recipe VALUES('bait_recipe','面团配方','bait_dough');`); err != nil {
		t.Fatal(err)
	}
	doc := aiconfig.LocalKnowledge{ItemAliases: []aiconfig.ItemAlias{
		{Alias: "蓝竿", ItemID: "rod_1"},
		{Alias: "长蓝竿", ItemID: "rod_2"},
		{Alias: "RX-10", ItemID: "rod_1"},
		{Alias: "短称", ItemID: "rod_1"},
		{Alias: "短称", ItemID: "rod_2"},
		{Alias: "搓团", ItemID: "bait_dough"},
		{Alias: "旧竿", ItemID: "retired_rod"},
	}}
	if err := doc.Normalize(); err != nil {
		t.Fatal(err)
	}
	k.local = func(context.Context) (aiconfig.LocalKnowledge, error) { return doc, nil }
	r := mustRegistry(t, k)
	env := ToolEnv{Allowed: map[string]bool{
		aiconfig.ToolItemLookup: true, aiconfig.ToolShopPrice: true, aiconfig.ToolRecipeLookup: true,
	}}
	query := func(tool, text string) (ToolResult, error) {
		args, _ := json.Marshal(toolArgs{"query": text})
		return r.Dispatch(t.Context(), tool, string(args), env)
	}
	for _, tc := range []struct{ tool, text, status, contains, excludes string }{
		{aiconfig.ToolShopPrice, "问下ＲＸ－１０多少钱", "ok", "12.34 银币 / 2.50 金币", ""},
		{aiconfig.ToolShopPrice, "长蓝竿多少钱", "ok", "9.99 银币", "12.34"},
		{aiconfig.ToolShopPrice, "蓝竿和长蓝竿", "ok", "请确认物品型号", "银币"},
		{aiconfig.ToolShopPrice, "短称", "ok", "请确认物品型号", "银币"},
		{aiconfig.ToolShopPrice, "RX-10-pro", "not_found", "", "银币"},
		{aiconfig.ToolShopPrice, "旧竿", "not_found", "", "银币"},
		{aiconfig.ToolShopPrice, "饵料", "not_found", "", "银币"},
		{aiconfig.ToolItemLookup, "rx-20", "ok", "长鲤竿（ID：rod_2）", ""},
		{aiconfig.ToolRecipeLookup, "搓团怎么做", "ok", "面团配方", ""},
		{aiconfig.ToolRecipeLookup, "短称", "ok", "多个物品", "rod_1"},
	} {
		got, err := query(tc.tool, tc.text)
		if err != nil || got.Status != tc.status || !strings.Contains(got.Data, tc.contains) ||
			(tc.excludes != "" && strings.Contains(got.Data, tc.excludes)) {
			t.Errorf("%s %q: %+v / %v", tc.tool, tc.text, got, err)
		}
	}
	if _, err := k.static.Exec(`DROP TABLE item_price`); err != nil {
		t.Fatal(err)
	}
	result, err := query(aiconfig.ToolShopPrice, "蓝竿")
	if result.Status != "unavailable" || !errors.Is(err, ErrToolUnavailable) ||
		toolResultText(result, err) == toolResultText(ToolResult{Status: "not_found"}, nil) {
		t.Fatalf("资料库故障与未匹配没有区分：%+v / %v", result, err)
	}
}

func TestRunnerRetrievesOnlyAuthorizedKnowledge(t *testing.T) {
	ctx := t.Context()
	d := openBridgeRuntimeDB(t)
	if _, err := d.SQL.Exec(`INSERT INTO item_catalog(item_id,name_zh) VALUES('rod_1','鲤竿')`); err != nil {
		t.Fatal(err)
	}
	_, err := d.AISaveLocalKnowledge(ctx, 0, aiconfig.LocalKnowledge{
		ItemAliases: []aiconfig.ItemAlias{{Alias: "蓝竿", ItemID: "rod_1"}},
		Notes: []aiconfig.KnowledgeNote{
			{Topic: "shop", Title: "商店心得", Keywords: []string{"蓝竿"}, Text: "核对规格", Source: "门店实测"},
			{Topic: "fishing", Title: "垂钓心得", Keywords: []string{"蓝竿"}, Text: "另查鱼情", Source: "水域实测"},
			{Topic: "craft", Title: "制作心得", Keywords: []string{"蓝竿"}, Text: "留意材料", Source: "工作台实测"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var searchHits atomic.Int32
	search := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		searchHits.Add(1)
		_, _ = io.WriteString(w, `{"results":[{"title":"外部测试","snippet":"搜索参考"}]}`)
	}))
	defer search.Close()
	if err := d.AISetSettings(ctx, map[string]string{"web_search_enabled": "1", "search_endpoint": search.URL}); err != nil {
		t.Fatal(err)
	}
	k := toolTestKnowledge(t)
	r := NewRunner(d, k, config.AI{}, nil, logging.Discard())
	k.client = newEgressHTTPClient(time.Second, search.Client().Transport)
	task := &store.AITask{ID: 1, Text: "蓝竿和河鲈", IntentEpoch: d.AIEmergencyState(ctx).Epoch}
	ctx = withAITaskIntent(ctx, task)
	agent, err := d.AIAgentByKey(ctx, "companion")
	if err != nil || agent == nil {
		t.Fatalf("读取角色：%v", err)
	}

	save := func(base int64, caps ...string) aiconfig.CapabilityState {
		if caps == nil {
			caps = []string{}
		}
		st, err := d.AISaveCapabilities(ctx, agent.Key, base, caps, nil)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	noAccess := save(0)
	if _, got := r.taskKnowledge(ctx, task, agent, noAccess, nil); got != "" || searchHits.Load() != 0 {
		t.Fatalf("空能力仍取资料：%q，搜索 %d 次", got, searchHits.Load())
	}
	shop := save(1, aiconfig.CapabilityShopKnowledge)
	if src, got := r.taskKnowledge(ctx, task, agent, shop, nil); src != "本地资料" ||
		!strings.Contains(got, "鲤竿") || !strings.Contains(got, "来源：门店实测") ||
		strings.Contains(got, "水域实测") || strings.Contains(got, "工作台实测") ||
		strings.Contains(got, "[species_lookup]") || searchHits.Load() != 0 {
		t.Fatalf("商店角色资料越界或缺失：%s %q，搜索 %d 次", src, got, searchHits.Load())
	}
	shopAndWeb := save(2, aiconfig.CapabilityShopKnowledge, aiconfig.CapabilityOnlineSearch)
	if _, got := r.taskKnowledge(ctx, task, agent, shopAndWeb, nil); got == "" || searchHits.Load() != 0 {
		t.Fatalf("本地命中后仍联网：%q，搜索 %d 次", got, searchHits.Load())
	}
	task.Text = "nonexistent-model"
	if src, got := r.taskKnowledge(ctx, task, agent, shopAndWeb, nil); src != "联网搜索资料" ||
		!strings.Contains(got, "搜索参考") || searchHits.Load() != 1 {
		t.Fatalf("双授权下的无本地命中：%s %q，搜索 %d 次", src, got, searchHits.Load())
	}
	save(3)
	task.Text = "蓝竿"
	if _, got := r.taskKnowledge(ctx, task, agent, shopAndWeb, nil); got != "" || searchHits.Load() != 1 {
		t.Fatalf("原授权快照绕过撤权：%q，搜索 %d 次", got, searchHits.Load())
	}
}

func TestRunnerProviderPromptKeepsLocalKnowledgeScoped(t *testing.T) {
	requests := make(chan ChatRequest, 2)
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- req
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"收到"}}]}`)
	}))
	defer vendor.Close()
	d, master := newGateTestDB(t, vendor.URL)
	ctx := t.Context()
	doc := aiconfig.LocalKnowledge{Notes: []aiconfig.KnowledgeNote{{
		Topic: "shop", Title: "独立的本地心得", Keywords: []string{"鲈鱼"}, Text: "核对装备型号。", Source: "店内测量",
	}}}
	if _, err := d.AISaveLocalKnowledge(ctx, 0, doc); err != nil {
		t.Fatal(err)
	}
	r := t104BridgeRunner(t, d, master, vendor)
	for i, caps := range [][]string{{}, {aiconfig.CapabilityShopKnowledge}} {
		if _, err := d.AISaveCapabilities(ctx, "companion", int64(i), caps, nil); err != nil {
			t.Fatal(err)
		}
		task := t104DMTask(t, d, "鲈鱼在哪能钓？")
		r.handle(ctx, task)
		select {
		case sent := <-requests:
			gotNote := false
			for _, msg := range sent.Messages {
				if strings.Contains(msg.Content, "水域:测试湖") {
					t.Fatalf("未获垂钓授权仍发送了鱼种资料：%q", msg.Content)
				}
				if strings.Contains(msg.Content, "店内测量") {
					gotNote = true
					if msg.Role != "user" {
						t.Fatalf("本地心得提升了消息角色：%s", msg.Role)
					}
				}
			}
			if gotNote != (i == 1) {
				t.Fatalf("权限 %v 时发送心得 = %v", caps, gotNote)
			}
		default:
			t.Fatal("没收到模型请求")
		}
	}
}
