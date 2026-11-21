package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"rf4server/internal/aiconfig"
)

const aiLocalKnowledgeKey = "ai_local_knowledge"

var ErrAILocalKnowledgeConflict = errors.New("本地资料已被更新，请重新读取")

type AILocalKnowledgeSnapshot struct {
	Revision int64                   `json:"revision"`
	Document aiconfig.LocalKnowledge `json:"document"`
}

func (d *DB) AIGetLocalKnowledge(ctx context.Context) (AILocalKnowledgeSnapshot, error) {
	return aiGetLocalKnowledge(ctx, d.SQL)
}

func aiGetLocalKnowledge(ctx context.Context, q AIQueryer) (AILocalKnowledgeSnapshot, error) {
	var snap AILocalKnowledgeSnapshot
	var raw string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM ai_setting WHERE key=?`, aiLocalKnowledgeKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		err := snap.Document.Normalize()
		return snap, err
	case err != nil:
		return snap, err
	}
	if len(raw) > aiconfig.MaxLocalKnowledgeBytes || json.Unmarshal([]byte(raw), &snap) != nil || snap.Revision < 1 {
		return AILocalKnowledgeSnapshot{}, errors.New("本地资料记录损坏")
	}
	err := snap.Document.Normalize()
	return snap, err
}

func (d *DB) AISaveLocalKnowledge(ctx context.Context, base int64, doc aiconfig.LocalKnowledge) (AILocalKnowledgeSnapshot, error) {
	if err := doc.Normalize(); err != nil {
		return AILocalKnowledgeSnapshot{}, err
	}
	snap := AILocalKnowledgeSnapshot{Revision: base + 1, Document: doc}
	raw, err := json.Marshal(snap)
	if err != nil {
		return AILocalKnowledgeSnapshot{}, err
	}
	if len(raw) > aiconfig.MaxLocalKnowledgeBytes {
		return AILocalKnowledgeSnapshot{}, fmt.Errorf("%w：文档超过 256 KB", aiconfig.ErrLocalKnowledgeInvalid)
	}
	err = d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		cur, err := aiGetLocalKnowledge(ctx, tx.SQL())
		if err != nil {
			return err
		}
		if cur.Revision != base {
			return ErrAILocalKnowledgeConflict
		}
		checked := make(map[string]bool, len(doc.ItemAliases))
		for _, a := range doc.ItemAliases {
			if checked[a.ItemID] {
				continue
			}
			var one int
			err := tx.SQL().QueryRowContext(ctx, `SELECT 1 FROM item_catalog WHERE item_id=?`, a.ItemID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w：目录中没有物品 ID %q", aiconfig.ErrLocalKnowledgeInvalid, a.ItemID)
			}
			if err != nil {
				return err
			}
			checked[a.ItemID] = true
		}
		_, err = tx.SQL().ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
			aiLocalKnowledgeKey, string(raw), nowTS())
		return err
	})
	return snap, err
}
