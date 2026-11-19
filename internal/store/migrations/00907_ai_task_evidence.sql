-- T-108 / DEV-121 A：两张从属于既有 ai_task / ai_result 的凭据表。
-- 与父行同事务写入，随父任务/结果的既有保留期和清理事务回收；不改原队列表。
CREATE TABLE IF NOT EXISTS ai_task_evidence(
    task_id      INTEGER PRIMARY KEY,
    binding_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ai_result_expression(
    result_id               INTEGER PRIMARY KEY,
    agent_key               TEXT NOT NULL DEFAULT '',
    generation              INTEGER NOT NULL DEFAULT 0,
    br_personality_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_ai_result_expression_agent_version
    ON ai_result_expression(agent_key, generation, br_personality_revision);
