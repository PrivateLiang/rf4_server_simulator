package aibridge

// 统一 Provider 抽象：Agent 层不感知厂商，差异全部收敛到配置表。
// 首发适配器走 OpenAI 兼容协议（DeepSeek 等国内厂商可直接用），支持 function calling。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
)

// ToolSpec 是给模型看的函数定义（OpenAI function calling 形状）。
//
// 只有 Name / Description / Parameters 会序列化给厂商；Kind、Route、HandlerKey
// 等内部字段必须留在服务端——它们决定权限与执行路径，不能外传给模型。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`

	ResultSchema json.RawMessage `json:"-"`
	Kind         ToolKind        `json:"-"`
	Route        ToolRoute       `json:"-"`
	AgentScoped  bool            `json:"-"`
	HandlerKey   string          `json:"-"`
	TimeoutMS    int             `json:"-"`
	// MaxResultBytes 按字符上限执行，保留改造前 trimRunes 的截断口径。
	MaxResultBytes int `json:"-"`
}

// ToolCall 是模型发起的一次工具调用（内部扁平表示）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// MarshalJSON 把内部扁平结构还原成 OpenAI 的嵌套 tool_calls 形状。
func (t ToolCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":   t.ID,
		"type": "function",
		"function": map[string]string{
			"name":      t.Name,
			"arguments": t.Arguments,
		},
	})
}

// Message 是对话消息，兼容 assistant tool_calls 与 tool 结果回传。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
}

// ThinkingConfig 是统一的思考开关与强度档。
type ThinkingConfig struct {
	Enabled bool
	Effort  string
}

// ChatRequest / ChatResponse 是与厂商无关的请求与响应。
type ChatRequest struct {
	Model     string
	Messages  []Message
	Tools     []ToolSpec
	Thinking  ThinkingConfig
	MaxTokens int
}

type ChatResponse struct {
	Text             string
	ToolCalls        []ToolCall
	ReasoningContent string
	RawUsage         string // 仅排障，不用于计费
}

// Provider 是所有模型厂商必须实现的接口。
type Provider interface {
	Chat(ctx context.Context, r ChatRequest) (ChatResponse, error)
	ID() string
}

// EgressGate 是真正发出 HTTP 请求前的最后一次外发判定。
// ctx 来自本次调用；返回非 nil 时请求不发出。
type EgressGate func(ctx context.Context) error

// EgressDeniedError 直接携带 aiconfig 的判定结果，运行期与后台用同一套原因。
type EgressDeniedError struct {
	State   string `json:"state"`
	Display string `json:"display"`
}

func (e *EgressDeniedError) Error() string { return "ai: 外发被拒绝：" + e.Display }

func newEgressDenied(status aiconfig.EgressStatus) *EgressDeniedError {
	return &EgressDeniedError{State: status.State, Display: status.Display}
}

// CheckEgressBeforeSend 是运行期出口与后台「复测已保存连接」共用的判定。
//
// V2 只保留急停：实际发送前必须重读同一份 ai_setting，确认未被紧急停止。
// 目的地/修订的绑定由配置快照（活动修订 + 凭据引用）保证，不再用授权快照比对。
func CheckEgressBeforeSend(_ ProviderSpec, lookup aiconfig.Lookup) error {
	if status := aiconfig.Egress(lookup); !status.Allowed {
		return newEgressDenied(status)
	}
	return nil
}

// ---- 外部出口的传输层防护 ----
//
// 三个出口（provider / 联网搜索 / 后台复测）共用同一个 HTTP 客户端构造：
//   - URL 级：必须 HTTPS、不含 userinfo、主机名合法（aiconfig.CheckEgressURL）；
//   - 连接级：拨号前解析主机并检查实际地址，拒绝环回/私网/链路本地，
//     只拨校验过的 IP（解析一次，防 DNS 重绑定），并显式禁用环境变量代理；
//   - 重定向：每一跳重新校验；跨主机/跨端口不转发 Bearer / x-api-key。
//
// 部署主机名单不再参与放行判断（T-102 / DEV-104），但上述防护全部保留。
// 测试可以注入 Transport（本地 fixture），生产构造路径只会使用安全 Transport。

// egressPolicyTransport 在底层 Transport 之前做 URL 级检查。
type egressPolicyTransport struct {
	base http.RoundTripper
}

