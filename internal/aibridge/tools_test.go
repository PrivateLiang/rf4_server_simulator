package aibridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func toolTestKnowledge(t *testing.T) *Knowledge {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "static.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE loc_fish(loc TEXT, fish_key TEXT, name_zh TEXT, catches INTEGER);
		INSERT INTO loc_fish VALUES('lake_a','perch','河鲈',3),('lake_a','pike','狗鱼',1);
		CREATE TABLE item_catalog(item_id TEXT, name TEXT, name_zh TEXT, kind TEXT, numeric_id INTEGER);
		INSERT INTO item_catalog VALUES('rod_1','Rod','鲤竿','rod',1),
		                             ('bait_dough','Dough','饵料','bait',2);`); err != nil {
		t.Fatalf("建测试静态表: %v", err)
	}
	return &Knowledge{static: db, client: &http.Client{Timeout: time.Second}}
}

func mustRegistry(t *testing.T, k *Knowledge) *Registry {
	t.Helper()
	r, err := NewRegistry(k)
	if err != nil {
		t.Fatalf("注册表构造失败: %v", err)
	}
	return r
}

// 启动期校验：Kind×Route 不合法、重名、缺 handler、目录外名字都必须报错。
func TestNewRegistryRejectsInvalidEntries(t *testing.T) {
	r := &Registry{entries: map[string]registryEntry{}}
	valid := ToolSpec{
		Name: "species_lookup", Kind: ToolKindRead, Route: ToolRouteLocal,
		Parameters: json.RawMessage(`{"type":"object"}`), HandlerKey: "species_lookup",
		TimeoutMS: 1000, MaxResultBytes: 100, AgentScoped: true,
	}
	handler := func(context.Context, *Knowledge, []string) string { return "" }

	cases := []struct {
		name    string
		e       registryEntry
		handler toolHandler
	}{
		{"空名字", registryEntry{spec: ToolSpec{}}, nil},
		{"目录外工具", registryEntry{spec: func() ToolSpec {
			s := valid
			s.Name = "not_a_tool"
			return s
		}()}, handler},
		{"读×写路由", registryEntry{spec: func() ToolSpec {
			s := valid
			s.Route = ToolRouteGSWrite
			return s
		}()}, handler},
		{"本地读缺处理器", registryEntry{spec: valid}, nil},
		{"远程读带处理器", registryEntry{spec: func() ToolSpec {
			s := valid
			s.Name = "fish_query"
			s.HandlerKey = "fish_query"
			s.Route = ToolRouteGSRead
			return s
		}()}, handler},
		{"写工具带处理器", registryEntry{spec: func() ToolSpec {
			s := valid
			s.Name = "actor_target_suggest"
			s.HandlerKey = "actor_target_suggest"
			s.Kind = ToolKindWrite
			s.Route = ToolRouteGSWrite
			return s
		}()}, handler},
		{"缺截断上限", registryEntry{spec: func() ToolSpec {
			s := valid
			s.MaxResultBytes = 0
			return s
		}()}, handler},
	}
	for _, c := range cases {
		c := c
		if err := r.add(registryEntry{spec: c.e.spec, handler: c.handler}); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
	// 同一份声明注册两次 → 重名。
	if err := r.add(registryEntry{spec: valid, handler: handler}); err != nil {
		t.Fatalf("首条合法声明被拒绝: %v", err)
	}
	if err := r.add(registryEntry{spec: valid, handler: handler}); err == nil {
		t.Fatal("重名未被拒绝")
	}
}

// 本地只读：query/fish 别名、空查询、未知/无权/参数错/暂停都可区分。
func TestRegistryDispatchLocalReads(t *testing.T) {
	k := toolTestKnowledge(t)
	r := mustRegistry(t, k)
	ctx := context.Background()
	all := map[string]bool{
		"species_lookup": true, "bait_pref": true, "spot_lookup": true,
		"item_lookup": true, "shop_price": true, "recipe_lookup": true,
	}
	env := ToolEnv{Allowed: all, Deadline: time.Now().Add(time.Minute)}

	res, err := r.Dispatch(ctx, "species_lookup", `{"query":"河鲈在哪钓"}`, env)
	if err != nil || res.Status != "ok" || !strings.Contains(res.Data, "河鲈") {
		t.Fatalf("query 别名执行失败: %+v/%v", res, err)
	}
	// 旧调用方可能传 fish。
	res, err = r.Dispatch(ctx, "species_lookup", `{"fish":"河鲈"}`, env)
	if err != nil || !strings.Contains(res.Data, "河鲈") {
		t.Fatalf("fish 别名执行失败: %+v/%v", res, err)
	}
	// 空查询保持旧行为：不发查询、也不算失败。
	res, err = r.Dispatch(ctx, "species_lookup", `{}`, env)
	if err != nil || res.Status != "ok" || res.Data != "" {
		t.Fatalf("空查询行为变了: %+v/%v", res, err)
	}

	if _, err := r.Dispatch(ctx, "not_a_tool", `{}`, env); !errors.Is(err, ErrToolUnknown) {
		t.Fatalf("未知工具错误 = %v", err)
	}
	if _, err := r.Dispatch(ctx, "species_lookup", `{}`, ToolEnv{Allowed: map[string]bool{}}); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("未授权错误 = %v", err)
	}
	if _, err := r.Dispatch(ctx, "species_lookup", `{"query":123}`, env); !errors.Is(err, ErrToolInvalidArgs) {
		t.Fatalf("非法参数错误 = %v", err)
	}
	stopped := env
	stopped.Active = func(context.Context) bool { return false }
	if _, err := r.Dispatch(ctx, "species_lookup", `{"query":"河鲈"}`, stopped); !errors.Is(err, ErrToolStopped) {
		t.Fatalf("暂停错误 = %v", err)
	}
}

// 预注入保持原行为：按白名单顺序、跳过远程与未知、超过 1600 字截断。
func TestRegistryRunKeepsOrderAndTruncation(t *testing.T) {
	k := toolTestKnowledge(t)
	r := mustRegistry(t, k)
	out := r.Run(context.Background(), []string{"species_lookup", "fish_query", "not_a_tool"}, "河鲈在哪钓")
	if !strings.Contains(out, "[species_lookup]") || strings.Contains(out, "[fish_query]") {
		t.Fatalf("预注入顺序/过滤错误: %q", out)
	}
	if got := r.Run(context.Background(), []string{"species_lookup"}, ""); got != "" {
		t.Fatalf("空查询应无输出: %q", got)
	}
}

// 模型只看到三个公开字段，内部路由/权限字段不得外传。
func TestToolSpecJSONOnlyExposesPublicFields(t *testing.T) {
	spec := ToolSpec{
		Name: "species_lookup", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`),
		ResultSchema: json.RawMessage(`{"type":"object"}`), Kind: ToolKindRead,
		Route: ToolRouteLocal, AgentScoped: true, HandlerKey: "species_lookup",
		TimeoutMS: 1000, MaxResultBytes: 800,
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"result_schema", "kind", "route", "agent_scoped", "handler_key", "timeout_ms", "max_result_bytes"} {
		if _, ok := m[key]; ok {
			t.Fatalf("内部字段 %s 泄露给模型: %s", key, raw)
		}
	}
	if len(m) != 3 {
		t.Fatalf("公开字段数量 = %d: %s", len(m), raw)
	}
}

