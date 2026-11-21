package aiconfig

import (
	"net/url"
	"strings"
)

const (
	KeySearchMode           = "web_search_mode"
	KeyWebSearchEnabled     = "web_search_enabled"
	KeyNativeSearchEvidence = "ai_native_search_evidence"
	SearchLocal             = "local"
	SearchNative            = "native"
	SearchCustom            = "custom"
)

type WebSearchSettings struct {
	Enabled  bool
	Mode     string
	Endpoint string
}

func ValidSearchMode(mode string) bool {
	return mode == SearchLocal || mode == SearchNative || mode == SearchCustom
}

func (s WebSearchSettings) EffectiveMode() string {
	if ValidSearchMode(s.Mode) {
		return s.Mode
	}
	if s.Mode == "" && strings.TrimSpace(s.Endpoint) != "" {
		return SearchCustom
	}
	return SearchLocal
}

func DeepSeekNativeSearchSupported(baseURL, protocol, model, auth string) bool {
	if (protocol != "" && protocol != "openai_chat") || model != "deepseek-flash" ||
		(auth != "" && auth != "bearer") {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.deepseek.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" {
		return false
	}
	switch strings.TrimRight(u.EscapedPath(), "/") {
	case "", "/v1", "/chat/completions", "/v1/chat/completions":
		return true
	}
	return false
}

type NativeSearchEvidence struct {
	Revision    int64 `json:"revision"`
	ConfirmedAt int64 `json:"confirmed_at"`
}
