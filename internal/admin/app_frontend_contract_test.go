package admin

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 新后台（web/app/）的前端门禁。
//
// 旧单页（legacy.html + web/ai/）的契约仍由 ai_frontend_contract_test.go 守护；
// 这里把其中与安全和数据正确性有关的要求，原样落到新后台的源码上：
//   - 不在前端自抄安全默认值，不让已废弃的授权/出网闸门回到页面；
//   - 急停只认后端回读的 emergency_stop，读不准时明确显示待确认；
//   - 复测只用已保存的连接，不携带页面草稿；保存连接必须带页面看到的修订号；
//   - 页面模块不读写登录态（token 只由 core.js 管理）；
//   - 钓场名册显示的鱼饵轮换来自后端权威字段。

func readAppSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(WebFS(), "app", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css")) {
			return nil
		}
		raw, err := fs.ReadFile(WebFS(), p)
		if err != nil {
			return err
		}
		out[p] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("读取 web/app: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("web/app 下没有前端源码")
	}
	return out
}

func TestAppFrontendSecurityContract(t *testing.T) {
	src := readAppSources(t)
	aiPage := src["app/pages/ai.js"]
	if aiPage == "" {
		t.Fatal("缺少 app/pages/ai.js")
	}

	t.Run("no_local_security_default", func(t *testing.T) {
		for _, issue := range noLocalSecurityDefaultIssues(src) {
			t.Error(issue)
		}
	})

	t.Run("no_old_gate_entries", func(t *testing.T) {
		for _, issue := range oldGateIssues(src, AISettingSpecs) {
			t.Error(issue)
		}
	})

	t.Run("emergency_from_backend", func(t *testing.T) {
		for _, want := range []string{
			"S.status.emergency_stop", // 唯一来源：后端回读
			"em.fault",                // 读不准时显示待确认
			"emergency_stop_epoch",    // 保存急停带上看到的代次
		} {
			if !strings.Contains(aiPage, want) {
				t.Errorf("AI 页急停处理缺少 %q", want)
			}
		}
	})

	t.Run("retest_uses_saved_config_only", func(t *testing.T) {
		if n := strings.Count(aiPage, "'/api/ai/test'"); n != 1 {
			t.Fatalf("复测调用出现 %d 次，期望恰好 1 次", n)
		}
		if !strings.Contains(aiPage, "post('/api/ai/test', {})") {
			t.Error("复测必须只 POST /api/ai/test 且不携带页面输入")
		}
		// 复测前必须拦住未保存的草稿
		i := strings.Index(aiPage, "post('/api/ai/test', {})")
		before := aiPage[max(0, i-600):i]
		if !strings.Contains(before, "form.isDirty()") {
			t.Error("复测前必须先检查连接表单是否有未保存修改")
		}
		if !strings.Contains(aiPage, "revision: Number(st.revision)") {
			t.Error("保存连接必须携带页面看到的活动修订")
		}
		for _, banned := range []string{"consent", "outbound_enabled", "egress_allowed_hosts"} {
			if strings.Contains(aiPage, banned) {
				t.Errorf("AI 页不得携带旧闸字段（命中 %q）", banned)
			}
		}
	})

	t.Run("pages_do_not_touch_auth", func(t *testing.T) {
		names := make([]string, 0, len(src))
		for name := range src {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !strings.HasPrefix(name, "app/pages/") {
				continue
			}
			for _, banned := range []string{"localStorage", "document.cookie", "getToken", "setToken"} {
				if strings.Contains(src[name], banned) {
					t.Errorf("%s 不得读写登录态（命中 %q）", name, banned)
				}
			}
		}
		if regexp.MustCompile(`window\s*\.\s*go\s*=`).MatchString(strings.Join(mapValues(src), "\n")) {
			t.Error("前端不得改写全局 go")
		}
	})
}

func TestAppFishPageRendersBaitRotation(t *testing.T) {
	src := readAppSources(t)["app/pages/waters.js"]
	for _, want := range []string{
		"row.bait_groups",     // 用后端权威轮换组
		"row.bait_rotate_sec", // 轮换间隔
		"轮换",                  // 名册里可见的轮换标记
		"/api/bait/prefs",     // 静态爱吃的饵来自鱼饵偏好
	} {
		if !strings.Contains(src, want) {
			t.Errorf("钓场名册缺少轮换同步标记：%q", want)
		}
	}
}

func TestAppFrontendAssetsServed(t *testing.T) {
	index, err := fs.ReadFile(WebFS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`src="app/main.js"`, `href="app/app.css"`, `type="module"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}

	// main.js 里登记的每个页面模块都必须真实存在
	src := readAppSources(t)
	for _, m := range regexp.MustCompile(`import\('\./(pages/[a-z]+\.js)'\)`).FindAllStringSubmatch(src["app/main.js"], -1) {
		if _, ok := src[path.Join("app", m[1])]; !ok {
			t.Errorf("main.js 引用的页面模块不存在：%s", m[1])
		}
	}

	srv := httptest.NewServer(WebHandler(WebFS()))
	defer srv.Close()
	for _, tc := range []struct{ path, mime, cache string }{
		{"/app/main.js", "javascript", "no-cache"},
		{"/app/core.js", "javascript", "no-cache"},
		{"/app/pages/ai.js", "javascript", "no-cache"},
		{"/app/app.css", "text/css", "no-cache"},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			t.Errorf("GET %s = %d，响应体 %d 字节", tc.path, resp.StatusCode, len(body))
		}
		if got := strings.ToLower(resp.Header.Get("Content-Type")); !strings.Contains(got, tc.mime) {
			t.Errorf("GET %s Content-Type = %q，期望包含 %q", tc.path, got, tc.mime)
		}
		if got := resp.Header.Get("Cache-Control"); got != tc.cache {
			t.Errorf("GET %s Cache-Control = %q，期望 %q", tc.path, got, tc.cache)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
