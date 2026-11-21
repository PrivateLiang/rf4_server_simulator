package aiconfig

import "testing"

func TestWebSearchModeAndProviderSupport(t *testing.T) {
	for _, tc := range []struct{ mode, endpoint, want string }{
		{"", "", SearchLocal}, {"", "https://search.example", SearchCustom},
		{SearchNative, "https://search.example", SearchNative},
		{SearchLocal, "https://search.example", SearchLocal},
		{"unknown", "https://search.example", SearchLocal},
	} {
		cfg := WebSearchSettings{Mode: tc.mode, Endpoint: tc.endpoint}
		if got := cfg.EffectiveMode(); got != tc.want {
			t.Errorf("%+v = %q，期望 %q", cfg, got, tc.want)
		}
	}
	for _, tc := range []struct {
		endpoint, protocol, model, auth string
		want                            bool
	}{
		{"https://api.deepseek.com", "openai_chat", "deepseek-flash", "bearer", true},
		{"https://api.deepseek.com/v1/chat/completions", "", "deepseek-flash", "", true},
		{"https://gateway.example", "openai_chat", "deepseek-flash", "", false},
		{"https://api.deepseek.com", "openai_chat", "model-x", "", false},
		{"https://api.deepseek.com:8443", "", "deepseek-flash", "", false},
		{"https://api.deepseek.com/?q=1", "", "deepseek-flash", "", false},
		{"https://api.deepseek.com", "", "deepseek-flash", "x-api-key", false},
	} {
		if got := DeepSeekNativeSearchSupported(tc.endpoint, tc.protocol, tc.model, tc.auth); got != tc.want {
			t.Errorf("%+v = %v", tc, got)
		}
	}
}