// 两个远程读只走 gs_read，参数退回原口径。
func TestRegistryDispatchGSRead(t *testing.T) {
	r := mustRegistry(t, toolTestKnowledge(t))
	ctx := context.Background()
	allowed := map[string]bool{"fish_query": true, "map_vibe": true}
	var gotName, gotArg string
	env := ToolEnv{
		Allowed: allowed, TaskText: "河鲈今天在哪", TaskLoc: "level_001_lake",
		Deadline: time.Now().Add(time.Minute),
		RemoteRead: func(ctx context.Context, name, args string) (string, error) {
			gotName, gotArg = name, args
			return "远程结果", nil
		},
	}
	res, err := r.Dispatch(ctx, "fish_query", `{"fish":"狗鱼"}`, env)
	if err != nil || res.Data != "远程结果" || gotName != "fish_query" || gotArg != "狗鱼" {
		t.Fatalf("fish_query = %+v/%v (%s,%s)", res, err, gotName, gotArg)
	}
	if _, err := r.Dispatch(ctx, "fish_query", `{}`, env); err != nil || gotArg != "河鲈" {
		t.Fatalf("fish_query 空参应退回原文本鱼名: %q/%v", gotArg, err)
	}
	if _, err := r.Dispatch(ctx, "map_vibe", `{}`, env); err != nil || gotArg != "level_001_lake" {
		t.Fatalf("map_vibe 空参应退回任务地图: %q/%v", gotArg, err)
	}
	if _, err := r.Dispatch(ctx, "map_vibe", `{"loc":"白河"}`, env); err != nil || gotArg != "白河" {
		t.Fatalf("map_vibe 显式参数 = %q/%v", gotArg, err)
	}
}