func (t *egressPolicyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := aiconfig.CheckEgressURL(req.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// egressResolver / egressDialer 是安全 Transport 的两个可注入点：
// 生产用 net.DefaultResolver / net.Dialer；测试用它们指向本地 fixture。
type egressResolver func(ctx context.Context, host string) ([]netip.Addr, error)
type egressDialer func(ctx context.Context, network, addr string) (net.Conn, error)

func defaultEgressResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func defaultEgressDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// newSafeTransport 构造生产用 Transport。代理显式禁用（Proxy=nil）：
// 不允许 HTTP(S)_PROXY 环境变量把已校验的连接改道到未授权主机。
func newSafeTransport() *http.Transport {
	return newSafeTransportWith(defaultEgressResolver, defaultEgressDialer)
}

func newSafeTransportWith(resolver egressResolver, dialer egressDialer) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("ai: 出口地址不合法: %w", err)
		}
		// 连接阶段再核对一次 URL/地址形状：不依赖调用方更早的判断。
		if err := aiconfig.CheckEgressURL(&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}); err != nil {
			return nil, err
		}
		addrs, err := resolveEgressAddrs(ctx, resolver, host)
		if err != nil {
			return nil, fmt.Errorf("ai: 出口主机解析失败: %w", err)
		}
		// 任何一条解析结果落在非公网地址即整体拒绝（防 DNS 重绑定/分答案）。
		for _, ip := range addrs {
			if !aiconfig.IsPublicEgressAddr(ip) {
				return nil, &aiconfig.HostPolicyError{
					State:   aiconfig.StateEgressAddrDenied,
					Display: "关（出口主机解析到非公网地址）",
				}
			}
		}
		var lastErr error
		for _, ip := range addrs {
			// 只拨校验过的 IP，不再让系统解析第二次。
			conn, err := dialer(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = errors.New("ai: 出口主机没有可用地址")
		}
		return nil, lastErr
	}
	return tr
}

func resolveEgressAddrs(ctx context.Context, resolver egressResolver, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	addrs, err := resolver(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("没有解析结果")
	}
	return addrs, nil
}

// newEgressHTTPClient 构造三个出口共用的 HTTP 客户端。base 为 nil 时使用
// 生产安全 Transport；测试可传入本地 fixture 的 Transport。
func newEgressHTTPClient(timeout time.Duration, base http.RoundTripper) *http.Client {
	if base == nil {
		base = newSafeTransport()
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &egressPolicyTransport{base: base},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("ai: 重定向次数过多")
			}
			// 每一跳都重新校验 URL：上一跳放行不代表下一跳可信。
			if err := aiconfig.CheckEgressURL(req.URL); err != nil {
				return err
			}
			// 鉴权只属于最初的主机：跨主机/跨端口不转发 Bearer / x-api-key。
			if len(via) > 0 && !aiconfig.SameEgressAuthority(via[0].URL, req.URL) {
				req.Header.Del("Authorization")
				req.Header.Del("x-api-key")
			}
			return nil
		},
	}
}

// ProviderSpec 来自活动配置快照（地址/模型）与凭据对象（已解密 Key）。
type ProviderSpec struct {
	Name       string
	Protocol   string
	BaseURL    string
	AuthScheme string
	ModelID    string
	ConfigJSON string
	APIKey     string
}

// OpenAICompatProvider 走 /chat/completions，支持思考档位映射与工具调用。
type OpenAICompatProvider struct {
	spec        ProviderSpec
	client      *http.Client
	timeout     time.Duration
	thinkingMap map[string]json.RawMessage
	gate        EgressGate
	base        http.RoundTripper // nil = 生产安全 Transport；仅测试注入本地 fixture
}

// NewProvider 构造 Provider。ModelID 或 APIKey 为空时返回明确错误，由上层降级。
func NewProvider(spec ProviderSpec, timeout time.Duration) (*OpenAICompatProvider, error) {
	return newProvider(spec, timeout, nil)
}

// NewProviderWithTransport 与 NewProvider 相同，但使用调用方提供的底层
// Transport。只用于测试注入本地 fixture（URL 与重定向检查仍由外层执行，
// 只是连接阶段由注入的 Transport 负责）；生产代码只调用 NewProvider。
func NewProviderWithTransport(spec ProviderSpec, timeout time.Duration, base http.RoundTripper) (*OpenAICompatProvider, error) {
	if base == nil {
		return nil, errors.New("ai: 测试 Transport 不能为空")
	}
	return newProvider(spec, timeout, base)
}

