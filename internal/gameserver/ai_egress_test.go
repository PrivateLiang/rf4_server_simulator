package gameserver

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// TestAIEgressOnlyEmergency gameserver 三条 AI 链路共用同一个闸门：
// V2 只留急停；旧 outbound 值（1/非法/0/缺键）都不再阻止。
func TestAIEgressOnlyEmergency(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "ai-egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	set := func(key, value string) {
		t.Helper()
		if _, err := d.SQL.ExecContext(ctx, `UPDATE ai_setting SET value=? WHERE key=?`, value, key); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name        string
		prepare     func()
		wantAllowed bool
		wantState   string
	}{
		{"历史值1放行", func() { set(aiconfig.KeyOutboundEnabled, "1") }, true, aiconfig.StateAllowed},
		{"非法值放行", func() { set(aiconfig.KeyOutboundEnabled, "true") }, true, aiconfig.StateAllowed},
		{"显式关放行", func() { set(aiconfig.KeyOutboundEnabled, "0") }, true, aiconfig.StateAllowed},
		{"急停拦住", func() { set(aiconfig.KeyEmergencyStop, "1") }, false, aiconfig.StateEmergency},
		{"急停非法值保守停止", func() { set(aiconfig.KeyEmergencyStop, "yes") }, false, aiconfig.StateEmergency},
		{"急停解除放行", func() { set(aiconfig.KeyEmergencyStop, "0") }, true, aiconfig.StateAllowed},
	}
	for _, c := range cases {
		c.prepare()
		got := aiEgress(ctx, d)
		if got.Allowed != c.wantAllowed {
			t.Errorf("%s：Allowed = %v，期望 %v（状态 %s）", c.name, got.Allowed, c.wantAllowed, got.State)
		}
		if got.State != c.wantState {
			t.Errorf("%s：状态 = %q，期望 %q", c.name, got.State, c.wantState)
		}
	}

	set(aiconfig.KeyEmergencyStop, "0")
	if _, err := d.SQL.ExecContext(ctx, `DELETE FROM ai_setting WHERE key=?`, aiconfig.KeyOutboundEnabled); err != nil {
		t.Fatal(err)
	}
	if got := aiEgress(ctx, d); !got.Allowed {
		t.Errorf("缺 outbound 键也必须放行，得到 (%v,%q)", got.Allowed, got.State)
	}
}

