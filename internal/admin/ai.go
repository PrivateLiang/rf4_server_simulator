package admin

// AI 设置后台：厂商/模型/API Key、思考强度、出网二分开关。
// API Key 用与 aibridge 相同的主密钥加密，明文不落库、不回显、不进日志。

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/store"
)

// ConfigureAI 由进程入口在读取配置后调用，让后台与 aibridge 使用同一份
// 主密钥路径和观测地址，避免两边各用一套默认值导致 Key 解不开。
func ConfigureAI(masterKeyFile, bridgeAddr string) {
	if strings.TrimSpace(masterKeyFile) != "" {
		aiMasterKeyFile = strings.TrimSpace(masterKeyFile)
	}
	if strings.TrimSpace(bridgeAddr) != "" {
		aiBridgeAddr = strings.TrimSpace(bridgeAddr)
	}
}

var (
	aiMasterKeyFile = "data/ai_master.key"
	aiBridgeAddr    = "127.0.0.1:9504"
)

// aiMasterKeyPath 优先使用进程入口注入的配置，其次环境变量。
func aiMasterKeyPath() string {
	if v := os.Getenv("RF4_AI_MASTER_KEY_FILE"); v != "" {
		return v
	}
	return aiMasterKeyFile
}

// aiToolCatalog 是 Agent 可勾选的只读工具清单。前台只渲染这份目录，
// 避免出现「填了个不存在的工具名，AI 永远用不上」的情况。
// 工具的真正实现在 internal/aibridge（Registry + fish_query 远程调用）。
var aiToolCatalog = []struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}{
	{"fish_query", "查鱼", "查某鱼在这张图用什么饵、什么点位（走游戏内实时数据）"},
	{"species_lookup", "鱼种资料", "按名字查鱼种的中文名与出没水域"},
	{"bait_pref", "饵料偏好", "查某种鱼常吃的饵"},
	{"spot_lookup", "点位", "查某张图的钓点坐标"},
	{"item_lookup", "物品资料", "查装备、道具的基本资料"},
	{"shop_price", "商店价格", "查商店里的售价"},
	{"recipe_lookup", "配方", "查制作配方"},
	{"map_vibe", "陪玩鱼情", "当前地图 + 此刻天气 + 本图鱼种（不含饵料，适合角色聊天）"},
}

func (s *Server) handleAITools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tools": aiToolCatalog})
}

