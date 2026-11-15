package store

// AI 接入的队列与配置仓储。复用主库事务，不触碰只读静态库。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AITask 是一次待处理的对话请求。只有 source_kind=human 才允许入队，
// 从结构上杜绝 Agent 回复回流触发新的 Agent 请求。
type AITask struct {
	ID           int64
	Kind         string
	Scope        string
	ScopeKey     string
	UID          int64
	Agent        string
	ContactUID   int64
	SourceKind   string
	Text         string
	State        string
	DeadlineTS   float64
	Attempt      int
	Participants string // roundtable：JSON 数组，参与 Agent 的 key
	Loc          string // 入队时的地图 key，用于注入「当前地图鱼情」
}

// AIResult 是一条待投递给客户端的回复。
type AIResult struct {
	ID         int64
	TaskID     int64
	Status     string
	ReplyText  string
	ContactUID int64
	UID        int64
	Kind       string // dm=私聊 mail；channel=公共频道 24/27
	ScopeKey   string // channel 时为频道名
}

// AIAgent 是 Agent 注册表的一行。
type AIAgent struct {
	Key             string
	Display         string
	Role            string
	ContactUID      int64
	Prompt          string
	ToolsJSON       string
	ThinkingEnabled bool
	ThinkingEffort  string
	Enabled         bool
	Joined          bool   // true 才会出现在游戏社交列表并接收 19/1 私聊
	ActorEnabled    bool   // true 才维护「在钓鱼」的角色状态
	ReplyMode       string // social=可主动插话，mention=只有被点名才说，silent=只看不说
	Talkativeness   int    // 0-100，越高越容易主动接话
}

func nowTS() float64 { return float64(time.Now().Unix()) }

// AIEnqueue 写入一条真人发起的对话任务。
func (d *DB) AIEnqueue(ctx context.Context, t *AITask) (int64, error) {
	if t == nil || t.SourceKind != "human" {
		return 0, errors.New("ai: only human messages can be enqueued")
	}
	if t.Participants == "" {
		t.Participants = "[]"
	}
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid,
		                    source_kind, text, state, deadline_ts, attempt, created_ts, participants, loc)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Kind, t.Scope, t.ScopeKey, t.UID, t.Agent, t.ContactUID,
		t.SourceKind, t.Text, "pending", t.DeadlineTS, 0, nowTS(), t.Participants, t.Loc)
	if err != nil {
		return 0, fmt.Errorf("ai enqueue: %w", err)
	}
	return res.LastInsertId()
}

// AIMergePendingDM 把同一玩家刚发的下一条私聊并入尚未处理的 DM 任务，
// 避免连发几句就产生多次模型请求。只有 pending 任务可合并；已开始处理的
// 消息会正常排到后面。
func (d *DB) AIMergePendingDM(ctx context.Context, uid int64, agent, text string, deadline float64, maxRunes int) (bool, error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	if maxRunes <= 0 {
		maxRunes = 500
	}
	res, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_task SET text=text||char(10)||?, deadline_ts=?
		WHERE id=(
			SELECT id FROM ai_task
			WHERE kind='dm' AND uid=? AND agent=? AND state='pending'
			ORDER BY id DESC LIMIT 1
		)
		AND length(text)+length(?)+1<=?`,
		text, deadline, uid, agent, text, maxRunes)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AIPopTask 事务内取最旧一条 pending 任务并标记 processing。
func (d *DB) AIPopTask(ctx context.Context) (*AITask, bool, error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `
		SELECT id, kind, scope, scope_key, uid, agent, contact_uid,
		       source_kind, text, state, deadline_ts, attempt, participants, loc
		FROM ai_task WHERE state='pending' ORDER BY id LIMIT 1`)
	var t AITask
	if err := row.Scan(&t.ID, &t.Kind, &t.Scope, &t.ScopeKey, &t.UID, &t.Agent,
		&t.ContactUID, &t.SourceKind, &t.Text, &t.State, &t.DeadlineTS, &t.Attempt,
		&t.Participants, &t.Loc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_task SET state='processing', attempt=attempt+1 WHERE id=?`, t.ID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

// ErrAITaskNotActive 表示任务已被过期清理或已被其他 worker 处理，不能再写结果。
var ErrAITaskNotActive = errors.New("ai: task is not active")

// AIComplete 写结果并把任务置为终态。
// 只接受 processing 状态：任务若已过期/被清理，迟到的模型输出直接丢弃，避免隔天补发。
// 同时清空 user 原文：聊天内容只留在内存会话与游戏邮件里，不在队列表长期堆积。
func (d *DB) AIComplete(ctx context.Context, taskID int64, status, reply, rawUsage, errText string) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE ai_task SET state=?, text='' WHERE id=? AND state='processing'`, status, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAITaskNotActive
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_result(task_id, status, reply_text, raw_usage, err, created_ts)
		VALUES(?,?,?,?,?,?)`, taskID, status, reply, rawUsage, errText, nowTS()); err != nil {
		return fmt.Errorf("ai result: %w", err)
	}
	return tx.Commit()
}

