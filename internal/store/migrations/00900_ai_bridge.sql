-- 本地 AI 助手：队列、结果、Agent/联系人、厂商与密钥、频道订阅、设置、
-- 只读工具作业、战绩记忆、角色状态与 AI 渔获。
-- 全部落在可写玩家库；静态库保持只读。重复执行只建缺失对象。

CREATE TABLE IF NOT EXISTS ai_task (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL DEFAULT 'dm',
    scope        TEXT NOT NULL DEFAULT 'uid',
    scope_key    TEXT NOT NULL DEFAULT '',
    uid          INTEGER NOT NULL,
    agent        TEXT NOT NULL DEFAULT '',
    contact_uid  INTEGER NOT NULL DEFAULT 0,
    source_kind  TEXT NOT NULL DEFAULT 'human',
    text         TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending',
    deadline_ts  REAL NOT NULL DEFAULT 0,
    attempt      INTEGER NOT NULL DEFAULT 0,
    created_ts   REAL NOT NULL,
    participants TEXT NOT NULL DEFAULT '[]',
    loc          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ai_task_state ON ai_task(state, id);

CREATE TABLE IF NOT EXISTS ai_result (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       INTEGER NOT NULL,
    status        TEXT NOT NULL,
    reply_text    TEXT NOT NULL DEFAULT '',
    raw_usage     TEXT NOT NULL DEFAULT '',
    err           TEXT NOT NULL DEFAULT '',
    delivered_ts  REAL,
    created_ts    REAL NOT NULL,
    contact_uid   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_result_pending ON ai_result(delivered_ts, id);

CREATE TABLE IF NOT EXISTS ai_agent (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    key               TEXT NOT NULL UNIQUE,
    display           TEXT NOT NULL,
    role              TEXT NOT NULL DEFAULT '',
    contact_uid       INTEGER NOT NULL UNIQUE,
    prompt            TEXT NOT NULL DEFAULT '',
    tools_json        TEXT NOT NULL DEFAULT '[]',
    thinking_enabled  INTEGER NOT NULL DEFAULT 0,
    thinking_effort   TEXT NOT NULL DEFAULT 'off',
    enabled           INTEGER NOT NULL DEFAULT 1,
    joined            INTEGER NOT NULL DEFAULT 0,
    actor_enabled     INTEGER NOT NULL DEFAULT 0,
    reply_mode        TEXT NOT NULL DEFAULT 'social',
    talkativeness     INTEGER NOT NULL DEFAULT 50,
    deleted_ts        REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_agent_deleted_ts ON ai_agent(deleted_ts);

CREATE TABLE IF NOT EXISTS ai_provider (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    protocol     TEXT NOT NULL DEFAULT 'openai_chat',
    base_url     TEXT NOT NULL,
    auth_scheme  TEXT NOT NULL DEFAULT 'bearer',
    config_json  TEXT NOT NULL DEFAULT '{}',
    enabled      INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS ai_model (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id        INTEGER NOT NULL,
    model_id           TEXT NOT NULL,
    display            TEXT NOT NULL DEFAULT '',
    context_limit      INTEGER NOT NULL DEFAULT 0,
    supports_tools     INTEGER NOT NULL DEFAULT 0,
    supports_thinking  INTEGER NOT NULL DEFAULT 0,
    enabled            INTEGER NOT NULL DEFAULT 1,
    UNIQUE(provider_id, model_id)
);

CREATE TABLE IF NOT EXISTS ai_secret (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id  INTEGER NOT NULL,
    ciphertext   BLOB NOT NULL,
    nonce        BLOB NOT NULL,
    key_id       TEXT NOT NULL DEFAULT 'default',
    masked       TEXT NOT NULL DEFAULT '',
    updated_ts   REAL NOT NULL,
    UNIQUE(provider_id)
);

CREATE TABLE IF NOT EXISTS ai_channel (
    name  TEXT PRIMARY KEY,
    label TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS ai_channel_agent (
    channel     TEXT NOT NULL,
    agent_id    INTEGER NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    priority    INTEGER NOT NULL DEFAULT 100,
    cooldown_ms INTEGER NOT NULL DEFAULT 8000,
    added_by    TEXT NOT NULL DEFAULT 'admin',
    added_ts    REAL NOT NULL,
    PRIMARY KEY(channel, agent_id)
);

CREATE TABLE IF NOT EXISTS ai_setting (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_ts  REAL NOT NULL
);

-- 只读工具跨进程队列：aibridge 入队，gameserver 用产线逻辑执行后写回。
CREATE TABLE IF NOT EXISTS ai_tool_job (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     INTEGER NOT NULL,
    tool        TEXT NOT NULL,
    args        TEXT NOT NULL,
    uid         INTEGER NOT NULL,
    state       TEXT NOT NULL DEFAULT 'pending',
    result_text TEXT NOT NULL DEFAULT '',
    created_ts  REAL NOT NULL,
    done_ts     REAL
);
CREATE INDEX IF NOT EXISTS idx_ai_tool_job_state ON ai_tool_job(state, id);

-- 结构化战绩记忆：星/蓝单独特记，不占对话历史。
CREATE TABLE IF NOT EXISTS ai_memory (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_key   TEXT NOT NULL,
    kind        TEXT NOT NULL,
    fish_key    TEXT NOT NULL DEFAULT '',
    fish_name   TEXT NOT NULL DEFAULT '',
    weight_g    INTEGER NOT NULL DEFAULT 0,
    loc         TEXT NOT NULL DEFAULT '',
    created_ts  REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_memory_recent ON ai_memory(agent_key, id DESC);
CREATE INDEX IF NOT EXISTS idx_ai_memory_best ON ai_memory(agent_key, kind, weight_g DESC);

-- AI 角色状态：与对话历史解耦，保证「在哪、在干什么」不会聊两句就飘。
CREATE TABLE IF NOT EXISTS ai_actor_state (
    agent_key         TEXT PRIMARY KEY,
    loc               TEXT NOT NULL DEFAULT '',
    loc_name          TEXT NOT NULL DEFAULT '',
    activity          TEXT NOT NULL DEFAULT '',
    target_fish       TEXT NOT NULL DEFAULT '',
    target_name       TEXT NOT NULL DEFAULT '',
    since_ts          REAL NOT NULL DEFAULT 0,
    loc_since_ts      REAL NOT NULL DEFAULT 0,
    switch_ts         REAL NOT NULL DEFAULT 0,
    activity_until_ts REAL NOT NULL DEFAULT 0,
    energy            REAL NOT NULL DEFAULT 70,
    updated_ts        REAL NOT NULL DEFAULT 0
);

-- AI 自己钓到的鱼：只记录，供玩家问起时查询，不发频道播报。
CREATE TABLE IF NOT EXISTS ai_catch (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_key   TEXT NOT NULL,
    fish_key    TEXT NOT NULL DEFAULT '',
    fish_name   TEXT NOT NULL DEFAULT '',
    weight_g    INTEGER NOT NULL DEFAULT 0,
    loc         TEXT NOT NULL DEFAULT '',
    loc_name    TEXT NOT NULL DEFAULT '',
    event       TEXT NOT NULL DEFAULT '',
    bait        TEXT NOT NULL DEFAULT '',
    created_ts  REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_catch_agent_ts ON ai_catch(agent_key, created_ts);

-- 默认 Agent 只存在于后台，joined=0 时不影响社交列表与 19/1 分流。
INSERT OR IGNORE INTO ai_agent
    (key, display, role, contact_uid, prompt, tools_json, thinking_enabled, thinking_effort, enabled, joined, reply_mode, talkativeness)
VALUES
    ('companion', 'AI 伙伴', '陪聊', 2147483645,
     '你是俄罗斯钓鱼4单机版里的轻松聊天伙伴，简短、口语，不剧透官服机制。', '["map_vibe"]', 0, 'off', 1, 0, 'social', 50),
    ('fishguide', '鱼情助手', '鱼情顾问', 2147483644,
     '你是本地鱼情助手。只依据工具返回的内部资料回答，缺数据时明确说不知道，不要编坐标或概率。',
     '["fish_query","species_lookup","bait_pref"]', 0, 'medium', 1, 0, 'social', 50),
    ('gearsmith', '装备助手', '装备顾问', 2147483643,
     '你是装备与钓组助手，只依据内部资料回答。', '["item_lookup"]', 0, 'low', 1, 0, 'social', 50),
    ('shopkeep', '商店助手', '商店顾问', 2147483642,
     '你是价格与配方助手，只依据内部资料回答。', '["shop_price","recipe_lookup"]', 0, 'low', 1, 0, 'social', 50),
    ('host', '频道主持', '频道主持', 2147483641,
     '你是多人频道里的主持人，简短接话，不主动挑起新话题。', '[]', 0, 'low', 1, 0, 'social', 50);

-- 运行期开关。默认：模型调用出网开、联网搜索关、紧急停止关。
INSERT OR IGNORE INTO ai_setting(key, value, updated_ts) VALUES
    ('outbound_enabled', '1', 0),
    ('web_search_enabled', '0', 0),
    ('emergency_stop', '0', 0),
    ('search_endpoint', '', 0),
    ('max_tool_rounds', '2', 0),
    ('roundtable_enabled', '0', 0),
    ('roundtable_max_speakers', '3', 0),
    ('roundtable_max_turns', '6', 0),
    ('roundtable_max_ms', '20000', 0),
    ('roundtable_cooldown_ms', '120000', 0),
    ('roundtable_hourly_limit', '3', 0),
    ('roundtable_on_speech_chance', '0.2', 0),
    ('roundtable_chime_chance', '0.55', 0),
    ('roundtable_gap_ms', '3000', 0),
    ('roundtable_max_streak', '3', 0),
    ('ai_actor_enabled', '1', 0),
    ('ai_actor_switch_chance', '0.25', 0),
    ('ai_actor_switch_cooldown_s', '1800', 0),
    ('ai_actor_fishing_session_min_s', '1200', 0),
    ('ai_actor_fishing_session_max_s', '4800', 0),
    ('ai_actor_rest_min_s', '600', 0),
    ('ai_actor_rest_max_s', '2400', 0),
    ('ai_actor_rest_chance', '0.35', 0),
    ('ai_catch_talk', '1', 0),
    ('ai_catch_star_chance', '0.6', 0),
    ('ai_catch_super_chance', '0.9', 0),
    ('ai_catch_roundtable_chance', '0.5', 0),
    ('ai_fishing_enabled', '0', 0),
    ('ai_fishing_hourly_limit', '12', 0),
    ('ai_fishing_base_interval_ms', '300000', 0),
    ('ai_fishing_max_interval_ms', '600000', 0),
    ('ai_fishing_star_weight', '0.02', 0),
    ('ai_fishing_super_weight', '0.002', 0),
    ('ai_fishing_near_weight', '0.12', 0),
    ('ai_fishing_miss_weight', '0.33', 0),
    ('ai_fishing_miss_ratio', '0.35', 0),
    ('ai_fishing_near_ratio', '0.9', 0),
    ('ai_memory_inject', '1', 0),
    ('ai_memory_recent', '3', 0),
    ('ai_memory_best', '2', 0),
    ('ai_memory_max_rows', '50', 0),
    ('ai_profile_internal_chars', '800', 0),
    ('ai_profile_dm_context_tokens', '12000', 0),
    ('ai_profile_dm_history_messages', '200', 0),
    ('ai_profile_dm_history_chars', '8000', 0),
    ('ai_profile_dm_reply_chars', '120', 0),
    ('ai_profile_dm_reply_soft_chars', '80', 0),
    ('ai_profile_channel_context_tokens', '10000', 0),
    ('ai_profile_channel_history_messages', '150', 0),
    ('ai_profile_channel_history_chars', '6000', 0),
    ('ai_profile_channel_reply_chars', '45', 0),
    ('ai_profile_channel_reply_soft_chars', '25', 0),
    ('ai_profile_roundtable_context_tokens', '4000', 0),
    ('ai_profile_roundtable_context_messages', '2', 0),
    ('ai_profile_roundtable_reply_chars', '30', 0),
    ('ai_profile_roundtable_reply_soft_chars', '20', 0);

-- 默认厂商：OpenAI 兼容协议；模型 ID 留空，由后台填写。思考档位按厂商配置映射。
INSERT OR IGNORE INTO ai_provider(name, protocol, base_url, auth_scheme, config_json, enabled)
VALUES ('deepseek', 'openai_chat', 'https://api.deepseek.com', 'bearer',
        '{"thinking_map":{"off":{"thinking":{"type":"disabled"}},"low":{"thinking":{"type":"enabled"},"reasoning_effort":"low"},"medium":{"thinking":{"type":"enabled"},"reasoning_effort":"high"},"high":{"thinking":{"type":"enabled"},"reasoning_effort":"max"},"max":{"thinking":{"type":"enabled"},"reasoning_effort":"max"}}}', 1);
INSERT OR IGNORE INTO ai_model(provider_id, model_id, display, context_limit, supports_tools, supports_thinking, enabled)
SELECT id, '', 'DeepSeek（待后台填模型 ID）', 0, 1, 1, 1 FROM ai_provider WHERE name='deepseek';