func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings := map[string]string{}
	for _, spec := range AISettingSpecs {
		settings[spec.Key] = s.db.AISetting(ctx, spec.Key, spec.Def)
	}
	var baseURL, modelID, providerName, masked string
	_ = s.db.SQL.QueryRowContext(ctx, `
		SELECT p.name, p.base_url, COALESCE(m.model_id,'')
		FROM ai_provider p LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.enabled=1 ORDER BY p.id LIMIT 1`).Scan(&providerName, &baseURL, &modelID)
	_ = s.db.SQL.QueryRowContext(ctx, `
		SELECT s.masked FROM ai_secret s JOIN ai_provider p ON p.id=s.provider_id
		ORDER BY p.id LIMIT 1`).Scan(&masked)

	type agentView struct {
		Key                string         `json:"key"`
		Display            string         `json:"display"`
		Role               string         `json:"role"`
		ContactUID         int64          `json:"contact_uid"`
		ThinkingEnabled    bool           `json:"thinking_enabled"`
		ThinkingEffort     string         `json:"thinking_effort"`
		Joined             bool           `json:"joined"`
		ActorEnabled       bool           `json:"actor_enabled"`
		ReplyMode          string         `json:"reply_mode"`
		Talkativeness      int            `json:"talkativeness"`
		FishingTalkEnabled bool           `json:"fishing_talk_enabled"`
		Actor              map[string]any `json:"actor,omitempty"`
		Tools              string         `json:"tools"`
		ToolsList          []string       `json:"tools_list"`
		Prompt             string         `json:"prompt"`
	}
	actorByKey := map[string]store.AIActor{}
	if actors, err := s.db.AIListActors(ctx); err == nil {
		for _, a := range actors {
			actorByKey[a.AgentKey] = a
		}
	}
	agents := []agentView{}
	if list, err := s.db.AIListAgents(ctx); err == nil {
		for _, a := range list {
			tools := []string{}
			_ = json.Unmarshal([]byte(a.ToolsJSON), &tools)
			v := agentView{
				Key: a.Key, Display: a.Display, Role: a.Role, ContactUID: a.ContactUID,
				ThinkingEnabled: a.ThinkingEnabled, ThinkingEffort: a.ThinkingEffort,
				Joined: a.Joined, ActorEnabled: a.ActorEnabled,
				ReplyMode: a.ReplyMode, Talkativeness: a.Talkativeness,
				FishingTalkEnabled: a.FishingTalkEnabled,
				Tools:              a.ToolsJSON, ToolsList: tools, Prompt: a.Prompt,
			}
			if act, ok := actorByKey[a.Key]; ok {
				v.Actor = map[string]any{
					"activity": act.Activity, "loc_name": act.LocName, "target_name": act.TargetName,
					"since_ts": act.SinceTS, "until_ts": act.ActivityUntilTS, "energy": act.Energy,
				}
			}
			agents = append(agents, v)
		}
	}
	// 最近一条失败任务的错误摘要，供后台直接看到「AI 节点出错」。
	lastError := map[string]any{}
	var errText, errAgent string
	var errTS float64
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT r.err, COALESCE(t.agent,''), r.created_ts
		FROM ai_result r LEFT JOIN ai_task t ON t.id=r.task_id
		WHERE r.status='fallback' AND r.err<>''
		ORDER BY r.id DESC LIMIT 1`).Scan(&errText, &errAgent, &errTS); err == nil {
		lastError = map[string]any{
			"agent": errAgent, "error": safeAIError(errors.New(errText)), "ts": errTS,
		}
	}
	queue := map[string]any{}
	if q, err := s.db.AIQueueStats(ctx); err == nil {
		queue = map[string]any{
			"pending": q.Pending, "processing": q.Processing,
			"tool_pending": q.ToolPending, "tool_processing": q.ToolProcessing,
		}
	}
	writeJSON(w, map[string]any{
		"settings": settings, "specs": AISettingSpecs, "modules": AIModules,
		"provider": providerName, "base_url": baseURL,
		"model_id": modelID, "api_key_masked": masked, "agents": agents,
		"last_error": lastError, "bridge": s.aiBridgeHealth(), "queue": queue,
	})
}

// validBaseURL 只允许 http/https，避免把模型调用指到 file:// 等非预期协议。
func validBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Server) handleAIConfigSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		ModelID string `json:"model_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.BaseURL != "" && !validBaseURL(req.BaseURL) {
		httpError(w, 400, "厂商地址必须是 http(s) URL")
		return
	}
	ctx := r.Context()
	var providerID int64
	if err := s.db.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_provider WHERE enabled=1 ORDER BY id LIMIT 1`).Scan(&providerID); err != nil {
		httpError(w, 500, "没有启用的厂商")
		return
	}
	if req.BaseURL != "" {
		if _, err := s.db.SQL.ExecContext(ctx, `UPDATE ai_provider SET base_url=? WHERE id=?`, req.BaseURL, providerID); err != nil {
			httpError(w, 500, "厂商地址保存失败")
			return
		}
	}
	if req.ModelID != "" {
		if _, err := s.db.SQL.ExecContext(ctx, `UPDATE ai_model SET model_id=? WHERE provider_id=?`, req.ModelID, providerID); err != nil {
			httpError(w, 500, "模型 ID 保存失败")
			return
		}
	}
	if req.APIKey != "" {
		key, err := aibridge.LoadOrCreateMasterKey(aiMasterKeyPath())
		if err != nil {
			httpError(w, 500, "主密钥不可用")
			return
		}
		ct, nonce, err := aibridge.Encrypt(key, []byte(req.APIKey))
		if err != nil {
			httpError(w, 500, "密钥加密失败")
			return
		}
		if err := s.db.AISaveSecret(ctx, providerID, ct, nonce, aibridge.Masked(req.APIKey)); err != nil {
			httpError(w, 500, "密钥保存失败")
			return
		}
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_key", strconv.FormatInt(providerID, 10), aibridge.Masked(req.APIKey))
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_config", strconv.FormatInt(providerID, 10), "base_url/model_id updated")
	writeJSON(w, map[string]any{"ok": true})
}

// handleAISettingsSave 按参数注册表通用写入。
// 参数声明见 ai_settings.go：新增参数只改注册表一行，不用再动这里。
func (s *Server) handleAISettingsSave(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	updates := make(map[string]string, len(body))
	for key, raw := range body {
		spec, ok := settingByKey[key]
		if !ok {
			continue // 未知 key 静默忽略，避免把前端拼错的字段写进库
		}
		if v, valid := NormalizeSettingValue(spec, raw); valid {
			updates[key] = v
		}
	}
	if len(updates) == 0 {
		writeJSON(w, map[string]any{"ok": true, "updated": 0})
		return
	}
	if err := s.db.AISetSettings(ctx, updates); err != nil {
		httpError(w, 500, "设置保存失败")
		return
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_settings", "global", "keys="+strings.Join(keys, ","))
	writeJSON(w, map[string]any{"ok": true, "updated": len(updates)})
}

func (s *Server) handleAIAgentSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key             string `json:"key"`
		ThinkingEnabled bool   `json:"thinking_enabled"`
		ThinkingEffort  string `json:"thinking_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	switch req.ThinkingEffort {
	case "off", "low", "medium", "high", "max":
	default:
		req.ThinkingEffort = "off"
	}
	ctx := r.Context()
	enabled := 0
	if req.ThinkingEnabled {
		enabled = 1
	}
	res, err := s.db.SQL.ExecContext(ctx, `
		UPDATE ai_agent SET thinking_enabled=?, thinking_effort=? WHERE key=?`,
		enabled, req.ThinkingEffort, req.Key)
	if err != nil {
		httpError(w, 500, "Agent 保存失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "Agent 不存在")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent", req.Key,
		"thinking="+strconv.Itoa(enabled)+" effort="+req.ThinkingEffort)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAIChannels(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("name")
	if channel == "" {
		channel = "nav1_03"
	}
	ctx := r.Context()
	type agentView struct {
		Key     string `json:"key"`
		Display string `json:"display"`
	}
	agents := []agentView{}
	if list, err := s.db.AIListAgents(ctx); err == nil {
		for _, a := range list {
			agents = append(agents, agentView{a.Key, a.Display})
		}
	}
	subscribed, err := s.db.AIChannelSubscribed(ctx, channel)
	if err != nil {
		subscribed = []string{}
	}
	// 频道下拉：数据库里见过的频道 + 已接入 Agent 数/名单。
	channels := []map[string]any{}
	if list, err := s.db.AIListChannels(ctx); err == nil {
		for _, c := range list {
			channels = append(channels, map[string]any{
				"name": c.Name, "label": c.Label, "count": c.Count, "agents": c.Agents,
			})
		}
	}
	// 地图级订阅：官服一张图有十几个到上百个区域频道，按地图订阅才不用逐个拉。
	maps := []map[string]any{}
	if list, err := s.db.AIListMaps(ctx); err == nil {
		for _, m := range list {
			maps = append(maps, map[string]any{
				"map_key": m.MapKey, "label": m.Label, "count": m.Count, "agents": m.Agents,
			})
		}
	}
	// 当前频道所属的地图：让后台能直接显示「这个频道归哪张图管」。
	curMap, curMapKey := "", ""
	if k, ok := s.db.ChannelMapKey(ctx, channel); ok {
		curMapKey = k
		for _, m := range maps {
			if mm, ok := m["map_key"].(string); ok && mm == k {
				if lb, ok := m["label"].(string); ok {
					curMap = lb
				}
			}
		}
	}
	writeJSON(w, map[string]any{
		"channel": channel, "agents": agents, "subscribed": subscribed,
		"channels": channels, "maps": maps,
		"channel_map_key": curMapKey, "channel_map_label": curMap,
	})
}

func (s *Server) handleAIChannelSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string  `json:"channel"`
		Agent   string  `json:"agent"`
		Enabled bool    `json:"enabled"`
		Label   *string `json:"label"`
		MapKey  string  `json:"map_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	// 地图级订阅：map_key 非空时按地图写，与精确频道订阅互不影响。
	if req.MapKey != "" {
		if req.Agent == "" {
			httpError(w, 400, "请选择 Agent")
			return
		}
		if err := s.db.AISetMapAgent(ctx, req.MapKey, req.Agent, req.Enabled); err != nil {
			httpError(w, 500, "地图订阅保存失败")
			return
		}
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_map_agent", req.MapKey,
			"agent="+req.Agent+" enabled="+strconv.FormatBool(req.Enabled))
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if req.Channel == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.Label != nil {
		if err := s.db.AISetChannelLabel(ctx, req.Channel, strings.TrimSpace(*req.Label)); err != nil {
			httpError(w, 500, "显示名保存失败")
			return
		}
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_channel_label", req.Channel, strings.TrimSpace(*req.Label))
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if req.Agent == "" {
		httpError(w, 400, "请选择 Agent")
		return
	}
	if err := s.db.AISetChannelAgent(ctx, req.Channel, req.Agent, req.Enabled); err != nil {
		httpError(w, 500, "频道订阅保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_channel", req.Channel,
		"agent="+req.Agent+" enabled="+strconv.FormatBool(req.Enabled))
	writeJSON(w, map[string]any{"ok": true})
}

// safeAIError 把厂商错误体里的密钥抹掉并截断，避免后台把 Key 回显出来。
func safeAIError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "sk-"); i >= 0 {
		s = s[:i] + "sk-***"
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// handleAITest 用页面当前输入（可未保存）发一次最小请求，确认厂商/模型/Key 联通。
// 只有用户主动点「测试连接」才会产生这一次出网调用。
func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		ModelID string `json:"model_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.BaseURL != "" && !validBaseURL(req.BaseURL) {
		writeJSON(w, map[string]any{"ok": false, "error": "厂商地址必须是 http(s) URL"})
		return
	}
	ctx := r.Context()
	masterKey, err := aibridge.LoadOrCreateMasterKey(aiMasterKeyPath())
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "主密钥不可用"})
		return
	}
	spec, err := aibridge.LoadSpecFromDB(ctx, s.db, masterKey)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "厂商配置读取失败：" + safeAIError(err)})
		return
	}
	// 页面当前输入优先，支持「先测试、再保存」。
	if v := strings.TrimSpace(req.BaseURL); v != "" {
		spec.BaseURL = v
	}
	if v := strings.TrimSpace(req.ModelID); v != "" {
		spec.ModelID = v
	}
	if v := strings.TrimSpace(req.APIKey); v != "" {
		spec.APIKey = v
	}
	provider, err := aibridge.NewProvider(spec, 12*time.Second)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeAIError(err)})
		return
	}
	started := time.Now()
	resp, err := provider.Chat(ctx, aibridge.ChatRequest{
		Messages:  []aibridge.Message{{Role: "user", Content: "请只回复两个字：正常"}},
		MaxTokens: 16,
	})
	latency := time.Since(started).Milliseconds()
	if err != nil {
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_test", spec.Name, "failed")
		writeJSON(w, map[string]any{
			"ok": false, "latency_ms": latency,
			"provider": spec.Name, "model": spec.ModelID, "error": safeAIError(err),
		})
		return
	}
	sample := strings.TrimSpace(resp.Text)
	if r := []rune(sample); len(r) > 40 {
		sample = string(r[:40])
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_test", spec.Name, "ok")
	writeJSON(w, map[string]any{
		"ok": true, "latency_ms": latency,
		"provider": spec.Name, "model": spec.ModelID, "sample": sample,
	})
}

