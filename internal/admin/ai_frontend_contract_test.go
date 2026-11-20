package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// T-025 · AI 前端拆分后的门禁（Go 静态断言 + 内嵌资源 HTTP 断言，不引入 playwright / node）。
//
// 背景 [事实]：T-019 的六类断言全部按「读一个 web/index.html」实现；T-025 把 AI 的
// CSS/markup/JS 迁到 web/ai/** 后，断言改为读实际 embed 资源集合（WebFS），并新增
// CSS 完整边界（每个逗号分支以 #view-ai 为根）、资源可服务/MIME/no-cache 与四负例。
//
// 真值来源：全部为手写字面量。官方基线取自官方 main 2b906d4 的
// internal/admin/web/index.html（提取方法见各常量注释），不由被测 HTML 反向生成期望值。

// officialFrontendViewMounts 是官方页面的 #view-* 挂载点（11 个）。
// 用途：AI 只允许新增 view-ai，不得改写/删除任何官方挂载点。
const officialFrontendViewMounts = `view-dashboard view-players view-stores view-fish view-hooks view-bait view-mail view-params view-release view-about view-audit`

// officialFrontendCSSClasses 是官方页面 CSS 中出现过的全部 class（171 个）。
// 提取方法：取 <style> 去注释后的每条规则选择器中的 .class 段，去重排序。
const officialFrontendCSSClasses = `about-card about-head actions alt area-badges attach-row ba-open bad bait-edit bait-head bar blue brand btn card clamp close collapsed danger-text duo edit empty event-gift-actions event-gift-card event-gift-editor event-gift-empty event-gift-index event-gift-intro event-gift-item event-gift-items event-gift-picker-groups event-gift-picker-results event-gift-plan event-gift-plan-head event-gift-plans event-gift-preview event-gift-selector event-gift-status field fish-layout fish-table foreign form-grid fp gold good grade-ratio-actions grade-ratio-bar grade-ratio-card grade-ratio-head grade-ratio-row grade-ratio-summary grid grow guide-close hint ico id k legend log login-card logo m mail-box main mark markdown-body mini-input modal modal-bg mode-on mono more msg muted n nav notice notice-on off ok on online-dot params-bar params-form params-tab params-tabs picked pill player-flags player-hero player-list player-row prop-sec pulse qa qa-grid qty release-card release-markdown result results right rule-help rule-item rule-mini rules-strip s sea-chip sea-chips section-head seg share shell side-foot sidebar skill skills sm span2 split stat state stats status-dot sub switch table-wrap tabs tag tip toast tool-brush tool-pan toolbar top top-actions topbar tunable-field tunable-fields tunable-group tunable-hint tunable-wide txt u update-actions update-banner v vals view w warn zc-fish zc-range zc-top zcard zone-canvas-wrap zone-empty-guide zone-legend zone-prop-footer zone-row zone-side zone-side-body zone-side-head zone-statusbar zone-tool zone-tool-sep zone-toolbar zone-workbench zs-row`

// officialFrontendBareClassCounts 是官方页面「裸类选择器」.name 的规则数
// （108 个类 / 148 条规则，格式 name:count）。
// 提取方法：选择器去掉首尾空白后恰为 .name 的规则，按类名计数。
const officialFrontendBareClassCounts = `about-card:1 about-head:2 actions:1 area-badges:1 attach-row:1 bar:1 brand:1 btn:1 card:2 danger-text:1 duo:2 empty:1 event-gift-actions:1 event-gift-card:1 event-gift-editor:1 event-gift-empty:1 event-gift-intro:1 event-gift-item:3 event-gift-items:1 event-gift-picker-groups:1 event-gift-picker-results:1 event-gift-plan:1 event-gift-plan-head:1 event-gift-plans:1 event-gift-preview:1 event-gift-selector:2 event-gift-status:1 form-grid:3 grade-ratio-actions:1 grade-ratio-bar:1 grade-ratio-card:1 grade-ratio-head:2 grade-ratio-row:2 grade-ratio-summary:2 grid:1 guide-close:1 ico:1 legend:1 log:1 login-card:1 logo:1 mail-box:2 main:3 markdown-body:2 mini-input:1 modal:1 modal-bg:2 mono:1 muted:1 nav:4 notice:1 online-dot:1 params-tab:2 params-tabs:2 pill:1 player-flags:1 player-hero:2 player-list:2 player-row:1 prop-sec:1 qa:1 qa-grid:1 release-card:1 release-markdown:2 result:1 results:1 right:1 rule-mini:1 sea-chip:1 sea-chips:1 section-head:3 seg:1 shell:3 side-foot:1 sidebar:3 skill:1 skills:2 split:2 stats:3 status-dot:1 switch:1 table-wrap:1 tabs:2 tag:1 toast:1 toolbar:1 top-actions:1 topbar:2 tunable-field:2 tunable-fields:3 tunable-hint:1 update-banner:1 view:2 zcard:1 zone-canvas-wrap:1 zone-empty-guide:1 zone-legend:1 zone-prop-footer:1 zone-row:1 zone-side:1 zone-side-body:1 zone-side-head:1 zone-statusbar:1 zone-tool:1 zone-tool-sep:1 zone-toolbar:1 zone-workbench:1 zs-row:1`