// AIExpireTasks 清理超时任务并回收异常退出留下的 processing 任务。
//   - pending 且已过 deadline：标记 expired，不再处理；
//   - processing 且超过 deadline+grace 且只跑过一次：退回 pending，允许重试一次；
//   - processing 且已重试过：标记 fallback，避免无限重跑。
func (d *DB) AIExpireTasks(ctx context.Context, now, grace float64) (expired, recovered int64, err error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE ai_task SET state='expired', text=''
		WHERE state='pending' AND deadline_ts>0 AND deadline_ts<?`, now)
	if err != nil {
		return 0, 0, err
	}
	expired, _ = res.RowsAffected()
	res, err = tx.ExecContext(ctx, `
		UPDATE ai_task SET state='pending'
		WHERE state='processing' AND deadline_ts>0 AND deadline_ts<? AND attempt<2`, now-grace)
	if err != nil {
		return 0, 0, err
	}
	recovered, _ = res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_task SET state='fallback', text=''
		WHERE state='processing' AND deadline_ts>0 AND deadline_ts<? AND attempt>=2`, now-grace); err != nil {
		return 0, 0, err
	}
	return expired, recovered, tx.Commit()
}

// AIExpireToolJobs 把长时间停在 processing 的工具作业标记为超时，
// 防止 gameserver 异常退出后作业永远占着队列。
func (d *DB) AIExpireToolJobs(ctx context.Context, before float64) (int64, error) {
	res, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_tool_job SET state='done', result_text='（查询超时）', done_ts=?
		WHERE state='processing' AND created_ts<?`, nowTS(), before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AIPrune 清理已投递/已终结的历史记录，避免 ai_* 表无限增长。
// 聊天原文在任务结束时已清空；这里只删元数据与迟到的空结果。
func (d *DB) AIPrune(ctx context.Context, before float64) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_tool_job WHERE state='done' AND created_ts<?`, before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_result WHERE delivered_ts IS NOT NULL AND created_ts<?`, before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_task
		WHERE state IN ('done','fallback','expired') AND created_ts<?
		  AND NOT EXISTS (SELECT 1 FROM ai_result WHERE task_id=ai_task.id)`, before); err != nil {
		return err
	}
	return tx.Commit()
}

// AIPendingResults 取尚未投递的结果，联表带回收件人与联系人 UID。
func (d *DB) AIPendingResults(ctx context.Context, limit int) ([]AIResult, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT r.id, r.task_id, r.status, r.reply_text,
		       COALESCE(NULLIF(r.contact_uid,0), t.contact_uid, 0),
		       COALESCE(t.uid,0),
		       CASE WHEN t.kind='roundtable' THEN 'channel' ELSE COALESCE(t.kind,'dm') END,
		       COALESCE(t.scope_key,'')
		FROM ai_result r LEFT JOIN ai_task t ON t.id=r.task_id
		WHERE r.delivered_ts IS NULL AND r.reply_text <> ''
		ORDER BY r.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIResult
	for rows.Next() {
		var r AIResult
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Status, &r.ReplyText,
			&r.ContactUID, &r.UID, &r.Kind, &r.ScopeKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AIMarkDelivered 标记结果已投递（或投递失败）。
// 投递成功后清空回复正文：DM 已写进游戏邮件，频道已在客户端展示，
// 不再让聊天内容长期留在 AI 队列里。
func (d *DB) AIMarkDelivered(ctx context.Context, resultID int64, ok bool) error {
	state := float64(0)
	if !ok {
		state = -1
	}
	if ok {
		_, err := d.SQL.ExecContext(ctx,
			`UPDATE ai_result SET delivered_ts=?, reply_text='' WHERE id=?`, nowTS(), resultID)
		return err
	}
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_result SET delivered_ts=? WHERE id=? AND delivered_ts IS NULL`, state, resultID)
	return err
}

// AIAgentByKey 取一个 Agent 配置。
func (d *DB) AIAgentByKey(ctx context.Context, key string) (*AIAgent, error) {
	row := d.SQL.QueryRowContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50)
		FROM ai_agent WHERE key=? AND deleted_ts=0`, key)
	var a AIAgent
	var thinking, enabled, joined, actor int
	if err := row.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
		&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness); err != nil {
		return nil, err
	}
	a.ThinkingEnabled = thinking != 0
	a.Enabled = enabled != 0
	a.Joined = joined != 0
	a.ActorEnabled = actor != 0
	return &a, nil
}

// AIListAgents 返回全部已启用的 Agent（后台用，含未接入的），按联系人 UID 降序。
func (d *DB) AIListAgents(ctx context.Context) ([]AIAgent, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50)
		FROM ai_agent WHERE enabled=1 AND deleted_ts=0 ORDER BY contact_uid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var thinking, enabled, joined, actor int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
			&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = thinking != 0
		a.Enabled = enabled != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// AIJoinedAgents 只返回已接入游戏的 Agent（社交成员列表用）。
func (d *DB) AIJoinedAgents(ctx context.Context) ([]AIAgent, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50)
		FROM ai_agent WHERE enabled=1 AND joined=1 AND deleted_ts=0 ORDER BY contact_uid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var thinking, enabled, joined, actor int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
			&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = thinking != 0
		a.Enabled = enabled != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// AIAgentByContactUID 按联系人 UID 找已接入的 Agent（19/1 分流与资料卡用）。
