-- Apply through store.Migrate: ALTERs, cleanup and the ledger entry commit together.
ALTER TABLE ai_tool_job ADD COLUMN terminal_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_task ADD COLUMN intent_epoch INTEGER;
ALTER TABLE ai_runtime_command ADD COLUMN intent_epoch INTEGER;
ALTER TABLE ai_runtime_command ADD COLUMN parent_task_id INTEGER;

INSERT INTO ai_setting(key, value, updated_ts)
VALUES('emergency_epoch_record', '{"version":1,"epoch":1}', unixepoch());

INSERT INTO ai_result(task_id, status, reply_text, raw_usage, err, created_ts, delivered_ts)
SELECT id, 'fallback', '', '', 'origin_unknown', unixepoch(), -1
FROM ai_task WHERE state IN ('pending', 'processing');
UPDATE ai_task SET state='fallback', text='' WHERE state IN ('pending', 'processing');
UPDATE ai_result SET delivered_ts=-1, reply_text='', err='origin_unknown' WHERE delivered_ts IS NULL AND reply_text<>'';
UPDATE ai_tool_job SET state='done', args='', result_text='', done_ts=unixepoch(), terminal_reason='origin_unknown'
WHERE state IN ('pending', 'processing');
UPDATE ai_runtime_command SET status='rejected', reason='origin_unknown', applied_ts=unixepoch()
WHERE status='pending';

UPDATE kv SET v=json_set(v, '$.status', 'rejected', '$.reason', 'origin_unknown',
                         '$.message', '触发请求的急停代次不可证', '$.finished_ts', unixepoch())
WHERE k='t027:fishing-talk-test'
  AND CASE WHEN json_valid(v) THEN json_extract(v, '$.status')='pending' ELSE 0 END;