// officialFrontendCSSVars 是官方 :root 里定义的变量；ai.css 只允许引用这些或新增 --ai-*。
const officialFrontendCSSVars = `bg bg2 panel panel2 line text muted accent accent2 danger warn ok shadow r`

// aiFrontendAssetPaths 是拆分后的完整 AI 前端资源集合（含官方单页）。
var aiFrontendAssetPaths = []string{"index.html", "ai/index.html", "ai/ai.css", "ai/ai.js"}

// aiFrontendOwnAssets 是 web/ai/ 下的三个文件（用于 WebFS/MIME/404 断言）。
var aiFrontendOwnAssets = []string{"ai/index.html", "ai/ai.css", "ai/ai.js"}

var (
	cssCommentPattern   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRulePattern      = regexp.MustCompile(`([^{}]+)\{`)
	cssClassPattern     = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_-]*)`)
	cssVarPattern       = regexp.MustCompile(`--[A-Za-z0-9_-]+`)
	bareClassPattern    = regexp.MustCompile(`^\.[A-Za-z_][A-Za-z0-9_-]*$`)
	selectorRootPattern = regexp.MustCompile(`^#view-ai(?:$|[\s>.:#\[])`)
	htmlTagPattern      = regexp.MustCompile(`(?i)\b(body|html)\b`)
	emergencyAssign     = regexp.MustCompile(`setText\('aiStatusEmergency',\s*([^;\n]+)\);`)
	destinationAssign   = regexp.MustCompile(`setText\('aiStatusDestination',\s*([^;\n]+)\);`)
	aiModuleScriptTag   = `<script type="module" src="ai/ai.js"></script>`
)

