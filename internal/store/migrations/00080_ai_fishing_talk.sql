-- AI 自己上鱼后主动说话（默认关）。
--
-- 虚拟上鱼原本只写 ai_catch 表、不发任何频道消息，玩家完全看不见。
-- 本迁移加一组开关与概率：抽中档位后按概率决定是否开口，开口时
-- 复用 AIEnqueueSystemChannel 往「抽中它的那个频道」发一条消息。
--
-- 配额语义同时改变：ai_fishing_hourly_limit 从「抽鱼次数」改为
-- 「说出口的话」，静默档与概率未命中都不占配额（代码侧实现）。

INSERT OR IGNORE INTO ai_setting(key, value, updated_ts) VALUES
    ('ai_fishing_talk_enabled', '0', 0),
    ('ai_fishing_talk_super_chance', '1', 0),
    ('ai_fishing_talk_star_chance', '0.9', 0),
    ('ai_fishing_talk_near_chance', '0.35', 0),
    ('ai_fishing_talk_miss_chance', '0.25', 0),
    ('ai_fishing_roundtable_chance', '0.5', 0);

-- 角色级开关：全局范围只决定「在哪能见到它」，这个开关决定「它能不能报鱼」。
-- 默认 0，想让某个角色报鱼要在 Agent 编辑里单独打开。
ALTER TABLE ai_agent ADD COLUMN fishing_talk_enabled INTEGER NOT NULL DEFAULT 0;