func (d *DB) AIAgentByContactUID(ctx context.Context, uid int64) (*AIAgent, error) {
	row := d.SQL.QueryRowContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50)
		FROM ai_agent WHERE contact_uid=? AND enabled=1 AND joined=1 AND deleted_ts=0`, uid)
	var a AIAgent
	var thinking, enabled, joined, actor int
	if err := row.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
		&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness); err != nil {
		return nil, err
	}
	a.ThinkingEnabled = thinking != 0
	a.Enabled = enabled != 0
	a.Joined = joined != 0
	a.ActorEnabled = actor != 0
	return &a, nil
}

// AINextContactUID 从 2147483640 向下找一个没被占用的 AI 联系人 UID。
func (d *DB) AINextContactUID(ctx context.Context) (int64, error) {
	used := map[int64]bool{}
	rows, err := d.SQL.QueryContext(ctx, `SELECT contact_uid FROM ai_agent`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			used[u] = true
		}
	}
	_ = rows.Close()
	for u := int64(2147483640); u > aiContactUIDMin; u-- {
		if !used[u] {
			return u, nil
		}
	}
	return 0, errors.New("ai: 没有可用的联系人 UID")
}

// ErrAIAgentExists 表示这个 key 已经有一个在用的 Agent。
var ErrAIAgentExists = errors.New("ai: agent key 已存在")

// ErrAIContactUIDUsed 表示指定的联系人 UID 已经被占用。
var ErrAIContactUIDUsed = errors.New("ai: contact_uid 已被占用")

// ErrAIContactUIDInvalid 表示联系人 UID 不在虚拟联系人专用段内。
// 允许管理员改 UID，但不能写进真实玩家号段，否则 19/1 私聊会被错误分流。
var ErrAIContactUIDInvalid = errors.New("ai: contact_uid 必须在 2147480000-2147483645")

const (
	aiContactUIDMin int64 = 2147480000
	aiContactUIDMax int64 = 2147483645
)

func validAIContactUID(uid int64) bool { return uid >= aiContactUIDMin && uid <= aiContactUIDMax }

// aiContactUIDUsed 判断联系人 UID 是否已被别的 Agent（含逻辑删除的）占用。
func (d *DB) aiContactUIDUsed(ctx context.Context, uid, exceptID int64) (bool, error) {
	var n int
	if err := d.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_agent WHERE contact_uid=? AND id<>?`, uid, exceptID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// AICreateAgent 后台新建 Agent，默认不接入游戏。
// 如果这个 key 之前被逻辑删除过，则恢复原记录并复用联系人 UID：
// 私聊对象与历史因此不会断，也不会把旧 UID 分配给别的新角色。
func (d *DB) AICreateAgent(ctx context.Context, a AIAgent, toolsJSON string) (int64, bool, error) {
	thinking := 0
	if a.ThinkingEnabled {
		thinking = 1
	}
	mode := a.ReplyMode
	if mode != "mention" && mode != "silent" {
		mode = "social"
	}
	talk := a.Talkativeness
	if talk < 0 || talk > 100 {
		talk = 50
	}

	var id, oldUID int64
	var deletedTS float64
	err := d.SQL.QueryRowContext(ctx,
		`SELECT id, contact_uid, deleted_ts FROM ai_agent WHERE key=?`, a.Key).
		Scan(&id, &oldUID, &deletedTS)
	switch {
	case err == nil && deletedTS == 0:
		return 0, false, ErrAIAgentExists
	case err == nil:
		// 恢复：保留联系人 UID、战绩与历史；角色配置按这次填的覆盖。
		uid := oldUID
		if a.ContactUID > 0 {
			if !validAIContactUID(a.ContactUID) {
				return 0, false, ErrAIContactUIDInvalid
			}
			used, checkErr := d.aiContactUIDUsed(ctx, a.ContactUID, id)
			if checkErr != nil {
				return 0, false, checkErr
			}
			if used {
				return 0, false, ErrAIContactUIDUsed
			}
			uid = a.ContactUID
		}
		_, upErr := d.SQL.ExecContext(ctx, `
			UPDATE ai_agent SET display=?, role=?, contact_uid=?, prompt=?, tools_json=?,
			  thinking_enabled=?, thinking_effort=?, enabled=1, joined=0, actor_enabled=0,
			  reply_mode=?, talkativeness=?, deleted_ts=0
			WHERE id=?`,
			a.Display, a.Role, uid, a.Prompt, toolsJSON,
			thinking, a.ThinkingEffort, mode, talk, id)
		if upErr != nil {
			return 0, false, upErr
		}
		return uid, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}

	uid := a.ContactUID
	if uid == 0 {
		uid, err = d.AINextContactUID(ctx)
		if err != nil {
			return 0, false, err
		}
	} else {
		if !validAIContactUID(uid) {
			return 0, false, ErrAIContactUIDInvalid
		}
		used, checkErr := d.aiContactUIDUsed(ctx, uid, 0)
		if checkErr != nil {
			return 0, false, checkErr
		}
		if used {
			return 0, false, ErrAIContactUIDUsed
		}
	}
	_, err = d.SQL.ExecContext(ctx, `
		INSERT INTO ai_agent(key, display, role, contact_uid, prompt, tools_json,
		                     thinking_enabled, thinking_effort, enabled, joined,
		                     reply_mode, talkativeness, deleted_ts)
		VALUES(?,?,?,?,?,?,?,?,1,0,?,?,0)`,
		a.Key, a.Display, a.Role, uid, a.Prompt, toolsJSON,
		thinking, a.ThinkingEffort, mode, talk)
	if err != nil {
		return 0, false, err
	}
	return uid, false, nil
}