// TestAIFrontendContract 覆盖票 §4 的六类断言，每类对应一个具体回归风险。
func TestAIFrontendContract(t *testing.T) {
	sources := readAIFrontendSources(t)
	index := sources["index.html"]
	fragment := sources["ai/index.html"]
	css := sources["ai/ai.css"]
	js := sources["ai/ai.js"]

	// 断言 1：前端不得自抄安全默认值（「缺省即开/即关」的本地解释）。
	t.Run("no_local_security_default", func(t *testing.T) {
		for _, issue := range noLocalSecurityDefaultIssues(sources) {
			t.Error(issue)
		}
	})

	// 断言 2：前端只消费后端状态；急停与实际去向都来自后端字段。
	t.Run("consumes_backend_status_description", func(t *testing.T) {
		for _, want := range []string{
			"function applyAIStatus(d) {",
			"S.settingStates = sec.keys || {};",
			"const actor = (sec.keys || {}).ai_actor_enabled || {};",
			"S.settingStates = (d.security || {}).keys || {};",
			"const badgeState = (S.settingStates || {})[spec.key];",
			"setText('aiKeyState', (d.credential || {}).display",
			"setText('aiSaveState', d.saved_display",
			"revision: previousRevision,",
			"current.revision !== d.revision",
		} {
			if !strings.Contains(js, want) {
				t.Errorf("AI 前端未消费后端状态，缺少 %q", want)
			}
		}
		// 急停状态卡只认后端回读的 emergency_stop，不得再用「外发开/关」文案，
		// 也不得拿控件勾选值或通用「已保存」当运行事实。
		if got := strings.Count(sources["ai/index.html"]+js, "aiStatusEmergency"); got != 2 {
			t.Errorf("#aiStatusEmergency 出现 %d 次，期望 2 次（1 个挂载元素 + 1 个显示写入点）", got)
		}
		matches := emergencyAssign.FindAllStringSubmatch(js, -1)
		if len(matches) != 1 {
			t.Fatalf("急停显示写入点 = %d 个，期望恰好 1 个", len(matches))
		}
		if strings.TrimSpace(matches[0][1]) != "text" {
			t.Errorf("急停显示应只由 setEmergencyCard 统一写入，得到 %q", matches[0][1])
		}
		emergencyBody := jsFunctionBody(t, js, "function applyEmergencyStatus(d) {")
		for _, want := range []string{
			"const em = (d || {}).emergency_stop;",
			"em.display",
			"'待保存'",
			"'正在保存'",
			"'状态待确认，请刷新'",
		} {
			if !strings.Contains(emergencyBody, want) {
				t.Errorf("急停状态卡缺少 %q", want)
			}
		}
		for _, banned := range []string{"security.egress", "sec.egress", "egress.display"} {
			if strings.Contains(emergencyBody, banned) {
				t.Errorf("急停状态卡不得再消费外发方向文案（命中 %q）", banned)
			}
		}
		if !strings.Contains(js, "seq !== aiStatusSeq") {
			t.Error("状态回读必须有迟到响应保护，不得覆盖更新的状态")
		}
		if got := strings.Count(sources["ai/index.html"]+js, "aiStatusDestination"); got != 2 {
			t.Errorf("#aiStatusDestination 出现 %d 次，期望 2 次（1 个挂载元素 + 1 个显示写入点）", got)
		}
		destMatches := destinationAssign.FindAllStringSubmatch(js, -1)
		if len(destMatches) != 1 {
			t.Fatalf("去向显示写入点 = %d 个，期望恰好 1 个", len(destMatches))
		}
		if !strings.Contains(destMatches[0][1], "destDisplay(d.destination)") {
			t.Errorf("去向显示未消费后端 destination：%q", destMatches[0][1])
		}
	})

	// 断言 3：旧闸（出网开关、出口名单、授权确认/撤销）不得留在页面或动态注册表里。
	t.Run("no_old_gate_entries", func(t *testing.T) {
		for _, issue := range oldGateIssues(sources, AISettingSpecs) {
			t.Error(issue)
		}
	})

	// 断言 4：复测只发已保存配置；保存动作携带活动修订。
	t.Run("retest_uses_saved_config_only", func(t *testing.T) {
		body := jsFunctionBody(t, js, "async function testAI() {")
		for _, banned := range []string{"$('aiBaseURL')", "$('aiModelID')", "$('aiKey')"} {
			if strings.Contains(body, banned) {
				t.Errorf("复测不得读取页面草稿 %q", banned)
			}
		}
		if !strings.Contains(body, "'/api/ai/test'") || !strings.Contains(body, "body: '{}'") {
			t.Error("复测必须只 POST /api/ai/test 且不携带页面输入")
		}
		save := jsFunctionBody(t, js, "async function saveAIConfig() {")
		if !strings.Contains(save, "revision:") {
			t.Error("保存必须携带页面看到的活动修订")
		}
		for _, banned := range []string{"consent", "outbound_enabled", "ai_consent", "egress_allowed_hosts"} {
			if strings.Contains(save, banned) {
				t.Errorf("保存动作不得携带旧闸字段（命中 %q）", banned)
			}
		}
	})

	// 断言 5：AI 页挂载点存在且为空，官方 #view-* 集合未被改写，片段不得重复挂载。
	t.Run("ai_mount_preserves_official_views", func(t *testing.T) {
		for _, issue := range viewMountIssues(index, fragment) {
			t.Error(issue)
		}
	})

	// 断言 6：AI 所有 CSS 只在 ai.css，且每个逗号分支都以 #view-ai 为根。
	t.Run("ai_css_scoped_to_ai_classes", func(t *testing.T) {
		for _, issue := range aiCSSBoundaryIssues(css) {
			t.Error(issue)
		}
		assertOfficialStyleUnchanged(t, index, css)
	})

	// 附加：窄桥不得暴露 TOKEN，AI 资源不得读写 localStorage/cookie，模块不得改写全局 go。
	t.Run("bridge_does_not_leak_token", func(t *testing.T) {
		for _, name := range aiFrontendOwnAssets {
			src := sources[name]
			for _, banned := range []string{"localStorage", "document.cookie"} {
				if strings.Contains(src, banned) {
					t.Errorf("%s 不得读写登录态（命中 %q）", name, banned)
				}
			}
		}
		if regexp.MustCompile(`window\s*\.\s*go\s*=`).MatchString(js) {
			t.Error("ai.js 不得 monkey patch 全局 go")
		}
		start := strings.Index(index, "window.RF4Admin=Object.freeze({")
		if start < 0 {
			t.Fatal("index.html 缺少窄桥 window.RF4Admin")
		}
		end := strings.Index(index[start:], "});")
		if end < 0 {
			t.Fatal("窄桥 window.RF4Admin 未闭合")
		}
		bridge := index[start : start+end]
		for _, banned := range []string{"TOKEN", "localStorage", "document.cookie"} {
			if strings.Contains(bridge, banned) {
				t.Errorf("窄桥不得暴露/读写 %q", banned)
			}
		}
		for _, want := range []string{"api:", "toast:", "registerExternalView:", "onLogout:"} {
			if !strings.Contains(bridge, want) {
				t.Errorf("窄桥缺少 %q", want)
			}
		}
	})
}