// TestWriteAICatchStoppedInsideTx 停前快照不能绕过写事务内复核：
// 急停开启时 writeAICatch 不写渔获/状态/记忆，返回 false。
func TestWriteAICatchStoppedInsideTx(t *testing.T) {
	ctx := context.Background()
	d, err := store.Open(filepath.Join(t.TempDir(), "ai-catch-stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	agent, err := d.AIAgentByKey(ctx, "fishguide")
	if err != nil || agent == nil {
		t.Fatalf("缺少种子 Agent: %v", err)
	}
	s := &Server{DB: d, log: logging.Discard()}
	fish := aiFishTemplate{FishKey: "test_fish", Name: "测试鱼", WeightG: 1000, Event: evOk, Loc: "test_loc"}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if ok := s.writeAICatch(ctx, d, *agent, fish, "test_loc"); ok {
		t.Fatal("急停时写渔获应被拒绝")
	}
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_catch`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("急停时仍写入 ai_catch %d 行", n)
	}
	// 解除后同一调用可以写入（写事务内读到的急停已解除）。
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if ok := s.writeAICatch(ctx, d, *agent, fish, "test_loc"); !ok {
		t.Fatal("解除急停后应能写入渔获")
	}
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_catch`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("解除急停后 ai_catch = %d 行，期望 1", n)
	}
}

// seedPendingChannelResult 造一条已生成、待投递的频道结果。
// 走真实表形状（ai_task + ai_result），由 pollAIResults 按创建时点判定是否失效。
func seedPendingChannelResult(t *testing.T, db *store.DB, channel, text string, createdTS float64, intentEpoch ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	epoch := db.AIEmergencyState(ctx).Epoch
	if len(intentEpoch) > 0 {
		epoch = intentEpoch[0]
	}
	res, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid, source_kind,
		                    text, state, deadline_ts, attempt, created_ts, participants, loc, intent_epoch)
		VALUES('channel','channel',?,?, 'companion',?, 'human', '', 'done', 0, 0, ?, '[]', '', ?)`,
		channel, 1, 2147483645, createdTS, epoch)
	if err != nil {
		t.Fatalf("造任务: %v", err)
	}
	taskID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	// T-108：频道结果在投递前要能回到父任务证据。这里的夹具是一条「基础对话」
	// 形状的频道回复（不经过事件入口），因此按直接对话绑定，只约束发言者代次。
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_task_evidence(task_id, binding_json) VALUES(?,?)`, taskID,
		`{"format_version":1,"direct_dialog":true,"participants":[{"key":"companion","generation":1,"cap_revision":0}]}`); err != nil {
		t.Fatalf("造任务证据: %v", err)
	}
	res, err = db.SQL.ExecContext(ctx, `
		INSERT INTO ai_result(task_id, status, reply_text, contact_uid, created_ts)
		VALUES(?, 'done', ?, ?, ?)`, taskID, text, 2147483645, createdTS)
	if err != nil {
		t.Fatalf("造结果: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func resultDeliveryState(t *testing.T, db *store.DB, id int64) (delivered sql.NullFloat64, reply string) {
	t.Helper()
	if err := db.SQL.QueryRow(`SELECT delivered_ts, reply_text FROM ai_result WHERE id=?`, id).
		Scan(&delivered, &reply); err != nil {
		t.Fatalf("读结果 %d: %v", id, err)
	}
	return delivered, reply
}

func liftNow(t *testing.T, db *store.DB) float64 {
	t.Helper()
	ctx := context.Background()
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	raw := db.AISetting(ctx, aiconfig.KeyEmergencyStopLiftedTS, "")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		t.Fatalf("解除急停时点 = %q, %v", raw, err)
	}
	return v
}

// TestPollResultsDropsStaleAfterResume 覆盖「停前已排队结果、急停再解除、再生成新结果」：
// 同一批只投递新结果；旧结果落既有终态并清正文；旧条目不占新条目的限额。
func TestPollResultsDropsStaleAfterResume(t *testing.T) {
	ctx := context.Background()
	channel := "t102_stale_results"

	t.Run("急停保持期间零投递", func(t *testing.T) {
		db := openMigratedTestDB(t)
		p := newT013ChatProbeWithDB(t, channel, db)
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			t.Fatal(err)
		}
		id := seedPendingChannelResult(t, db, channel, "stop-era-reply", float64(time.Now().Unix())-10)
		p.srv.pollAIResults(ctx, db)
		delivered, reply := resultDeliveryState(t, db, id)
		if !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("急停期间结果未被丢弃: delivered=%+v reply=%q", delivered, reply)
		}
		p.expectNoMore(t)
	})

	t.Run("恢复后同批只发新结果", func(t *testing.T) {
		db := openMigratedTestDB(t)
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		oldID := seedPendingChannelResult(t, db, channel, "stop-era-reply", lifted-10, db.AIEmergencyState(ctx).Epoch-2)
		freshID := seedPendingChannelResult(t, db, channel, "fresh-reply", lifted+5)
		p.srv.pollAIResults(ctx, db)
		if delivered, reply := resultDeliveryState(t, db, oldID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("旧结果未被丢弃: delivered=%+v reply=%q", delivered, reply)
		}
		if delivered, reply := resultDeliveryState(t, db, freshID); !delivered.Valid || delivered.Float64 <= 0 || reply != "" {
			t.Fatalf("新结果未被正常投递/清正文: delivered=%+v reply=%q", delivered, reply)
		}
		r := p.next(t)
		if r.text != "fresh-reply" {
			t.Fatalf("应只收到新回复，实得 %q（main/sub %d/%d）", r.text, r.main, r.sub)
		}
		p.expectNoMore(t)
	})

	t.Run("旧条目不占新条目限额", func(t *testing.T) {
		db := openMigratedTestDB(t)
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		var staleIDs []int64
		for i := 0; i < aiMaxDeliveryBatch; i++ {
			staleIDs = append(staleIDs, seedPendingChannelResult(t, db, channel, "stale", lifted-10, db.AIEmergencyState(ctx).Epoch-2))
		}
		freshID := seedPendingChannelResult(t, db, channel, "fresh-beyond-batch", lifted+5)
		p.srv.pollAIResults(ctx, db)
		for _, id := range staleIDs {
			if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
				t.Fatalf("旧结果 %d 未被丢弃: %+v %q", id, delivered, reply)
			}
		}
		if delivered, _ := resultDeliveryState(t, db, freshID); !delivered.Valid || delivered.Float64 <= 0 {
			t.Fatalf("批次外的新结果仍待投递（旧条占用了限额）: %+v", delivered)
		}
		r := p.next(t)
		if r.text != "fresh-beyond-batch" {
			t.Fatalf("应投递批次外的新结果，实得 %q", r.text)
		}
		p.expectNoMore(t)
	})
}

