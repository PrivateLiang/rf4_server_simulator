-- AI 运行态单写者命令契约（T-020 / DEV-004）。
-- AD 改意图、BR 提建议、GS 公聊命令都先落 ai_runtime_command；
-- 唯一活动 gameserver 在同一写事务里校验代次/版本、更新运行态并记终态。
-- ai_runtime_control 是每 Agent 的单调代次与版本真源：删除 Actor 行也保留
-- control 行，禁止删除后代次归零；现有 Agent 首次接管 generation=1、revision=0。

CREATE TABLE IF NOT EXISTS ai_runtime_command (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id          TEXT NOT NULL UNIQUE,
    source              TEXT NOT NULL,
    kind                TEXT NOT NULL,
    agent_key           TEXT NOT NULL,
    expected_generation INTEGER NOT NULL DEFAULT 0,
    expected_revision   INTEGER NOT NULL DEFAULT 0,
    payload             TEXT NOT NULL DEFAULT '',
    payload_hash        TEXT NOT NULL,
    created_ts          REAL NOT NULL,
    expires_ts          REAL NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    reason              TEXT NOT NULL DEFAULT '',
    applied_ts          REAL NOT NULL DEFAULT 0,
    result_revision     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_ai_runtime_command_status_id
    ON ai_runtime_command(status, id);
CREATE INDEX IF NOT EXISTS idx_ai_runtime_command_agent_id
    ON ai_runtime_command(agent_key, id);

CREATE TABLE IF NOT EXISTS ai_runtime_control (
    agent_key  TEXT PRIMARY KEY,
    generation INTEGER NOT NULL DEFAULT 1,
    revision   INTEGER NOT NULL DEFAULT 0,
    tombstoned INTEGER NOT NULL DEFAULT 0
);

-- 现有 Agent 首次接管：generation=1、revision=0；已逻辑删除的建墓碑。
-- INSERT OR IGNORE 保证重复迁移不重置代次。
INSERT OR IGNORE INTO ai_runtime_control(agent_key, generation, revision, tombstoned)
SELECT key, 1, 0, CASE WHEN deleted_ts>0 THEN 1 ELSE 0 END FROM ai_agent;