// AISetAgentBehavior 设置频道里的说话方式与活跃度。
func (d *DB) AISetAgentBehavior(ctx context.Context, key, mode string, talk int) error {
	if mode != "social" && mode != "mention" && mode != "silent" {
		return errors.New("ai: 未知的 reply_mode")
	}
	if talk < 0 {
		talk = 0
	}
	if talk > 100 {
		talk = 100
	}
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_agent SET reply_mode=?, talkativeness=? WHERE key=? AND deleted_ts=0`, mode, talk, key)
	return err
}

// AISetAgentJoined 接入/移出游戏；接入时可同时改联系人 UID。
func (d *DB) AISetAgentJoined(ctx context.Context, key string, joined bool, contactUID int64) error {
	j := 0
	if joined {
		j = 1
	}
	if contactUID != 0 && !validAIContactUID(contactUID) {
		return ErrAIContactUIDInvalid
	}
	if contactUID > 0 {
		_, err := d.SQL.ExecContext(ctx,
			`UPDATE ai_agent SET joined=?, contact_uid=? WHERE key=? AND deleted_ts=0`, j, contactUID, key)
		return err
	}
	_, err := d.SQL.ExecContext(ctx, `UPDATE ai_agent SET joined=? WHERE key=? AND deleted_ts=0`, j, key)
	return err
}

// AIUpdateAgent 改角色、提示词与展示名。
func (d *DB) AIUpdateAgent(ctx context.Context, key, display, role, prompt, toolsJSON string) error {
	_, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_agent SET display=?, role=?, prompt=?, tools_json=? WHERE key=? AND deleted_ts=0`,
		display, role, prompt, toolsJSON, key)
	return err
}

// AIDeleteAgent 逻辑删除：删的是账号，不删聊天对象与历史。
// 保留联系人 UID、ai_result 与 ai_memory，之后用同一个 key 新建可恢复；
// 同时清掉运行态、频道订阅与尚未投递的旧消息，避免污染新角色。
func (d *DB) AIDeleteAgent(ctx context.Context, key string) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var agentID, uid int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id, contact_uid FROM ai_agent WHERE key=? AND deleted_ts=0`, key).
		Scan(&agentID, &uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 已经删过：保持幂等
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_agent SET deleted_ts=?, enabled=0, joined=0, actor_enabled=0 WHERE id=?`,
		nowTS(), agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_actor_state WHERE agent_key=?`, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_channel_agent SET enabled=0 WHERE agent_id=?`, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_task SET state='fallback' WHERE agent=? AND state IN ('pending','processing')`,
		key); err != nil {
		return err
	}
	if uid > 0 {
		// 旧回复不再投递（delivered_ts=-1 表示作废），但记录本身保留。
		if _, err := tx.ExecContext(ctx,
			`UPDATE ai_result SET delivered_ts=-1 WHERE delivered_ts IS NULL AND contact_uid=?`,
			uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AIAddChannelResult 为圆桌追加一条频道发言，发送者单独记在 contact_uid。
// 任务已过期时迟到的发言不再入库，避免客户端收到过时内容。
func (d *DB) AIAddChannelResult(ctx context.Context, taskID, contactUID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_result(task_id,status,reply_text,raw_usage,err,created_ts,contact_uid)
		SELECT ?,'done',?,'','',?,?
		WHERE EXISTS (SELECT 1 FROM ai_task WHERE id=? AND state='processing')`,
		taskID, text, nowTS(), contactUID, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAITaskNotActive
	}
	return nil
}

// AIFinishTask 只改任务状态，不额外插结果（圆桌已逐条写过结果）。
// 只更新 processing 任务，避免把已过期任务重新写成 done。
func (d *DB) AIFinishTask(ctx context.Context, id int64, state string) error {
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_task SET state=?, text='' WHERE id=? AND state='processing'`, state, id)
	return err
}

// AIEnqueueSystemRoundtable 只允许系统创建圆桌任务（AI 上鱼后的庆祝圆桌）。
// 普通入队仍然只接受 source_kind=human，Agent 输出无法借此回流。
func (d *DB) AIEnqueueSystemRoundtable(ctx context.Context, channel string, uid int64, text string, participants []string) (int64, error) {
	if len(participants) < 2 {
		return 0, errors.New("ai: 圆桌至少需要 2 个参与者")
	}
	parts, err := json.Marshal(participants)
	if err != nil {
		return 0, err
	}
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid,
		                    source_kind, text, state, deadline_ts, attempt, created_ts, participants)
		VALUES('roundtable','channel',?,?, '',0,'system',?,'pending',?,0,?,?)`,
		channel, uid, text, nowTS()+60, nowTS(), string(parts))
	if err != nil {
		return 0, fmt.Errorf("ai system roundtable: %w", err)
	}
	return res.LastInsertId()
}

// AIMemory 是一条结构化战绩记忆（星/蓝），不占对话历史。
type AIMemory struct {
	ID        int64
	Kind      string
	FishKey   string
	FishName  string
	WeightG   int64
	Loc       string
	CreatedTS float64
}