// seedPendingDMResult 造一条已生成、待投递的私聊结果（真实 ai_task + ai_result 形状）。
func seedPendingDMResult(t *testing.T, db *store.DB, uid, contactUID int64, text string, createdTS float64, intentEpoch ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	epoch := db.AIEmergencyState(ctx).Epoch
	if len(intentEpoch) > 0 {
		epoch = intentEpoch[0]
	}
	res, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid, source_kind,
		                    text, state, deadline_ts, attempt, created_ts, participants, loc, intent_epoch)
		VALUES('dm','uid',?,?, 'companion',?, 'human', '', 'done', 0, 0, ?, '[]', '', ?)`,
		strconv.FormatInt(uid, 10), uid, contactUID, createdTS, epoch)
	if err != nil {
		t.Fatalf("造私聊任务: %v", err)
	}
	taskID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = db.SQL.ExecContext(ctx, `
		INSERT INTO ai_result(task_id, status, reply_text, contact_uid, created_ts)
		VALUES(?, 'done', ?, ?, ?)`, taskID, text, contactUID, createdTS)
	if err != nil {
		t.Fatalf("造私聊结果: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// installStopOnDropTrigger 受控屏障：GS 丢弃任一待投递结果时，同一 SQLite
// 连接把急停打开，精确落在「批次筛选」与「投递」之间。
func installStopOnDropTrigger(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.SQL.Exec(`
		CREATE TRIGGER IF NOT EXISTS t102_stop_on_drop
		AFTER UPDATE ON ai_result
		WHEN OLD.delivered_ts IS NULL AND NEW.delivered_ts = -1
		BEGIN
			UPDATE ai_setting SET value='1' WHERE key='emergency_stop';
		END`); err != nil {
		t.Fatalf("安装急停屏障: %v", err)
	}
}

func mailCount(t *testing.T, db *store.DB, uid int64) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM mail WHERE uid=?`, uid).Scan(&n); err != nil {
		t.Fatalf("统计私聊邮件: %v", err)
	}
	return n
}