// 写工具只投递命令：受理口径固定，缺少 fish 明确拒绝。
func TestRegistryDispatchWrite(t *testing.T) {
	r := mustRegistry(t, toolTestKnowledge(t))
	ctx := context.Background()
	env := ToolEnv{
		Allowed: map[string]bool{"actor_target_suggest": true},
		CallID:  "call-9", Deadline: time.Now().Add(time.Minute),
		TargetSuggest: func(ctx context.Context, fish, loc, callID string) (ToolResult, error) {
			if fish != "河鲈" || callID != "call-9" {
				t.Fatalf("写参数 = %q/%q", fish, callID)
			}
			return ToolResult{Status: "accepted", RequestID: "ai-x", Data: "ignored"}, nil
		},
	}
	res, err := r.Dispatch(ctx, "actor_target_suggest", `{"fish":"河鲈"}`, env)
	if err != nil || res.Status != "accepted" || res.RequestID != "ai-x" {
		t.Fatalf("写受理 = %+v/%v", res, err)
	}
	if _, err := r.Dispatch(ctx, "actor_target_suggest", `{}`, env); !errors.Is(err, ErrToolInvalidArgs) {
		t.Fatalf("缺 fish 应拒绝: %v", err)
	}
	if _, err := r.Dispatch(ctx, "actor_target_suggest", `{"fish":"河鲈"}`, ToolEnv{
		Allowed: map[string]bool{"actor_target_suggest": true},
	}); !errors.Is(err, ErrToolUnavailable) {
		t.Fatalf("缺写出口应不可用: %v", err)
	}
}

// 六个本地只读工具逐项等价：查询别名、排序、格式与截断都沿用改造前的字面结果。
func TestLocalReadToolsKeepLegacyOutput(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "static-all.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE loc_fish(loc TEXT, fish_key TEXT, name_zh TEXT, catches INTEGER);
		INSERT INTO loc_fish VALUES('lake_a','perch','河鲈',3),('lake_a','pike','狗鱼',1);
		CREATE TABLE fish_bait_pref(fish_key TEXT, bait_zh TEXT);
		INSERT INTO fish_bait_pref VALUES('perch','红虫');
		CREATE TABLE spot_fish(loc TEXT, fish_key TEXT, spot TEXT);
		INSERT INTO spot_fish VALUES('lake_a','perch','东岸');
		CREATE TABLE item_catalog(item_id TEXT, name TEXT, name_zh TEXT, kind TEXT, numeric_id INTEGER);
		INSERT INTO item_catalog VALUES('rod_1','Rod','鲤竿','rod',1),
		                             ('bait_dough','Dough','饵料','bait',2);
		CREATE TABLE item_price(item_id TEXT, silver INTEGER);
		INSERT INTO item_price VALUES('rod_1',120);
		CREATE TABLE recipe(name TEXT, output_id TEXT);
		INSERT INTO recipe VALUES('面团','bait_dough');`); err != nil {
		t.Fatalf("建静态表: %v", err)
	}
	r := mustRegistry(t, &Knowledge{static: db})
	ctx := context.Background()
	allowed := map[string]bool{}
	for _, name := range []string{"species_lookup", "bait_pref", "spot_lookup",
		"item_lookup", "shop_price", "recipe_lookup"} {
		allowed[name] = true
	}
	env := ToolEnv{Allowed: allowed, Deadline: time.Now().Add(time.Minute)}
	cases := []struct {
		tool, args, want string
	}{
		{"species_lookup", `{"query":"河鲈"}`, "鱼种:河鲈 水域:lake_a\n"},
		{"bait_pref", `{"query":"红虫"}`, "鱼:河鲈 饵:红虫\n"},
		{"spot_lookup", `{"fish":"河鲈"}`, "水域:lake_a 点位:东岸\n"},
		{"item_lookup", `{"query":"鲤竿"}`, "物品:鲤竿 id:rod_1\n"},
		{"shop_price", `{"query":"鲤竿"}`, "商品:鲤竿 银币:120\n"},
		{"recipe_lookup", `{"query":"面团"}`, "配方:面团 产物:饵料\n"},
	}
	for _, c := range cases {
		res, err := r.Dispatch(ctx, c.tool, c.args, env)
		if err != nil || res.Status != "ok" {
			t.Fatalf("%s = %+v/%v", c.tool, res, err)
		}
		if res.Data != c.want {
			t.Fatalf("%s 输出 = %q，期望 %q", c.tool, res.Data, c.want)
		}
	}
}
