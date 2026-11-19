package aibridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testThinkingMap = `{"thinking_map":{
  "off":{"thinking":{"type":"disabled"}},
  "medium":{"reasoning_effort":"high"},
  "max":{"reasoning_effort":"max"}
}}`

func testProvider(t *testing.T, spec ProviderSpec, capture *map[string]any, status int, body string) (*OpenAICompatProvider, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, capture)
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Bearer sk-secret") {
			t.Errorf("缺少 Bearer 鉴权头: %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	spec.BaseURL = srv.URL
	if spec.APIKey == "" {
		spec.APIKey = "sk-secret"
	}
	if spec.ModelID == "" {
		spec.ModelID = "test-model"
	}
	// Transport 注入只替换连接阶段；URL/重定向检查仍走共用实现。
	p, err := NewProviderWithTransport(spec, 2*time.Second, srv.Client().Transport)
	if err != nil {
		t.Fatalf("构造 Provider: %v", err)
	}
	return p, srv.Close
}

func TestThinkingOffSendsDisabled(t *testing.T) {
	var got map[string]any
	p, done := testProvider(t, ProviderSpec{Name: "deepseek", ConfigJSON: testThinkingMap},
		&got, 200, `{"choices":[{"message":{"content":" 你好 "}}],"usage":{"total_tokens":12}}`)
	defer done()
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "在吗"}},
		Thinking:  ThinkingConfig{Enabled: false},
		MaxTokens: 300,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != " 你好 " {
		t.Fatalf("文本 = %q", resp.Text)
	}
	if !strings.Contains(resp.RawUsage, "total_tokens") {
		t.Fatalf("raw usage 丢失: %s", resp.RawUsage)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("关闭思考未下发 disabled: %v", got["thinking"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("关闭思考不应带 reasoning_effort: %v", got)
	}
	if got["max_tokens"] != float64(300) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
}

func TestThinkingEffortUsesConfiguredMap(t *testing.T) {
	var got map[string]any
	p, done := testProvider(t, ProviderSpec{Name: "deepseek", ConfigJSON: testThinkingMap},
		&got, 200, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer done()
	if _, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "鱼在哪"}},
		Thinking: ThinkingConfig{Enabled: true, Effort: "medium"},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("medium 档映射错误: %v", got["reasoning_effort"])
	}
}

func TestProviderRejectsIncompleteConfig(t *testing.T) {
	for _, spec := range []ProviderSpec{
		{BaseURL: "https://example.invalid"},
		{BaseURL: "https://example.invalid", ModelID: "m"},
	} {
		if _, err := NewProvider(spec, time.Second); err == nil {
			t.Fatalf("缺配置应报错: %+v", spec)
		}
	}
}

func TestProviderErrorDoesNotLeakKey(t *testing.T) {
	var got map[string]any
	p, done := testProvider(t, ProviderSpec{Name: "deepseek", ConfigJSON: testThinkingMap},
		&got, 401, `{"error":"unauthorized"}`)
	defer done()
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("错误信息泄露密钥: %v", err)
	}
}

// 有些代理会把请求头回显在错误体里，错误信息里不能出现明文 Key。
func TestProviderErrorRedactsEchoedKey(t *testing.T) {
	var got map[string]any
	p, done := testProvider(t, ProviderSpec{Name: "deepseek", ConfigJSON: testThinkingMap},
		&got, 401, `{"error":"bad key sk-secret"}`)
	defer done()
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("错误信息泄露密钥: %v", err)
	}
}

func TestProviderToolCallsAndThinkingForcedOff(t *testing.T) {
	var got map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"fish_query","arguments":"{\"fish\":\"河鲈\"}"}}]}}],"usage":{"total_tokens":5}}`)
	}))
	defer srv.Close()
	p, err := NewProviderWithTransport(ProviderSpec{
		Name: "deepseek", BaseURL: srv.URL, ModelID: "m", APIKey: "sk-secret",
		ConfigJSON: testThinkingMap,
	}, 2*time.Second, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "河鲈在哪"}},
		Tools: []ToolSpec{{
			Name: "fish_query", Description: "查鱼",
			Parameters: json.RawMessage(`{"type":"object","properties":{"fish":{"type":"string"}}}`),
		}},
		Thinking:  ThinkingConfig{Enabled: true, Effort: "high"},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "fish_query" ||
		!strings.Contains(resp.ToolCalls[0].Arguments, "河鲈") {
		t.Fatalf("tool_calls 解析错误: %+v", resp.ToolCalls)
	}
	if _, ok := got["tools"]; !ok {
		t.Fatalf("请求体缺少 tools: %v", got)
	}
	if thinking, ok := got["thinking"].(map[string]any); !ok || thinking["type"] != "disabled" {
		t.Fatalf("工具轮应强制关闭思考: %v", got["thinking"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("工具轮不应带 high 思考档: %v", got)
	}
}

func TestToolCallMarshalShape(t *testing.T) {
	raw, err := json.Marshal(Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: "c1", Name: "fish_query", Arguments: `{"fish":"河鲈"}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	calls, ok := m["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 序列化错误: %s", raw)
	}
	first := calls[0].(map[string]any)
	if first["type"] != "function" || first["id"] != "c1" {
		t.Fatalf("形状错误: %v", first)
	}
	fn := first["function"].(map[string]any)
	if fn["name"] != "fish_query" {
		t.Fatalf("函数名错误: %v", fn)
	}
}