// TestPollResultsStopsWhenStopTurnedOnMidPoll 票 §2 第 1 项：
// pollAIResults 开始时已读到解除状态，处理同批旧条目期间急停生效，
// 进入发送前必须重新观察到急停：停后无公聊推送、无私聊写入；
// 停前正常投递、再次解除后与新意图恢复。
func TestPollResultsStopsWhenStopTurnedOnMidPoll(t *testing.T) {
	ctx := context.Background()

	t.Run("停后不推公聊", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_midstop_channel"
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		oldID := seedPendingChannelResult(t, db, channel, "stale-channel", lifted-10, db.AIEmergencyState(ctx).Epoch-2)
		freshID := seedPendingChannelResult(t, db, channel, "fresh-channel", lifted+5)
		installStopOnDropTrigger(t, db)

		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if delivered, reply := resultDeliveryState(t, db, oldID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("旧结果未被终止: %+v %q", delivered, reply)
		}
		if delivered, reply := resultDeliveryState(t, db, freshID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("急停生效后新结果不得投递，应落终态并清正文: %+v %q", delivered, reply)
		}
		if raw := db.AISetting(ctx, aiconfig.KeyEmergencyStop, ""); raw != "1" {
			t.Fatalf("屏障未把急停打开: %q", raw)
		}
	})

	t.Run("停后不写私聊", func(t *testing.T) {
		db := openMigratedTestDB(t)
		lifted := liftNow(t, db)
		uid, contact := int64(9100001), int64(2147483645)
		oldID := seedPendingDMResult(t, db, uid, contact, "stale-dm", lifted-10, db.AIEmergencyState(ctx).Epoch-2)
		freshID := seedPendingDMResult(t, db, uid, contact, "fresh-dm", lifted+5)
		installStopOnDropTrigger(t, db)

		srv := &Server{DB: db, log: logging.Discard()}
		srv.pollAIResults(ctx, db)
		if n := mailCount(t, db, uid); n != 0 {
			t.Fatalf("急停生效后不得写入私聊，mail 行数 = %d", n)
		}
		for _, id := range []int64{oldID, freshID} {
			if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
				t.Fatalf("结果 %d 未落终态: %+v %q", id, delivered, reply)
			}
		}
	})

	t.Run("停前正常投递且解除后新结果可投递", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_midstop_resume"
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		firstID := seedPendingChannelResult(t, db, channel, "before-stop", lifted+5)
		p.srv.pollAIResults(ctx, db)
		if delivered, _ := resultDeliveryState(t, db, firstID); !delivered.Valid || delivered.Float64 <= 0 {
			t.Fatalf("停前正常结果应投递: %+v", delivered)
		}
		if r := p.next(t); r.text != "before-stop" {
			t.Fatalf("停前推送内容 = %q", r.text)
		}
		p.expectNoMore(t)

		// 停中：新结果不投递。
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			t.Fatal(err)
		}
		stoppedID := seedPendingChannelResult(t, db, channel, "during-stop", float64(time.Now().Unix()))
		p.srv.pollAIResults(ctx, db)
		if delivered, reply := resultDeliveryState(t, db, stoppedID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("停中结果不得投递: %+v %q", delivered, reply)
		}
		p.expectNoMore(t)

		// 再次解除：新结果可投递。
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
			t.Fatal(err)
		}
		afterID := seedPendingChannelResult(t, db, channel, "after-resume", float64(time.Now().Unix())+1)
		p.srv.pollAIResults(ctx, db)
		if delivered, _ := resultDeliveryState(t, db, afterID); !delivered.Valid || delivered.Float64 <= 0 {
			t.Fatalf("再次解除后新结果应投递: %+v", delivered)
		}
		if r := p.next(t); r.text != "after-resume" {
			t.Fatalf("解除后推送内容 = %q", r.text)
		}
		p.expectNoMore(t)
	})

	t.Run("解除后新意图重新入队而停中不入队", func(t *testing.T) {
		db := openMigratedTestDB(t)
		p := newT013ChatProbeWithDB(t, "t102_midstop_intent", db)
		ctxSess := &Ctx{Sess: p.sess, ExpectResponse: true}
		countTasks := func() int {
			var n int
			if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_task WHERE kind='dm'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			t.Fatal(err)
		}
		if _, ok := p.srv.receiveAIChat(ctxSess, 2147483645, "companion", "停中意图"); !ok {
			t.Fatal("停中私聊入口应 ACK")
		}
		if n := countTasks(); n != 0 {
			t.Fatalf("停中不得入队新任务，ai_task=%d", n)
		}
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
			t.Fatal(err)
		}
		if _, ok := p.srv.receiveAIChat(ctxSess, 2147483645, "companion", "解除后新意图"); !ok {
			t.Fatal("解除后私聊入口应 ACK")
		}
		if n := countTasks(); n != 1 {
			t.Fatalf("解除后新意图应入队一次，ai_task=%d", n)
		}
	})
}

