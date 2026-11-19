package store

// AI 运行态单写者命令（T-020 / DEV-004）。
//
// AD 改意图、BR 提建议、GS 公聊命令都先写 ai_runtime_command；唯一活动
// gameserver 在 BEGIN IMMEDIATE 事务里选最小 pending、校验代次/版本、更新
// 运行态并记终态。ai_runtime_control 是每 Agent 的单调代次/版本真源。
//
// 本文件是「运行态业务写者只有 GS」的落点：导出的写入口只有
// AIEnqueueRuntimeCommand（入队，不碰运行态）与两个 Apply*（必须由 GS 调用
// 并在同一事务里完成校验 + 状态更新 + 终态）。其余 AI* 表级写函数要么已
// 删除，要么只保留给 store 内事务助手与既有测试。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
)

// 命令来源。由受信入口填写，禁止 HTTP body 自报。
const (
	AIRuntimeSourceAdmin      = "admin"
	AIRuntimeSourceBridge     = "bridge"
	AIRuntimeSourceGameServer = "gameserver"
)

// 有限命令表（票 §2.2）。新增 kind 必须同时补来源矩阵、payload 校验与
// GS 应用分支，否则入队直接拒绝。
const (
	AIRuntimeKindActorSetEnabled    = "actor_set_enabled"
	AIRuntimeKindActorReroll        = "actor_reroll"
	AIRuntimeKindActorTargetSuggest = "actor_target_suggest"
	AIRuntimeKindChannelSetMember   = "channel_set_member"
	AIRuntimeKindMemoryClear        = "memory_clear"
	AIRuntimeKindAgentDelete        = "agent_delete"
)

// 命令终态。pending 之外的三个都是终态，保留 7 天后可被 GC。
const (
	AIRuntimeStatusPending  = "pending"
	AIRuntimeStatusApplied  = "applied"
	AIRuntimeStatusRejected = "rejected"
	AIRuntimeStatusExpired  = "expired"
)

const (
	// AIRuntimeMaxPayloadBytes 是单个已接受命令规范化 payload 的字节上限。
	AIRuntimeMaxPayloadBytes = 4096
	// AIRuntimeMaxPendingGlobal / AIRuntimeMaxPendingAgent 是 pending 队列上限，
	// 限额检查与 INSERT 同事务；满了返回 ErrAIRuntimeQueueFull。
	AIRuntimeMaxPendingGlobal = 1024
	AIRuntimeMaxPendingAgent  = 32
	// AIRuntimeMaxTotalRows 是命令表总行数上限；达到时先 GC 过期终态。
	AIRuntimeMaxTotalRows = 100000
	// AIRuntimeTerminalKeep 是终态保留时长。
	AIRuntimeTerminalKeep = 7 * 24 * time.Hour
	// AIRuntimeRetryWindow 是 request_id 的有效窗口：超过后重试按 expired 处理，
	// 即使终态行已经被 GC 也不重放旧命令。
	AIRuntimeRetryWindow = 24 * time.Hour
	// AIRuntimeMaxTTL 是单条命令的最长寿命。
	AIRuntimeMaxTTL = 24 * time.Hour
)

var (
	// ErrAIRuntimeCommandConflict：同一 request_id 被用于不同 payload。
	ErrAIRuntimeCommandConflict = errors.New("ai runtime: request_id 已用于不同意图")
	// ErrAIRuntimeQueueFull：全局或单 Agent pending 队列已满。
	ErrAIRuntimeQueueFull = errors.New("ai runtime: 命令队列已满")
	// ErrAIRuntimeInvalidCommand：来源/kind/参数/长度不合法。
	ErrAIRuntimeInvalidCommand = errors.New("ai runtime: 命令不合法")
	// ErrAIRuntimeCommandNotFound：按 id 查询不存在。
	ErrAIRuntimeCommandNotFound = errors.New("ai runtime: 命令不存在")
)

// AIQueryer 是 *sql.DB / *sql.Tx / *sql.Conn 的公共子集：运行态的 SQL 助手
// 既能在 DB 方法里复用，也能在 BEGIN IMMEDIATE 的事务连接上执行。
type AIQueryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AIRuntimeCommand 是一行持久命令。
type AIRuntimeCommand struct {
	ID                 int64
	RequestID          string
	Source             string
	Kind               string
	AgentKey           string
	ExpectedGeneration int64
	ExpectedRevision   int64
	Payload            string
	PayloadHash        string
	CreatedTS          float64
	ExpiresTS          float64
	Status             string
	Reason             string
	AppliedTS          float64
	ResultRevision     int64
	IntentEpoch        int64
	ParentTaskID       int64
}