// TestT105FiveModuleFrontendContract 固化 B-103 五模块与四步总览的静态边界：
// 页面只留五个设置模块；角色表单不再手填 UID/工具/说话方式；预算按组并排；
// 钓鱼战绩注入归钓鱼；能力区一次保存并区分已保存/已生效。
func TestT105FiveModuleFrontendContract(t *testing.T) {
	sources := readAIFrontendSources(t)
	fragment := sources["ai/index.html"]
	js := sources["ai/ai.js"]
	css := sources["ai/ai.css"]

	for _, tab := range []string{"overview", "conn", "agents", "talk", "fishing", "personality"} {
		want := `data-ai-tab="` + tab + `"`
		if got := strings.Count(fragment, want); got != 1 {
			t.Errorf("模块标签 %s 出现 %d 次，期望 1 次", tab, got)
		}
	}
	for _, want := range []string{
		`id="aiSteps"`, `id="aiOverviewPrimary"`, `id="aiHelloGuide"`,
		`id="aiSettings-budget"`, `id="aiSettings-fishing"`, `id="aiCapList"`,
		`data-action="save-capabilities"`, `data-action="retry-capabilities"`,
		`data-action="copy-hello"`, `data-action="save-emergency"`,
	} {
		if !strings.Contains(fragment, want) {
			t.Errorf("片段缺少 %q", want)
		}
	}

	// 旧操作与旧术语不得回到页面或动态渲染。
	banned := []string{
		"aiNewUID", "aiNewTools", "aiEditTools", "aiNewReplyMode", "aiNewTalk",
		"aiTestFishingAgent", "aiFishingTalkTestCard", "AI_REPLY_MODES",
		"save-agent-behavior", "save-actor", "save-fishing-talk", "test-fishing-talk",
		"说话方式", "活跃度", "工具白名单", "上鱼报话", "角色模拟", "发鱼测试",
		"请服主分配角色",
	}
	for _, text := range banned {
		if strings.Contains(fragment, text) {
			t.Errorf("片段仍含旧操作/旧术语 %q", text)
		}
		if strings.Contains(js, text) {
			t.Errorf("ai.js 仍含旧操作/旧术语 %q", text)
		}
	}

	// 新增/编辑都不再提交 UID 或旧工具字段。
	create := jsFunctionBody(t, js, "async function createAI() {")
	for _, banned := range []string{"contact_uid:", "tools:", "reply_mode", "talkativeness"} {
		if strings.Contains(create, banned) {
			t.Errorf("新增 Agent 仍提交旧字段 %q", banned)
		}
	}
	edit := jsFunctionBody(t, js, "async function saveAIEdit(key, part) {")
	if strings.Contains(edit, "tools") {
		t.Errorf("编辑 Agent 仍提交旧 tools 字段")
	}
	for _, want := range []string{"只保存基础资料", "只保存思考档"} {
		if !strings.Contains(fragment+js, want) {
			t.Errorf("编辑弹窗缺少单项重试入口 %q", want)
		}
	}

	// 预算：第一行上下文/历史，第二行软目标/硬上限，按组保存；错组不提交。
	rows := jsFunctionBody(t, js, "function budgetRows(group) {")
	for _, order := range [][2]string{
		{"_context_tokens", "_history_messages"},
		{"_reply_soft_chars", "_reply_chars"},
	} {
		if strings.Index(rows, order[0]) < 0 || strings.Index(rows, order[1]) < 0 ||
			strings.Index(rows, order[0]) > strings.Index(rows, order[1]) {
			t.Errorf("预算行顺序不符：%s 应在 %s 前", order[0], order[1])
		}
	}
	if !strings.Contains(js, "软目标不能大于硬上限") || !strings.Contains(js, "本组有") {
		t.Error("预算缺少软≤硬校验或整组拒绝提交提示")
	}
	if !strings.Contains(css, "@media(max-width:720px)") || !strings.Contains(css, "ai-budget-grid") {
		t.Error("预算缺少窄屏单列规则")
	}

	// 钓鱼：历史/今日分开；清除只清战绩摘要；生产页没有发鱼测试入口。
	for _, want := range []string{"历史累计", "今日（0 点起）", "清除此角色的星蓝战绩摘要，渔获流水仍保留"} {
		if !strings.Contains(fragment+js, want) {
			t.Errorf("钓鱼面板缺少 %q", want)
		}
	}
	if strings.Contains(fragment+js, "aiFishingTalkEnqueue") || strings.Contains(fragment+js, "/api/ai/test/fishing-talk") {
		t.Error("生产页不得再暴露发鱼测试入口")
	}

	// 能力：一次保存提交完整集合与 base_revision；已保存与已生效分开。
	save := jsFunctionBody(t, js, "async function saveCapabilities(retry) {")
	for _, want := range []string{"/api/ai/capabilities", "base_revision", "capabilities: caps", "CAPABILITY_AVAILABILITY", "d.revision"} {
		if !strings.Contains(save, want) {
			t.Errorf("能力保存缺少 %q", want)
		}
	}
	if !strings.Contains(js, "已保存修订") || !strings.Contains(js, "commandStatusText") {
		t.Error("能力区未区分已保存修订与命令/生效状态")
	}

	// 首句失败要回到对应步骤并给可直接重试的动作，不显示假回复。
	if !strings.Contains(js, "last_reply_status === 'fallback'") || !strings.Contains(js, "last_reply_error") {
		t.Error("首句步骤未消费 AI 回复失败原因")
	}

	// 总览状态优先级与键盘可达/模态焦点回归。
	for _, want := range []string{"missing_key", "connection_failed", "command_pending", "ready", "login", "emergency"} {
		if !strings.Contains(js, want) {
			t.Errorf("总览缺少状态优先级 %q", want)
		}
	}
	for _, want := range []string{"trapModalFocus", "S.lastFocus", "back.focus()"} {
		if !strings.Contains(js, want) {
			t.Errorf("模态缺少焦点恢复/键盘约束 %q", want)
		}
	}
	if !strings.Contains(css, "focus-visible") || !strings.Contains(css, "overflow-wrap:anywhere") {
		t.Error("缺少键盘焦点样式或窄屏防溢出规则")
	}
}