// AIAddMemory 写入一条战绩。
func (d *DB) AIAddMemory(ctx context.Context, agentKey, kind, fishKey, fishName string, weightG int64, loc string) error {
	_, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_memory(agent_key,kind,fish_key,fish_name,weight_g,loc,created_ts)
		VALUES(?,?,?,?,?,?,?)`,
		agentKey, kind, fishKey, fishName, weightG, loc, nowTS())
	return err
}

// AIRecentMemories 最近 N 条。
func (d *DB) AIRecentMemories(ctx context.Context, agentKey string, limit int) ([]AIMemory, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.aiMemories(ctx, `
		SELECT id,kind,fish_key,fish_name,weight_g,loc,created_ts
		FROM ai_memory WHERE agent_key=? ORDER BY id DESC LIMIT ?`, agentKey, limit)
}

// AIBestMemories 最佳 N 条：蓝冠优先，其次按重量。
func (d *DB) AIBestMemories(ctx context.Context, agentKey string, limit int) ([]AIMemory, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.aiMemories(ctx, `
		SELECT id,kind,fish_key,fish_name,weight_g,loc,created_ts
		FROM ai_memory WHERE agent_key=?
		ORDER BY CASE WHEN kind='super' THEN 0 ELSE 1 END, weight_g DESC, id DESC LIMIT ?`, agentKey, limit)
}

func (d *DB) aiMemories(ctx context.Context, query string, args ...any) ([]AIMemory, error) {
	rows, err := d.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIMemory
	for rows.Next() {
		var m AIMemory
		if err := rows.Scan(&m.ID, &m.Kind, &m.FishKey, &m.FishName, &m.WeightG, &m.Loc, &m.CreatedTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AIPruneMemories 只保留最近 keep 条。
func (d *DB) AIPruneMemories(ctx context.Context, agentKey string, keep int) error {
	if keep <= 0 {
		keep = 50
	}
	_, err := d.SQL.ExecContext(ctx, `
		DELETE FROM ai_memory WHERE agent_key=? AND id NOT IN (
			SELECT id FROM ai_memory WHERE agent_key=? ORDER BY id DESC LIMIT ?
		)`, agentKey, agentKey, keep)
	return err
}

// AIClearMemories 清空某 Agent 的全部战绩。
func (d *DB) AIClearMemories(ctx context.Context, agentKey string) error {
	_, err := d.SQL.ExecContext(ctx, `DELETE FROM ai_memory WHERE agent_key=?`, agentKey)
	return err
}

// AIChannelInfo 是后台频道下拉的一项。
type AIChannelInfo struct {
	Name   string
	Label  string
	Count  int
	Agents []string
}

// AIMapInfo 是后台地图级订阅下拉的一项。
type AIMapInfo struct {
	MapKey string
	Label  string
	Count  int
	Agents []string
}

// AIUpsertChannel 记录客户端实际用过的频道名（切频道时调用）。
func (d *DB) AIUpsertChannel(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	_, err := d.SQL.ExecContext(ctx, `INSERT OR IGNORE INTO ai_channel(name) VALUES(?)`, name)
	return err
}

// AIListChannels 返回频道 + 显示名 + 已接入 Agent 数/名单。
// 频道来源 = ai_channel ∪ ai_channel_agent，兼容历史数据。
func (d *DB) AIListChannels(ctx context.Context) ([]AIChannelInfo, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT ch.name, COALESCE(c.label,''), ag.display
		FROM (SELECT name FROM ai_channel UNION SELECT channel FROM ai_channel_agent) ch
		LEFT JOIN ai_channel c ON c.name=ch.name
		LEFT JOIN ai_channel_agent a ON a.channel=ch.name AND a.enabled=1
		LEFT JOIN ai_agent ag ON ag.id=a.agent_id AND ag.enabled=1 AND ag.deleted_ts=0
		ORDER BY ch.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	order := make([]string, 0, 8)
	byName := map[string]*AIChannelInfo{}
	for rows.Next() {
		var name, label string
		var display sql.NullString
		if err := rows.Scan(&name, &label, &display); err != nil {
			return nil, err
		}
		info, ok := byName[name]
		if !ok {
			info = &AIChannelInfo{Name: name, Label: label}
			byName[name] = info
			order = append(order, name)
		}
		if display.Valid && display.String != "" {
			info.Agents = append(info.Agents, display.String)
			info.Count++
		}
	}
	out := make([]AIChannelInfo, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, rows.Err()
}

// AISetChannelLabel 设置频道显示名；空字符串表示恢复显示原始 name。
func (d *DB) AISetChannelLabel(ctx context.Context, name, label string) error {
	_, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_channel(name, label) VALUES(?,?)
		ON CONFLICT(name) DO UPDATE SET label=excluded.label`, name, label)
	return err
}

