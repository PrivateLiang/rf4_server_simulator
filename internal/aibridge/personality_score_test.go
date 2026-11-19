package aibridge

// T-107 评分草稿的模型往返：严格解析、只评估一次、失败不产出建议。
//
// 全部使用本地 stub Provider：不连接真实厂商、不使用真实凭据。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
)

const personalityScoreCanonicalReply = `{"status":"ok",` +
	`"values":{"sociability":60,"exploration":40,"persistence":70,"regularity":55},` +
	`"ranges":{"sociability":[45,70],"exploration":[30,55],"persistence":[55,80],"regularity":[40,70]},` +
	`"reasons":{"sociability":"描述里爱主动约人","exploration":"愿意尝试新水域",` +
	`"persistence":"做事有耐心","regularity":"提到固定作息"}}`

// personalityScoreStubProvider 是只回固定文本的 Provider，同时记录调用次数与请求。
type personalityScoreStubProvider struct {
	calls int
	reply string
	err   error
	last  ChatRequest
}

func (p *personalityScoreStubProvider) ID() string { return "stub" }

func (p *personalityScoreStubProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	p.calls++
	p.last = req
	if p.err != nil {
		return ChatResponse{}, p.err
	}
	return ChatResponse{Text: p.reply}, nil
}

func testPersonalityScoreRequest() PersonalityScoreRequest {
	return PersonalityScoreRequest{
		RequestID:              "ps-1-abc",
		AgentKey:               "hutao",
		BaseRevision:           3,
		Description:            "喜欢安静钓鱼，愿意尝试新水域，做事有耐心。",
		DimensionSchemaVersion: aiconfig.PersonalitySchemaVersion,
		CurrentValues:          aiconfig.PersonalityScoreValues{Sociability: 50, Exploration: 50, Persistence: 50, Regularity: 50},
	}
}

// TestParsePersonalityScoreReplyAcceptsTicketShape 票面形状（含整段代码块包装）
// 都能解析成同一份草稿。
func TestParsePersonalityScoreReplyAcceptsTicketShape(t *testing.T) {
	for name, raw := range map[string]string{
		"纯对象":  personalityScoreCanonicalReply,
		"代码块":  "```json\n" + personalityScoreCanonicalReply + "\n```",
		"无语言":  "```\n" + personalityScoreCanonicalReply + "\n```",
		"首尾空白": "  " + personalityScoreCanonicalReply + "\n",
	} {
		status, draft, err := parsePersonalityScoreReply(raw)
		if err != nil {
			t.Fatalf("%s 应可解析：%v", name, err)
		}
		if status != aiconfig.PersonalityScoreStatusOK {
			t.Fatalf("%s status = %q", name, status)
		}
		if draft.Values.Sociability != 60 || draft.Values.Exploration != 40 ||
			draft.Values.Persistence != 70 || draft.Values.Regularity != 55 {
			t.Fatalf("%s values = %+v", name, draft.Values)
		}
		if got := draft.Ranges.Sociability; got.Min() != 45 || got.Max() != 70 {
			t.Fatalf("%s range = %+v", name, got)
		}
		if draft.Reasons.Regularity != "提到固定作息" {
			t.Fatalf("%s reason = %q", name, draft.Reasons.Regularity)
		}
	}
}

