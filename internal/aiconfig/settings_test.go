package aiconfig

import (
	"testing"
)

func lookupMap(values map[string]string) Lookup {
	return func(key string) string { return values[key] }
}

// TestEgressOnlyEmergency 锁定 V2 的唯一放行条件（DEV-104「只留急停」）：
// outbound_enabled、四项 consent、目的地快照、旧部署名单都不再参与判断；
// 只有急停能拦住外发，且非法值按保守停止处理。
func TestEgressOnlyEmergency(t *testing.T) {
	cases := []struct {
		name        string
		values      map[string]string
		wantAllowed bool
		wantState   string
		wantDisplay string
	}{
		{"缺键放行", nil, true, StateAllowed, "开（未急停）"},
		{"历史值1放行", map[string]string{KeyOutboundEnabled: "1"}, true, StateAllowed, "开（未急停）"},
		{"显式关也放行", map[string]string{KeyOutboundEnabled: "0"}, true, StateAllowed, "开（未急停）"},
		{"非法值也放行", map[string]string{KeyOutboundEnabled: "true"}, true, StateAllowed, "开（未急停）"},
		{"无 consent 放行", map[string]string{}, true, StateAllowed, "开（未急停）"},
		{"撤销 consent 放行", map[string]string{KeyConsentOutbound: `{"schema":1,"item":"outbound","state":"revoked","subject":"a","ts":1}`}, true, StateAllowed, "开（未急停）"},
		{"急停拦住", map[string]string{KeyOutboundEnabled: "1", KeyEmergencyStop: "1"}, false, StateEmergency, "关（紧急停止）"},
		{"急停解析失败保守停止", map[string]string{KeyOutboundEnabled: "0", KeyEmergencyStop: "yes"}, false, StateEmergency, "关（紧急停止）"},
		{"急停明确关", map[string]string{KeyEmergencyStop: "0"}, true, StateAllowed, "开（未急停）"},
	}
	for _, c := range cases {
		got := Egress(lookupMap(c.values))
		if got.Allowed != c.wantAllowed {
			t.Errorf("%s：Allowed = %v，期望 %v", c.name, got.Allowed, c.wantAllowed)
		}
		if got.State != c.wantState || got.Display != c.wantDisplay {
			t.Errorf("%s：得到 (%s,%q)，期望 (%s,%q)", c.name, got.State, got.Display, c.wantState, c.wantDisplay)
		}
	}
}

func TestEmergencyStop(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		want   bool
	}{
		{"缺键取默认0", nil, false},
		{"明确关", map[string]string{KeyEmergencyStop: "0"}, false},
		{"明确开", map[string]string{KeyEmergencyStop: "1"}, true},
		{"非法值保守停止true", map[string]string{KeyEmergencyStop: "true"}, true},
		{"非法值保守停止2", map[string]string{KeyEmergencyStop: "2"}, true},
	}
	for _, c := range cases {
		if got := EmergencyStop(lookupMap(c.values)); got != c.want {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestSecurityViewHasNoOutboundGate 后台视图里不再有「模型调用出网」开关：
// 历史全 1 也放行；三个本地行为键仍按历史规则显示「未确认」。
func TestSecurityViewHasNoOutboundGate(t *testing.T) {
	allOn := map[string]string{
		KeyOutboundEnabled: "1", KeyActorEnabled: "1", KeyCatchTalk: "1", KeyMemoryInject: "1",
	}
	view := Security(lookupMap(allOn))
	if !view.Egress.Allowed {
		t.Fatal("V2 只留急停：没有急停时必须放行")
	}
	if _, ok := view.Keys[KeyOutboundEnabled]; ok {
		t.Fatal("security.keys 不应再暴露 outbound_enabled")
	}
	for _, key := range []string{KeyActorEnabled, KeyCatchTalk, KeyMemoryInject} {
		st, ok := view.Keys[key]
		if !ok {
			t.Fatalf("缺少 %s 的展示状态", key)
		}
		if st.State != StateUnconfirmed || st.Display != "未确认" {
			t.Errorf("%s 得到 (%s,%q)，期望 (unconfirmed,未确认)", key, st.State, st.Display)
		}
	}
	// 旧部署名单只作记录展示，不表示「拒绝」。
	if view.Hosts.Display == "" || view.Hosts.Configured != 0 {
		t.Errorf("空旧名单描述 = %+v", view.Hosts)
	}
	if st := Security(nil); !st.Egress.Allowed {
		t.Error("缺省配置不得阻止发送")
	}
}

func TestDefaultValue(t *testing.T) {
	cases := map[string]string{
		KeyOutboundEnabled: "0",
		KeyEmergencyStop:   "0",
		KeyActorEnabled:    "1",
		KeyCatchTalk:       "1",
		KeyMemoryInject:    "1",
	}
	for key, want := range cases {
		got, ok := DefaultValue(key)
		if !ok || got != want {
			t.Errorf("DefaultValue(%s) = (%q,%v)，期望 (%q,true)", key, got, ok, want)
		}
	}
	if _, ok := DefaultValue("no_such_key"); ok {
		t.Error("未知键不应返回默认值")
	}
}

func TestEvaluateEmergencyAndIntent(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reading EmergencyReading
		blocked bool
		reason  string
	}{
		{"default", EmergencyReading{Version: 1, Epoch: 3}, false, ""},
		{"audit timestamp irrelevant", EmergencyReading{Version: 1, Epoch: 3, LiftedRaw: "damaged"}, false, ""},
		{"stopped", EmergencyReading{Version: 1, Epoch: 3, StopRaw: "1"}, true, IntentEmergencyStop},
		{"invalid switch", EmergencyReading{Version: 1, Epoch: 3, StopRaw: "yes"}, true, FaultStopInvalid},
		{"read error", EmergencyReading{Err: errEmptyRecord}, true, FaultReadError},
		{"missing epoch", EmergencyReading{Version: 1}, true, FaultEpochInvalid},
		{"unknown version", EmergencyReading{Version: 2, Epoch: 3}, true, FaultEpochInvalid},
		{"exhausted epoch", EmergencyReading{Version: 1, Epoch: 1<<63 - 1}, true, FaultEpochInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := EvaluateEmergency(tt.reading)
			if blocked, reason := d.RecordBlocked(); blocked != tt.blocked || reason != tt.reason {
				t.Fatalf("blocked=%v reason=%q, want %v/%s", blocked, reason, tt.blocked, tt.reason)
			}
		})
	}
	dec := EvaluateEmergency(EmergencyReading{Version: 1, Epoch: 3})
	for _, tt := range []struct {
		epoch  int64
		reason string
	}{
		{0, IntentOriginUnknown}, {2, IntentQueuedBeforeResume}, {3, ""}, {4, IntentQueuedBeforeResume},
	} {
		if ok, reason := dec.IntentActive(tt.epoch); ok != (tt.reason == "") || reason != tt.reason {
			t.Errorf("epoch=%d active=%v reason=%s, want %s", tt.epoch, ok, reason, tt.reason)
		}
	}
}