// AIChannelCooldownMS 取频道内 Agent 的最小冷却（毫秒）。
func (d *DB) AIChannelCooldownMS(ctx context.Context, channel string) int {
	var v int
	if err := d.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(cooldown_ms),8000) FROM ai_channel_agent
		WHERE channel=? AND enabled=1`, channel).Scan(&v); err != nil || v <= 0 {
		return 8000
	}
	return v
}

// ChannelMapKey 把频道名解析成它所属的地图 key。
//
// 官服频道名形如 nav<地图序号>_<图内区域序号>：nav 前缀的数字就是该地图
// level_0NN_xxx 的 NN。实测 13 张有 nav 数组的图全部命中，无需映射表。
// 补零不统一（nav1 与 nav03 都存在），所以按数字解析而不是按固定宽度切串。
//
// 解析不出地图时返回 ("", false)：调用方降级为「精确订阅 ∪ 全局」，
// 不报错、不静默丢弃。私人频道（玩家自建，名字不遵循 nav 命名）也走这条路。
func (d *DB) ChannelMapKey(ctx context.Context, channel string) (string, bool) {
	channel = strings.TrimSpace(channel)
	if !strings.HasPrefix(channel, "nav") {
		return "", false
	}
	rest := channel[len("nav"):]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", false
	}
	num, err := strconv.Atoi(rest[:i])
	if err != nil || num <= 0 {
		return "", false
	}
	// level_%03d_ 前缀在地图清单里唯一，loc_alias 的 alias 是 level key。
	// 注意 LIKE 里的下划线是单字符通配符，要用 ESCAPE 转义，否则 level_001_
	// 会匹配不到 level_001_lake（缺 % 时）或误匹配别的 key。
	var key string
	err = d.SQL.QueryRowContext(ctx, `
		SELECT alias FROM loc_alias
		WHERE alias LIKE ? ESCAPE '\' ORDER BY alias LIMIT 1`,
		"level\\_"+fmt.Sprintf("%03d", num)+"\\_%").Scan(&key)
	if err != nil || key == "" {
		return "", false
	}
	return key, true
}

// aiChannelAgentRows 是三源并集查询共用的列清单。
const aiChannelAgentRows = `a.key, a.display, COALESCE(a.role,''), a.contact_uid, a.prompt,
	       COALESCE(a.tools_json,'[]'), COALESCE(a.thinking_enabled,0),
	       COALESCE(a.thinking_effort,''), COALESCE(a.enabled,1), COALESCE(a.joined,0),
	       COALESCE(a.actor_enabled,0), COALESCE(a.reply_mode,'social'), COALESCE(a.talkativeness,50)`

// AIChannelAgents 返回该频道可发言的全部 Agent。
//
// 可见性 = 地图级订阅 ∪ 精确频道订阅 ∪ （全局开关开启时的全部已接入 Agent）。
// 官服一张图有十几个到上百个区域频道，只按精确频道订阅时玩家在图内切区域
// 就会失聪，所以地图级订阅是主路径，精确订阅保留为高级用法。
// 全局开关默认关，且默认不含 reply_mode='silent'（只看不说）的角色。
func (d *DB) AIChannelAgents(ctx context.Context, channel string) ([]AIAgent, error) {
	mapKey, hasMap := d.ChannelMapKey(ctx, channel)
	// 无地图时把 map_key 参数置空串：ai_map_agent 里不会有空 map_key，
	// 因此地图级来源自然为空，只剩精确订阅（私人频道走的就是这条路）。
	mapArg := ""
	if hasMap {
		mapArg = mapKey
	}

	// 全局开关：开启后所有已接入且会主动接话的 Agent 在任何频道都能回复。
	// 默认关；默认不含 reply_mode='silent'（只看不说）的角色。
	globalOn := d.AISetting(ctx, "ai_global_subscribe", "0") == "1"
	includeSilent := d.AISetting(ctx, "ai_global_subscribe_include_silent", "0") == "1"

	globalSrc := ``
	if globalOn {
		globalSrc = `
			UNION
			SELECT a.id, a.key, 100 FROM ai_agent a
				WHERE a.enabled=1 AND a.joined=1 AND a.deleted_ts=0
				  AND (?=1 OR a.reply_mode<>'silent')`
	}

	rows, err := d.SQL.QueryContext(ctx, `
		SELECT `+aiChannelAgentRows+`
		FROM (
			SELECT a.id, a.key, c.priority FROM ai_channel_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.channel=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1`+globalSrc+`
		) src
		JOIN ai_agent a ON a.id=src.id AND a.enabled=1 AND a.deleted_ts=0
		ORDER BY src.priority, a.id`, channel, mapArg, boolToInt(includeSilent))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var te, en, joined, actor int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt,
			&a.ToolsJSON, &te, &a.ThinkingEffort, &en, &joined, &actor,
			&a.ReplyMode, &a.Talkativeness); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = te != 0
		a.Enabled = en != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AIHasNewerHumanTask 判断圆桌期间是否有新的真人消息（人类优先，立即中止）。
