package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestAILocalKnowledgeHTTP(t *testing.T) {
	db := testDB(t)
	if _, err := db.SQL.Exec(`INSERT OR REPLACE INTO item_catalog(item_id,name_zh) VALUES('rod_test','测试竿')`); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := signAdminToken(t, srv)
	const route = "/api/ai/local-knowledge"
	if code, _ := capabilityRequest(t, ts, "", "GET", route, ""); code != http.StatusUnauthorized {
		t.Fatalf("匿名读取本地资料：%d", code)
	}
	read := func() store.AILocalKnowledgeSnapshot {
		code, body := capabilityRequest(t, ts, token, "GET", route, "")
		var snap store.AILocalKnowledgeSnapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil || code != http.StatusOK {
			t.Fatalf("读取：%d %s", code, body)
		}
		return snap
	}
	if got := read(); got.Revision != 0 || got.Document.ItemAliases == nil || got.Document.Notes == nil {
		t.Fatalf("空资料回执：%+v", got)
	}
	const valid = `{"base_revision":0,"document":{"item_aliases":[{"alias":"ＲＸ－１０","item_id":"rod_test"}],"notes":[{"topic":"shop","title":"测试心得","keywords":["ＲＸ－１０"],"source":"本地实测","text":"请核对型号"}]}}`
	if code, body := capabilityRequest(t, ts, token, "POST", route, valid); code != http.StatusOK {
		t.Fatalf("保存：%d %s", code, body)
	}
	for _, tc := range []struct {
		request string
		code    int
	}{
		{valid, http.StatusConflict},
		{`{"base_revision":1,"document":{"item_aliases":[{"alias":"测试竿","item_id":"missing_id"}]}}`, http.StatusBadRequest},
		{`{"base_revision":1,"document":{"notes":[{"topic":"wrong","title":"测试","keywords":["测试"],"source":"实测","text":"测试"}]}}`, http.StatusBadRequest},
		{`{"base_revision":1,"document":{}}{}`, http.StatusBadRequest},
	} {
		if code, body := capabilityRequest(t, ts, token, "POST", route, tc.request); code != tc.code {
			t.Fatalf("请求 %s：%d %s，期望 %d", tc.request, code, body, tc.code)
		}
	}
	got := read()
	if got.Revision != 1 || len(got.Document.ItemAliases) != 1 || got.Document.ItemAliases[0].Alias != "rx-10" ||
		len(got.Document.Notes) != 1 || got.Document.Notes[0].Keywords[0] != "rx-10" {
		t.Fatalf("冲突或非法输入更改了已保存资料：%+v", got)
	}
}

func TestAIWebSearchModeHTTP(t *testing.T) {
	ts, token := capabilityServer(t)
	save := func(raw string, want int) {
		t.Helper()
		if code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/settings", raw); code != want {
			t.Fatalf("%s 保存状态 %d：%s", raw, code, body)
		}
	}
	mode := func() string {
		t.Helper()
		code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/status", "")
		var got struct {
			Search struct {
				Mode string `json:"mode"`
			} `json:"web_search"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != http.StatusOK {
			t.Fatalf("读取状态 %d：%s", code, body)
		}
		return got.Search.Mode
	}
	save(`{"search_endpoint":"https://search.example.com/query"}`, http.StatusOK)
	if got := mode(); got != "custom" {
		t.Fatalf("旧配置模式 = %s", got)
	}
	save(`{"web_search_mode":"native","web_search_enabled":true}`, http.StatusOK)
	save(`{"web_search_mode":"broken","web_search_enabled":false}`, http.StatusBadRequest)
	if got := mode(); got != "native" {
		t.Fatalf("非法设置覆盖了原生模式：%s", got)
	}
}

func TestNativeSearchEvidenceKeepsCurrentRevision(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	for _, evidence := range []aiconfig.NativeSearchEvidence{
		{Revision: 4, ConfirmedAt: 100},
		{Revision: 3, ConfirmedAt: 200},
		{Revision: 4, ConfirmedAt: 90},
		{Revision: 4, ConfirmedAt: 110},
	} {
		if err := db.AIRecordNativeSearch(ctx, evidence); err != nil {
			t.Fatal(err)
		}
	}
	var got aiconfig.NativeSearchEvidence
	if err := json.Unmarshal([]byte(db.AISetting(ctx, aiconfig.KeyNativeSearchEvidence, "")), &got); err != nil ||
		got.Revision != 4 || got.ConfirmedAt != 110 {
		t.Fatalf("最近确认 = %+v / %v", got, err)
	}
}