func validAgentKey(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// handleAIAgentCreate 先建 Agent（角色 + 系统提示词），默认不接入游戏。
func (s *Server) handleAIAgentCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key             string   `json:"key"`
		Display         string   `json:"display"`
		Role            string   `json:"role"`
		Prompt          string   `json:"prompt"`
		Tools           []string `json:"tools"`
		ThinkingEnabled bool     `json:"thinking_enabled"`
		ThinkingEffort  string   `json:"thinking_effort"`
		ContactUID      int64    `json:"contact_uid"`
		ReplyMode       string   `json:"reply_mode"`
		Talkativeness   int      `json:"talkativeness"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	if !validAgentKey(req.Key) {
		httpError(w, 400, "key 只能用小写字母、数字、- 和 _")
		return
	}
	if strings.TrimSpace(req.Display) == "" {
		httpError(w, 400, "请填写展示名")
		return
	}
	switch req.ThinkingEffort {
	case "off", "low", "medium", "high", "max":
	default:
		req.ThinkingEffort = "off"
	}
	if req.Tools == nil {
		req.Tools = []string{}
	}
	toolsJSON, _ := json.Marshal(req.Tools)
	ctx := r.Context()
	uid, revived, err := s.db.AICreateAgent(ctx, store.AIAgent{
		Key: req.Key, Display: strings.TrimSpace(req.Display),
		Role: strings.TrimSpace(req.Role), ContactUID: req.ContactUID,
		Prompt: req.Prompt, ThinkingEnabled: req.ThinkingEnabled,
		ThinkingEffort: req.ThinkingEffort,
		ReplyMode:      req.ReplyMode, Talkativeness: req.Talkativeness,
	}, string(toolsJSON))
	if errors.Is(err, store.ErrAIAgentExists) {
		httpError(w, 400, "这个 Key 已经有一个在用的 Agent")
		return
	}
	if errors.Is(err, store.ErrAIContactUIDUsed) {
		httpError(w, 400, "这个联系人 UID 已经被占用")
		return
	}
	if errors.Is(err, store.ErrAIContactUIDInvalid) {
		httpError(w, 400, "联系人 UID 必须留空或使用 2147480000-2147483645")
		return
	}
	if err != nil {
		httpError(w, 500, "Agent 创建失败")
		return
	}
	action := "created"
	if revived {
		action = "revived"
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_create", req.Key,
		action+" uid="+strconv.FormatInt(uid, 10))
	writeJSON(w, map[string]any{
		"ok": true, "key": req.Key, "contact_uid": uid, "revived": revived,
	})
}

// handleAIAgentJoin 接入/移出游戏；接入时才分配并写入联系人 UID。
func (s *Server) handleAIAgentJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key        string `json:"key"`
		Joined     bool   `json:"joined"`
		ContactUID int64  `json:"contact_uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	a, err := s.db.AIAgentByKey(ctx, req.Key)
	if err != nil || a == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	uid := req.ContactUID
	if uid == 0 {
		uid = a.ContactUID
	}
	if req.Joined && uid == 0 {
		next, err := s.db.AINextContactUID(ctx)
		if err != nil {
			httpError(w, 500, "没有可用的联系人 UID")
			return
		}
		uid = next
	}
	if err := s.db.AISetAgentJoined(ctx, req.Key, req.Joined, uid); err != nil {
		if errors.Is(err, store.ErrAIContactUIDInvalid) {
			httpError(w, 400, "联系人 UID 必须使用 2147480000-2147483645")
			return
		}
		httpError(w, 500, "接入状态保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_join", req.Key,
		"joined="+strconv.FormatBool(req.Joined)+" uid="+strconv.FormatInt(uid, 10))
	writeJSON(w, map[string]any{"ok": true, "contact_uid": uid})
}

// handleAIAgentUpdate 改展示名、角色与系统提示词。
func (s *Server) handleAIAgentUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string    `json:"key"`
		Display string    `json:"display"`
		Role    string    `json:"role"`
		Prompt  string    `json:"prompt"`
		Tools   *[]string `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.Display) == "" {
		httpError(w, 400, "请填写展示名")
		return
	}
	ctx := r.Context()
	// 没传 tools 时保留原白名单，避免「只改提示词却把工具清空」。
	toolsJSON := ""
	if req.Tools != nil {
		b, _ := json.Marshal(*req.Tools)
		toolsJSON = string(b)
	} else {
		var cur string
		_ = s.db.SQL.QueryRowContext(ctx, `SELECT tools_json FROM ai_agent WHERE key=?`, req.Key).Scan(&cur)
		toolsJSON = cur
	}
	if err := s.db.AIUpdateAgent(ctx, req.Key, strings.TrimSpace(req.Display),
		strings.TrimSpace(req.Role), req.Prompt, toolsJSON); err != nil {
		httpError(w, 500, "Agent 保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_update", req.Key, "updated")
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentDelete 逻辑删除 Agent：聊天对象与历史保留，key 与 UID 继续占位。
func (s *Server) handleAIAgentDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	if a, err := s.db.AIAgentByKey(ctx, req.Key); err != nil || a == nil {
		httpError(w, 404, "Agent 不存在或已删除")
		return
	}
	if err := s.db.AIDeleteAgent(ctx, req.Key); err != nil {
		httpError(w, 500, "删除失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_delete", req.Key, "soft_deleted")
	writeJSON(w, map[string]any{"ok": true, "soft_deleted": true})
}

// aiBridgeHealth 探测 aibridge 的观测端口，让后台能看到 AI 进程是否在线。
func (s *Server) aiBridgeHealth() map[string]any {
	addr := aiBridgeAddr
	if v := os.Getenv("RF4_AI_OBS_ADDR"); v != "" {
		addr = v
	}
	// 配置里可能写 ":9504" 或 "0.0.0.0:9504"，探测时统一走本机回环。
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return map[string]any{"online": false}
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	body["online"] = resp.StatusCode == 200
	return body
}

// handleAIMemoryClear 清空某个 Agent 的战绩记忆。
func (s *Server) handleAIMemoryClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	if err := s.db.AIClearMemories(ctx, req.Key); err != nil {
		httpError(w, 500, "清空失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_memory_clear", req.Key, "cleared")
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentBehavior 保存「什么时候开口」：主动/点名/沉默，以及活跃度。
func (s *Server) handleAIAgentBehavior(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key           string `json:"key"`
		ReplyMode     string `json:"reply_mode"`
		Talkativeness *int   `json:"talkativeness"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.ReplyMode != "social" && req.ReplyMode != "mention" && req.ReplyMode != "silent" {
		httpError(w, 400, "回复方式不合法")
		return
	}
	talk := 50
	if req.Talkativeness != nil {
		talk = *req.Talkativeness
	}
	ctx := r.Context()
	if a, err := s.db.AIAgentByKey(ctx, req.Key); err != nil || a == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	if err := s.db.AISetAgentBehavior(ctx, req.Key, req.ReplyMode, talk); err != nil {
		httpError(w, 500, "说话方式保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_behavior", req.Key,
		req.ReplyMode+" talkativeness="+strconv.Itoa(talk))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentFishingTalk 开关某个 Agent 的「上鱼报话」。
// 全局范围决定在哪能见到它，这个决定它上了鱼能不能主动开口。
func (s *Server) handleAIAgentFishingTalk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	if err := s.db.AISetAgentFishingTalk(ctx, req.Key, req.Enabled); err != nil {
		httpError(w, 500, "上鱼报话保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_fishing_talk", req.Key,
		"enabled="+strconv.FormatBool(req.Enabled))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentActor 开关某个 Agent 的「角色状态」（是否在钓鱼）。
// 关掉=永远不钓鱼；打开后会按状态机在钓鱼/休息/闲逛之间自己切换。
func (s *Server) handleAIAgentActor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
		Reroll  bool   `json:"reroll"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	ag, err := s.db.AIAgentByKey(ctx, req.Key)
	if err != nil || ag == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	wasEnabled := ag.ActorEnabled
	if err := s.db.AISetActorEnabled(ctx, req.Key, req.Enabled); err != nil {
		httpError(w, 500, "状态开关保存失败")
		return
	}
	if !req.Enabled {
		_ = s.db.AIDeleteActor(ctx, req.Key)
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_actor", req.Key, "actor_enabled=false")
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	now := float64(time.Now().Unix())
	cfg := s.db.AIActorConfig(ctx)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	a, err := s.db.AIGetActor(ctx, req.Key)
	if err != nil {
		httpError(w, 500, "状态读取失败")
		return
	}
	locKey, locName := "", ""
	if k, n, mapErr := s.db.AIRandomMap(ctx); mapErr == nil {
		locKey, locName = k, n
	}
	if a == nil {
		a = store.AIActorStart(req.Key, locKey, locName, now, cfg, rng.Float64)
	} else {
		if a.Loc == "" && locKey != "" {
			a.Loc, a.LocName, a.LocSinceTS = locKey, locName, now
		}
		if req.Reroll {
			store.AIActorReroll(a, now, cfg, rng.Float64)
		}
	}
	if err := s.db.AISaveActor(ctx, a); err != nil {
		httpError(w, 500, "状态保存失败")
		return
	}
	// 纯「换个状态」只是角色扮演，不写审计；只有真正开启角色模拟时才留痕。
	if !wasEnabled {
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_actor", req.Key,
			"actor_enabled=true reroll="+strconv.FormatBool(req.Reroll))
	}
	writeJSON(w, map[string]any{"ok": true, "actor": map[string]any{
		"activity": a.Activity, "loc_name": a.LocName, "target_name": a.TargetName,
		"since_ts": a.SinceTS, "until_ts": a.ActivityUntilTS, "energy": a.Energy,
	}})
}