func (d *DB) AIHasNewerHumanTask(ctx context.Context, afterID int64) (bool, error) {
	var n int
	err := d.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ai_task
		WHERE id>? AND state IN ('pending','processing') AND source_kind='human'`, afterID).Scan(&n)
	return n > 0, err
}

// AISetting 读一个全局开关；缺失时返回 def。
func (d *DB) AISetting(ctx context.Context, key, def string) string {
	var v string
	if err := d.SQL.QueryRowContext(ctx, `SELECT value FROM ai_setting WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// AISetSetting 写全局开关（同时用于后台手动打开联网搜索与紧急停止）。
func (d *DB) AISetSetting(ctx context.Context, key, value string) error {
	return d.AISetSettings(ctx, map[string]string{key: value})
}

// AISetSettings 一次事务写入多个开关，避免后台保存一半失败后配置处于混合状态。
func (d *DB) AISetSettings(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
			k, v, nowTS()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AIQueueStats 是后台状态卡用的队列积压快照。
type AIQueueStats struct {
	Pending        int
	Processing     int
	ToolPending    int
	ToolProcessing int
}

// AIQueueStats 读取当前队列积压。
func (d *DB) AIQueueStats(ctx context.Context) (AIQueueStats, error) {
	var s AIQueueStats
	err := d.SQL.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM ai_task WHERE state='pending'),
		  (SELECT COUNT(*) FROM ai_task WHERE state='processing'),
		  (SELECT COUNT(*) FROM ai_tool_job WHERE state='pending'),
		  (SELECT COUNT(*) FROM ai_tool_job WHERE state='processing')`).
		Scan(&s.Pending, &s.Processing, &s.ToolPending, &s.ToolProcessing)
	return s, err
}

// AISecret 读写厂商密钥的密文。明文不落库。
func (d *DB) AISecret(ctx context.Context, providerID int64) (ciphertext, nonce []byte, err error) {
	err = d.SQL.QueryRowContext(ctx,
		`SELECT ciphertext, nonce FROM ai_secret WHERE provider_id=?`, providerID).
		Scan(&ciphertext, &nonce)
	return
}

// AISaveSecret 保存密文，masked 只保留尾四位用于后台展示。
func (d *DB) AISaveSecret(ctx context.Context, providerID int64, ciphertext, nonce []byte, masked string) error {
	_, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES(?,?,?,'default',?,?)
		ON CONFLICT(provider_id) DO UPDATE SET
		  ciphertext=excluded.ciphertext, nonce=excluded.nonce,
		  masked=excluded.masked, updated_ts=excluded.updated_ts`,
		providerID, ciphertext, nonce, masked, nowTS())
	return err
}

// AISetChannelAgent 把 Agent 拉进或移出频道（对应 @邀请 / @移出 指令与后台勾选）。
func (d *DB) AISetChannelAgent(ctx context.Context, channel, agentKey string, enable bool) error {
	if !enable {
		_, err := d.SQL.ExecContext(ctx, `
			DELETE FROM ai_channel_agent
			WHERE channel=? AND agent_id=(SELECT id FROM ai_agent WHERE key=?)`, channel, agentKey)
		return err
	}
	_, err := d.SQL.ExecContext(ctx, `
		INSERT OR IGNORE INTO ai_channel_agent(channel,agent_id,enabled,priority,cooldown_ms,added_by,added_ts)
		SELECT ?, id, 1, 100, 8000, 'chat', ? FROM ai_agent WHERE key=?`, channel, nowTS(), agentKey)
	return err
}

// AISetMapAgent 把 Agent 拉进或移出地图（后台地图级订阅勾选）。
// 订阅一张地图 = 该图所有区域频道都能发言，是官服频道体系下的主路径。
func (d *DB) AISetMapAgent(ctx context.Context, mapKey, agentKey string, enable bool) error {
	mapKey = strings.TrimSpace(mapKey)
	if mapKey == "" || agentKey == "" {
		return nil
	}
	if !enable {
		_, err := d.SQL.ExecContext(ctx, `
			DELETE FROM ai_map_agent
			WHERE map_key=? AND agent_id=(SELECT id FROM ai_agent WHERE key=?)`, mapKey, agentKey)
		return err
	}
	_, err := d.SQL.ExecContext(ctx, `
		INSERT OR IGNORE INTO ai_map_agent(map_key,agent_id,enabled,priority,cooldown_ms,added_by,added_ts)
		SELECT ?, id, 1, 100, 8000, 'admin', ? FROM ai_agent WHERE key=?`, mapKey, nowTS(), agentKey)
	return err
}