// TestSupervisorT102StopAndResumeAtDeliveryBoundary 复评矩阵「同一 1→0 落在
// 最终频道屏障」：旧公聊结果已进待发批次，在现有频道发送屏障里通过真实设置
// 路径连续保存 1、0，同轮释放。停前意图早于最近解除点，必须零 24/27。
func TestSupervisorT102StopAndResumeAtDeliveryBoundary(t *testing.T) {
	ctx := context.Background()
	db := openMigratedTestDB(t)
	channel := "t102_supervisor_boundary"
	p := newT013ChatProbeWithDB(t, channel, db)
	oldID := seedPendingChannelResult(t, db, channel, "reply-from-before-stop", float64(time.Now().Unix())-10)

	aiDeliveryBarrierForTest = func() {
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			t.Errorf("屏障置急停: %v", err)
		}
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
			t.Errorf("屏障解除急停: %v", err)
		}
	}
	defer func() { aiDeliveryBarrierForTest = nil }()

	p.srv.pollAIResults(ctx, db)
	p.expectNoMore(t)
	if delivered, reply := resultDeliveryState(t, db, oldID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
		t.Fatalf("停前旧公聊在 1→0 后仍投递: delivered=%+v reply=%q", delivered, reply)
	}
	// 解除提交后普通玩家公聊仍正常到达（AI 停过一轮不影响官方广播）。
	p.send(t, "普通玩家发言")
	t013AssertBroadcast(t, p.next(t), "普通玩家发言")
}

// TestSupervisorT102DMStopAndResumeAtTransaction 复评矩阵「同一 1→0 落在
// 私聊事务边界」：旧私聊结果已读取，在真正开写事务前连续保存 1、0。
// 旧意图必须零 mail、无在线通知，并落既有终态。
func TestSupervisorT102DMStopAndResumeAtTransaction(t *testing.T) {
	ctx := context.Background()
	db := openMigratedTestDB(t)
	uid, contact := int64(9100101), int64(2147483645)
	oldID := seedPendingDMResult(t, db, uid, contact, "pre-stop reply", float64(time.Now().Unix())-10)

	aiDeliveryBarrierForTest = func() {
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			t.Errorf("屏障置急停: %v", err)
		}
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
			t.Errorf("屏障解除急停: %v", err)
		}
	}
	defer func() { aiDeliveryBarrierForTest = nil }()

	srv := &Server{DB: db, log: logging.Discard()}
	srv.pollAIResults(ctx, db)
	if n := mailCount(t, db, uid); n != 0 {
		t.Fatalf("停前旧私聊在 1→0 后写入 mail %d 行，期望 0", n)
	}
	if delivered, reply := resultDeliveryState(t, db, oldID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
		t.Fatalf("停前旧私聊未落终态/未清正文: delivered=%+v reply=%q", delivered, reply)
	}
}

// TestAIDeliveryBoundaryRechecksStop 受控屏障把急停插在最后一次判定之前：
// 频道最终推送边界与私聊事务边界都必须用同一急停判断复核，停后零推送、零写入。
func TestAIDeliveryBoundaryRechecksStop(t *testing.T) {
	ctx := context.Background()

	t.Run("频道最终推送边界", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_boundary_channel"
		p := newT013ChatProbeWithDB(t, channel, db)
		id := seedPendingChannelResult(t, db, channel, "boundary-channel", float64(time.Now().Unix()))
		aiDeliveryBarrierForTest = func() {
			if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
				t.Errorf("屏障置急停: %v", err)
			}
		}
		defer func() { aiDeliveryBarrierForTest = nil }()

		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("频道最终推送边界观察到急停后结果应落终态: %+v %q", delivered, reply)
		}
	})

	t.Run("私聊事务边界", func(t *testing.T) {
		db := openMigratedTestDB(t)
		uid, contact := int64(9100002), int64(2147483645)
		id := seedPendingDMResult(t, db, uid, contact, "boundary-dm", float64(time.Now().Unix()))
		aiDeliveryBarrierForTest = func() {
			if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
				t.Errorf("屏障置急停: %v", err)
			}
		}
		defer func() { aiDeliveryBarrierForTest = nil }()

		srv := &Server{DB: db, log: logging.Discard()}
		srv.pollAIResults(ctx, db)
		if n := mailCount(t, db, uid); n != 0 {
			t.Fatalf("私聊事务边界观察到急停后不得写 mail，行数 = %d", n)
		}
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("私聊结果应落终态并清正文: %+v %q", delivered, reply)
		}
	})
}

