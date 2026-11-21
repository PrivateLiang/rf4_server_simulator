package aibridge

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
)

type nativeSearchTransport func(*http.Request) (*http.Response, error)

func (f nativeSearchTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRunnerNativeSearchIsolatedAndFallsBack(t *testing.T) {
	const success = `{"choices":[{"message":{"content":"资料摘要","server_tool_calls":[{"calls":[{"type":"deepseek_web_search","status":"completed","action":{"type":"search"}}]}],"annotations":[{"type":"url_citation","url_citation":{"url":"https://example.org/guide"}}]}}]}`
	const noSearch = `{"choices":[{"message":{"content":"资料摘要","server_tool_calls":[{"calls":[{"type":"deepseek_web_search","status":"failed","action":{"type":"search"}}]}]}}]}`
	for _, tc := range []struct {
		name, host, mode, response                              string
		enabled, roleAllowed, localHit, wantCall, wantReference bool
	}{
		{"confirmed", "api.deepseek.com", "native", success, true, true, false, true, true},
		{"v1 connection", "api.deepseek.com/v1", "native", success, true, true, false, true, true},
		{"no proof", "api.deepseek.com", "native", noSearch, true, true, false, true, false},
		{"embedded error", "api.deepseek.com", "native", `{"error":{"message":"unavailable"}}`, true, true, false, true, false},
		{"turned off in flight", "api.deepseek.com", "native", success, true, true, false, true, false},
		{"unknown gateway", "compatible.example", "native", "", true, true, false, false, false},
		{"global off", "api.deepseek.com", "native", "", false, true, false, false, false},
		{"role denied", "api.deepseek.com", "native", "", true, false, false, false, false},
		{"local first", "api.deepseek.com", "native", "", true, true, true, false, false},
		{"local only", "api.deepseek.com", "local", "", true, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d, key := newGateTestDB(t, "https://"+tc.host)
			if _, err := d.SQL.Exec(`UPDATE ai_model SET model_id='deepseek-flash' WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
				t.Fatal(err)
			}
			on := "0"
			if tc.enabled {
				on = "1"
			}
			if err := d.AISetSettings(ctx, map[string]string{
				aiconfig.KeyWebSearchEnabled: on, aiconfig.KeySearchMode: tc.mode, aiconfig.KeySearchEndpoint: "https://custom.example/search",
			}); err != nil {
				t.Fatal(err)
			}
			caps := []string{aiconfig.CapabilityFishingKnowledge}
			if tc.roleAllowed {
				caps = append(caps, aiconfig.CapabilityOnlineSearch)
			}
			if _, err := d.AISaveCapabilities(ctx, "companion", 0, caps, nil); err != nil {
				t.Fatal(err)
			}
			k := bridgeStaticKnowledge(t)
			r := NewRunner(d, k, config.AI{RequestTimeoutSec: 5}, key, logging.Discard())
			var native, ordinary, external int
			var hasReference bool
			transport := nativeSearchTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "custom.example" {
					external++
				}
				var body struct {
					Tools []struct {
						Type     string
						Function struct{ Name string }
					}
					Messages []Message
					Thinking map[string]string
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				raw := `{"choices":[{"message":{"content":"正常回复","annotations":{"vendor_specific":true},"server_tool_calls":"vendor-specific"}}]}`
				if len(body.Tools) > 0 && body.Tools[0].Type == "deepseek_web_search" {
					native++
					raw = tc.response
					if tc.name == "turned off in flight" {
						if err := d.AISetSetting(ctx, aiconfig.KeyWebSearchEnabled, "0"); err != nil {
							t.Fatal(err)
						}
					}
					if len(body.Tools) != 1 || len(body.Messages) != 2 || body.Thinking["type"] != "disabled" ||
						strings.Contains(body.Messages[1].Content, "123456789") || req.URL.Path != "/chat/completions" ||
						req.Header.Get("Authorization") != "Bearer sk-synthetic-gate" {
						t.Errorf("搜索请求不符合隔离约定：%+v", body)
					}
				} else {
					ordinary++
					if len(body.Tools) == 0 || body.Tools[0].Type != "function" {
						t.Errorf("普通对话丢失游戏工具：%+v", body.Tools)
					}
					for _, m := range body.Messages {
						if strings.Contains(m.Content, "资料摘要") && strings.Contains(m.Content, "https://example.org/guide") {
							hasReference = true
						}
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
			})
			r.providerTransport = transport
			k.client = newEgressHTTPClient(0, transport)
			question := "社区新说法 123456789"
			if tc.localHit {
				question = "鲈鱼"
			}
			task := t104DMTask(t, d, question)
			r.handle(ctx, task)
			state, reply, detail := aiResultOf(t, d, task.ID)
			if state != "done" || reply != "正常回复" || ordinary != 1 || external != 0 ||
				(native == 1) != tc.wantCall || native > 1 || hasReference != tc.wantReference {
				t.Fatalf("对话 %s %q %q，联网 %d，普通 %d，自定义 %d，带资料 %v", state, reply, detail, native, ordinary, external, hasReference)
			}
		})
	}
}
