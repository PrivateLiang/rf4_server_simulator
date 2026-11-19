package aiconfig

import (
	"strings"
	"testing"
)

// 这些 JSON 是手写字面量，代表「独立真值」：不由被测构造函数生成，
// 避免用实现验证实现。目的地用 .invalid，测试不发起任何网络请求。
const (
	testDestJSON = `{"base_url":"https://vendor.invalid/v1","model_id":"model-a"}`

	grantedOutboundJSON = `{"schema":1,"item":"outbound","state":"granted","subject":"admin","ts":1700000000,` +
		`"scope":{"content_version":1,"contents":["chat_text","player_identity","fishing_records","memory_summary"],` +
		`"dest_base_url":"https://vendor.invalid/v1","dest_model":"model-a"}}`

	revokedOutboundJSON = `{"schema":1,"item":"outbound","state":"revoked","subject":"admin","ts":1700000100,` +
		`"scope":{"content_version":1,"contents":["chat_text","player_identity","fishing_records","memory_summary"],` +
		`"dest_base_url":"https://vendor.invalid/v1","dest_model":"model-a"}}`

	grantedActorJSON = `{"schema":1,"item":"actor","state":"granted","subject":"admin","ts":1700000000,` +
		`"scope":{"content_version":1,"contents":["actor_local"],"dest_base_url":"","dest_model":""}}`

	grantedCatchTalkJSON = `{"schema":1,"item":"catch_talk","state":"granted","subject":"admin","ts":1700000000,` +
		`"scope":{"content_version":1,"contents":["fishing_records"],` +
		`"dest_base_url":"https://vendor.invalid/v1","dest_model":"model-a"}}`
)

func consentLookup(extra map[string]string) Lookup {
	values := map[string]string{
		KeyOutboundEnabled:   "1",
		KeyEgressDestination: testDestJSON,
	}
	for k, v := range extra {
		values[k] = v
	}
	return func(key string) string { return values[key] }
}

// TestConsentRecordsStillParseForCompat 旧 confirm/revoke 兼容路径仍能读写旧格式；
// 这些记录只用于展示与历史，不改变 V2 的运行资格。
func TestConsentRecordsStillParseForCompat(t *testing.T) {
	lookup := consentLookup(map[string]string{KeyConsentOutbound: grantedOutboundJSON})
	if got := EvaluateConsent(lookup, ConsentItemOutbound); !got.Valid || got.State != ConsentStateGranted {
		t.Fatalf("合法 outbound 记录应有效，得到 (%v,%q)", got.Valid, got.State)
	}
	if got := Egress(lookup); !got.Allowed {
		t.Fatal("consent 不再参与放行，急停未触发时必须放行")
	}
	cases := []struct {
		name      string
		values    map[string]string
		wantState string
	}{
		{"无记录", nil, ConsentStateMissing},
		{"空串记录", map[string]string{KeyConsentOutbound: " "}, ConsentStateMissing},
		{"裸1伪造", map[string]string{KeyConsentOutbound: "1"}, ConsentStateInvalid},
		{"非法JSON", map[string]string{KeyConsentOutbound: `{granted:true`}, ConsentStateInvalid},
		{"schema不符", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `"schema":1`, `"schema":2`, 1)}, ConsentStateInvalid},
		{"item不符", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `"item":"outbound"`, `"item":"actor"`, 1)}, ConsentStateInvalid},
		{"主体为空", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `"subject":"admin"`, `"subject":""`, 1)}, ConsentStateInvalid},
		{"时间非法", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `"ts":1700000000`, `"ts":0`, 1)}, ConsentStateInvalid},
		{"已撤销", map[string]string{KeyConsentOutbound: revokedOutboundJSON}, ConsentStateRevoked},
		{"内容版本变化", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `"content_version":1`, `"content_version":2`, 1)}, ConsentStateScopeChanged},
		{"内容范围缩小", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `["chat_text","player_identity","fishing_records","memory_summary"]`, `["chat_text"]`, 1)}, ConsentStateScopeChanged},
		{"目的地变化", map[string]string{KeyConsentOutbound: strings.Replace(grantedOutboundJSON, `dest_model":"model-a"`, `dest_model":"model-b"`, 1)}, ConsentStateScopeChanged},
		{"目的地快照缺失", map[string]string{KeyConsentOutbound: grantedOutboundJSON, KeyEgressDestination: ""}, ConsentStateScopeChanged},
		{"目的地快照非法", map[string]string{KeyConsentOutbound: grantedOutboundJSON, KeyEgressDestination: `{"base_url":"","model_id":""}`}, ConsentStateScopeChanged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EvaluateConsent(consentLookup(c.values), ConsentItemOutbound); got.State != c.wantState {
				t.Fatalf("State = %q，期望 %q", got.State, c.wantState)
			}
			// 无论记录状态如何，V2 的放行只由急停决定。
			if got := Egress(consentLookup(c.values)); !got.Allowed {
				t.Fatalf("旧授权状态不得阻止发送：%+v", got)
			}
		})
	}
}