// TestAIFrontendAssetsServed 断言 web/ai/** 被 embed、能经 FileServer 返回 200 + 正确 MIME，
// AI 源文件带 no-cache，官方页面缓存语义不变，404 不被当作片段。
func TestAIFrontendAssetsServed(t *testing.T) {
	for _, name := range aiFrontendOwnAssets {
		raw, err := fs.ReadFile(WebFS(), name)
		if err != nil {
			t.Fatalf("WebFS 打不开 %s: %v", name, err)
		}
		if len(raw) == 0 {
			t.Fatalf("WebFS 资源 %s 为空", name)
		}
	}

	srv := httptest.NewServer(WebHandler(WebFS()))
	defer srv.Close()

	for _, tc := range []struct {
		path  string
		mime  string
		cache string
	}{
		{"/ai/ai.js", "javascript", "no-cache"},
		{"/ai/ai.css", "text/css", "no-cache"},
		{"/ai/index.html", "text/html", "no-cache"},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d，期望 200", tc.path, resp.StatusCode)
		}
		if got := strings.ToLower(resp.Header.Get("Content-Type")); !strings.Contains(got, tc.mime) {
			t.Errorf("GET %s Content-Type = %q，期望包含 %q", tc.path, got, tc.mime)
		}
		if got := resp.Header.Get("Cache-Control"); got != tc.cache {
			t.Errorf("GET %s Cache-Control = %q，期望 %q", tc.path, got, tc.cache)
		}
		if len(body) == 0 {
			t.Errorf("GET %s 返回空响应体", tc.path)
		}
	}

	// 官方页面缓存语义不得被 AI 规则改变。
	resp, err := http.Get(srv.URL + "/index.html")
	if err != nil {
		t.Fatalf("GET /index.html: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got == "no-cache" {
		t.Errorf("官方 index.html 的缓存语义被改成 no-cache，期望保持原样")
	}

	// 404 不得被当作 AI 片段。
	resp, err = http.Get(srv.URL + "/ai/does-not-exist.js")
	if err != nil {
		t.Fatalf("GET 404 探针: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的 AI 资源返回 %d，期望 404", resp.StatusCode)
	}
}

// TestAIFrontendCSSBoundaryNegatives 断言四类负例都会变红；前三例直接喂给 CSS 边界检查，
// 第四例（重复挂载）喂给页面挂载检查。
func TestAIFrontendCSSBoundaryNegatives(t *testing.T) {
	for _, tc := range []struct {
		name string
		css  string
	}{
		{"块外 .card .section-head", `.card .section-head{color:red}`},
		{"逗号第二分支逃逸", `#view-ai .ai-x, .ai-y{color:red}`},
		{"@media 内逃逸", `@media(max-width:720px){.ai-x{color:red}}`},
	} {
		if issues := aiCSSBoundaryIssues(tc.css); len(issues) == 0 {
			t.Errorf("负例「%s」未被拒绝", tc.name)
		}
	}

	index := readEmbeddedAsset(t, "index.html")
	duplicated := index + "\n<section class=\"view\" id=\"view-ai\"></section>"
	if issues := viewMountIssues(duplicated, ""); len(issues) == 0 {
		t.Error("负例「重复挂载」未被拒绝")
	}
}

// readEmbeddedAsset 读取实际 embed 资源，而不是磁盘上的单页。
func readEmbeddedAsset(t *testing.T, name string) string {
	t.Helper()
	// 过渡期：旧单页已改名为 legacy.html（新后台在 web/app/），旧 AI 模块仍挂在旧单页上。
	// 这里的契约继续守护旧单页；新后台的同类安全断言见 app_frontend_contract_test.go。
	if name == "index.html" {
		name = "legacy.html"
	}
	raw, err := fs.ReadFile(WebFS(), name)
	if err != nil {
		t.Fatalf("读取内嵌资源 %s: %v", name, err)
	}
	if len(raw) == 0 {
		t.Fatalf("内嵌资源 %s 为空", name)
	}
	return string(raw)
}

