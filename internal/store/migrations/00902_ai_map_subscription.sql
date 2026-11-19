-- 地图级 AI 订阅：官服一张地图有十几个到上百个区域频道（nav<地图序号>_<图内序号>），
-- 按精确频道订阅必然漏（玩家在图内切区域就失聪）。新增按地图 key 订阅的表，
-- 查询时与精确订阅、全局开关三者取并集。旧的 ai_channel_agent 语义不变，
-- 保留为「精确频道的额外订阅」，历史数据无需迁移。

CREATE TABLE IF NOT EXISTS ai_map_agent (
    map_key     TEXT NOT NULL,          -- 地图 key，形如 level_001_lake
    agent_id    INTEGER NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    priority    INTEGER NOT NULL DEFAULT 100,
    cooldown_ms INTEGER NOT NULL DEFAULT 8000,
    added_by    TEXT NOT NULL DEFAULT 'admin',
    added_ts    REAL NOT NULL,
    PRIMARY KEY(map_key, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_map_agent_map ON ai_map_agent(map_key, enabled);

-- 全局开关：默认关。全局开是例外模式，常态是「哪些 AI 在哪个地图说话」。
-- include_silent 默认不含 reply_mode='silent'（只看不说）的角色。
INSERT OR IGNORE INTO ai_setting(key, value, updated_ts) VALUES
    ('ai_global_subscribe', '0', 0),
    ('ai_global_subscribe_include_silent', '0', 0);