// TestConsentItemsAreIndependent 四项分别记录的能力保留；不再互相影响外发。
func TestConsentItemsAreIndependent(t *testing.T) {
	lookup := consentLookup(map[string]string{KeyConsentActor: grantedActorJSON})
	if got := Egress(lookup); !got.Allowed {
		t.Fatalf("只有 actor 记录时也必须放行，得到 (%v,%q)", got.Allowed, got.State)
	}
	actor := EvaluateConsent(lookup, ConsentItemActor)
	if !actor.Valid || actor.State != ConsentStateGranted {
		t.Fatalf("actor 记录自身应有效，得到 (%v,%q)", actor.Valid, actor.State)
	}
	if actor.Scope != nil && (actor.Scope.DestBaseURL != "" || actor.Scope.DestModel != "") {
		t.Fatalf("actor 是本地意图，范围里不应绑定目的地: %+v", actor.Scope)
	}
	lookup = consentLookup(map[string]string{KeyConsentCatchTalk: grantedCatchTalkJSON})
	if got := EvaluateConsent(lookup, ConsentItemCatchTalk); !got.Valid {
		t.Fatalf("catch_talk 记录自身应有效，得到 (%v,%q)", got.Valid, got.State)
	}
}

// TestConsentScopeMatches 确认接口的幂等判断依赖这个纯函数。
func TestConsentScopeMatches(t *testing.T) {
	dest := ConsentDestination{BaseURL: "https://vendor.invalid/v1", ModelID: "model-a"}
	rec, ok := NewConsentRecord(ConsentItemOutbound, "admin", 1700000000, dest)
	if !ok {
		t.Fatal("NewConsentRecord 拒绝合法项")
	}
	if !ConsentScopeMatches(rec.Scope, ConsentItemOutbound, dest) {
		t.Fatal("相同范围应匹配")
	}
	other := ConsentDestination{BaseURL: dest.BaseURL, ModelID: "model-b"}
	if ConsentScopeMatches(rec.Scope, ConsentItemOutbound, other) {
		t.Fatal("目的地变化后不应匹配")
	}
	if _, ok := NewConsentRecord("no_such_item", "admin", 1, dest); ok {
		t.Fatal("未知授权项不应生成记录")
	}
	if _, ok := ConsentKeyFor("no_such_item"); ok {
		t.Fatal("未知授权项不应有键")
	}
}

// TestSecurityConsentViewCompat 兼容视图仍四项齐全；egress 只反映急停。
func TestSecurityConsentViewCompat(t *testing.T) {
	view := Security(consentLookup(map[string]string{KeyConsentOutbound: grantedOutboundJSON}))
	if !view.Egress.Allowed {
		t.Fatal("没有急停时 egress.allowed 应为 true")
	}
	if len(view.Consent.Items) != len(ConsentItemSpecs) {
		t.Fatalf("授权项数量 = %d，期望 %d", len(view.Consent.Items), len(ConsentItemSpecs))
	}
	seen := map[string]bool{}
	for _, item := range view.Consent.Items {
		seen[item.Item] = true
		if len(item.Contents) == 0 {
			t.Errorf("%s 缺少内容范围", item.Item)
		}
	}
	for _, want := range []string{ConsentItemOutbound, ConsentItemActor, ConsentItemCatchTalk, ConsentItemMemoryInject} {
		if !seen[want] {
			t.Errorf("security.consent 缺少 %s", want)
		}
	}
	stop := Security(consentLookup(map[string]string{KeyEmergencyStop: "1"}))
	if stop.Egress.Allowed || stop.Egress.State != StateEmergency {
		t.Fatalf("急停时 egress = (%v,%q)，期望 (false,emergency)", stop.Egress.Allowed, stop.Egress.State)
	}
}

