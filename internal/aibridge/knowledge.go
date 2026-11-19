package aibridge

// 知识来源：默认只查内部只读资料（静态库）；联网搜索默认关，后台手动打开并配置端点后才生效。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

var hanWords = regexp.MustCompile(`[\p{Han}]{2,}`)

// fishNameStopRunes 是查鱼问题里常见的非鱼名用字，单字模糊匹配时跳过，
// 避免「在哪」「什么」把匹配带到无关鱼种。
var fishNameStopRunes = map[rune]bool{}

func init() {
	// 水域前缀字也跳过：玩家说「河鲈」，若先按「河」匹配会落到「河螯虾」，
	// 跳过「河湖海江」后由核心字「鲈」命中「鲈鱼」。
	for _, r := range "在哪什么饵钓点怎推荐去请帮我用的吗呢用查好有是个不上下大小一二三这那请问河湖海江" {
		fishNameStopRunes[r] = true
	}
}

// ResolveFishName 用静态库里的真实鱼种名做归一化：
// 玩家说「河鲈」，库里叫「鲈鱼」，用双字片段与单字回退找到库里存在的名字。
// 返回的一定是库中真实名称，不臆造鱼种。
func (k *Knowledge) ResolveFishName(ctx context.Context, query string) string {
	if k.static == nil {
		return ""
	}
	rows, err := k.static.QueryContext(ctx, `
		SELECT DISTINCT name_zh FROM loc_fish
		WHERE name_zh<>'' ORDER BY LENGTH(name_zh), name_zh`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	names := make([]string, 0, 512)
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	for _, kw := range keywordsOf(query) {
		w := strings.Trim(kw, "%")
		for _, n := range names {
			if strings.Contains(n, w) {
				return n
			}
		}
	}
	for _, ch := range query {
		if !unicode.Is(unicode.Han, ch) || fishNameStopRunes[ch] {
			continue
		}
		s := string(ch)
		for _, n := range names {
			if strings.Contains(n, s) {
				return n
			}
		}
	}
	return ""
}

// Knowledge 持有只读静态库连接与可选的外部搜索端点。
type Knowledge struct {
	static *sql.DB
	client *http.Client
	gate   EgressGate
}

// SetEgressGate 安装联网搜索出口前的判定；拒绝时 WebSearch 直接返回空串，
// 不构造、也不发出外部请求。判定逻辑与模型调用共用 aiconfig（V2 只查急停）。
func (k *Knowledge) SetEgressGate(gate EgressGate) { k.gate = gate }

// OpenKnowledge 以只读 URI 打开静态库：文件缺失时直接报错，不创建空库；
// mode=ro 与每连接 query_only 保证运行期不可能写入静态资料。
func OpenKnowledge(path string) (*Knowledge, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("静态配置库不可用: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Knowledge{
		static: db,
		// 与模型调用共用同一套传输层防护（HTTPS、解析地址、重定向、证书）。
		client: newEgressHTTPClient(5*time.Second, nil),
	}, nil
}

func (k *Knowledge) Close() error {
	if k.static != nil {
		return k.static.Close()
	}
	return nil
}

// Internal 用问题里的中文片段检索内部静态资料，返回可直接注入提示词的文本。
func (k *Knowledge) Internal(ctx context.Context, query string) string {
	if k.static == nil {
		return ""
	}
	likes := keywordsOf(query)
	if len(likes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, like := range likes {
		rows, err := k.static.QueryContext(ctx, `
			SELECT name_zh, loc FROM loc_fish WHERE name_zh LIKE ? LIMIT 3`, like)
		if err == nil {
			for rows.Next() {
				var name, loc string
				if rows.Scan(&name, &loc) == nil {
					fmt.Fprintf(&b, "鱼种:%s 水域:%s\n", name, loc)
				}
			}
			_ = rows.Close()
		}
		items, err := k.static.QueryContext(ctx, `
			SELECT name_zh FROM item_catalog WHERE name_zh LIKE ? LIMIT 2`, like)
		if err == nil {
			for items.Next() {
				var name string
				if items.Scan(&name) == nil {
					fmt.Fprintf(&b, "物品:%s\n", name)
				}
			}
			_ = items.Close()
		}
		if b.Len() > 1000 {
			break
		}
	}
	s := b.String()
	if r := []rune(s); len(r) > 1000 {
		s = string(r[:1000])
	}
	return s
}

// sanitizeSearchQuery 在出网检索前去掉长数字与长标识符，避免把 UID、
// token 或本机路径带出。只做保守替换，不改写正常钓法问题。
func sanitizeSearchQuery(q string) string {
	q = regexp.MustCompile(`\d{6,}`).ReplaceAllString(q, "[数字]")
	q = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`).ReplaceAllString(q, "[标识]")
	return trimRunes(strings.TrimSpace(q), 200)
}

// WebSearch 调用后台配置的外部检索端点。端点为空时返回空串（仍只用内部资料）。
// 期望 JSON {"results":[{"title","snippet"}]}，也接受纯文本。
func (k *Knowledge) WebSearch(ctx context.Context, query, endpoint string) string {
	u := strings.TrimSpace(endpoint)
	if u == "" || k.client == nil {
		return ""
	}
	query = sanitizeSearchQuery(query)
	if query == "" {
		return ""
	}
	// 搜索也是出网出口：每次真正请求前重查当前急停，不依赖调用方早先的判断。
	if k.gate != nil {
		if err := k.gate(ctx); err != nil {
			return ""
		}
	}
	if strings.Contains(u, "?") {
		u += "&q=" + url.QueryEscape(query)
	} else {
		u += "?q=" + url.QueryEscape(query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &out) == nil && len(out.Results) > 0 {
		var b strings.Builder
		for i, r := range out.Results {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "%s: %s\n", r.Title, r.Snippet)
		}
		return trimRunes(b.String(), 800)
	}
	return trimRunes(string(raw), 800)
}

// trimRunes 截断到 n 个字符（按 rune，不切坏中文）。
func trimRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
