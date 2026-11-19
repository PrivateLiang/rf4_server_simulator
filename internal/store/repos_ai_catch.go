package store

// AI 渔获库：AI 自己钓到的鱼。与玩家 catch_log 分开，不改玩家库存与经济。
// AI 上鱼不再往频道发播报；玩家问起时由 aibridge 查这张表回答。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AICatch 是一条 AI 渔获。
type AICatch struct {
	ID        int64
	AgentKey  string
	FishKey   string
	FishName  string
	WeightG   int64
	Loc       string
	LocName   string
	Event     string
	Bait      string
	CreatedTS float64
}

// AIAddCatch 写入一条 AI 渔获。
func (d *DB) AIAddCatch(ctx context.Context, c AICatch) error {
	return aiAddCatch(ctx, d.SQL, c)
}

// aiAddCatch 是渔获写入的唯一 SQL；GS 在上鱼事务里直接调用。
func aiAddCatch(ctx context.Context, q AIQueryer, c AICatch) error {
	if c.AgentKey == "" || c.FishKey == "" {
		return errors.New("ai catch: 缺少 agent_key 或 fish_key")
	}
	ts := c.CreatedTS
	if ts <= 0 {
		ts = nowTS()
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO ai_catch(agent_key, fish_key, fish_name, weight_g, loc, loc_name,
		                     event, bait, created_ts)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		c.AgentKey, c.FishKey, c.FishName, c.WeightG, c.Loc, c.LocName,
		c.Event, c.Bait, ts)
	return err
}

// AICatchStats 是某个 Agent 的渔获汇总。
type AICatchStats struct {
	Total       int
	TotalG      int64
	Today       int
	BestName    string
	BestWeightG int64
	BestLoc     string
}

// AICatchStats 读某个 Agent 的累计/今日/最大渔获。
func (d *DB) AICatchStats(ctx context.Context, agentKey string) (AICatchStats, error) {
	var s AICatchStats
	if err := d.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(weight_g),0) FROM ai_catch WHERE agent_key=?`,
		agentKey).Scan(&s.Total, &s.TotalG); err != nil {
		return s, err
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	_ = d.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ai_catch WHERE agent_key=? AND created_ts>=?`,
		agentKey, float64(today)).Scan(&s.Today)
	err := d.SQL.QueryRowContext(ctx, `
		SELECT fish_name, weight_g, COALESCE(loc_name,'') FROM ai_catch
		WHERE agent_key=? ORDER BY weight_g DESC, id DESC LIMIT 1`, agentKey).
		Scan(&s.BestName, &s.BestWeightG, &s.BestLoc)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	return s, nil
}

// AIRecentCatches 读最近几条 AI 渔获（新的在前）。
func (d *DB) AIRecentCatches(ctx context.Context, agentKey string, limit int) ([]AICatch, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT id, agent_key, fish_key, fish_name, weight_g, loc, loc_name,
		       event, bait, created_ts
		FROM ai_catch WHERE agent_key=? ORDER BY id DESC LIMIT ?`, agentKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AICatch
	for rows.Next() {
		var c AICatch
		if err := rows.Scan(&c.ID, &c.AgentKey, &c.FishKey, &c.FishName, &c.WeightG,
			&c.Loc, &c.LocName, &c.Event, &c.Bait, &c.CreatedTS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
