-- 旧版本创建过 ai_session / ai_message，但会话记忆一直只存在内存，
-- 两张表从未写入。升级时清掉，保持表归属清单与实际库一致。
DROP TABLE IF EXISTS ai_message;
DROP TABLE IF EXISTS ai_session;