// AIRuntimeControl 是每 Agent 的单调代次与版本。
type AIRuntimeControl struct {
	AgentKey   string
	Generation int64
	Revision   int64
	Tombstoned bool
}

// AIRuntimeEnqueueRequest 是入队参数。Payload 是入口收到的原始 JSON（可为 nil）；
// 入队时会按 kind 严格解析并重新编码成规范 JSON，键序不影响 payload_hash。
type AIRuntimeEnqueueRequest struct {
	RequestID          string
	Source             string
	Kind               string
	AgentKey           string
	ExpectedGeneration int64
	ExpectedRevision   int64
	Payload            []byte
	ParentTaskID       int64 // trusted server-side origin for BR suggestions
	TTL                time.Duration
	Now                float64 // 0 = 当前时间；测试用
}

// IRuntime 的 payload 结构。字段顺序即规范 JSON 顺序，因此同一意图的
// 不同键序请求会得到同一个 payload_hash。
type AIRuntimeActorSetEnabledPayload struct {
	Enabled bool `json:"enabled"`
}

type AIRuntimeChannelSetMemberPayload struct {
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}

type AIRuntimeActorTargetSuggestPayload struct {
	FishKey string `json:"fish_key"`
	LocKey  string `json:"loc_key"`
	// Capability / CapRevision 是 T-104 的可复核来源：GS 在写事务里按它重验
	// 当前权限与配置修订。旧行没有这两个字段，按跨版本任务拒绝。
	Capability  string `json:"capability,omitempty"`
	CapRevision int64  `json:"cap_revision,omitempty"`
}

// aiRuntimeSourceAllowed 是来源 × kind 矩阵。source 只能由受信入口填写。
func aiRuntimeSourceAllowed(kind, source string) bool {
	switch kind {
	case AIRuntimeKindActorSetEnabled, AIRuntimeKindActorReroll,
		AIRuntimeKindMemoryClear, AIRuntimeKindAgentDelete:
		return source == AIRuntimeSourceAdmin
	case AIRuntimeKindActorTargetSuggest:
		return source == AIRuntimeSourceBridge
	case AIRuntimeKindChannelSetMember:
		return source == AIRuntimeSourceGameServer || source == AIRuntimeSourceAdmin
	default:
		return false
	}
}

// aiRuntimeValidRequestID 限制 request_id 为可见 ASCII、1..200 字节：
// 它来自 AD 页面 / BR 任务 / GS 会话摘要，不需要 Unicode。
func aiRuntimeValidRequestID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 33 || id[i] > 126 {
			return false
		}
	}
	return true
}

func aiRuntimeValidKey(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// aiRuntimeDecodeStrict 只接受一个完整 JSON 对象：未知字段、尾随内容、
// null 都算坏命令。
func aiRuntimeDecodeStrict(raw string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("payload 含多余内容")
	}
	return nil
}

// aiRuntimeCanonicalPayload 把入口 payload 校验并规范成固定 JSON。
func aiRuntimeCanonicalPayload(kind string, raw []byte) (string, error) {
	text := strings.TrimSpace(string(raw))
	switch kind {
	case AIRuntimeKindActorSetEnabled:
		if text == "" || text == "null" {
			return "", fmt.Errorf("%w: %s 需要 enabled", ErrAIRuntimeInvalidCommand, kind)
		}
		var p AIRuntimeActorSetEnabledPayload
		if err := aiRuntimeDecodeStrict(text, &p); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAIRuntimeInvalidCommand, err)
		}
		return aiRuntimeMarshal(p)
	case AIRuntimeKindChannelSetMember:
		if text == "" || text == "null" {
			return "", fmt.Errorf("%w: %s 需要 channel/enabled", ErrAIRuntimeInvalidCommand, kind)
		}
		var p AIRuntimeChannelSetMemberPayload
		if err := aiRuntimeDecodeStrict(text, &p); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAIRuntimeInvalidCommand, err)
		}
		p.Channel = strings.TrimSpace(p.Channel)
		if !aiRuntimeValidKey(p.Channel, 64) {
			return "", fmt.Errorf("%w: channel 不合法", ErrAIRuntimeInvalidCommand)
		}
		return aiRuntimeMarshal(p)
	case AIRuntimeKindActorTargetSuggest:
		if text == "" || text == "null" {
			return "", fmt.Errorf("%w: %s 需要 fish_key/loc_key", ErrAIRuntimeInvalidCommand, kind)
		}
		var p AIRuntimeActorTargetSuggestPayload
		if err := aiRuntimeDecodeStrict(text, &p); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAIRuntimeInvalidCommand, err)
		}
		p.FishKey = strings.TrimSpace(p.FishKey)
		p.LocKey = strings.TrimSpace(p.LocKey)
		if !aiRuntimeValidKey(p.FishKey, 96) || !aiRuntimeValidKey(p.LocKey, 96) {
			return "", fmt.Errorf("%w: fish_key/loc_key 不合法", ErrAIRuntimeInvalidCommand)
		}
		if p.CapRevision < 0 {
			return "", fmt.Errorf("%w: cap_revision 不能为负", ErrAIRuntimeInvalidCommand)
		}
		if p.Capability != "" && p.Capability != aiconfig.CapabilityAcceptTarget {
			return "", fmt.Errorf("%w: 未知能力 %q", ErrAIRuntimeInvalidCommand, p.Capability)
		}
		if p.CapRevision > 0 && p.Capability == "" {
			return "", fmt.Errorf("%w: 携带修订时必须写明能力", ErrAIRuntimeInvalidCommand)
		}
		return aiRuntimeMarshal(p)
	case AIRuntimeKindActorReroll, AIRuntimeKindMemoryClear, AIRuntimeKindAgentDelete:
		if text != "" && text != "{}" && text != "null" {
			return "", fmt.Errorf("%w: %s 不接受 payload", ErrAIRuntimeInvalidCommand, kind)
		}
		return "{}", nil
	default:
		return "", fmt.Errorf("%w: 未知 kind %q", ErrAIRuntimeInvalidCommand, kind)
	}
}

func aiRuntimeMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AIRuntimePayloadActorSetEnabled 解析规范 payload（GS 应用侧）。
func AIRuntimePayloadActorSetEnabled(raw string) (bool, error) {
	var p AIRuntimeActorSetEnabledPayload
	if err := aiRuntimeDecodeStrict(raw, &p); err != nil {
		return false, err
	}
	return p.Enabled, nil
}

// AIRuntimePayloadChannel 解析规范 payload（GS 应用侧）。
func AIRuntimePayloadChannel(raw string) (string, bool, error) {
	var p AIRuntimeChannelSetMemberPayload
	if err := aiRuntimeDecodeStrict(raw, &p); err != nil {
		return "", false, err
	}
	return p.Channel, p.Enabled, nil
}

// AIRuntimePayloadTargetSuggest 解析规范 payload（GS 应用侧）。
func AIRuntimePayloadTargetSuggest(raw string) (string, string, error) {
	p, err := AIRuntimePayloadTargetSuggestFull(raw)
	return p.FishKey, p.LocKey, err
}

// AIRuntimePayloadTargetSuggestFull 连同能力与修订一起解析，供 GS 写事务复核。
func AIRuntimePayloadTargetSuggestFull(raw string) (AIRuntimeActorTargetSuggestPayload, error) {
	var p AIRuntimeActorTargetSuggestPayload
	if err := aiRuntimeDecodeStrict(raw, &p); err != nil {
		return p, err
	}
	return p, nil
}

// aiRuntimePayloadHash 对「kind + agent + 规范 payload」取 SHA-256。
// 混入 kind/agent 是为了让两个空 payload 的不同意图不会撞同一个 hash；
// 规范 payload 由结构体重编码产生，因此 JSON 键序不会影响 hash。
func aiRuntimePayloadHash(kind, agentKey, canonical string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + agentKey + "\x00" + canonical))
	return hex.EncodeToString(sum[:])
}

const aiRuntimeCommandColumns = `id, request_id, source, kind, agent_key,
	expected_generation, expected_revision, payload, payload_hash, created_ts,
	expires_ts, status, reason, applied_ts, result_revision,
	COALESCE(CASE WHEN typeof(intent_epoch)='integer' THEN intent_epoch END,0),
	COALESCE(CASE WHEN typeof(parent_task_id)='integer' THEN parent_task_id END,0)`

type aiRuntimeScanner interface{ Scan(dest ...any) error }

func scanAIRuntimeCommand(row aiRuntimeScanner) (*AIRuntimeCommand, error) {
	var c AIRuntimeCommand
	if err := row.Scan(&c.ID, &c.RequestID, &c.Source, &c.Kind, &c.AgentKey,
		&c.ExpectedGeneration, &c.ExpectedRevision, &c.Payload, &c.PayloadHash,
		&c.CreatedTS, &c.ExpiresTS, &c.Status, &c.Reason, &c.AppliedTS,
		&c.ResultRevision, &c.IntentEpoch, &c.ParentTaskID); err != nil {
		return nil, err
	}
	return &c, nil
}