// TestParsePersonalityScoreReplyRejectsIllegalOutput 自然语言、额外键、缺项、
// 非整数、超域、区间不含建议值、理由不合约、unassessable 带值都拒绝。
func TestParsePersonalityScoreReplyRejectsIllegalOutput(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(personalityScoreCanonicalReply, old) {
			t.Fatalf("夹具里没有 %q", old)
		}
		return strings.Replace(personalityScoreCanonicalReply, old, new, 1)
	}
	cases := map[string]string{
		"自然语言":      "我认为这个角色交往意愿偏高。",
		"解释夹在中间":    "结果如下：" + personalityScoreCanonicalReply,
		"对象后面还有话":   personalityScoreCanonicalReply + " 以上。",
		"两个对象":      personalityScoreCanonicalReply + personalityScoreCanonicalReply,
		"空回复":       "",
		"缺 status":  `{"values":{},"ranges":{},"reasons":{}}`,
		"未知 status": replace(`"status":"ok"`, `"status":"maybe"`),
		"额外顶层键":     replace(`"status":"ok"`, `"status":"ok","capabilities":["guide_help"]`),
		"额外安全键":     replace(`"status":"ok"`, `"status":"ok","emergency_stop":1`),
		"额外嵌套键":     replace(`"sociability":60`, `"sociability":60,"extraversion":70`),
		"缺一维":       replace(`,"regularity":55`, ``),
		"小数取值":      replace(`"persistence":70`, `"persistence":70.5`),
		"字符串取值":     replace(`"persistence":70`, `"persistence":"70"`),
		"取值超域":      replace(`"persistence":70`, `"persistence":120`),
		"区间倒置":      replace(`"sociability":[45,70]`, `"sociability":[70,45]`),
		"区间越界":      replace(`"sociability":[45,70]`, `"sociability":[-5,70]`),
		"区间不含建议值":   replace(`"sociability":[45,70]`, `"sociability":[65,70]`),
		"区间不是两元":    replace(`"sociability":[45,70]`, `"sociability":[45,70,80]`),
		"区间只有一元":    replace(`"sociability":[45,70]`, `"sociability":[45]`),
		"理由为空":      replace(`"regularity":"提到固定作息"`, `"regularity":""`),
		"理由过长":      replace(`"regularity":"提到固定作息"`, `"regularity":"`+strings.Repeat("长", 41)+`"`),
		"缺 reasons": replace(`"reasons":`, `"notes":`),
		"无法评估带值":    `{"status":"unassessable","values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`,
		"无法评估带区间":   `{"status":"unassessable","ranges":{}}`,
	}
	for name, raw := range cases {
		if _, _, err := parsePersonalityScoreReply(raw); !errors.Is(err, ErrPersonalityScoreReply) {
			t.Errorf("%s 应报 ErrPersonalityScoreReply，得到 %v", name, err)
		}
	}
}

// TestRunPersonalityScoreCallsProviderOnce 一次请求只评估一次：不重试、不递归。
func TestRunPersonalityScoreCallsProviderOnce(t *testing.T) {
	provider := &personalityScoreStubProvider{reply: personalityScoreCanonicalReply}
	req := testPersonalityScoreRequest()
	got, err := RunPersonalityScore(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("评分失败: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("厂商调用 %d 次，期望 1 次", provider.calls)
	}
	if got.RequestID != req.RequestID || got.BaseRevision != req.BaseRevision ||
		got.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		t.Fatalf("回执未按请求回填: %+v", got)
	}
	if got.Status != aiconfig.PersonalityScoreStatusOK || got.Values == nil ||
		got.Ranges == nil || got.Reasons == nil {
		t.Fatalf("ok 回执必须带 values/ranges/reasons: %+v", got)
	}
	if got.Values.Persistence != 70 {
		t.Fatalf("values 未回填: %+v", got.Values)
	}
	// 提示词只来自同一份定义：系统消息固定、用户消息带描述与当前四维。
	if len(provider.last.Messages) != 2 {
		t.Fatalf("消息条数 = %d，期望 system + user", len(provider.last.Messages))
	}
	if provider.last.Messages[0].Role != "system" || !strings.Contains(provider.last.Messages[0].Content, "只输出一个 JSON 对象") {
		t.Fatalf("system 消息不符: %+v", provider.last.Messages[0])
	}
	user := provider.last.Messages[1].Content
	for _, want := range []string{req.Description, "交往意愿 50", "作息规律 50"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user 消息缺 %q：\n%s", want, user)
		}
	}
	if provider.last.MaxTokens != PersonalityScoreMaxTokens {
		t.Fatalf("MaxTokens = %d，期望 %d", provider.last.MaxTokens, PersonalityScoreMaxTokens)
	}
}