// readAIFrontendSources 读取完整资源集合：官方单页 + web/ai/ 三个文件。
func readAIFrontendSources(t *testing.T) map[string]string {
	t.Helper()
	out := make(map[string]string, len(aiFrontendAssetPaths))
	for _, name := range aiFrontendAssetPaths {
		out[name] = readEmbeddedAsset(t, name)
	}
	return out
}

// noLocalSecurityDefaultIssues 扫完整资源集合，避免只检查官方单页而漏掉迁出的 ai.js。
func noLocalSecurityDefaultIssues(sources map[string]string) []string {
	var issues []string
	banned := []string{
		"outbound_enabled!=='0'",
		"outbound_enabled !== '0'",
		"outbound_enabled==='1'",
		"outbound_enabled === '1'",
		"st.outbound_enabled",
		"ai_actor_enabled!=='0'",
		"ai_actor_enabled !== '0'",
		"emergency_stop!=='0'",
		"emergency_stop !== '0'",
		"emergency_stop==='1'",
		"emergency_stop === '1'",
	}
	compactBanned := []string{
		"outbound_enabled!==",
		"outbound_enabled===",
		"ai_actor_enabled!==",
		"ai_actor_enabled===",
		"emergency_stop!==",
		"emergency_stop===",
		"st.outbound_enabled",
		"st.emergency_stop",
		"st.ai_actor_enabled",
		// V2：旧闸不得以任何形式回到前端。
		"aiStatusOutbound",
		"aiEgressHosts",
		"applyAISecurity",
		"renderAIConsent",
		"confirmAIConsent",
		"revokeAIConsent",
	}
	for name, src := range sources {
		for _, text := range banned {
			if strings.Contains(src, text) {
				issues = append(issues, fmt.Sprintf("%s 仍在自抄安全默认值: %q", name, text))
			}
		}
		compact := strings.Join(strings.Fields(src), "")
		for _, text := range compactBanned {
			if strings.Contains(compact, text) {
				issues = append(issues, fmt.Sprintf("%s 仍在自抄安全默认值（去空白后命中）: %q", name, text))
			}
		}
	}
	sort.Strings(issues)
	return issues
}

// oldGateIssues 扫描 HTML/JS 与动态参数注册表：V2 不得再出现旧闸入口
// （出网开关、出口名单、授权确认/撤销、整卡授权键）。
func oldGateIssues(sources map[string]string, specs []SettingSpec) []string {
	var issues []string
	banned := []string{
		"outbound_enabled", "aiStatusOutbound", "aiEgressHosts",
		"applyAISecurity", "renderAIConsent", "confirmAIConsent", "revokeAIConsent",
		"ai_consent", "/api/ai/consent/", "egress_allowed_hosts",
		"出网总闸", "出口白名单", "显式授权与撤销",
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, text := range banned {
			if strings.Contains(sources[name], text) {
				issues = append(issues, fmt.Sprintf("%s 仍含旧闸入口 %q", name, text))
			}
		}
	}
	raw, err := json.Marshal(specs)
	if err != nil {
		issues = append(issues, fmt.Sprintf("动态参数注册表无法序列化: %v", err))
		return issues
	}
	if strings.Contains(string(raw), "outbound_enabled") {
		issues = append(issues, "动态参数注册表仍暴露 outbound_enabled")
	}
	return issues
}