func newProvider(spec ProviderSpec, timeout time.Duration, base http.RoundTripper) (*OpenAICompatProvider, error) {
	if strings.TrimSpace(spec.BaseURL) == "" {
		return nil, errors.New("ai: base_url 未配置")
	}
	if strings.TrimSpace(spec.ModelID) == "" {
		return nil, errors.New("ai: 模型 ID 未配置，请在后台填写")
	}
	if strings.TrimSpace(spec.APIKey) == "" {
		return nil, errors.New("ai: API Key 未配置，请在后台填写")
	}
	if spec.Protocol != "" && spec.Protocol != "openai_chat" {
		return nil, fmt.Errorf("ai: 暂不支持协议 %q", spec.Protocol)
	}
	switch spec.AuthScheme {
	case "", "bearer", "x-api-key":
	default:
		return nil, fmt.Errorf("ai: 不支持的鉴权方式 %q", spec.AuthScheme)
	}
	var cfg struct {
		ThinkingMap map[string]json.RawMessage `json:"thinking_map"`
	}
	if spec.ConfigJSON != "" {
		if err := json.Unmarshal([]byte(spec.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("ai: 厂商配置解析失败: %w", err)
		}
	}
	if cfg.ThinkingMap == nil {
		cfg.ThinkingMap = map[string]json.RawMessage{}
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	p := &OpenAICompatProvider{
		spec:        spec,
		timeout:     timeout,
		thinkingMap: cfg.ThinkingMap,
		base:        base,
	}
	p.client = newEgressHTTPClient(timeout, base)
	return p, nil
}

func (p *OpenAICompatProvider) ID() string { return p.spec.Name }

// SetEgressGate 安装出口前检查。Provider 不解释授权，只负责在发送前拒绝；
// 判定逻辑由 aiconfig 提供、由调用方注入，避免两处各写一套开关解释。
func (p *OpenAICompatProvider) SetEgressGate(gate EgressGate) { p.gate = gate }

// setTransport 替换连接阶段的 Transport，保留 URL/重定向检查。
// 仅供包内 Runner 测试注入本地 fixture；生产路径保持 nil（安全 Transport）。
func (p *OpenAICompatProvider) setTransport(base http.RoundTripper) {
	p.base = base
	p.client = newEgressHTTPClient(p.timeout, base)
}

func (p *OpenAICompatProvider) endpoint() string {
	base := strings.TrimRight(p.spec.BaseURL, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func (p *OpenAICompatProvider) Chat(ctx context.Context, r ChatRequest) (ChatResponse, error) {
	body := map[string]any{
		"model":      p.spec.ModelID,
		"messages":   r.Messages,
		"max_tokens": r.MaxTokens,
		"stream":     false,
	}
	if len(r.Tools) > 0 {
		wire := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			wire = append(wire, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		body["tools"] = wire
		body["tool_choice"] = "auto"
	}
	level := r.Thinking.Effort
	if level == "" {
		level = "off"
	}
	if !r.Thinking.Enabled || len(r.Tools) > 0 {
		// 思考模式下部分厂商要求回传 reasoning_content，多轮工具调用容易踩 400；
		// 工具调用轮一律关闭思考（工具本身已给出确定性事实）。
		level = "off"
	}
	if raw, ok := p.thinkingMap[level]; ok {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(raw, &extra); err == nil {
			for k, v := range extra {
				body[k] = v
			}
		}
	} else if level == "off" {
		body["thinking"] = map[string]string{"type": "disabled"}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.spec.AuthScheme == "x-api-key" {
		httpReq.Header.Set("x-api-key", p.spec.APIKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+p.spec.APIKey)
	}

	// 真正的出口检查放在发送前最后一刻：预检之后撤销授权、换目的地
	// 或触发急停，都在这里拦住，不依赖调用方更早的判断。
	if p.gate != nil {
		if err := p.gate(ctx); err != nil {
			return ChatResponse{}, err
		}
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai: 请求厂商失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.ToValidUTF8(string(raw), "")
		// 少数代理会把请求头或密钥回显在错误体里，先抹掉再返回。
		snippet = strings.ReplaceAll(snippet, p.spec.APIKey, "sk-***")
		if r := []rune(snippet); len(r) > 300 {
			snippet = string(r[:300])
		}
		return ChatResponse{}, fmt.Errorf("ai: 厂商返回 %d: %s", resp.StatusCode, snippet)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("ai: 响应解析失败: %w", err)
	}
	if len(out.Choices) == 0 {
		return ChatResponse{}, errors.New("ai: 厂商响应无 choices")
	}
	m := out.Choices[0].Message
	calls := make([]ToolCall, 0, len(m.ToolCalls))
	for _, c := range m.ToolCalls {
		calls = append(calls, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	if len(calls) == 0 && strings.TrimSpace(m.Content) == "" {
		return ChatResponse{}, errors.New("ai: 厂商响应无文本内容")
	}
	return ChatResponse{
		Text: m.Content, ToolCalls: calls,
		ReasoningContent: m.ReasoningContent, RawUsage: string(out.Usage),
	}, nil
}