// TestRunPersonalityScoreUnassessable 无法评估只回固定文案，不带 values/ranges。
func TestRunPersonalityScoreUnassessable(t *testing.T) {
	provider := &personalityScoreStubProvider{reply: `{"status":"unassessable"}`}
	got, err := RunPersonalityScore(context.Background(), provider, testPersonalityScoreRequest())
	if err != nil {
		t.Fatalf("无法评估不是错误: %v", err)
	}
	if got.Status != aiconfig.PersonalityScoreStatusUnassessable {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Reason != aiconfig.PersonalityScoreUnassessableText {
		t.Fatalf("固定文案 = %q，期望 %q", got.Reason, aiconfig.PersonalityScoreUnassessableText)
	}
	if got.Values != nil || got.Ranges != nil || got.Reasons != nil {
		t.Fatalf("无法评估不得带 values/ranges/reasons: %+v", got)
	}
}

// TestRunPersonalityScoreProviderErrorIsReplyError 厂商失败与非法输出都不产出草稿。
func TestRunPersonalityScoreProviderErrorIsReplyError(t *testing.T) {
	provider := &personalityScoreStubProvider{err: errors.New("boom")}
	if _, err := RunPersonalityScore(context.Background(), provider, testPersonalityScoreRequest()); err == nil {
		t.Fatal("厂商错误应上抛")
	}
	provider = &personalityScoreStubProvider{reply: "交往意愿偏高"}
	if _, err := RunPersonalityScore(context.Background(), provider, testPersonalityScoreRequest()); !errors.Is(err, ErrPersonalityScoreReply) {
		t.Fatalf("非法输出应报 ErrPersonalityScoreReply，得到 %v", err)
	}
}

// TestPersonalityScoreRequestValidate 请求本身不合约时直接拒绝，不发出任何调用。
func TestPersonalityScoreRequestValidate(t *testing.T) {
	base := testPersonalityScoreRequest()
	if err := base.Validate(); err != nil {
		t.Fatalf("合法请求被拒: %v", err)
	}
	cases := map[string]func(r *PersonalityScoreRequest){
		"缺 request_id":  func(r *PersonalityScoreRequest) { r.RequestID = "  " },
		"request_id 非法": func(r *PersonalityScoreRequest) { r.RequestID = "ps 1" },
		"request_id 过长": func(r *PersonalityScoreRequest) { r.RequestID = strings.Repeat("a", 65) },
		"缺 agent_key":   func(r *PersonalityScoreRequest) { r.AgentKey = "" },
		"负修订":           func(r *PersonalityScoreRequest) { r.BaseRevision = -1 },
		"未知维度版本":        func(r *PersonalityScoreRequest) { r.DimensionSchemaVersion = 2 },
		"空描述":           func(r *PersonalityScoreRequest) { r.Description = " " },
		"描述过长": func(r *PersonalityScoreRequest) {
			r.Description = strings.Repeat("鱼", PersonalityScoreDescriptionMaxRunes+1)
		},
		"当前值超域": func(r *PersonalityScoreRequest) { r.CurrentValues.Sociability = 101 },
	}
	for name, mutate := range cases {
		req := base
		mutate(&req)
		if err := req.Validate(); !errors.Is(err, ErrPersonalityScoreRequest) {
			t.Errorf("%s 应报 ErrPersonalityScoreRequest，得到 %v", name, err)
		}
	}
	// 校验失败时 RunPersonalityScore 不得调用厂商。
	provider := &personalityScoreStubProvider{reply: personalityScoreCanonicalReply}
	bad := base
	bad.Description = ""
	if _, err := RunPersonalityScore(context.Background(), provider, bad); err == nil {
		t.Fatal("非法请求应报错")
	}
	if provider.calls != 0 {
		t.Fatalf("非法请求仍调用了厂商 %d 次", provider.calls)
	}
}

// TestPersonalityScoreSharesDimensionSchemaWithExpression 评分与表达用同一 schema：
// 评分草稿的维度版本就是 T-103 的存储版本，不另开 v2。
func TestPersonalityScoreSharesDimensionSchemaWithExpression(t *testing.T) {
	_, draft, err := parsePersonalityScoreReply(personalityScoreCanonicalReply)
	if err != nil {
		t.Fatal(err)
	}
	if draft.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		t.Fatalf("草稿 schema = %d，期望 %d", draft.SchemaVersion, aiconfig.PersonalitySchemaVersion)
	}
	expr := aiconfig.CompilePersonalityExpression(aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion, Revision: 3,
		Sociability: 60, Exploration: 40, Persistence: 70, Regularity: 55,
	})
	if !strings.Contains(expr, "schema v1 · 修订 3") {
		t.Fatalf("表达编译版本不符：%s", expr)
	}
	for _, dim := range aiconfig.PersonalityDimensions() {
		if !strings.Contains(expr, dim.Label) {
			t.Errorf("表达编译缺维度 %s", dim.ID)
		}
	}
}