// TestT102MatrixEqualClockRollbackAndMissingOrigin 复评矩阵的异常时点行：
// 等时、时钟回拨（解除点在未来）与父任务缺失都必须保守失效，零推送；
// 正常新意图仍按各自规则投递。
func TestT102MatrixEqualClockRollbackAndMissingOrigin(t *testing.T) {
	ctx := context.Background()

	t.Run("等时保守失效", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_equal_ts"
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		id := seedPendingChannelResult(t, db, channel, "equal-ts-reply", lifted, db.AIEmergencyState(ctx).Epoch-2)
		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("等时结果未被保守丢弃: %+v %q", delivered, reply)
		}
	})

	t.Run("时钟回拨不重放", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_clock_back"
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		// 模拟保存解除时进程时钟偏快：解除点落在未来，当前新任务也早于它。
		if _, err := db.SQL.ExecContext(ctx,
			`UPDATE ai_setting SET value=? WHERE key=?`,
			strconv.FormatFloat(lifted+3600, 'f', 6, 64), aiconfig.KeyEmergencyStopLiftedTS); err != nil {
			t.Fatal(err)
		}
		id := seedPendingChannelResult(t, db, channel, "rollback-reply", float64(time.Now().Unix()), db.AIEmergencyState(ctx).Epoch-2)
		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("时钟回拨后旧结果未被保守丢弃: %+v %q", delivered, reply)
		}
	})

	t.Run("父任务缺失保守失效", func(t *testing.T) {
		db := openMigratedTestDB(t)
		channel := "t102_origin_missing"
		p := newT013ChatProbeWithDB(t, channel, db)
		lifted := liftNow(t, db)
		res, err := db.SQL.ExecContext(ctx, `
			INSERT INTO ai_result(task_id,status,reply_text,contact_uid,created_ts)
			VALUES(999999,'done','no-parent',2147483645,?)`, lifted+5)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
			t.Fatalf("父任务缺失的结果未被保守丢弃: %+v %q", delivered, reply)
		}
	})
}

// TestT102MatrixRestartAndFreshIntent 复评矩阵的重启与混批行：
// 新进程（新 Server 实例）只按持久状态处理旧 pending，解除后不重放；
// 解除提交后产生的新意图与残留旧行混批时，只有新意图到达。
func TestT102MatrixRestartAndFreshIntent(t *testing.T) {
	ctx := context.Background()
	db := openMigratedTestDB(t)
	uid, contact := int64(9100201), int64(2147483645)
	oldID := seedPendingDMResult(t, db, uid, contact, "stop-era-dm", float64(time.Now().Unix())-10)
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	// 停中重启：新实例读取旧 pending，停中零出站、零投递。
	srv := &Server{DB: db, log: logging.Discard()}
	srv.pollAIResults(ctx, db)
	if n := mailCount(t, db, uid); n != 0 {
		t.Fatalf("停中重启后写入了 %d 行私聊", n)
	}
	if delivered, reply := resultDeliveryState(t, db, oldID); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
		t.Fatalf("停中重启后旧结果未落终态: %+v %q", delivered, reply)
	}

	// 解除；旧行不得复活，新意图（严格晚于解除点）正常投递。
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	lifted := db.AISetting(ctx, aiconfig.KeyEmergencyStopLiftedTS, "")
	lift, err := strconv.ParseFloat(lifted, 64)
	if err != nil || lift <= 0 {
		t.Fatalf("解除点 = %q (%v)", lifted, err)
	}
	oldID2 := seedPendingDMResult(t, db, uid, contact, "old-still-pending", lift-5, db.AIEmergencyState(ctx).Epoch-2)
	freshID := seedPendingDMResult(t, db, uid, contact, "fresh-after-resume", lift+5)
	srv2 := &Server{DB: db, log: logging.Discard()} // 解除后再次重启
	srv2.pollAIResults(ctx, db)
	if n := mailCount(t, db, uid); n != 1 {
		t.Fatalf("混批投递后 mail = %d 行，期望只增加新意图的 1 行", n)
	}
	if delivered, reply := resultDeliveryState(t, db, oldID2); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
		t.Fatalf("解除后旧 pending 被复活: %+v %q", delivered, reply)
	}
	if delivered, reply := resultDeliveryState(t, db, freshID); !delivered.Valid || delivered.Float64 <= 0 || reply != "" {
		t.Fatalf("解除后新意图未正常投递/清正文: %+v %q", delivered, reply)
	}
}

