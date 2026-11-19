package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/config"
)

func TestAIBridgeStatusDisabled(t *testing.T) {
	aiProc = aiBridgeProc{}
	a := &app{cfg: &config.Config{AI: config.AI{Enabled: false}}}
	st := a.aiBridgeStatus()
	if st["state"] != "disabled" {
		t.Fatalf("state = %v", st["state"])
	}
	if st["name"] != "aibridge" {
		t.Fatalf("name = %v", st["name"])
	}
}

// /api/status 必须带 ai 字段，面板才能在首屏渲染 aibridge 卡片。
func TestStatusIncludesAI(t *testing.T) {
	aiProc = aiBridgeProc{}
	a := &app{cfg: &config.Config{AI: config.AI{Enabled: true, ObsAddr: "127.0.0.1:1"}}}
	rec := httptest.NewRecorder()
	a.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ai, ok := body["ai"].(map[string]any)
	if !ok || ai["state"] != "stopped" {
		t.Fatalf("ai = %#v", body["ai"])
	}
}

func TestProbeAIHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if !probeAIHealth(strings.TrimPrefix(ok.URL, "http://")) {
		t.Fatal("200 应视为健康")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if probeAIHealth(strings.TrimPrefix(bad.URL, "http://")) {
		t.Fatal("500 不应视为健康")
	}
	if probeAIHealth("") {
		t.Fatal("空地址不应视为健康")
	}
}