// viewMountIssues 返回挂载/边界问题；返回空切片表示通过。
func viewMountIssues(index, fragment string) []string {
	var issues []string
	expected := wordSet(officialFrontendViewMounts)
	expected["view-ai"] = true

	actual := map[string]int{}
	for _, m := range regexp.MustCompile(`id="(view-[A-Za-z0-9_-]+)"`).FindAllStringSubmatch(index, -1) {
		actual[m[1]]++
	}
	for id, n := range actual {
		if !expected[id] {
			issues = append(issues, fmt.Sprintf("出现未登记的 #view-* 挂载点 %q", id))
		}
		if n != 1 {
			issues = append(issues, fmt.Sprintf("#view-* 挂载点 %q 出现 %d 次，期望恰好 1 次", id, n))
		}
	}
	var missing []string
	for id := range expected {
		if actual[id] == 0 {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		issues = append(issues, fmt.Sprintf("官方 #view-* 挂载点被删除/改写: %v", missing))
	}
	if !regexp.MustCompile(`<section class="view" id="view-ai">\s*</section>`).MatchString(index) {
		issues = append(issues, "AI 挂载点必须是空的 <section class=\"view\" id=\"view-ai\"></section>")
	}
	if got := strings.Count(index, aiModuleScriptTag); got != 1 {
		issues = append(issues, fmt.Sprintf("index.html 的 AI module 引用出现 %d 次，期望恰好 1 次", got))
	}
	if got := len(regexp.MustCompile(`id="view-[A-Za-z0-9_-]+"`).FindAllString(fragment, -1)); got != 0 {
		issues = append(issues, fmt.Sprintf("AI 片段含 %d 个 #view-* 挂载点（重复挂载）", got))
	}
	for _, banned := range []string{
		"AI 助手页", "ai-status", "ai-tabs", "aiBridgeStatus", "aiAgentList",
		"aiConsentItems", "aiMapName", "aiNewKey", "applyAISecurity", "loadAIChannels",
		"pollAICommand", "aiAgentMeta", "AI_SETTING_STATES", "AI_REPLY_MODES", "AI_TAB_MODULES",
	} {
		if strings.Contains(index, banned) {
			issues = append(issues, fmt.Sprintf("index.html 仍含 AI 专用内容 %q", banned))
		}
	}
	return issues
}

// aiCSSBoundaryIssues 是保守 CSS 子集解析：只支持注释、@media、@keyframes 与普通规则；
// 其他嵌套/at 规则一律拒绝，不跳过。
func aiCSSBoundaryIssues(css string) []string {
	var issues []string
	code := cssCommentPattern.ReplaceAllString(css, "")
	if strings.Contains(code, "@import") {
		issues = append(issues, "ai.css 不得使用 @import")
	}
	if strings.Contains(code, "http://") || strings.Contains(code, "https://") {
		issues = append(issues, "ai.css 不得引用外部 URL")
	}
	officialClasses := wordSet(officialFrontendCSSClasses)
	officialVars := wordSet(officialFrontendCSSVars)
	for _, variable := range cssVarPattern.FindAllString(code, -1) {
		name := strings.TrimPrefix(variable, "--")
		if officialVars[name] || strings.HasPrefix(name, "ai-") {
			continue
		}
		issues = append(issues, fmt.Sprintf("ai.css 使用了未登记的变量 %q（新变量必须以 --ai- 开头）", variable))
	}

	parsed, parseIssues := parseAICSS(css)
	issues = append(issues, parseIssues...)
	for _, name := range parsed.keyframes {
		if !strings.HasPrefix(name, "ai-") {
			issues = append(issues, fmt.Sprintf("keyframes %q 未使用 ai- 前缀", name))
		}
	}
	for _, selector := range parsed.selectors {
		if !selectorRootPattern.MatchString(selector) {
			issues = append(issues, fmt.Sprintf("选择器 %q 未从 #view-ai 根开始", selector))
			continue
		}
		if rest := strings.TrimSpace(strings.TrimPrefix(selector, "#view-ai")); strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "~") {
			issues = append(issues, fmt.Sprintf("选择器 %q 是逃出根的兄弟选择器", selector))
		}
		if htmlTagPattern.MatchString(selector) || strings.Contains(selector, ":root") {
			issues = append(issues, fmt.Sprintf("选择器 %q 不得命中全局 body/html/:root", selector))
		}
		for _, m := range cssClassPattern.FindAllStringSubmatch(selector, -1) {
			name := m[1]
			if officialClasses[name] || strings.HasPrefix(name, "ai-") {
				continue
			}
			issues = append(issues, fmt.Sprintf("选择器 %q 使用了未登记且无 ai- 前缀的 class %q", selector, name))
		}
	}
	return issues
}

type parsedCSS struct {
	selectors []string
	keyframes []string
}

// parseAICSS 只解析实际使用的保守子集；未知嵌套语法返回 issue 而不是跳过。
func parseAICSS(css string) (parsedCSS, []string) {
	var out parsedCSS
	var issues []string
	src := cssCommentPattern.ReplaceAllString(css, "")
	pos := 0
	for {
		for pos < len(src) && strings.ContainsRune(" \t\r\n", rune(src[pos])) {
			pos++
		}
		if pos >= len(src) {
			break
		}
		open := strings.IndexByte(src[pos:], '{')
		semi := strings.IndexByte(src[pos:], ';')
		if open < 0 {
			issues = append(issues, "CSS 规则缺少规则体")
			break
		}
		if semi >= 0 && semi < open {
			header := strings.TrimSpace(src[pos : pos+semi])
			issues = append(issues, fmt.Sprintf("不支持的无规则体 @ 语句 %q", header))
			pos += semi + 1
			continue
		}
		header := strings.TrimSpace(src[pos : pos+open])
		closeAt, ok := matchCSSBrace(src, pos+open)
		if !ok {
			issues = append(issues, fmt.Sprintf("规则体未闭合：%q", header))
			break
		}
		body := src[pos+open+1 : closeAt]
		switch {
		case strings.HasPrefix(header, "@media"):
			inner, innerIssues := parseAICSS(body)
			out.selectors = append(out.selectors, inner.selectors...)
			out.keyframes = append(out.keyframes, inner.keyframes...)
			issues = append(issues, innerIssues...)
		case strings.HasPrefix(header, "@keyframes"):
			name := strings.TrimSpace(strings.TrimPrefix(header, "@keyframes"))
			out.keyframes = append(out.keyframes, name)
		case strings.HasPrefix(header, "@"):
			issues = append(issues, fmt.Sprintf("不支持的 @ 规则 %q", header))
		default:
			if strings.Contains(body, "{") {
				issues = append(issues, fmt.Sprintf("未知嵌套语法 %q", header))
			}
			for _, selector := range strings.Split(header, ",") {
				if selector = strings.TrimSpace(selector); selector != "" {
					out.selectors = append(out.selectors, selector)
				}
			}
		}
		pos = closeAt + 1
	}
	return out, issues
}