func TestSupervisorT102RollbackBeforeSecondLiftDoesNotRevivePendingResult(t *testing.T) {
	ctx := context.Background()
	db := openMigratedTestDB(t)
	channel := "t102_second_lift"
	p := newT013ChatProbeWithDB(t, channel, db)
	future := time.Now().Add(time.Hour).Unix()
	if err := db.AISetSetting(ctx, aiconfig.KeyEmergencyStopLiftedTS, strconv.FormatInt(future, 10)); err != nil {
		t.Fatal(err)
	}
	old := seedPendingChannelResult(t, db, channel, "pre-stop-after-clock-rollback", float64(future-10))
	for _, stop := range []string{"1", "0"} {
		if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: stop}); err != nil {
			t.Fatal(err)
		}
	}
	p.srv.pollAIResults(ctx, db)
	p.expectNoMore(t)
	if delivered, reply := resultDeliveryState(t, db, old); !delivered.Valid || delivered.Float64 != -1 || reply != "" {
		t.Fatalf("old result after rollback: %+v %q", delivered, reply)
	}
	fresh := seedPendingChannelResult(t, db, channel, "fresh-at-lower-clock", float64(future-3600))
	p.srv.pollAIResults(ctx, db)
	if hit := p.next(t); hit.text != "fresh-at-lower-clock" {
		t.Fatalf("fresh low-clock reply: %+v", hit)
	}
	if delivered, _ := resultDeliveryState(t, db, fresh); !delivered.Valid || delivered.Float64 <= 0 {
		t.Fatalf("fresh low-clock result was not delivered: %+v", delivered)
	}
	t.Logf("old result %d: zero 24/27 and terminal -1; fresh result %d: one 24/27 despite lower wall clock", old, fresh)

}