// AIListMaps 返回全部地图 key + 中文名 + 已订阅的 Agent 名单。
// 地图来源 = loc_alias 里 level_ 前缀的行（即地图清单），不依赖玩家是否到过。
func (d *DB) AIListMaps(ctx context.Context) ([]AIMapInfo, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT m.alias, COALESCE(m.loc,''), ag.display
		FROM (SELECT alias, loc FROM loc_alias WHERE alias LIKE ? ESCAPE '\') m
		LEFT JOIN ai_map_agent c ON c.map_key=m.alias AND c.enabled=1
		LEFT JOIN ai_agent ag ON ag.id=c.agent_id AND ag.enabled=1 AND ag.deleted_ts=0
		ORDER BY m.alias`, `level\_%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	order := make([]string, 0, 8)
	byKey := map[string]*AIMapInfo{}
	for rows.Next() {
		var key, label string
		var display sql.NullString
		if err := rows.Scan(&key, &label, &display); err != nil {
			return nil, err
		}
		info, ok := byKey[key]
		if !ok {
			info = &AIMapInfo{MapKey: key, Label: label}
			byKey[key] = info
			order = append(order, key)
		}
		if display.Valid && display.String != "" {
			info.Agents = append(info.Agents, display.String)
			info.Count++
		}
	}
	out := make([]AIMapInfo, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out, rows.Err()
}

// AIMapAgents 返回订阅了某地图的 Agent key 列表（后台展示用）。
func (d *DB) AIMapAgents(ctx context.Context, mapKey string) ([]string, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT a.key FROM ai_map_agent c
		JOIN ai_agent a ON a.id=c.agent_id AND a.enabled=1 AND a.deleted_ts=0
		WHERE c.map_key=? AND c.enabled=1 ORDER BY c.priority, a.id`, mapKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AIChannelSubscribed 返回该频道可发言的 Agent key 列表（后台展示、钓鱼选人）。
// 与 AIChannelAgents 同一套可见性口径：地图级 ∪ 精确 ∪ （全局开关开启时全部）。
func (d *DB) AIChannelSubscribed(ctx context.Context, channel string) ([]string, error) {
	mapKey, hasMap := d.ChannelMapKey(ctx, channel)
	mapArg := ""
	if hasMap {
		mapArg = mapKey
	}
	globalOn := d.AISetting(ctx, "ai_global_subscribe", "0") == "1"
	includeSilent := d.AISetting(ctx, "ai_global_subscribe_include_silent", "0") == "1"

	globalSrc := ``
	if globalOn {
		globalSrc = `
			UNION
			SELECT a.id, a.key, 100 FROM ai_agent a
				WHERE a.enabled=1 AND a.joined=1 AND a.deleted_ts=0
				  AND (?=1 OR a.reply_mode<>'silent')`
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT a.key FROM (
			SELECT a.id, a.key, c.priority FROM ai_channel_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.channel=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1`+globalSrc+`
		) src
		JOIN ai_agent a ON a.id=src.id AND a.enabled=1 AND a.deleted_ts=0
		ORDER BY src.priority, a.id`, channel, mapArg, boolToInt(includeSilent))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AIToolJob 是交给 gameserver 执行的只读工具请求。
type AIToolJob struct {
	ID     int64
	TaskID int64
	Tool   string
	Args   string
	UID    int64
}

// AIEnqueueToolJob 写入一条 fish_query 工具请求。
func (d *DB) AIEnqueueToolJob(ctx context.Context, taskID int64, tool, args string, uid int64) (int64, error) {
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_tool_job(task_id,tool,args,uid,state,created_ts)
		VALUES(?,?,?,?,'pending',?)`, taskID, tool, args, uid, nowTS())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AIPendingToolJobs 取待处理工具请求并标记 processing。
func (d *DB) AIPendingToolJobs(ctx context.Context, limit int) ([]AIToolJob, error) {
	if limit <= 0 {
		limit = 8
	}
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, task_id, tool, args, uid FROM ai_tool_job
		WHERE state='pending' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []AIToolJob
	for rows.Next() {
		var j AIToolJob
		if err := rows.Scan(&j.ID, &j.TaskID, &j.Tool, &j.Args, &j.UID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	_ = rows.Close()
	for _, j := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE ai_tool_job SET state='processing' WHERE id=?`, j.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// AICompleteToolJob 写回工具结果。
func (d *DB) AICompleteToolJob(ctx context.Context, id int64, result string) error {
	_, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_tool_job SET state='done', result_text=?, done_ts=? WHERE id=?`,
		result, nowTS(), id)
	return err
}

// AIGetToolResult 供 aibridge 轮询结果；未完成时 ok=false。
func (d *DB) AIGetToolResult(ctx context.Context, id int64) (string, bool, error) {
	var state, result string
	err := d.SQL.QueryRowContext(ctx,
		`SELECT state, result_text FROM ai_tool_job WHERE id=?`, id).Scan(&state, &result)
	if err != nil {
		return "", false, err
	}
	return result, state == "done", nil
}

// AISetActorEnabled 开关某个 Agent 的角色状态（是否"在钓鱼"）。
func (d *DB) AISetActorEnabled(ctx context.Context, key string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := d.SQL.ExecContext(ctx, `UPDATE ai_agent SET actor_enabled=? WHERE key=? AND deleted_ts=0`, v, key)
	if err != nil {
		return err
	}
	// 关掉时顺手清掉它已有的状态，避免后台还挂着「在克马羚诺也湖钓鱼」。
	if !on {
		_, _ = d.SQL.ExecContext(ctx, `DELETE FROM ai_actor_state WHERE agent_key=?`, key)
	}
	return nil
}

// AIEnqueueSystemChannel 只允许系统创建「某个 Agent 在频道里接一句」的任务，
// 用于玩家自己上星/上蓝时让 AI 反应一下。Agent 的输出仍然只进 ai_result，
// 不会回流成新任务。
func (d *DB) AIEnqueueSystemChannel(ctx context.Context, channel string, uid int64,
	text, agentKey string, contactUID int64) (int64, error) {
	if channel == "" || agentKey == "" {
		return 0, errors.New("ai: 频道或 Agent 为空")
	}
	res, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid,
		                    source_kind, text, state, deadline_ts, attempt, created_ts, participants, loc)
		VALUES('channel','channel',?,?,?,?,'system',?,'pending',?,0,?,'[]','')`,
		channel, uid, agentKey, contactUID, text, nowTS()+30, nowTS())
	if err != nil {
		return 0, fmt.Errorf("ai system channel: %w", err)
	}
	return res.LastInsertId()
}
