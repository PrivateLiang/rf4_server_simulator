package aiconfig

// 旧部署出口名单的兼容解析与传输层 URL 防护的独立真值：
// 条目与期望值全部手写，不用被测的构造函数生成；地址分类用固定的 IP 字面量。
//
// V2（DEV-104）后名单不再参与放行判断，这里锁两件事：
//  1. 旧配置仍能被解析（后台只显示条数）；
//  2. 真正发送前的 URL 形状检查（HTTPS / 无 userinfo / 主机名合法）仍然生效，
//     且不再要求主机出现在名单里。

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestEgressHostPolicyParsingStillWorks(t *testing.T) {
	policy, err := NewEgressHostPolicy([]string{
		"HTTPS://API.Example.COM:443/",
		"search.example.com",
		"127.0.0.1:8443",
	})
	if err != nil {
		t.Fatalf("解析旧名单: %v", err)
	}
	cases := []struct {
		host, port string
		allowed    bool
	}{
		{"api.example.com", "443", true},
		{"API.EXAMPLE.COM.", "443", true},
		{"search.example.com", "443", true},
		{"api.example.com", "8443", false},
		{"api.example.com.evil.test", "443", false},
		{"evilapi.example.com", "443", false},
		{"127.0.0.1", "8443", true},
		{"127.0.0.1", "9443", false},
	}
	for _, c := range cases {
		if got := policy.Allows(c.host, c.port); got != c.allowed {
			t.Errorf("Allows(%q,%q) = %v，期望 %v", c.host, c.port, got, c.allowed)
		}
	}
}

func TestCheckEgressURLNoLongerUsesList(t *testing.T) {
	// 名单为空、主机不在名单里，都不再阻止发送。
	var empty EgressHostPolicy
	for _, raw := range []string{
		"https://api.deepseek.com/v1",
		"https://other.example.com/v1",
		"https://127.0.0.1:8443/x",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckEgressURL(u); err != nil {
			t.Errorf("%q 不应再因旧名单被拒绝: %v", raw, err)
		}
		if err := empty.CheckURL(u); err != nil {
			t.Errorf("%q 经兼容入口也不应被拒绝: %v", raw, err)
		}
	}
	// 形状检查全部保留。
	cases := []struct {
		raw   string
		state string
	}{
		{"http://api.example.com/v1", StateEgressSchemeDenied},
		{"https://user:pass@api.example.com/v1", StateEgressUserinfoDenied},
		{"https://api.example.com:70000/v1", StateEgressHostDenied},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			// 端口越界的 URL 可能在这里就解析失败，等价于拒绝。
			continue
		}
		err = CheckEgressURL(u)
		var denied *HostPolicyError
		if !errors.As(err, &denied) {
			t.Errorf("%q 应被拒绝，得到 %v", c.raw, err)
			continue
		}
		if denied.State != c.state {
			t.Errorf("%q state = %q，期望 %q", c.raw, denied.State, c.state)
		}
	}
	var denied *HostPolicyError
	if err := CheckEgressURL(nil); !errors.As(err, &denied) {
		t.Error("nil URL 应被拒绝")
	}
}

func TestDescribeEgressHostsIsInformational(t *testing.T) {
	var empty EgressHostPolicy
	if got := DescribeEgressHosts(empty); got.Configured != 0 || !strings.Contains(got.Display, "不参与放行") {
		t.Fatalf("空旧名单描述 = %+v", got)
	}
	withOne, err := NewEgressHostPolicy([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := DescribeEgressHosts(withOne); got.Configured != 1 || !strings.Contains(got.Display, "不参与放行") {
		t.Fatalf("一条旧名单描述 = %+v", got)
	}
}

func TestEgressHostPolicyRejectsBadEntries(t *testing.T) {
	cases := []struct{ name, entry string }{
		{"空条目", "   "},
		{"通配", "*.example.com"},
		{"中间通配", "api.*.example.com"},
		{"userinfo", "https://user:pass@api.example.com"},
		{"非 HTTPS", "http://api.example.com"},
		{"带路径", "https://api.example.com/v1"},
		{"带查询", "https://api.example.com?a=1"},
		{"双点", "api..example.com"},
		{"结尾双点", "api.example.com.."},
		{"端口越界", "https://api.example.com:70000"},
		{"端口非数字", "https://api.example.com:abc"},
		{"IPv6 zone", "https://[fe80::1%25eth0]:443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewEgressHostPolicy([]string{c.entry}); err == nil {
				t.Fatalf("条目 %q 应被拒绝", c.entry)
			}
		})
	}
}

func TestEgressHostPolicyCheckEndpoint(t *testing.T) {
	policy, err := NewEgressHostPolicy([]string{"search.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckEndpoint("https://search.example.com/query?q=x"); err != nil {
		t.Fatalf("完整 URL 应放行: %v", err)
	}
	if err := policy.CheckEndpoint("https://not-listed.example.com/query"); err != nil {
		t.Fatalf("旧名单外的主机也不应被拒绝: %v", err)
	}
	var denied *HostPolicyError
	if err := policy.CheckEndpoint("search.example.com/query"); !errors.As(err, &denied) {
		t.Fatalf("缺少 scheme 的地址应被拒绝，得到 %v", err)
	}
	if err := policy.CheckEndpoint("http://search.example.com/query"); !errors.As(err, &denied) || denied.State != StateEgressSchemeDenied {
		t.Fatalf("http 端点应返回 %q，得到 %v", StateEgressSchemeDenied, err)
	}
}

func TestIsPublicEgressAddr(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"127.1.2.3", false},
		{"::1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.10.1", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"fd12::1", false},
		{"0.0.0.0", false},
		{"100.64.0.1", false},
		{"224.0.0.1", false},
		{"192.0.2.10", false},
		{"198.51.100.7", false},
		{"203.0.113.9", false},
		{"198.18.0.1", false},
		{"255.255.255.255", false},
		{"::", false},
		{"2001:db8::1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:8.8.8.8", true},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("解析 %q: %v", c.ip, err)
		}
		if got := IsPublicEgressAddr(ip); got != c.public {
			t.Fatalf("IsPublicEgressAddr(%s) = %v，期望 %v", c.ip, got, c.public)
		}
	}
	if IsPublicEgressAddr(netip.Addr{}) {
		t.Fatal("零值地址不应被视为公网")
	}
}

func TestSameEgressAuthority(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		same bool
	}{
		{"大小写与尾点", "https://api.example.com/v1", "https://API.EXAMPLE.COM./other", true},
		{"缺省与显式 443", "https://api.example.com/v1", "https://api.example.com:443/other", true},
		{"端口不同", "https://api.example.com/v1", "https://api.example.com:8443/v1", false},
		{"主机不同", "https://api.example.com/v1", "https://other.example.com/v1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := url.Parse(c.a)
			b, _ := url.Parse(c.b)
			if got := SameEgressAuthority(a, b); got != c.same {
				t.Fatalf("SameEgressAuthority = %v，期望 %v", got, c.same)
			}
		})
	}
	if SameEgressAuthority(nil, nil) {
		t.Fatal("nil URL 不应视为同一主机")
	}
}