func TestT102DeliveryAfterPartialRelease(t *testing.T) {
	ctx := t.Context()
	db := openMigratedTestDB(t)
	t.Run("between channel recipients", func(t *testing.T) {
		a := newT013ChatProbeWithDB(t, "t102_two_peers", db)
		b := newT013ChatProbeWithDB(t, "t102_two_peers", db)
		b.sess.ID = 2
		a.srv.sessions[b.sess.ID] = b.sess
		id := seedPendingChannelResult(t, db, "t102_two_peers", "partial-reply", float64(time.Now().Unix()))
		checks := 0
		aiDeliveryBarrierForTest = func() {
			checks++
			if checks == 2 {
				liftNow(t, db)
			}
		}
		t.Cleanup(func() { aiDeliveryBarrierForTest = nil })
		a.srv.pollAIResults(ctx, db)
		a.srv.pollAIResults(ctx, db)
		select {
		case hit := <-a.records:
			if hit.text != "partial-reply" {
				t.Fatalf("first recipient: %+v", hit)
			}
			b.expectNoMore(t)
		case hit := <-b.records:
			if hit.text != "partial-reply" {
				t.Fatalf("second recipient: %+v", hit)
			}
			a.expectNoMore(t)
		case <-time.After(time.Second):
			t.Fatal("no recipient released before switch")
		}
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 <= 0 || reply != "" || checks != 2 {
			t.Fatalf("partial delivery result %d: delivered=%+v reply=%q checks=%d", id, delivered, reply, checks)
		}
		t.Logf("result %d: one 24/27 released, second blocked, consumed once", id)
	})
	t.Run("committed mail before online notification", func(t *testing.T) {
		p := newT013ChatProbeWithDB(t, "t102_dm_notify", db)
		id := seedPendingDMResult(t, db, p.sess.Userid, 2147483645, "committed-dm", float64(time.Now().Unix()))
		checks := 0
		aiMailNotifyBarrierForTest = func() { checks++; liftNow(t, db) }
		t.Cleanup(func() { aiMailNotifyBarrierForTest = nil })
		p.srv.pollAIResults(ctx, db)
		p.srv.pollAIResults(ctx, db)
		p.expectNoMore(t)
		if count := mailCount(t, db, p.sess.Userid); count != 1 || checks != 1 {
			t.Fatalf("mail count=%d, notify checks=%d", count, checks)
		}
		if delivered, reply := resultDeliveryState(t, db, id); !delivered.Valid || delivered.Float64 <= 0 || reply != "" {
			t.Fatalf("mail was not committed with delivery marker: %+v %q", delivered, reply)
		}
		t.Logf("result %d: one committed mail, zero online pushes; duplicate poll inert", id)
	})
}

func TestT102SystemAndRuntimeWritesKeepOriginalEpoch(t *testing.T) {
	ctx := t.Context()
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	loc, fishKey := runtimeGSRoster(t, db)
	agent, err := db.AIAgentByKey(ctx, runtimeGSAgent)
	if err != nil || agent == nil {
		t.Fatalf("agent: %v", err)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_agent SET actor_enabled=1, joined=1, fishing_talk_enabled=1 WHERE key=?`, agent.Key); err != nil {
		t.Fatal(err)
	}
	fish := aiFishTemplate{FishKey: fishKey, Name: "synthetic", WeightG: 100, Loc: loc, Event: evStar}
	agent.FishingTalkEnabled = true
	// T-108：系统事件资格来自能力集合，不再来自旧全局开关。
	grantCapabilities(t, db, agent.Key,
		aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityBroadcastCatch)
	cuts := 0
	switchEpoch := func() { cuts++; liftNow(t, db) }
	aiRuntimeWriteBarrierForTest = switchEpoch
	aiSystemEnqueueBarrierForTest = switchEpoch
	t.Cleanup(func() { aiRuntimeWriteBarrierForTest, aiSystemEnqueueBarrierForTest = nil, nil })
	if srv.actorTick(ctx, agent.Key, loc) != nil {
		t.Fatal("old actor tick returned active state")
	}
	if srv.writeAICatch(ctx, db, *agent, fish, loc) {
		t.Fatal("old fish write committed")
	}
	if _, err := srv.aiFishingTalkEnqueue(ctx, db, "synthetic-channel", 1, *agent, fish, 10, 0); err == nil {
		t.Fatal("old system event entered the queue")
	}
	if cuts != 3 {
		t.Fatalf("expected 3 attempted boundaries, got %d", cuts)
	}
	for _, table := range []string{"ai_actor_state", "ai_catch", "ai_memory", "ai_task"} {
		var n int
		if err := db.SQL.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows=%d err=%v, want zero", table, n, err)
		}
	}
	aiRuntimeWriteBarrierForTest, aiSystemEnqueueBarrierForTest = nil, nil
	_, enqueueErr := srv.aiFishingTalkEnqueue(ctx, db, "synthetic-channel", 1, *agent, fish, 10, 0)
	if srv.actorTick(ctx, agent.Key, loc) == nil || !srv.writeAICatch(ctx, db, *agent, fish, loc) ||
		enqueueErr != nil {
		t.Fatal("fresh actor, catch and system intent should succeed")
	}
	t.Log("3 stop/resume boundaries; actor, catch, memory and task deltas zero; fresh actor, catch and system task succeed")
}