// TestDestinationURLValidation 是保存侧的目的地口径：
// 只接受 http(s)、拒绝 userinfo 与密钥类查询参数；展示与记录只用域，不回显查询串。
func TestDestinationURLValidation(t *testing.T) {
	okCases := []struct {
		raw         string
		wantDisplay string
		wantHost    string
	}{
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1", "api.deepseek.com"},
		{"https://API.Example.com:8443/v1?api-version=2024-01-01", "https://API.Example.com:8443/v1", "api.example.com:8443"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1", "127.0.0.1:8080"},
		{" https://api.example.com ", "https://api.example.com", "api.example.com"},
	}
	for _, c := range okCases {
		normalized, display, err := ValidateDestinationURL(c.raw)
		if err != nil {
			t.Fatalf("%q 应通过校验: %v", c.raw, err)
		}
		if normalized != strings.TrimSpace(c.raw) {
			t.Errorf("%q normalized = %q", c.raw, normalized)
		}
		if display != c.wantDisplay {
			t.Errorf("%q display = %q，期望 %q", c.raw, display, c.wantDisplay)
		}
		if got := DestinationHost(c.raw); got != c.wantHost {
			t.Errorf("%q host = %q，期望 %q", c.raw, got, c.wantHost)
		}
	}
	bad := []string{
		"ftp://api.example.com/v1",
		"https://user:pass@api.example.com/v1",
		"https://api.example.com/v1?api_key=sk-secret",
		"https://api.example.com/v1?TOKEN=abc",
		"not a url",
		"",
	}
	for _, raw := range bad {
		normalized, display, err := ValidateDestinationURL(raw)
		if raw == "" {
			if err != nil || normalized != "" || display != "" {
				t.Errorf("空地址应视为「未填写」而不是错误: (%q,%q,%v)", normalized, display, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q 应被拒绝", raw)
		}
	}
	if RedactURLForDisplay("https://api.example.com/v1?key=sk-secret#frag") != "https://api.example.com/v1" {
		t.Error("展示地址必须去掉查询串与片段")
	}
	if RedactURLForDisplay("not a url") != "" {
		t.Error("非法地址不得回显任何内容")
	}
	if !SameDestinationOrigin("https://api.example.com/v1", "https://API.example.com:443/v2/x") {
		t.Error("同 scheme/主机/有效端口应视为同源")
	}
	if SameDestinationOrigin("https://api.example.com/v1", "https://api.example.com:8443/v1") {
		t.Error("端口不同不是同源")
	}
	if SameDestinationOrigin("https://api.example.com/v1", "http://api.example.com/v1") {
		t.Error("scheme 不同不是同源")
	}
}

// TestProtectedConfigKeys 通用设置保存必须拒收授权与发布元数据。
func TestProtectedConfigKeys(t *testing.T) {
	protected := []string{
		KeyConsentOutbound, KeyConsentActor, KeyConsentCatchTalk, KeyConsentMemoryInject,
		KeyEgressDestination, KeyConfigActive, KeyConfigRetest, KeyEmergencyStopLiftedTS,
		ConfigRecordKey(7), SearchRecordKey(7),
	}
	for _, key := range protected {
		if !IsProtectedConfigKey(key) {
			t.Errorf("%q 应受保护", key)
		}
	}
	for _, key := range []string{KeyEmergencyStop, KeySearchEndpoint, KeyActorEnabled, "web_search_enabled"} {
		if IsProtectedConfigKey(key) {
			t.Errorf("%q 不应被当成发布元数据", key)
		}
	}
}

// TestActiveConfigRoundTrip 是活动指针的序列化边界：损坏记录一律 ok=false。
func TestActiveConfigRoundTrip(t *testing.T) {
	pub := ActiveConfig{
		Schema: ConfigSchemaVersion, Revision: 3, ProviderID: 1, ProviderName: "deepseek",
		Protocol: "openai_chat", AuthScheme: "bearer", BaseURL: "https://api.example.com/v1",
		ModelRowID: 2, ModelID: "model-a", CredentialRef: "0123456789abcdef0123456789abcdef",
		Source: "admin_save", TS: 1700000000,
	}
	raw, err := MarshalActive(pub)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ParseActive(raw)
	if !ok || got.Revision != 3 || got.CredentialRef != pub.CredentialRef || got.BaseURL != pub.BaseURL {
		t.Fatalf("往返失败: %+v ok=%v", got, ok)
	}
	for _, bad := range []string{
		"", "{", `{"schema":2,"revision":1,"provider":"p","base_url":"https://a/v1","model_id":"m"}`,
		`{"schema":1,"revision":0,"provider":"p","base_url":"https://a/v1","model_id":"m"}`,
		`{"schema":1,"revision":1,"provider":"p","base_url":"","model_id":"m"}`,
	} {
		if _, ok := ParseActive(bad); ok {
			t.Errorf("坏记录不应解析成功: %q", bad)
		}
	}
}