// matchCSSBrace 返回与 open 处 '{' 匹配的 '}' 下标。
func matchCSSBrace(src string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// assertOfficialStyleUnchanged 断言官方 <style> 只移除了 AI 块：官方类集合与裸类规则数不变，
// 且 ai.css 的新类不得出现在官方样式里。
func assertOfficialStyleUnchanged(t *testing.T, index, aiCSS string) {
	t.Helper()
	style := frontendStyleBlock(t, index)
	official := wordSet(officialFrontendCSSClasses)
	current := frontendClassSet(style)

	var missing []string
	for name := range official {
		if !current[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("官方 class 选择器被删除/改名: %v", missing)
	}
	for name := range frontendClassSet(aiCSS) {
		if official[name] {
			continue
		}
		if strings.Contains(style, "."+name) {
			t.Errorf("AI 新类 %q 出现在官方 <style> 中（AI 样式必须只在 ai.css）", name)
		}
	}

	wantBare := parseOfficialBareClassCounts(t)
	gotBare := frontendBareClassCounts(style)
	for name, want := range wantBare {
		if got := gotBare[name]; got != want {
			t.Errorf("官方裸类选择器 .%s 的规则数由 %d 变为 %d", name, want, got)
		}
	}
	for name := range official {
		if _, tracked := wantBare[name]; tracked {
			continue
		}
		if got := gotBare[name]; got != 0 {
			t.Errorf("新增了官方类 .%s 的裸类规则（官方基线 0 条，实际 %d 条）", name, got)
		}
	}
}

// frontendStyleBlock 取 index.html 里唯一的 <style> 内容。
func frontendStyleBlock(t *testing.T, html string) string {
	t.Helper()
	const open, close = "<style>", "</style>"
	i := strings.Index(html, open)
	j := strings.Index(html, close)
	if i < 0 || j < i {
		t.Fatalf("web/index.html 缺少 <style> 块")
	}
	return html[i+len(open) : j]
}

// frontendSelectors 提取 CSS 选择器：去注释后按规则头拆分，跳过 @ 规则头。
func frontendSelectors(css string) []string {
	css = cssCommentPattern.ReplaceAllString(css, "")
	var out []string
	for _, m := range cssRulePattern.FindAllStringSubmatch(css, -1) {
		head := strings.TrimSpace(m[1])
		if head == "" || strings.HasPrefix(head, "@") {
			continue
		}
		for _, sel := range strings.Split(head, ",") {
			if sel = strings.TrimSpace(sel); sel != "" {
				out = append(out, sel)
			}
		}
	}
	return out
}

// frontendClassSet 返回 CSS 选择器里出现过的全部 class 名。
func frontendClassSet(css string) map[string]bool {
	out := map[string]bool{}
	for _, sel := range frontendSelectors(css) {
		for _, m := range cssClassPattern.FindAllStringSubmatch(sel, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// frontendBareClassCounts 统计裸类选择器 .name 的规则数。
func frontendBareClassCounts(css string) map[string]int {
	out := map[string]int{}
	for _, sel := range frontendSelectors(css) {
		if bareClassPattern.MatchString(sel) {
			out[sel[1:]]++
		}
	}
	return out
}

// parseOfficialBareClassCounts 解析手写基线常量 name:count。
func parseOfficialBareClassCounts(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, entry := range strings.Fields(officialFrontendBareClassCounts) {
		name, num, ok := strings.Cut(entry, ":")
		if !ok {
			t.Fatalf("官方裸类规则基线格式非法: %q", entry)
		}
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			t.Fatalf("官方裸类规则基线计数非法: %q", entry)
		}
		out[name] = n
	}
	return out
}

// wordSet 把空格分隔的字面量转成集合。
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		out[w] = true
	}
	return out
}

// jsFunctionBody 按花括号配对截取 JS 函数体（适配模块的 2 空格缩进）。
func jsFunctionBody(t *testing.T, src, signature string) string {
	t.Helper()
	i := strings.Index(src, signature)
	if i < 0 {
		t.Fatalf("未找到前端函数 %q", signature)
	}
	open := strings.IndexByte(src[i:], '{')
	if open < 0 {
		t.Fatalf("函数 %q 缺少函数体", signature)
	}
	start := i + open
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : j+1]
			}
		}
	}
	t.Fatalf("函数 %q 未闭合", signature)
	return ""
}