// AIGetRuntimeCommand 按 id 读命令。
func (d *DB) AIGetRuntimeCommand(ctx context.Context, id int64) (*AIRuntimeCommand, error) {
	row := d.SQL.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command WHERE id=?`, id)
	c, err := scanAIRuntimeCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAIRuntimeCommandNotFound
	}
	return c, err
}

// AIGetRuntimeCommandByRequest 按 request_id 读命令（去重与恢复后回显用）。
func (d *DB) AIGetRuntimeCommandByRequest(ctx context.Context, requestID string) (*AIRuntimeCommand, error) {
	row := d.SQL.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command WHERE request_id=?`, requestID)
	c, err := scanAIRuntimeCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAIRuntimeCommandNotFound
	}
	return c, err
}

// AIGetRuntimeCommandByParent 按父任务与 kind 读首条命令。
// 一次任务只允许一条目标建议；旁路与模型工具据此收敛到同一效果。
func (d *DB) AIGetRuntimeCommandByParent(ctx context.Context, parentTaskID int64, kind string) (*AIRuntimeCommand, error) {
	row := d.SQL.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command
		 WHERE parent_task_id=? AND kind=? ORDER BY id LIMIT 1`, parentTaskID, kind)
	c, err := scanAIRuntimeCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAIRuntimeCommandNotFound
	}
	return c, err
}

// AIRecentTerminalTargetSuggests 返回某 Agent 最近 limit 条目标建议终态。
//
// 只取 applied / rejected / expired：pending 不属于终态，不能当成"已经生效"；
// 查询按 agent_key 过滤，别的角色的结果不会进入本角色的下一轮上下文。
func (d *DB) AIRecentTerminalTargetSuggests(ctx context.Context, agentKey string,
	limit int) ([]*AIRuntimeCommand, error) {
	if limit <= 0 || limit > 16 {
		limit = 8
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command
		WHERE agent_key=? AND kind=? AND status IN (?,?,?)
		ORDER BY id DESC LIMIT ?`, agentKey, AIRuntimeKindActorTargetSuggest,
		AIRuntimeStatusApplied, AIRuntimeStatusRejected, AIRuntimeStatusExpired, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AIRuntimeCommand, 0, limit)
	for rows.Next() {
		c, err := scanAIRuntimeCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AIGetRuntimeControl 读某 Agent 的代次/版本；没有 control 行时按
// generation=1、revision=0 处理（现有 Agent 首次接管的定义值）。
func (d *DB) AIGetRuntimeControl(ctx context.Context, agentKey string) (*AIRuntimeControl, error) {
	var c AIRuntimeControl
	var tomb int
	err := d.SQL.QueryRowContext(ctx, `
		SELECT agent_key, generation, revision, tombstoned
		FROM ai_runtime_control WHERE agent_key=?`, agentKey).
		Scan(&c.AgentKey, &c.Generation, &c.Revision, &tomb)
	if errors.Is(err, sql.ErrNoRows) {
		return &AIRuntimeControl{AgentKey: agentKey, Generation: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	c.Tombstoned = tomb != 0
	return &c, nil
}

func aiRuntimeCommandByID(ctx context.Context, q AIQueryer, id int64) (*AIRuntimeCommand, error) {
	return scanAIRuntimeCommand(q.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command WHERE id=?`, id))
}

func aiRuntimeCommandByRequest(ctx context.Context, q AIQueryer, requestID string) (*AIRuntimeCommand, error) {
	return scanAIRuntimeCommand(q.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command WHERE request_id=?`, requestID))
}

// AIEnqueueRuntimeCommand 持久化一条意图：去重、限额与 INSERT 在同一
// BEGIN IMMEDIATE 事务里完成。它绝不修改运行态。
func (d *DB) AIEnqueueRuntimeCommand(ctx context.Context, req AIRuntimeEnqueueRequest) (*AIRuntimeCommand, bool, error) {
	if req.Source != AIRuntimeSourceBridge {
		ctx = aiCaptureIntent(ctx, d)
	}
	requestID := strings.TrimSpace(req.RequestID)
	if !aiRuntimeValidRequestID(requestID) {
		return nil, false, fmt.Errorf("%w: request_id 不合法", ErrAIRuntimeInvalidCommand)
	}
	agentKey := strings.TrimSpace(req.AgentKey)
	if !aiRuntimeValidKey(agentKey, 64) {
		return nil, false, fmt.Errorf("%w: agent_key 不合法", ErrAIRuntimeInvalidCommand)
	}
	if !aiRuntimeSourceAllowed(req.Kind, req.Source) {
		return nil, false, fmt.Errorf("%w: 来源 %q 不能提交 %q", ErrAIRuntimeInvalidCommand, req.Source, req.Kind)
	}
	if req.ExpectedGeneration < 0 || req.ExpectedRevision < 0 {
		return nil, false, fmt.Errorf("%w: 代次/版本不能为负", ErrAIRuntimeInvalidCommand)
	}
	canonical, err := aiRuntimeCanonicalPayload(req.Kind, req.Payload)
	if err != nil {
		return nil, false, err
	}
	if len(canonical) > AIRuntimeMaxPayloadBytes {
		return nil, false, fmt.Errorf("%w: payload 超过 %d 字节", ErrAIRuntimeInvalidCommand, AIRuntimeMaxPayloadBytes)
	}
	hash := aiRuntimePayloadHash(req.Kind, agentKey, canonical)

	now := req.Now
	if now <= 0 {
		now = nowTS()
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if ttl > AIRuntimeMaxTTL {
		return nil, false, fmt.Errorf("%w: TTL 超过 24 小时", ErrAIRuntimeInvalidCommand)
	}
	expires := now + ttl.Seconds()

	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, false, fmt.Errorf("ai runtime: 开启入队事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	existing, err := aiRuntimeCommandByRequest(ctx, conn, requestID)
	switch {
	case err == nil:
		if existing.PayloadHash != hash || existing.Source != req.Source ||
			(req.ParentTaskID != 0 && existing.ParentTaskID != req.ParentTaskID) {
			return nil, false, fmt.Errorf("%w: request_id=%s", ErrAIRuntimeCommandConflict, requestID)
		}
		if existing.Status == AIRuntimeStatusPending &&
			(now >= existing.ExpiresTS || now-existing.CreatedTS > AIRuntimeRetryWindow.Seconds()) {
			if _, err := conn.ExecContext(ctx, `
				UPDATE ai_runtime_command SET status=?, reason='expired', applied_ts=?
				WHERE id=? AND status='pending'`,
				AIRuntimeStatusExpired, now, existing.ID); err != nil {
				return nil, false, err
			}
			existing.Status = AIRuntimeStatusExpired
			existing.Reason = "expired"
			existing.AppliedTS = now
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, false, fmt.Errorf("ai runtime: 提交去重: %w", err)
		}
		committed = true
		return existing, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}

	epoch, _ := AIIntentEpoch(ctx)
	var parent any
	if req.Source == AIRuntimeSourceBridge {
		bound := epoch
		epoch, err = AITaskEpoch(ctx, conn, req.ParentTaskID)
		if err != nil {
			return nil, false, err
		}
		if epoch <= 0 || (bound > 0 && bound != epoch) {
			return nil, false, fmt.Errorf("%w: parent task", ErrAIIntentInactive)
		}
		parent = req.ParentTaskID
	} else if req.ParentTaskID != 0 {
		return nil, false, ErrAIRuntimeInvalidCommand
	}
	reason, err := aiCheckIntent(ctx, conn, epoch)
	if err != nil {
		return nil, false, err
	}
	var intent any
	if epoch > 0 {
		intent = epoch
	}
	status, applied := AIRuntimeStatusPending, float64(0)
	if reason != "" {
		status, applied = AIRuntimeStatusRejected, now
	}
	if err := aiRuntimeCheckQueueLimits(ctx, conn, agentKey, now); err != nil {
		return nil, false, err
	}
	res, err := conn.ExecContext(ctx, `
		INSERT INTO ai_runtime_command(request_id, source, kind, agent_key,
			expected_generation, expected_revision, payload, payload_hash,
			created_ts, expires_ts, status, reason, applied_ts, intent_epoch, parent_task_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		requestID, req.Source, req.Kind, agentKey, req.ExpectedGeneration, req.ExpectedRevision,
		canonical, hash, now, expires, status, reason, applied, intent, parent)
	if err != nil {
		return nil, false, fmt.Errorf("ai runtime: 写入命令: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, false, fmt.Errorf("ai runtime: 提交命令: %w", err)
	}
	committed = true
	return &AIRuntimeCommand{
		ID: id, RequestID: requestID, Source: req.Source, Kind: req.Kind,
		AgentKey: agentKey, ExpectedGeneration: req.ExpectedGeneration,
		ExpectedRevision: req.ExpectedRevision, Payload: canonical, PayloadHash: hash,
		CreatedTS: now, ExpiresTS: expires, Status: status, Reason: reason, AppliedTS: applied,
		IntentEpoch: epoch, ParentTaskID: req.ParentTaskID,
	}, false, nil
}

// aiRuntimeCheckQueueLimits 在入队事务内检查全局/单 Agent pending 上限与
// 总行数上限（达到总上限时先 GC 过期终态）。
func aiRuntimeCheckQueueLimits(ctx context.Context, q AIQueryer, agentKey string, now float64) error {
	var pending int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_runtime_command WHERE status='pending'`).Scan(&pending); err != nil {
		return err
	}
	if pending >= AIRuntimeMaxPendingGlobal {
		return ErrAIRuntimeQueueFull
	}
	var agentPending int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_runtime_command WHERE status='pending' AND agent_key=?`,
		agentKey).Scan(&agentPending); err != nil {
		return err
	}
	if agentPending >= AIRuntimeMaxPendingAgent {
		return ErrAIRuntimeQueueFull
	}
	var total int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&total); err != nil {
		return err
	}
	if total < AIRuntimeMaxTotalRows {
		return nil
	}
	cutoff := now - AIRuntimeTerminalKeep.Seconds()
	if _, err := q.ExecContext(ctx, `
		DELETE FROM ai_runtime_command
		WHERE status<>'pending'
		  AND ((applied_ts>0 AND applied_ts<?) OR (applied_ts=0 AND created_ts<?))`,
		cutoff, cutoff); err != nil {
		return err
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&total); err != nil {
		return err
	}
	if total >= AIRuntimeMaxTotalRows {
		return ErrAIRuntimeQueueFull
	}
	return nil
}

// AIRuntimeApplyFunc 是 GS 的单条命令应用函数。它在 apply 事务内执行：
// 只能通过 tx 读写运行态；返回 applied=false + reason 表示按业务规则拒绝。
type AIRuntimeApplyFunc func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (applied bool, reason string, err error)

// AIApplyNextRuntimeCommand 取最小 pending 并在同一事务应用。
// 返回 nil 表示当前没有待处理命令；过期命令会被标 expired 并返回。
func (d *DB) AIApplyNextRuntimeCommand(ctx context.Context, now float64, fn AIRuntimeApplyFunc) (*AIRuntimeCommand, error) {
	if fn == nil {
		return nil, errors.New("ai runtime: 缺少应用函数")
	}
	if now <= 0 {
		now = nowTS()
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("ai runtime: 开启应用事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	cmd, err := scanAIRuntimeCommand(conn.QueryRowContext(ctx,
		`SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command
		 WHERE status='pending' ORDER BY id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx := &AIRuntimeTx{conn: conn, ctx: ctx}
	if cmd.ExpiresTS > 0 && cmd.ExpiresTS <= now {
		if err := aiRuntimeFinish(ctx, conn, cmd.ID, AIRuntimeStatusExpired, "expired", now, 0); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
		committed = true
		cmd.Status, cmd.Reason, cmd.AppliedTS = AIRuntimeStatusExpired, "expired", now
		return cmd, nil
	}
	if err := aiRuntimeApplyLocked(ctx, tx, cmd, now, fn); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("ai runtime: 提交应用: %w", err)
	}
	committed = true
	return cmd, nil
}

// AIApplyRuntimeCommand 立即应用指定 pending 命令（GS 公聊的同步回执路径）。
// 已终态的命令原样返回，不重复应用。
func (d *DB) AIApplyRuntimeCommand(ctx context.Context, id int64, now float64, fn AIRuntimeApplyFunc) (*AIRuntimeCommand, error) {
	if fn == nil {
		return nil, errors.New("ai runtime: 缺少应用函数")
	}
	if now <= 0 {
		now = nowTS()
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("ai runtime: 开启应用事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	cmd, err := aiRuntimeCommandByID(ctx, conn, id)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		return nil, ErrAIRuntimeCommandNotFound
	}
	if err != nil {
		return nil, err
	}
	if cmd.Status != AIRuntimeStatusPending {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
		committed = true
		return cmd, nil
	}
	if err := aiRuntimeApplyLocked(ctx, &AIRuntimeTx{conn: conn, ctx: ctx}, cmd, now, fn); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("ai runtime: 提交应用: %w", err)
	}
	committed = true
	return cmd, nil
}

// aiRuntimeApplyLocked 在同一事务里复查坏命令、应用并写终态。
// 业务错误必须通过 (false, reason) 返回；返回 error 会整体回滚。
func aiRuntimeApplyLocked(ctx context.Context, tx *AIRuntimeTx, cmd *AIRuntimeCommand, now float64, fn AIRuntimeApplyFunc) error {
	status := AIRuntimeStatusRejected
	reason := ""
	bad := !aiRuntimeSourceAllowed(cmd.Kind, cmd.Source) ||
		aiRuntimePayloadHash(cmd.Kind, cmd.AgentKey, cmd.Payload) != cmd.PayloadHash ||
		len(cmd.Payload) > AIRuntimeMaxPayloadBytes
	if bad {
		reason = "bad_command"
	} else {
		if _, err := tx.Control(cmd.AgentKey); err != nil {
			return err
		}
		applied, why, err := fn(tx, cmd)
		if err != nil {
			return err
		}
		reason = why
		if applied {
			status = AIRuntimeStatusApplied
		}
	}
	rev, err := tx.controlRevision(cmd.AgentKey)
	if err != nil {
		return err
	}
	if err := aiRuntimeFinish(ctx, tx.conn, cmd.ID, status, reason, now, rev); err != nil {
		return err
	}
	cmd.Status, cmd.Reason, cmd.AppliedTS, cmd.ResultRevision = status, reason, now, rev
	return nil
}

// aiRuntimeFinish 只在 pending 上写终态；行数不是 1 说明另一个消费者已经
// 处理过，必须整体回滚而不是覆盖。
func aiRuntimeFinish(ctx context.Context, q AIQueryer, id int64, status, reason string, now float64, revision int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE ai_runtime_command
		   SET status=?, reason=?, applied_ts=?, result_revision=?
		 WHERE id=? AND status='pending'`, status, reason, now, revision, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("ai runtime: 命令 %d 已不是 pending（并发消费者已处理）", id)
	}
	return nil
}

// AIRuntimeTx 是运行态应用事务。所有方法只走事务连接，禁止在事务内调用
// d.SQL（单连接池会自锁）。
type AIRuntimeTx struct {
	conn *sql.Conn
	ctx  context.Context
}

// SQL 返回事务连接，供 GS 在同一事务里读静态鱼表等本地资料。
func (t *AIRuntimeTx) SQL() *sql.Conn { return t.conn }

// Context 返回事务上下文。
func (t *AIRuntimeTx) Context() context.Context { return t.ctx }

// Control 读（必要时建立）某 Agent 的 control 行：generation=1、revision=0。
func (t *AIRuntimeTx) Control(agentKey string) (*AIRuntimeControl, error) {
	if _, err := t.conn.ExecContext(t.ctx, `
		INSERT OR IGNORE INTO ai_runtime_control(agent_key, generation, revision, tombstoned)
		VALUES(?,1,0,0)`, agentKey); err != nil {
		return nil, fmt.Errorf("ai runtime: 建立 control: %w", err)
	}
	var c AIRuntimeControl
	var tomb int
	if err := t.conn.QueryRowContext(t.ctx, `
		SELECT agent_key, generation, revision, tombstoned
		FROM ai_runtime_control WHERE agent_key=?`, agentKey).
		Scan(&c.AgentKey, &c.Generation, &c.Revision, &tomb); err != nil {
		return nil, err
	}
	c.Tombstoned = tomb != 0
	return &c, nil
}

// BumpControl 单调推进代次与版本：genDelta 通常 0 或 1；tombstone 为 true
// 时置墓碑。revision 每次 +1。
func (t *AIRuntimeTx) BumpControl(agentKey string, genDelta int64, tombstone bool) (*AIRuntimeControl, error) {
	if _, err := t.Control(agentKey); err != nil {
		return nil, err
	}
	tomb := 0
	if tombstone {
		tomb = 1
	}
	if _, err := t.conn.ExecContext(t.ctx, `
		UPDATE ai_runtime_control
		   SET generation = generation + ?,
		       revision = revision + 1,
		       tombstoned = CASE WHEN ?>0 THEN 1 ELSE tombstoned END
		 WHERE agent_key=?`, genDelta, tomb, agentKey); err != nil {
		return nil, err
	}
	return t.Control(agentKey)
}

// BumpRevision 只推进版本（tick/上鱼等相对状态变化）。
func (t *AIRuntimeTx) BumpRevision(agentKey string) (*AIRuntimeControl, error) {
	return t.BumpControl(agentKey, 0, false)
}

func (t *AIRuntimeTx) controlRevision(agentKey string) (int64, error) {
	var rev int64
	err := t.conn.QueryRowContext(t.ctx,
		`SELECT COALESCE((SELECT revision FROM ai_runtime_control WHERE agent_key=?),0)`,
		agentKey).Scan(&rev)
	return rev, err
}

// AgentFlags 读运行态需要的三个开关（未删除才 found）。
func (t *AIRuntimeTx) AgentFlags(agentKey string) (enabled, joined, actorEnabled, found bool, err error) {
	var e, j, a int
	err = t.conn.QueryRowContext(t.ctx,
		`SELECT enabled, joined, actor_enabled FROM ai_agent WHERE key=? AND deleted_ts=0`,
		agentKey).Scan(&e, &j, &a)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, false, nil
	}
	if err != nil {
		return false, false, false, false, err
	}
	return e != 0, j != 0, a != 0, true, nil
}

// AgentIdentity 读逻辑删除需要的 id 与联系人 UID。
func (t *AIRuntimeTx) AgentIdentity(agentKey string) (id, contactUID int64, found bool, err error) {
	err = t.conn.QueryRowContext(t.ctx,
		`SELECT id, contact_uid FROM ai_agent WHERE key=? AND deleted_ts=0`, agentKey).
		Scan(&id, &contactUID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return id, contactUID, true, nil
}

// Setting 读 ai_setting（事务内）。
func (t *AIRuntimeTx) Setting(key, def string) string {
	var v string
	if err := t.conn.QueryRowContext(t.ctx,
		`SELECT value FROM ai_setting WHERE key=?`, key).Scan(&v); err != nil || v == "" {
		return def
	}
	return v
}

// SettingRaw 区分「缺行」「空值」和「读取失败」：
// ok=false 且 err=nil 表示缺行；err!=nil 表示事务内读取失败。
// 急停判定必须用这个版本，不能把读失败当成缺键。
func (t *AIRuntimeTx) SettingRaw(key string) (string, bool, error) {
	var v string
	err := t.conn.QueryRowContext(t.ctx,
		`SELECT value FROM ai_setting WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ActorConfig 读角色活动参数（事务内，口径与 DB.AIActorConfig 相同）。
func (t *AIRuntimeTx) ActorConfig() AIActorConfig {
	return aiActorConfigFromSetting(func(key string) string { return t.Setting(key, "") })
}

// RandomMap 随机挑一张有鱼的地图（事务内）。
func (t *AIRuntimeTx) RandomMap() (loc, name string, err error) {
	return airRandomMap(t.ctx, t.conn)
}

// Actor 读角色状态；没有行时返回 nil, nil。
func (t *AIRuntimeTx) Actor(agentKey string) (*AIActor, error) {
	row := t.conn.QueryRowContext(t.ctx, `
		SELECT agent_key, loc, loc_name, activity, target_fish, target_name,
		       since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts
		FROM ai_actor_state WHERE agent_key=?`, agentKey)
	a, err := scanAIActorRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// SaveActor 在事务内覆盖写角色状态。
func (t *AIRuntimeTx) SaveActor(a *AIActor) error { return saveAIActor(t.ctx, t.conn, a) }

// DeleteActor 在事务内删除角色状态；control 行保留。
func (t *AIRuntimeTx) DeleteActor(agentKey string) error {
	return deleteAIActor(t.ctx, t.conn, agentKey)
}

// SetChannelAgent 在事务内写频道成员。
func (t *AIRuntimeTx) SetChannelAgent(channel, agentKey string, enable bool) error {
	return setChannelAgent(t.ctx, t.conn, channel, agentKey, enable)
}

// AddMemory 在事务内追加一条战绩记忆。
func (t *AIRuntimeTx) AddMemory(agentKey, kind, fishKey, fishName string, weightG int64, loc string) error {
	return addAIMemory(t.ctx, t.conn, agentKey, kind, fishKey, fishName, weightG, loc)
}

// PruneMemories 在事务内只保留最近 keep 条。
func (t *AIRuntimeTx) PruneMemories(agentKey string, keep int) error {
	return pruneAIMemories(t.ctx, t.conn, agentKey, keep)
}

// ClearMemories 在事务内清空某 Agent 的全部战绩记忆。
func (t *AIRuntimeTx) ClearMemories(agentKey string) error {
	return clearAIMemories(t.ctx, t.conn, agentKey)
}

// AddCatch 在事务内写一条 AI 渔获。
func (t *AIRuntimeTx) AddCatch(c AICatch) error { return aiAddCatch(t.ctx, t.conn, c) }

// DeleteAgent 在事务内逻辑删除 Agent 并级联（保留聊天历史/记忆/key/UID）。
// 返回 false 表示此前已删除（幂等）。
func (t *AIRuntimeTx) DeleteAgent(agentKey string) (bool, error) {
	return aiSoftDeleteAgent(t.ctx, t.conn, agentKey)
}

// AIWithRuntimeTx 在一个 BEGIN IMMEDIATE 事务里执行 fn，供 GS 的 tick、
// 上鱼记忆等运行态写入复用同一把锁。
func (d *DB) AIWithRuntimeTx(ctx context.Context, fn func(*AIRuntimeTx) error) error {
	if d == nil || d.SQL == nil {
		return errors.New("ai runtime: 数据库未连接")
	}
	if fn == nil {
		return errors.New("ai runtime: 缺少事务函数")
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("ai runtime: 开启事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := fn(&AIRuntimeTx{conn: conn, ctx: ctx}); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("ai runtime: 提交事务: %w", err)
	}
	committed = true
	return nil
}
