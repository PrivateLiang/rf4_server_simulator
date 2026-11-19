package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"rf4server/internal/aiconfig"
)

func TestT102EmergencyEpochMigrationAndRollback(t *testing.T) {
	db := openCatchHistoryDBAtVersion(t, 905)
	ctx := t.Context()
	if _, err := db.EnsureProfile(ctx, 1); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.SQL.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE profile SET silver=76543 WHERE uid=1;
		INSERT INTO ai_task(id,uid,state,text,created_ts) VALUES
			(1,1,'pending','old queued',1),(2,1,'processing','old running',2),(3,1,'done','',3);
		INSERT INTO ai_result(task_id,status,reply_text,created_ts,delivered_ts,err)
			VALUES(3,'done','history',3,4,''),(3,'done','old undelivered',3,NULL,''),
			      (3,'fallback','',3,NULL,'historic terminal');
		INSERT INTO ai_tool_job(task_id,tool,args,uid,state,created_ts,result_text)
			VALUES(1,'fish_query','old',1,'pending',1,''),(2,'fish_query','old',1,'processing',2,''),
			      (3,'fish_query','old',1,'done',3,'history');
		INSERT INTO ai_runtime_command(request_id,source,kind,agent_key,payload_hash,created_ts,expires_ts,status,reason)
			VALUES('old-pending','admin','actor_set_enabled','companion','h1',1,100,'pending',''),
			      ('old-applied','admin','actor_set_enabled','companion','h2',1,100,'applied','history');
		INSERT INTO kv(k,v) VALUES('t027:fishing-talk-test','{"status":"pending","request_id":"old"}');
		CREATE TRIGGER reject_epoch_registration BEFORE INSERT ON schema_migrations
		WHEN NEW.version=906 BEGIN SELECT RAISE(ABORT,'test migration failure'); END;`)
	if err := db.Migrate(); err == nil {
		t.Fatal("expected atomic migration failure")
	}
	assertCount(t, db, `SELECT count(*) FROM pragma_table_info('ai_task') WHERE name='intent_epoch'`, 0)
	assertCount(t, db, `SELECT count(*) FROM ai_task WHERE state IN ('pending','processing') AND text<>''`, 2)
	assertCount(t, db, `SELECT count(*) FROM ai_setting WHERE key='emergency_epoch_record'`, 0)
	exec(`DROP TRIGGER reject_epoch_registration`)
	for range 2 {
		if err := db.Migrate(); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, db, `SELECT count(*) FROM schema_migrations WHERE version=906`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_task WHERE state='fallback' AND text='' AND intent_epoch IS NULL`, 2)
	assertCount(t, db, `SELECT count(*) FROM ai_result WHERE delivered_ts=-1 AND reply_text='' AND err='origin_unknown'`, 3)
	assertCount(t, db, `SELECT count(*) FROM ai_tool_job WHERE state='done' AND terminal_reason='origin_unknown' AND result_text=''`, 2)
	assertCount(t, db, `SELECT count(*) FROM ai_runtime_command WHERE status='rejected' AND reason='origin_unknown'`, 1)
	assertCount(t, db, `SELECT count(*) FROM kv WHERE k='t027:fishing-talk-test' AND json_extract(v,'$.reason')='origin_unknown'`, 1)
	if got := db.AIEmergencyState(ctx); got.Version != 1 || got.Epoch != 1 || got.Err != nil {
		t.Fatalf("initial epoch: %+v", got)
	}

	activeEpoch := db.AIEmergencyState(ctx).Epoch
	if err := db.AISetAdminSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}, &activeEpoch, "tester", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.AISetAdminSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}, &activeEpoch, "tester", nil); !errors.Is(err, ErrAIEmergencyConflict) {
		t.Fatalf("stale save: %v", err)
	}
	exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES('ai_config_active','synthetic-publication',1);
		UPDATE profile SET silver=87654 WHERE uid=1;`)

	// Group-off rollback keeps additive schema and history. A legacy producer
	// writes work with NULL origin; the next isolated upgrade must clear it.
	exec(`BEGIN IMMEDIATE;
		UPDATE ai_task SET state='fallback',text='' WHERE state IN ('pending','processing');
		UPDATE ai_result SET delivered_ts=-1,reply_text='' WHERE delivered_ts IS NULL AND reply_text<>'';
		UPDATE ai_tool_job SET state='done',args='',result_text='' WHERE state IN ('pending','processing');
		UPDATE ai_runtime_command SET status='rejected',reason='emergency_stop' WHERE status='pending';
		COMMIT;`)
	assertCount(t, db, `SELECT count(*) FROM profile WHERE uid=1 AND silver=87654`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_result WHERE delivered_ts=4 AND reply_text='history'`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_tool_job WHERE result_text='history'`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_result WHERE delivered_ts IS NULL AND err='historic terminal'`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_runtime_command WHERE status='applied' AND reason='history'`, 1)
	assertCount(t, db, `SELECT count(*) FROM audit_log WHERE action='ai_emergency_stop'`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_setting WHERE key='ai_config_active' AND value='synthetic-publication'`, 1)
	assertCount(t, db, `SELECT count(*) FROM ai_task WHERE state IN ('pending','processing')`, 0)
	assertCount(t, db, `SELECT count(*) FROM ai_result WHERE delivered_ts IS NULL AND reply_text<>''`, 0)
	assertCount(t, db, `SELECT count(*) FROM ai_runtime_command WHERE status='pending'`, 0)
	exec(`INSERT INTO ai_task(uid,state,text,created_ts) VALUES(1,'pending','legacy replay',10)`)
	if _, _, err := db.AIExpireTasks(ctx, 11, 0); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT count(*) FROM ai_task WHERE state='fallback' AND intent_epoch IS NULL`, 3)
	assertCount(t, db, `SELECT count(*) FROM ai_tool_job WHERE terminal_reason='origin_unknown'`, 2)
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT count(*) FROM ai_result WHERE delivered_ts=-1 AND err='origin_unknown'`, 3)
	t.Log("00905→00906 rollback-on-failure, replay and isolated legacy-writer rollback/re-upgrade retained player data and terminal rows")
}

func TestT102EmergencyReadAndWriteContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epoch-lock.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	bound := AIIntentContext(ctx, db.AIEmergencyState(ctx).Epoch)
	producer := &AITask{Kind: "dm", SourceKind: "human", Text: "synthetic"}
	exec := func(q string) {
		t.Helper()
		if _, err := db.SQL.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE ai_setting RENAME TO transient_setting_failure`)
	if _, _, err := db.AIExpireTasks(ctx, 10, 0); err == nil {
		t.Fatal("read fault should abort sweep")
	}
	if d := aiconfig.EvaluateEmergency(db.AIEmergencyState(ctx)); d.Fault != aiconfig.FaultReadError {
		t.Fatalf("SQL read fault: %+v", d)
	}
	exec(`ALTER TABLE transient_setting_failure RENAME TO ai_setting`)
	exec(`PRAGMA busy_timeout=0`)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AIEnqueue(bound, producer); err == nil {
		t.Fatal("writer contention should reject the attempted transaction")
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task`, 0)
	for _, stop := range []string{"1", "0"} {
		if err := db.AISetSetting(ctx, aiconfig.KeyEmergencyStop, stop); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AIEnqueue(bound, producer); !errors.Is(err, ErrAIIntentInactive) {
		t.Fatalf("stale retry: %v", err)
	}
	if _, err := db.AIEnqueue(ctx, producer); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task WHERE intent_epoch=3`, 1)
	t.Log("temporary read error and competing write lock left zero rows; stale retry rejected; fresh epoch 3 entered the queue")
}
