package gameserver

// T-108 E1–E4：跨包运行取证。
//
// 每个子例都走真实入口，不直接塞队列：
//   - GS 侧：24/10 公聊入口或真实玩家上星回调 → 真实 ai_task 入队事务；
//   - BR 侧：真实 aibridge.Runner 按 AIPopTask 认领并处理，厂商是本地 TLS 计数桩；
//   - GS 投递侧：真实 pollAIResults → 24/27，由带真实隧道的客户端侧计数。
//
// 受控时点只有两个：认领后不跑 BR（暂停 BR），以及写结果后不轮询（暂停 GS 投递），
// 因此「撤权发生在哪一侧」是可复跑的事实，而不是注释。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// t108EvidenceVendor 是可计数、可记录请求正文的本地 TLS 厂商。
type t108EvidenceVendor struct {
	srv    *httptest.Server
	hits   int32
	mu     sync.Mutex
	bodies []string
	reply  string
	mutate func() // 第一次请求到达时执行一次（受控时点）
}

func newT108EvidenceVendor(t *testing.T, reply string) *t108EvidenceVendor {
	t.Helper()
	v := &t108EvidenceVendor{reply: reply}
	var once sync.Once
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&v.hits, 1)
		if req.Body != nil {
			if body, err := io.ReadAll(req.Body); err == nil {
				v.capture(body)
			}
		}
		if v.mutate != nil {
			once.Do(v.mutate)
		}
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": v.replyText()}}},
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *t108EvidenceVendor) calls() int { return int(atomic.LoadInt32(&v.hits)) }

func (v *t108EvidenceVendor) setMutate(f func()) {
	v.mu.Lock()
	v.mutate = f
	v.mu.Unlock()
}

// t108EvidenceRuntime 是一套「真实 GS + 真实 BR + 计数厂商 + 真实隧道客户端」的隔离运行。
type t108EvidenceRuntime struct {
	t       *testing.T
	ctx     context.Context
	db      *store.DB
	srv     *Server
	probe   *t013ChatProbe
	runner  *aibridge.Runner
	vendor  *t108EvidenceVendor
	channel string
}

func newT108EvidenceRuntime(t *testing.T, channel, reply string) *t108EvidenceRuntime {
	t.Helper()
	return newT108EvidenceRuntimeWithDB(t, channel, reply, openStaticTestDB(t))
}

// newT108EvidenceRuntimeWithDB 用调用方给的库装配同一套运行夹具。
// 供「同版运行夹具库」用例把真实链写进一个保留在磁盘上的库（T-107 P4 交接）。
func newT108EvidenceRuntimeWithDB(t *testing.T, channel, reply string, db *store.DB) *t108EvidenceRuntime {
	t.Helper()
	ctx := context.Background()
	vendor := newT108EvidenceVendor(t, reply)
	if _, err := db.SQL.Exec(`UPDATE ai_provider SET base_url=? WHERE name='deepseek'`, vendor.srv.URL); err != nil {
		t.Fatalf("配置厂商地址: %v", err)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_model SET model_id='model-a'
		WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatalf("配置模型: %v", err)
	}
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	ciphertext, nonce, err := aibridge.Encrypt(masterKey, []byte("sk-synthetic-t108"))
	if err != nil {
		t.Fatalf("加密合成密钥: %v", err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','****t108',0)
		ON CONFLICT(provider_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce`,
		ciphertext, nonce); err != nil {
		t.Fatalf("写入合成密钥: %v", err)
	}
	if err := db.AISetSettings(ctx, map[string]string{
		aiconfig.KeyEmergencyStop: "0",
		// 圆桌的硬限仍在区间内，只把本轮时长压短，避免用例等待 20 秒。
		"roundtable_max_speakers": "2",
		"roundtable_max_turns":    "2",
		"roundtable_max_streak":   "1",
		"roundtable_gap_ms":       "500",
		"roundtable_max_ms":       "6000",
	}); err != nil {
		t.Fatalf("初始化设置: %v", err)
	}
	probe := newT013ChatProbeWithDB(t, channel, db)
	restore := aibridge.SetRunnerProviderTransportForTest(vendor.srv.Client().Transport)
	t.Cleanup(restore)
	runner := aibridge.NewRunner(db, nil,
		config.AI{Enabled: true, RequestTimeoutSec: 5, MaxConcurrent: 1}, masterKey, logging.Discard())
	return &t108EvidenceRuntime{
		t: t, ctx: ctx, db: db, srv: probe.srv, probe: probe,
		runner: runner, vendor: vendor, channel: channel,
	}
}

// addAgent 接入并订阅一个内置 Agent，返回保存后的能力修订。
func (rt *t108EvidenceRuntime) addAgent(key string, caps ...string) int64 {
	rt.t.Helper()
	t108Agent(rt.t, rt.db, rt.channel, key)
	return t108SetCapabilities(rt.t, rt.db, key, caps...)
}

// setCapabilities 用当前修订保存一次能力集合（撤权/重授都走这里）。
func t108SetCapabilities(t *testing.T, db *store.DB, key string, ids ...string) int64 {
	t.Helper()
	ctx := context.Background()
	view, err := db.AIGetCapabilities(ctx, key)
	if err != nil {
		t.Fatalf("读 %s 能力: %v", key, err)
	}
	saved, err := db.AISaveCapabilities(ctx, key, view.Revision, ids, nil)
	if err != nil {
		t.Fatalf("保存 %s 能力: %v", key, err)
	}
	return saved.Revision
}

// savePersonality 以当前修订为基线保存四维，返回新修订。
func t108SavePersonality(t *testing.T, db *store.DB, key string, soc, exp, per, reg int) int64 {
	t.Helper()
	ctx := context.Background()
	cur, err := db.AIGetPersonality(ctx, key)
	if err != nil {
		t.Fatalf("读 %s 性格: %v", key, err)
	}
	rev, err := db.AISavePersonality(ctx, key, cur.Config.Revision, personality(soc, exp, per, reg))
	if err != nil {
		t.Fatalf("保存 %s 性格: %v", key, err)
	}
	return rev
}

// processOne 认领最旧的一条任务并同步跑一次 BR。
func (rt *t108EvidenceRuntime) processOne() *store.AITask {
	rt.t.Helper()
	task, ok, err := rt.db.AIPopTask(rt.ctx)
	if err != nil || !ok {
		rt.t.Fatalf("认领任务失败: ok=%v err=%v", ok, err)
	}
	rt.runner.HandleTaskForTest(rt.ctx, task)
	return task
}

// poll 走真实 GS 投递轮询。
func (rt *t108EvidenceRuntime) poll() {
	rt.t.Helper()
	rt.srv.pollAIResults(rt.ctx, rt.db)
}

// drainBroadcast 吃掉公聊入口自己的 24/27 广播（真实入口的副作用，不属于 AI 帧）。
func (rt *t108EvidenceRuntime) drainBroadcast(msg string) {
	rt.t.Helper()
	rec := rt.probe.next(rt.t)
	if rec.main != 24 || rec.sub != 27 || rec.text != msg {
		rt.t.Fatalf("公聊广播不符: %d/%d text=%q", rec.main, rec.sub, rec.text)
	}
}

// t108EvidenceResult 是一条结果及其附带凭据。
type t108EvidenceResult struct {
	ID         int64
	Status     string
	Reply      string
	Err        string
	Delivered  float64 // -999 表示未投递
	ContactUID int64
	BRRevision int64
	HasExpr    bool
}

func t108EvidenceResults(t *testing.T, db *store.DB, taskID int64) []t108EvidenceResult {
	t.Helper()
	rows, err := db.SQL.Query(`
		SELECT r.id, COALESCE(r.status,''), COALESCE(r.reply_text,''), COALESCE(r.err,''),
		       COALESCE(r.delivered_ts,-999), COALESCE(r.contact_uid,0),
		       COALESCE(e.br_personality_revision,0), (e.result_id IS NOT NULL)
		FROM ai_result r LEFT JOIN ai_result_expression e ON e.result_id=r.id
		WHERE r.task_id=? ORDER BY r.id`, taskID)
	if err != nil {
		t.Fatalf("读结果: %v", err)
	}
	defer rows.Close()
	var out []t108EvidenceResult
	for rows.Next() {
		var r t108EvidenceResult
		if err := rows.Scan(&r.ID, &r.Status, &r.Reply, &r.Err, &r.Delivered,
			&r.ContactUID, &r.BRRevision, &r.HasExpr); err != nil {
			t.Fatalf("扫结果: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历结果: %v", err)
	}
	return out
}

func (rt *t108EvidenceRuntime) evidence(taskID int64) store.AITaskEvidence {
	rt.t.Helper()
	ev, ok, err := rt.db.AITaskEvidenceOf(rt.ctx, taskID)
	if err != nil || !ok {
		rt.t.Fatalf("读任务证据: ok=%v err=%v", ok, err)
	}
	return ev
}

func (rt *t108EvidenceRuntime) report(taskID int64) store.AITaskEvidenceReport {
	rt.t.Helper()
	rep, err := rt.db.AITaskEvidenceReport(rt.ctx, taskID)
	if err != nil {
		rt.t.Fatalf("读证据报告: %v", err)
	}
	return rep
}

func (rt *t108EvidenceRuntime) agentGVR(key string) (int64, int64) {
	rt.t.Helper()
	view, err := rt.db.AIGetCapabilities(rt.ctx, key)
	if err != nil {
		rt.t.Fatalf("读 %s 能力: %v", key, err)
	}
	return view.Generation, view.Revision
}

func (rt *t108EvidenceRuntime) expectFrame(want string) t013ChatRecord {
	rt.t.Helper()
	rec := rt.probe.next(rt.t)
	if rec.main != 24 || rec.sub != 27 {
		rt.t.Fatalf("AI 频道帧必须走 24/27，实得 %d/%d", rec.main, rec.sub)
	}
	if !strings.Contains(rec.text, want) {
		rt.t.Fatalf("频道帧 %q 未包含 %q", rec.text, want)
	}
	return rec
}

func (rt *t108EvidenceRuntime) expectNoFrame() {
	rt.t.Helper()
	rt.probe.expectNoMore(rt.t)
}

// onlyTask 断言库里恰好一条任务并返回它。
func (rt *t108EvidenceRuntime) onlyTask() t108TaskRow {
	rt.t.Helper()
	tasks := t108Tasks(rt.t, rt.db)
	if len(tasks) != 1 {
		rt.t.Fatalf("应恰好 1 条任务，实得 %d：%+v", len(tasks), tasks)
	}
	return tasks[0]
}

// t108WantSeed 找一个让该角色按自己的四维愿意做某事的种子。
func t108WantSeed(t *testing.T, db *store.DB, key string, feature aiconfig.PersonalityFeature, offset int64) int64 {
	t.Helper()
	p, err := db.AIGetPersonality(context.Background(), key)
	if err != nil {
		t.Fatalf("读 %s 性格: %v", key, err)
	}
	return t108SeedForOutcome(t, p.Config, feature, true, offset)
}

// playerStarFish 是一条奖杯档渔获（E2/E3 用真实玩家上星回调）。
func playerStarFish() *RolledFish {
	return &RolledFish{Grade: 3, Species: "perch", Name: "鲈鱼", WeightG: 2400}
}

// ---------------------------------------------------------------------------
// TestT108EvidenceRuntime
// ---------------------------------------------------------------------------

func TestT108EvidenceRuntime(t *testing.T) {
	// E1：新库默认设置下，普通后台能力与订阅即可起桌；旧全局键不再是决定项。
	t.Run("DefaultRoundtable", func(t *testing.T) {
		for _, legacy := range []string{"0", "1"} {
			legacy := legacy
			t.Run("legacy_switch_"+legacy, func(t *testing.T) {
				rt := newT108EvidenceRuntime(t, "t108_ev_rt_"+legacy, "好家伙，这条不小")
				// 新库默认：旧全局键不存在（读默认 0）；这里再按旧写兼容显式写同一个键。
				if got := rt.db.AISetting(rt.ctx, "roundtable_enabled", "0"); got != "0" {
					t.Fatalf("新库默认 roundtable_enabled=%q，期望 0", got)
				}
				if err := rt.db.AISetSetting(rt.ctx, "roundtable_enabled", legacy); err != nil {
					t.Fatal(err)
				}
				revs := map[string]int64{}
				for _, key := range []string{"companion", "fishguide"} {
					revs[key] = rt.addAgent(key, aiconfig.CapabilityJoinRoundtable)
					t108Personality(t, rt.db, key, 90, 60, 40, 50)
				}
				// 至少一个人愿意起头：种子固定后结论可复跑。
				seed := int64(0)
				for s := int64(1); s <= 4096 && seed == 0; s++ {
					for _, key := range []string{"companion", "fishguide"} {
						p, _ := rt.db.AIGetPersonality(rt.ctx, key)
						want, _ := activityWants(p.Config, s^store.AIActorSeed(0, key), activityGatherFeature())
						if want {
							seed = s
							break
						}
					}
				}
				if seed == 0 {
					t.Fatal("找不到愿意起桌的种子")
				}
				t108SeedFixed(t, seed)

				rt.probe.send(t, "你们今天钓得怎么样")
				rt.drainBroadcast("你们今天钓得怎么样")
				task := rt.onlyTask()
				if task.Kind != "roundtable" || task.SourceKind != "human" {
					t.Fatalf("默认设置下应入队圆桌，实得 %s/%s", task.Kind, task.SourceKind)
				}
				ev := rt.evidence(task.ID)
				if ev.Category != aiconfig.EventRoundtable || ev.Direct || len(ev.Participants) != 2 {
					t.Fatalf("圆桌证据不符: %+v", ev)
				}
				for _, p := range ev.Participants {
					if p.Capability != aiconfig.CapabilityJoinRoundtable {
						t.Fatalf("参与者 %s 的绑定能力=%q", p.Key, p.Capability)
					}
					if p.Generation != 1 || p.CapRevision != revs[p.Key] {
						t.Fatalf("参与者 %s 绑定代次/修订=%d/%d，期望 1/%d",
							p.Key, p.Generation, p.CapRevision, revs[p.Key])
					}
				}

				rt.processOne()
				if rt.vendor.calls() == 0 {
					t.Fatal("圆桌应至少调用一次厂商")
				}
				// 先看写结果时的形状（投递后正文会按既有保留期清空）。
				beforePoll := t108EvidenceResults(t, rt.db, task.ID)
				spoken := 0
				for _, r := range beforePoll {
					if r.Status == "done" && r.Reply != "" {
						spoken++
						if !r.HasExpr {
							t.Fatalf("成功发言缺表达凭据: %+v", r)
						}
						if r.BRRevision != revs["companion"] && r.BRRevision != revs["fishguide"] {
							t.Fatalf("表达凭据修订 %d 不在本轮角色修订内", r.BRRevision)
						}
					}
				}
				if spoken == 0 {
					t.Fatalf("应至少有一句实际发言: %+v", beforePoll)
				}
				rt.poll()
				rt.expectFrame("这条不小")
				delivered := 0
				for _, r := range t108EvidenceResults(t, rt.db, task.ID) {
					if r.Delivered >= 0 {
						delivered++
					}
				}
				if delivered == 0 {
					t.Fatal("应至少有一帧真实投递")
				}
				t.Logf("E1 legacy=%s task=%d kind=roundtable participants=%d seed=%d vendor_calls=%d spoken=%d delivered=%d",
					legacy, task.ID, len(ev.Participants), seed, rt.vendor.calls(), spoken, delivered)
			})
		}
	})

	// E2 ①：真实上星入队后暂停 BR，仅撤 reply_star（保留 join_roundtable）再放行。
	t.Run("RevokedBeforeBR", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_rev_br", "这条鱼真漂亮")
		rev0 := rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		ev := rt.evidence(task.ID)
		if ev.Category != aiconfig.EventReplyStar || len(ev.Participants) != 1 {
			t.Fatalf("上星证据不符: %+v", ev)
		}
		bound := ev.Participants[0]
		if bound.Capability != aiconfig.CapabilityReplyStar || bound.CapRevision != rev0 {
			t.Fatalf("上星绑定 = %+v，期望 reply_star@%d", bound, rev0)
		}
		// 暂停 BR（不认领），只撤 reply_star。
		rev1 := t108SetCapabilities(t, rt.db, "companion", aiconfig.CapabilityJoinRoundtable)

		rt.processOne()
		if got := rt.vendor.calls(); got != 0 {
			t.Fatalf("撤权后 BR 不得调用厂商，实得 %d 次", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "fallback" || results[0].Reply != "" {
			t.Fatalf("撤权后应落不可投递终态: %+v", results)
		}
		if results[0].Err != store.AITaskEvidenceReasonCapability {
			t.Fatalf("终态原因 = %q，期望 %q", results[0].Err, store.AITaskEvidenceReasonCapability)
		}
		rt.poll()
		rt.expectNoFrame()

		// 撤后重授：修订已变，旧意图仍然终止。
		rev2 := t108SetCapabilities(t, rt.db, "companion",
			aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		if rev2 <= rev1 {
			t.Fatalf("重授应推进修订: %d -> %d", rev1, rev2)
		}

		// 撤权不影响其他能力，也不影响新授权事件：再开一次圆桌并真实投递。
		for _, key := range []string{"companion", "fishguide"} {
			rt.addAgent(key, aiconfig.CapabilityJoinRoundtable)
			t108Personality(t, rt.db, key, 90, 60, 40, 50)
		}
		seed := int64(0)
		for s := int64(1); s <= 4096 && seed == 0; s++ {
			for _, key := range []string{"companion", "fishguide"} {
				p, _ := rt.db.AIGetPersonality(rt.ctx, key)
				if want, _ := activityWants(p.Config, s^store.AIActorSeed(0, key),
					activityGatherFeature()); want {
					seed = s
					break
				}
			}
		}
		t108SeedFixed(t, seed)
		rt.probe.send(t, "你们还在钓吗")
		rt.drainBroadcast("你们还在钓吗")
		rt.tasksAfter(t, 2)
		newTask := rt.onlyTaskWithKind(t, "roundtable")
		rt.processOne()
		rt.poll()
		rt.expectFrame("这条鱼真漂亮")
		t.Logf("E2① task=%d bound=reply_star@%d revoked_to=%d terminal=%q vendor_calls=0 frames=0 new_roundtable=%d ok",
			task.ID, rev0, rev1, results[0].Err, newTask.ID)
	})

	// E2 ②：正常消费拿到结果后暂停 GS 投递，再撤权放行。
	t.Run("RevokedBeforeGS", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_rev_gs", "好看，收着")
		rev0 := rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("正常消费应调用一次厂商，实得 %d", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "done" || results[0].Reply == "" {
			t.Fatalf("BR 应写入一条可投递结果: %+v", results)
		}
		if !results[0].HasExpr || results[0].BRRevision == 0 {
			t.Fatalf("成功发言应带表达凭据: %+v", results[0])
		}
		// 暂停 GS（不轮询），只撤 reply_star。
		rev1 := t108SetCapabilities(t, rt.db, "companion", aiconfig.CapabilityJoinRoundtable)
		rt.poll()
		rt.expectNoFrame()
		after := t108EvidenceResults(t, rt.db, task.ID)
		if len(after) != 1 || after[0].Delivered != -1 || after[0].Reply != "" {
			t.Fatalf("投递前撤权应落终态并清空正文: %+v", after)
		}
		if after[0].Err != store.AITaskEvidenceReasonCapability {
			t.Fatalf("投递拒绝原因 = %q，期望 %q", after[0].Err, store.AITaskEvidenceReasonCapability)
		}
		// 再次轮询不补发。
		rt.poll()
		rt.expectNoFrame()
		t.Logf("E2② task=%d result=%d bound=reply_star@%d revoked_to=%d vendor_calls=1 frames=0 terminal=%q",
			task.ID, results[0].ID, rev0, rev1, after[0].Err)
	})

	// E3：同版正例。管理员保存非中性四维，真实上星链按同版编译并送出。
	t.Run("SameRevision", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_same", "这条得记一下")
		rev0 := rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revP := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		ev := rt.evidence(task.ID)
		if len(ev.Participants) != 1 || ev.Participants[0].GSPersonalityRevision == nil {
			t.Fatalf("GS 应绑定本次选择实际用到的四维修订: %+v", ev.Participants)
		}
		if got := *ev.Participants[0].GSPersonalityRevision; got != revP {
			t.Fatalf("GS 绑定修订 = %d，期望 %d", got, revP)
		}

		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("同版正例应调用一次厂商，实得 %d", got)
		}
		// 独立预期片段：用同一组固定输入 + 已知保存修订重新编译，不从运行日志里抄。
		wantCfg := personality(88, 12, 64, 31)
		wantCfg.Revision = revP
		wantFragment := expressionFragment(wantCfg)
		body := rt.vendor.lastBody(t)
		if !strings.Contains(body, wantFragment) {
			t.Fatalf("厂商请求缺预期表达片段 %q", wantFragment)
		}
		rt.poll()
		rt.expectFrame("这条得记一下")
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || !results[0].HasExpr || results[0].BRRevision != revP {
			t.Fatalf("BR 表达凭据应与 GS 同版 %d: %+v", revP, results)
		}
		rep := rt.report(task.ID)
		if !rep.Matched || len(rep.Results) != 1 || rep.Results[0].CurrentRevision != revP {
			t.Fatalf("同库只读查询应判匹配: %+v", rep)
		}
		// 改四维只改角色，不改权限：能力修订保持不变。
		gen2, capRev2 := rt.agentGVR("companion")
		if capRev2 != rev0 || gen2 != 1 {
			t.Fatalf("保存四维改动了能力代次/修订: generation=%d revision=%d，期望 1/%d", gen2, capRev2, rev0)
		}
		// 同种子换一组可区分的四维：约束随输入变化。
		revP2 := t108SavePersonality(t, rt.db, "companion", 10, 90, 20, 80)
		aiChannelCooldown.Delete(rt.channel + "|catch")
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		second := rt.onlyTaskWithKind(t, "channel")
		rt.processOne()
		body2 := rt.vendor.lastBody(t)
		if strings.Contains(body2, wantFragment) {
			t.Fatalf("四维改变后请求仍带旧约束")
		}
		secondCfg := personality(10, 90, 20, 80)
		secondCfg.Revision = revP2
		if !strings.Contains(body2, expressionFragment(secondCfg)) {
			t.Fatalf("四维改变后请求未带新约束 %q", expressionFragment(secondCfg))
		}
		t.Logf("E3 task=%d result=%d agent=companion generation=%d gs_rev=%d br_rev=%d caps_rev=%d matched=true second_task=%d second_rev=%d",
			task.ID, results[0].ID, ev.Participants[0].Generation, revP, results[0].BRRevision, capRev2, second.ID, revP2)
	})

	// E4 反例 A：GS 已按 r 入队、BR 尚未取配置时保存 r+1。
	t.Run("ChangedBeforeBR", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_chg_br", "看到了")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		rev1 := t108SavePersonality(t, rt.db, "companion", 70, 40, 60, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()

		// BR 还没取配置：正常保存路径推进到 r+1。
		rev2 := t108SavePersonality(t, rt.db, "companion", 20, 80, 30, 70)
		if rev2 != rev1+1 {
			t.Fatalf("保存后修订 = %d，期望 %d", rev2, rev1+1)
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 0 {
			t.Fatalf("跨版旧任务不得调用厂商，实得 %d", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "fallback" || results[0].HasExpr {
			t.Fatalf("跨版旧任务不得标双端成功: %+v", results)
		}
		if results[0].Err != store.AITaskEvidenceReasonPersonaDiff {
			t.Fatalf("终止原因 = %q，期望 %q", results[0].Err, store.AITaskEvidenceReasonPersonaDiff)
		}
		rep := rt.report(task.ID)
		if rep.Matched || rep.Results[0].CurrentRevision != rev2 {
			t.Fatalf("旧任务不得点亮新版本: %+v", rep)
		}
		// 新任务真正按 r+1 运行后才生效。
		aiChannelCooldown.Delete(rt.channel + "|catch")
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		fresh := rt.onlyTaskWithKind(t, "channel")
		if fresh.ID == task.ID {
			t.Fatal("新任务应另起一行")
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("新任务应调用一次厂商，实得 %d", got)
		}
		freshResults := t108EvidenceResults(t, rt.db, fresh.ID)
		if len(freshResults) != 1 || !freshResults[0].HasExpr || freshResults[0].BRRevision != rev2 {
			t.Fatalf("新任务应按 r+1 落凭据: %+v", freshResults)
		}
		if rep2 := rt.report(fresh.ID); !rep2.Matched {
			t.Fatalf("新任务应判同版: %+v", rep2)
		}
		t.Logf("E4a old_task=%d old_reason=%q old_vendor_calls=0 new_task=%d gs_rev=%d br_rev=%d current=%d",
			task.ID, results[0].Err, fresh.ID, rev2, freshResults[0].BRRevision, rev2)
	})

	// E4 反例 B：BR 已按 r 取快照后保存 r+1，旧任务按已绑定旧版完成，但不得点亮新版。
	t.Run("ChangedAfterBR", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_chg_after", "收到")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		rev1 := t108SavePersonality(t, rt.db, "companion", 76, 33, 55, 44)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()

		var savedRev int64
		rt.vendor.setMutate(func() {
			savedRev = t108SavePersonality(t, rt.db, "companion", 30, 70, 40, 60)
		})
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("快照后保存不应打断已开始的调用，实得 %d", got)
		}
		if savedRev != rev1+1 {
			t.Fatalf("受控时点应保存到 r+1，实得 %d", savedRev)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].BRRevision != rev1 {
			t.Fatalf("旧任务只能按已绑定的 r=%d 完成: %+v", rev1, results)
		}
		if results[0].BRRevision == savedRev {
			t.Fatalf("旧任务点亮了新版本 %d", savedRev)
		}
		rep := rt.report(task.ID)
		if !rep.Matched || rep.Results[0].CurrentRevision != savedRev {
			t.Fatalf("旧任务应判与已绑定版本同版，且当前保存版本是 r+1: %+v", rep)
		}
		rt.poll()
		rt.expectFrame("收到")
		t.Logf("E4b task=%d result=%d gs_rev=%d br_rev=%d saved_during_call=%d posted=true",
			task.ID, results[0].ID, rev1, results[0].BRRevision, savedRev)
	})

	// E2 ③：只撤频道订阅（能力/代次都不动），BR 认领后真正出站前复核当前范围。
	t.Run("SubscriptionRevokedBeforeBR", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_sub_br", "看到了")
		rev0 := rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		ev := rt.evidence(task.ID)
		if ev.Category != aiconfig.EventReplyStar || len(ev.Participants) != 1 {
			t.Fatalf("上星证据不符: %+v", ev)
		}
		if ev.Participants[0].CapRevision != rev0 {
			t.Fatalf("上星绑定修订 = %d，期望 %d", ev.Participants[0].CapRevision, rev0)
		}
		// 暂停 BR（不认领），只撤频道订阅：能力与修订保持原样。
		if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, "companion", false); err != nil {
			t.Fatal(err)
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 0 {
			t.Fatalf("撤订阅后 BR 不得调用厂商，实得 %d 次", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "fallback" || results[0].Reply != "" {
			t.Fatalf("撤订阅后应落不可投递终态: %+v", results)
		}
		if results[0].Err != store.AIEvidenceReasonOutOfScope {
			t.Fatalf("终止原因 = %q，期望 %q", results[0].Err, store.AIEvidenceReasonOutOfScope)
		}
		rt.poll()
		rt.expectNoFrame()
		t.Logf("E2③ task=%d kind=%s bound=reply_star@%d 终止=%q vendor_calls=0 frames=0",
			task.ID, task.Kind, rev0, results[0].Err)
	})

	// E2 ④：正常消费拿到结果后暂停 GS 投递，只撤频道订阅再放行。
	t.Run("SubscriptionRevokedBeforeGS", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_sub_gs", "收着")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("正常消费应调用一次厂商，实得 %d", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "done" || results[0].Reply == "" {
			t.Fatalf("BR 应写入一条可投递结果: %+v", results)
		}
		// 暂停 GS（不轮询），只撤频道订阅。
		if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, "companion", false); err != nil {
			t.Fatal(err)
		}
		rt.poll()
		rt.expectNoFrame()
		after := t108EvidenceResults(t, rt.db, task.ID)
		if len(after) != 1 || after[0].Delivered != -1 || after[0].Reply != "" {
			t.Fatalf("投递前撤订阅应落终态并清空正文: %+v", after)
		}
		if after[0].Err != store.AIEvidenceReasonOutOfScope {
			t.Fatalf("投递拒绝原因 = %q，期望 %q", after[0].Err, store.AIEvidenceReasonOutOfScope)
		}
		// 再次轮询不补发。
		rt.poll()
		rt.expectNoFrame()
		t.Logf("E2④ task=%d result=%d 终止=%q vendor_calls=1 frames=0", task.ID, results[0].ID, after[0].Err)
	})

	// E2 ⑤：圆桌中途撤订阅——本轮已发生的一次调用保留，但这一句不写、
	// 剩余发言者全部失效，不足两人即终止。
	t.Run("SubscriptionRevokedMidRoundtable", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_sub_mid", "接一句")
		for _, key := range []string{"companion", "fishguide"} {
			rt.addAgent(key, aiconfig.CapabilityJoinRoundtable)
			t108Personality(t, rt.db, key, 90, 60, 40, 50)
		}
		seed := int64(0)
		for s := int64(1); s <= 4096 && seed == 0; s++ {
			for _, key := range []string{"companion", "fishguide"} {
				p, _ := rt.db.AIGetPersonality(rt.ctx, key)
				if want, _ := activityWants(p.Config, s^store.AIActorSeed(0, key),
					activityGatherFeature()); want {
					seed = s
					break
				}
			}
		}
		if seed == 0 {
			t.Fatal("找不到愿意起桌的种子")
		}
		t108SeedFixed(t, seed)
		rt.probe.send(t, "你们今天钓得怎么样")
		rt.drainBroadcast("你们今天钓得怎么样")
		task := rt.onlyTask()
		if task.Kind != "roundtable" {
			t.Fatalf("应入队圆桌，实得 %s", task.Kind)
		}
		// 第一次厂商请求到达时撤掉两名参与者的频道订阅（圆桌中途撤）。
		rt.vendor.setMutate(func() {
			for _, key := range []string{"companion", "fishguide"} {
				if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, key, false); err != nil {
					t.Errorf("圆桌中途撤订阅 %s: %v", key, err)
				}
			}
		})
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("圆桌中途撤订阅后不得继续出站，厂商调用 %d 次（期望 1）", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		deliverable := 0
		sawOutOfScope := false
		for _, r := range results {
			if r.Status == "done" && r.Reply != "" {
				deliverable++
			}
			if r.Err == store.AIEvidenceReasonOutOfScope {
				sawOutOfScope = true
			}
		}
		if deliverable != 0 {
			t.Fatalf("圆桌中途撤订阅后不得写出可投递发言: %+v", results)
		}
		var state string
		if err := rt.db.SQL.QueryRow(`SELECT state FROM ai_task WHERE id=?`, task.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "fallback" {
			t.Fatalf("不足两人应落 fallback，实得 %q", state)
		}
		if !sawOutOfScope {
			t.Fatalf("终止原因应为 %q: %+v", store.AIEvidenceReasonOutOfScope, results)
		}
		rt.poll()
		rt.expectNoFrame()
		t.Logf("E2⑤ task=%d vendor_calls=1 deliverable=0 state=%s reason=%q frames=0",
			task.ID, state, store.AIEvidenceReasonOutOfScope)
	})

	// E2 ⑥：改到别的图——地图级订阅从本图换到另一张图，等同于离开本频道范围。
	t.Run("ScopeChangedToOtherMap", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "nav1_04", "好家伙")
		// nav1_04 属于 level_001_lake；companion 只走地图级订阅。
		if err := rt.db.AISetAgentJoined(rt.ctx, "companion", true, 0); err != nil {
			t.Fatal(err)
		}
		t108SetCapabilities(t, rt.db, "companion",
			aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		if err := rt.db.AISetMapAgent(rt.ctx, "level_001_lake", "companion", true); err != nil {
			t.Fatal(err)
		}
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		// 改到别的图：撤本图地图级订阅，改订白河（level_010_belaya）。
		if err := rt.db.AISetMapAgent(rt.ctx, "level_001_lake", "companion", false); err != nil {
			t.Fatal(err)
		}
		if err := rt.db.AISetMapAgent(rt.ctx, "level_010_belaya", "companion", true); err != nil {
			t.Fatal(err)
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 0 {
			t.Fatalf("改到别的图后不得调用厂商，实得 %d 次", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Err != store.AIEvidenceReasonOutOfScope {
			t.Fatalf("改图后应按 out_of_scope 终止: %+v", results)
		}
		rt.poll()
		rt.expectNoFrame()
		t.Logf("E2⑥ task=%d channel=nav1_04 level_001_lake->level_010_belaya reason=%q vendor_calls=0 frames=0",
			task.ID, results[0].Err)
	})

	// E2 ⑦：撤后重授再放行——订阅不是修订号；重授后新任务照常，旧终态不被复活。
	t.Run("SubscriptionRegrantedBeforeEgress", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_sub_regrant", "好家伙")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		// 第一条：撤订阅后 BR 前终止（终态不可复活）。
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		first := rt.onlyTask()
		if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, "companion", false); err != nil {
			t.Fatal(err)
		}
		rt.processOne()
		firstResults := t108EvidenceResults(t, rt.db, first.ID)
		if len(firstResults) != 1 || firstResults[0].Err != store.AIEvidenceReasonOutOfScope {
			t.Fatalf("撤订阅任务应终止: %+v", firstResults)
		}

		// 重授后再来一条新任务：放行、真实投递。
		if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, "companion", true); err != nil {
			t.Fatal(err)
		}
		aiChannelCooldown.Delete(rt.channel + "|catch")
		rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
		second := rt.onlyTaskWithKind(t, "channel")
		if second.ID == first.ID {
			t.Fatal("重授后应另起一条新任务")
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("重授后新任务应调用一次厂商，实得 %d", got)
		}
		secondResults := t108EvidenceResults(t, rt.db, second.ID)
		if len(secondResults) != 1 || secondResults[0].Status != "done" || secondResults[0].Reply == "" {
			t.Fatalf("重授后新任务应有可投递结果: %+v", secondResults)
		}
		rt.poll()
		rt.expectFrame("好家伙")
		// 旧任务仍为终态，不补发。
		if after := t108EvidenceResults(t, rt.db, first.ID); after[0].Delivered != -1 {
			t.Fatalf("旧终态被重授复活: %+v", after)
		}
		rt.expectNoFrame()
		t.Logf("E2⑦ first=%d 终止=%q second=%d posted=true 旧任务不补发",
			first.ID, firstResults[0].Err, second.ID)
	})

	// E2 ⑧：Direct/DM 对照——撤订阅不终止「直接对话」任务。
	//
	// 频道里的点名本身仍要先在范围里（否则不进频道，见 E1「越界点名不冒头」）；
	// 但只要任务带的是「直接对话」证据，BR 出站与 GS 投递都不再按范围终止。
	t.Run("DirectDialogUnaffected", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_direct", "在的")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		const msg = "AI 伙伴 你在吗"
		rt.probe.send(t, msg)
		rt.drainBroadcast(msg)
		task := rt.onlyTask()
		if task.Kind != "channel" || task.Agent != "companion" {
			t.Fatalf("点名询答应为 channel/companion，实得 %s/%s", task.Kind, task.Agent)
		}
		ev := rt.evidence(task.ID)
		if !ev.Direct {
			t.Fatalf("被点名的直接询答应写「直接对话」证据: %+v", ev)
		}
		// 暂停 BR，撤频道订阅：直接对话不受范围限制。
		if err := rt.db.AISetChannelAgent(rt.ctx, rt.channel, "companion", false); err != nil {
			t.Fatal(err)
		}
		rt.processOne()
		if got := rt.vendor.calls(); got != 1 {
			t.Fatalf("直接询答不应受撤订阅影响，厂商调用 %d 次", got)
		}
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Status != "done" || results[0].Reply == "" {
			t.Fatalf("直接询答应有可投递结果: %+v", results)
		}
		// GS 投递同样不按范围终止（Direct 例外）。
		rt.poll()
		rt.expectFrame("在的")

		// DM 对照：没有任何频道订阅也照常回答。
		dmID, err := rt.db.AIEnqueue(rt.ctx, &store.AITask{
			Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: t013SelfUID,
			Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: "私聊一句",
		})
		if err != nil {
			t.Fatalf("DM 入队: %v", err)
		}
		dmTask, ok, err := rt.db.AIPopTask(rt.ctx)
		if err != nil || !ok || dmTask.ID != dmID {
			t.Fatalf("认领 DM: ok=%v err=%v", ok, err)
		}
		rt.runner.HandleTaskForTest(rt.ctx, dmTask)
		if got := rt.vendor.calls(); got != 2 {
			t.Fatalf("DM 不应受范围限制，累计厂商调用 %d 次（期望 2）", got)
		}
		dmResults := t108EvidenceResults(t, rt.db, dmID)
		if len(dmResults) != 1 || dmResults[0].Status != "done" || dmResults[0].Reply == "" {
			t.Fatalf("DM 应有可投递结果: %+v", dmResults)
		}
		t.Logf("E2⑧ task=%d direct=%v vendor_calls=1 frame=1；DM task=%d vendor_calls=2（均不受范围限制）",
			task.ID, ev.Direct, dmID)
	})

	// E5：活动只读事实路径——一任务一份快照、圆桌全部发言者共用、输出诚实。
	t.Run("ActivityFactsPath", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_facts", "看情况")
		// 真实 GS 写入路径的赛事投影：一条报名中、一条进行中（已过本地预计结束）。
		if err := rt.db.CreateCompetition(rt.ctx, &store.Competition{
			ID: "c-signup", Name: "报名赛", Owner: t013SelfUID,
			MapKey: "level_001_lake", DurationSec: 3600,
		}, true); err != nil {
			t.Fatalf("建报名赛事: %v", err)
		}
		if err := rt.db.CreateCompetition(rt.ctx, &store.Competition{
			ID: "c-running", Name: "进行赛", Owner: t013SelfUID,
			MapKey: "level_001_lake", DurationSec: 60,
		}, true); err != nil {
			t.Fatalf("建进行赛事: %v", err)
		}
		if _, err := rt.db.StartCompetition(rt.ctx, "c-running", t013SelfUID); err != nil {
			t.Fatalf("开始赛事: %v", err)
		}
		// 时间控制：把开始时刻推到两小时前（本地预计结束已过 → stale，不宣称结束）。
		if _, err := rt.db.SQL.Exec(`UPDATE competition SET started_ts=started_ts-7200 WHERE id='c-running'`); err != nil {
			t.Fatal(err)
		}
		snapshots := 0
		restore := aibridge.SetActivityFactSnapshotHookForTest(func() { snapshots++ })
		t.Cleanup(restore)

		for _, key := range []string{"companion", "fishguide"} {
			rt.addAgent(key, aiconfig.CapabilityJoinRoundtable)
			t108Personality(t, rt.db, key, 90, 60, 40, 50)
		}
		// 点名两名发言者：圆桌会按点名顺序各说一句，两份提示词必须共用同一份快照。
		const msg = "AI 伙伴 鱼情助手 你们今天钓得怎么样"
		rt.probe.send(t, msg)
		rt.drainBroadcast(msg)
		task := rt.onlyTask()
		if task.Kind != "roundtable" {
			t.Fatalf("应入队圆桌，实得 %s", task.Kind)
		}
		rt.processOne()
		if snapshots != 1 {
			t.Fatalf("一个任务只应取一份活动快照，实得 %d", snapshots)
		}
		bodies := rt.vendor.allBodies(t)
		if len(bodies) < 2 {
			t.Fatalf("两名被点名发言者应各出站一次，实得 %d 次", len(bodies))
		}
		sampled := t108Between(bodies[0], "sampled_at=", "）")
		if sampled == "" {
			t.Fatalf("请求正文缺 sampled_at: %s", bodies[0])
		}
		for _, body := range bodies {
			for _, want := range []string{
				"source=official-db(本地投影)",
				"id=c-running", "id=c-signup",
				"状态=", "开始=", "预计结束=",
				"registered=false", "capability=unavailable", "资格=unknown",
				"event_snapshot=unknown(source_unavailable)",
				"不调用懒结算", "不注册事件", "不写官方表",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("请求正文缺 %q", want)
				}
			}
			if strings.Contains(body, "finished") {
				t.Fatal("请求正文出现 finished 字样（不得声称官方已结束）")
			}
			// 同一任务的所有发言者共用同一份快照：采样时刻必须一致。
			if got := t108Between(body, "sampled_at=", "）"); got != sampled {
				t.Fatalf("圆桌发言者用了不同快照：%q vs %q", sampled, got)
			}
			// 报名中不计入进行中：两条投影里只有一条 running。
			if !strings.Contains(body, "本地赛事 2 条，进行中 1") {
				t.Fatalf("进行中计数不符（报名中不应计入）")
			}
		}
		t.Logf("E5 task=%d snapshots=%d speakers=%d sampled_at=%s 赛事=2（报名中1/进行中1，stale=true）",
			task.ID, snapshots, len(bodies), sampled)
	})
}

// expressionFragment 取编译后表达约束的第一行（不含换行、含 schema 与修订号）。
//
// E3 用它做「独立预期片段」：同一组固定输入 + 已知保存修订，由纯函数重新编译，
// 不从运行日志里抄。
func expressionFragment(cfg aiconfig.PersonalityConfig) string {
	line := aiconfig.CompilePersonalityExpression(cfg)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// tasksAfter 断言任务总数达到下一轮预期（公聊入口本身不产生任务）。
func (rt *t108EvidenceRuntime) tasksAfter(t *testing.T, want int) {
	t.Helper()
	tasks := t108Tasks(t, rt.db)
	if len(tasks) != want {
		t.Fatalf("任务总数 = %d，期望 %d：%+v", len(tasks), want, tasks)
	}
}

// onlyTaskWithKind 取该类型的最新一条任务。
func (rt *t108EvidenceRuntime) onlyTaskWithKind(t *testing.T, kind string) t108TaskRow {
	t.Helper()
	var out *t108TaskRow
	for _, task := range t108Tasks(t, rt.db) {
		if task.Kind == kind {
			row := task
			out = &row
		}
	}
	if out == nil {
		t.Fatalf("没有 %s 任务", kind)
	}
	return *out
}

// lastBody 返回最近一次厂商请求正文（脱敏：只用于查预期片段）。
func (v *t108EvidenceVendor) lastBody(t *testing.T) string {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.bodies) == 0 {
		t.Fatal("厂商没有收到请求正文")
	}
	return v.bodies[len(v.bodies)-1]
}

// setReply 换掉后续请求的模型回复（同一计数桩，不同尝试给出可区分的句子）。
func (v *t108EvidenceVendor) setReply(s string) {
	v.mu.Lock()
	v.reply = s
	v.mu.Unlock()
}

// replyText 读当前回复（加锁，避免与请求 goroutine 竞争）。
func (v *t108EvidenceVendor) replyText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.reply
}

// 记录请求正文：供 E3 的「请求预期片段」断言使用。
func (v *t108EvidenceVendor) capture(body []byte) {
	v.mu.Lock()
	v.bodies = append(v.bodies, string(body))
	v.mu.Unlock()
}

// allBodies 返回本轮全部厂商请求正文（脱敏：用于查预算/事实片段）。
func (v *t108EvidenceVendor) allBodies(t *testing.T) []string {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, len(v.bodies))
	copy(out, v.bodies)
	return out
}

// t108Between 取 s 中 start 与紧随其后的 end 之间的片段；找不到返回空串。
func t108Between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// ---------------------------------------------------------------------------
// T-108-DoD-v3 / E4：重领迟到（store 入口运行证据）
// ---------------------------------------------------------------------------

// t108LateTask 入队一条已过期、可被回收的 DM 任务并返回 id。
func t108LateTask(t *testing.T, db *store.DB, text string) int64 {
	t.Helper()
	id, err := db.AIEnqueue(context.Background(), &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: t013SelfUID,
		Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: text,
		DeadlineTS: float64(time.Now().Unix()) - 10,
	})
	if err != nil {
		t.Fatalf("入队: %v", err)
	}
	return id
}

// t108Reclaim 用真实 AIExpireTasks 回收 processing 任务。
func t108Reclaim(t *testing.T, db *store.DB, now float64) (int64, int64) {
	t.Helper()
	expired, recovered, err := db.AIExpireTasks(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("AIExpireTasks: %v", err)
	}
	return expired, recovered
}

// t108LateTaskState 读任务的 state/attempt 与结果行数。
func t108LateTaskState(t *testing.T, db *store.DB, taskID int64) (string, int, int) {
	t.Helper()
	var state string
	var attempt, results int
	if err := db.SQL.QueryRow(`SELECT state, attempt FROM ai_task WHERE id=?`, taskID).
		Scan(&state, &attempt); err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_result WHERE task_id=?`, taskID).
		Scan(&results); err != nil {
		t.Fatalf("读结果: %v", err)
	}
	return state, attempt, results
}

// TestT108LateResultAfterReclaim 覆盖 E4 的五个子例：
//
//	LateSingle               迟到单条
//	LateRoundtableSpeaker    圆桌迟到发言（回收时旧结果置终态）
//	SameRevisionOtherAgent   同号异角色（修订不变，靠 attempt 拒绝）
//	RebuiltAgentGeneration   角色重建后旧尝试仍被拒
//	ReclaimTwice             连续两轮回收
//
// 断言：旧尝试的迟到写入返回 ErrAITaskNotActive、不写结果、不改任务状态；
// 新尝试照常写入；回收时未投递的旧结果被同事务置终态且不再进入待投递集合。
func TestT108LateResultAfterReclaim(t *testing.T) {
	ctx := context.Background()

	t.Run("LateSingle", func(t *testing.T) {
		db := openStaticTestDB(t)
		id := t108LateTask(t, db, "第一句")
		first, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || first.ID != id || first.Attempt != 1 {
			t.Fatalf("第一次认领 = %+v ok=%v err=%v，期望 attempt=1", first, ok, err)
		}
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("回收计数 = %d，期望 1", recovered)
		}
		second, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || second.ID != id || second.Attempt != 2 {
			t.Fatalf("第二次认领 = %+v ok=%v err=%v，期望 attempt=2", second, ok, err)
		}
		err = db.AICompleteWithExpression(ctx, id, first.Attempt, "done", "迟到的一句", "", "", nil)
		state, attempt, results := t108LateTaskState(t, db, id)
		if state != "processing" || attempt != 2 || results != 0 {
			t.Fatalf("state=%s attempt=%d results=%d，期望 processing/2/0（旧尝试迟到写入不得生效）",
				state, attempt, results)
		}
		if !errors.Is(err, store.ErrAITaskNotActive) {
			t.Fatalf("旧尝试迟到写入 = %v，期望 ErrAITaskNotActive", err)
		}
		if err := db.AICompleteWithExpression(ctx, id, second.Attempt, "done", "新尝试的一句", "", "", nil); err != nil {
			t.Fatalf("新尝试写入: %v", err)
		}
		if _, _, results = t108LateTaskState(t, db, id); results != 1 {
			t.Fatalf("新尝试应写出一行结果，实得 %d", results)
		}
		t.Logf("E4 LateSingle task=%d 旧尝试写入 err=%v state=processing attempt=2 results=0；新尝试 results=1",
			id, err)
	})

	t.Run("LateRoundtableSpeaker", func(t *testing.T) {
		db := openStaticTestDB(t)
		id, err := db.AIEnqueueSystemRoundtable(ctx, "t108_late_rt", t013SelfUID, "大家好",
			[]string{"companion", "fishguide"}, nil)
		if err != nil {
			t.Fatalf("入队圆桌: %v", err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
			float64(time.Now().Unix())-10, id); err != nil {
			t.Fatal(err)
		}
		first, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || first.Attempt != 1 {
			t.Fatalf("第一次认领: %+v ok=%v err=%v", first, ok, err)
		}
		// 首轮发言已写、尚未投递，此时 task 被回收。
		if err := db.AIAddChannelResultWithExpression(ctx, id, first.Attempt, 2147483645, "旧尝试的一句", nil); err != nil {
			t.Fatalf("写首轮发言: %v", err)
		}
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("回收计数 = %d，期望 1", recovered)
		}
		var delivered float64
		var errText, reply string
		if err := db.SQL.QueryRow(`SELECT COALESCE(delivered_ts,0), err, reply_text FROM ai_result WHERE task_id=?`, id).
			Scan(&delivered, &errText, &reply); err != nil {
			t.Fatal(err)
		}
		if delivered != -1 || errText != store.AIEvidenceReasonAttemptReclaimed || reply != "" {
			t.Fatalf("旧尝试结果仍在待投递集合: delivered=%v err=%q reply=%q", delivered, errText, reply)
		}
		pending, err := db.AIPendingResults(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pending {
			if r.TaskID == id {
				t.Fatalf("回收后旧结果仍出现在待投递集合: %+v", r)
			}
		}
		second, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || second.Attempt != 2 {
			t.Fatalf("第二次认领: %+v ok=%v err=%v", second, ok, err)
		}
		if err := db.AIAddChannelResultWithExpression(ctx, id, first.Attempt, 2147483645, "旧尝试迟到", nil); !errors.Is(err, store.ErrAITaskNotActive) {
			t.Fatalf("旧尝试迟到发言 = %v，期望 ErrAITaskNotActive", err)
		}
		if err := db.AIAddChannelResultWithExpression(ctx, id, second.Attempt, 2147483646, "新尝试的一句", nil); err != nil {
			t.Fatalf("新尝试发言: %v", err)
		}
		t.Logf("E4 LateRoundtableSpeaker task=%d 旧结果终态 delivered=-1 err=%q；新尝试写入成功",
			id, store.AIEvidenceReasonAttemptReclaimed)
	})

	t.Run("SameRevisionOtherAgent", func(t *testing.T) {
		db := openStaticTestDB(t)
		id, err := db.AIEnqueueSystemRoundtable(ctx, "t108_late_same", t013SelfUID, "大家好",
			[]string{"companion", "fishguide"}, nil)
		if err != nil {
			t.Fatalf("入队圆桌: %v", err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
			float64(time.Now().Unix())-10, id); err != nil {
			t.Fatal(err)
		}
		first, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok {
			t.Fatalf("第一次认领: %+v ok=%v err=%v", first, ok, err)
		}
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("回收计数 = %d，期望 1", recovered)
		}
		second, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || second.Attempt != 2 {
			t.Fatalf("第二次认领: %+v ok=%v err=%v", second, ok, err)
		}
		// 同一角色、同一代次与修订：能力/版本都没变，只有 attempt 不同。
		err = db.AIAddChannelResultWithExpression(ctx, id, first.Attempt, 2147483645, "同号旧尝试", nil)
		if !errors.Is(err, store.ErrAITaskNotActive) {
			t.Fatalf("同号异尝试的迟到发言 = %v，期望 ErrAITaskNotActive", err)
		}
		// 新尝试换另一个角色发言：只有 attempt 对得上才写。
		if err := db.AIAddChannelResultWithExpression(ctx, id, second.Attempt, 2147483646, "换人新尝试", nil); err != nil {
			t.Fatalf("新尝试发言: %v", err)
		}
		if _, _, results := t108LateTaskState(t, db, id); results != 1 {
			t.Fatalf("应只有新尝试的一行结果，实得 %d", results)
		}
		t.Logf("E4 SameRevisionOtherAgent task=%d 修订不变时仍按 attempt 拒绝（err=%v）", id, err)
	})

	t.Run("RebuiltAgentGeneration", func(t *testing.T) {
		db := openStaticTestDB(t)
		id, err := db.AIEnqueueSystemRoundtable(ctx, "t108_late_rebuilt", t013SelfUID, "大家好",
			[]string{"companion", "fishguide"}, nil)
		if err != nil {
			t.Fatalf("入队圆桌: %v", err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
			float64(time.Now().Unix())-10, id); err != nil {
			t.Fatal(err)
		}
		first, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok {
			t.Fatalf("第一次认领: %+v ok=%v err=%v", first, ok, err)
		}
		// 角色重建：逻辑删除 → 同 key 重建（恢复历史与 UID）→ 代次 +1。
		if err := db.AIDeleteAgent(ctx, "companion"); err != nil {
			t.Fatalf("删除角色: %v", err)
		}
		if _, _, err := db.AICreateAgent(ctx, store.AIAgent{
			Key: "companion", Display: "AI 伙伴", Role: "陪聊", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("重建角色: %v", err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_runtime_control SET generation=generation+1 WHERE agent_key='companion'`); err != nil {
			t.Fatal(err)
		}
		var gen int64
		if err := db.SQL.QueryRow(`SELECT generation FROM ai_runtime_control WHERE agent_key='companion'`).Scan(&gen); err != nil {
			t.Fatal(err)
		}
		if gen < 2 {
			t.Fatalf("重建后代次 = %d，期望 >=2", gen)
		}
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("回收计数 = %d，期望 1", recovered)
		}
		second, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || second.Attempt != 2 {
			t.Fatalf("第二次认领: %+v ok=%v err=%v", second, ok, err)
		}
		err = db.AIAddChannelResultWithExpression(ctx, id, first.Attempt, 2147483645, "重建后旧尝试", nil)
		if !errors.Is(err, store.ErrAITaskNotActive) {
			t.Fatalf("角色重建后旧尝试迟到发言 = %v，期望 ErrAITaskNotActive", err)
		}
		if err := db.AIAddChannelResultWithExpression(ctx, id, second.Attempt, 2147483645, "重建后新尝试", nil); err != nil {
			t.Fatalf("新尝试发言: %v", err)
		}
		t.Logf("E4 RebuiltAgentGeneration task=%d 代次=%d 旧尝试仍被拒（err=%v）", id, gen, err)
	})

	t.Run("ReclaimTwice", func(t *testing.T) {
		db := openStaticTestDB(t)
		id := t108LateTask(t, db, "连续回收")
		first, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || first.Attempt != 1 {
			t.Fatalf("第一次认领: %+v ok=%v err=%v", first, ok, err)
		}
		// 第一轮回收：退回 pending。
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("第一轮回收 = %d，期望 1", recovered)
		}
		second, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || second.Attempt != 2 {
			t.Fatalf("第二次认领: %+v ok=%v err=%v", second, ok, err)
		}
		// 第二轮回收：既有策略在 attempt>=2 时按 fallback 终结，不再重试。
		if _, recovered := t108Reclaim(t, db, float64(time.Now().Unix())+200); recovered != 0 {
			t.Fatalf("第二轮不应再回收: recovered=%d", recovered)
		}
		state, attempt, results := t108LateTaskState(t, db, id)
		if state != "fallback" || attempt != 2 || results != 0 {
			t.Fatalf("第二轮应终结为 fallback: state=%s attempt=%d results=%d", state, attempt, results)
		}
		// 两次旧尝试的迟到写入都不得成功。
		for _, late := range []int{first.Attempt, second.Attempt} {
			if err := db.AICompleteWithExpression(ctx, id, late, "done", "迟到", "", "", nil); !errors.Is(err, store.ErrAITaskNotActive) {
				t.Fatalf("attempt=%d 的迟到写入 = %v，期望 ErrAITaskNotActive", late, err)
			}
		}
		if _, _, results = t108LateTaskState(t, db, id); results != 0 {
			t.Fatalf("终结后不得写出任何结果，实得 %d", results)
		}
		t.Logf("E4 ReclaimTwice task=%d 第一轮回收→attempt=2，第二轮按既有上限终结 fallback；两次迟到写入均被拒",
			id)
	})
}

// ---------------------------------------------------------------------------
// T-108-DoD-v3 / E4：最终发前复核（R-157）
// ---------------------------------------------------------------------------

// TestT108ReclaimAtFinalPush 覆盖 R-157 的最终发前复核时序：
//
// 真实圆桌入队 → 第一次认领 → 写一条待发结果 → GS 轮询把正文读入内存 →
// 在既有最终发送屏障处走真实 AIExpireTasks 回收并重领同一任务到 attempt 2 →
// 释放屏障。此时旧结果必须已落 delivered_ts=-1 / err=attempt_reclaimed / 正文空，
// 释放仍是 0 新帧，且迟到的成功标记不得覆盖终态。随后第二次尝试正常写入与
// 投递作为对照：旧句不补发、无重复发送，新句只到一次。
func TestT108ReclaimAtFinalPush(t *testing.T) {
	t108ReclaimAtFinalPush(t, "t108_ev_final_push", "before-authorization")
}

func TestT108ReclaimAndChannelPushOrdering(t *testing.T) {
	t.Run("ReclaimAfterAuthorization", func(t *testing.T) {
		t108ReclaimAtFinalPush(t, "t108_ev_reclaim_first", "after-authorization")
	})
	t.Run("SendFirst", func(t *testing.T) {
		t108ReclaimAtFinalPush(t, "t108_ev_send_first", "send-first")
	})
}

func t108ReclaimAtFinalPush(t *testing.T, channel, order string) {
	t.Helper()
	ctx := context.Background()
	rt := newT108EvidenceRuntime(t, channel, "第二次尝试的一句")
	rt.probe.sess.Token = "synthetic-t108-token"
	for _, key := range []string{"companion", "fishguide"} {
		rt.addAgent(key, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, rt.db, key, 90, 60, 40, 50)
	}
	seed := int64(0)
	for s := int64(1); s <= 4096 && seed == 0; s++ {
		for _, key := range []string{"companion", "fishguide"} {
			p, _ := rt.db.AIGetPersonality(rt.ctx, key)
			if want, _ := activityWants(p.Config, s^store.AIActorSeed(0, key), activityGatherFeature()); want {
				seed = s
				break
			}
		}
	}
	if seed == 0 {
		t.Fatal("找不到愿意起桌的种子")
	}
	t108SeedFixed(t, seed)
	rt.probe.send(t, "你们今天钓得怎么样")
	rt.drainBroadcast("你们今天钓得怎么样")
	task := rt.onlyTask()
	if task.Kind != "roundtable" {
		t.Fatalf("应入队圆桌，实得 %s", task.Kind)
	}

	// 第一次认领（真实 AIPopTask），用圆桌逐句写入入口写一条待发结果，任务保持 processing。
	first, ok, err := rt.db.AIPopTask(ctx)
	if err != nil || !ok || first.ID != task.ID || first.Attempt != 1 {
		t.Fatalf("第一次认领 = %+v ok=%v err=%v，期望 id=%d attempt=1", first, ok, err, task.ID)
	}
	persona, err := rt.db.AIGetPersonality(rt.ctx, "companion")
	if err != nil {
		t.Fatalf("读 companion 四维: %v", err)
	}
	const oldText = "这句话不该发"
	if err := rt.db.AIAddChannelResultWithExpression(ctx, task.ID, first.Attempt, 2147483645,
		oldText, &store.AIResultExpression{
			AgentKey: "companion", Generation: 1,
			BRPersonalityRevision: persona.Config.Revision,
		}); err != nil {
		t.Fatalf("写待发结果: %v", err)
	}
	beforePoll := t108EvidenceResults(t, rt.db, task.ID)
	if len(beforePoll) != 1 || beforePoll[0].Reply != oldText {
		t.Fatalf("应有一条待投递旧结果: %+v", beforePoll)
	}
	oldID := beforePoll[0].ID

	// 让这条 processing 任务进入可回收窗口：deadline 推早，回收上限仍是 attempt<2。
	if _, err := rt.db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
		float64(time.Now().Unix())-10, task.ID); err != nil {
		t.Fatalf("推早 deadline: %v", err)
	}

	// 独立连接代表 BR 的回收和重领；GS 继续使用运行夹具自己的连接。
	reclaimer, err := store.Open(rt.db.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reclaimer.Close() })
	var second *store.AITask
	reclaim := func() (*store.AITask, error) {
		if _, recovered, err := reclaimer.AIExpireTasks(ctx, float64(time.Now().Unix()), 0); err != nil || recovered != 1 {
			return nil, fmt.Errorf("真实回收: recovered=%d err=%v", recovered, err)
		}
		claimed, ok, err := reclaimer.AIPopTask(ctx)
		if err != nil || !ok || claimed.ID != task.ID || claimed.Attempt != 2 || claimed.State != "processing" {
			return nil, fmt.Errorf("真实重领: task=%+v ok=%v err=%v，期望同一任务 attempt=2 processing", claimed, ok, err)
		}
		return claimed, nil
	}
	assertOld := func() t108EvidenceResult {
		t.Helper()
		old := t108ResultByID(t, t108EvidenceResults(t, rt.db, task.ID), oldID)
		if order == "send-first" {
			if old.Delivered <= 0 || old.Err != "" || old.Reply != "" {
				t.Fatalf("已发结果被回收或未清正文: %+v", old)
			}
		} else if old.Delivered != -1 || old.Err != store.AIEvidenceReasonAttemptReclaimed || old.Reply != "" {
			t.Fatalf("旧结果回收终态应保持: %+v", old)
		}
		return old
	}

	oldFrames := 0
	if order == "send-first" {
		type reclaimOutcome struct {
			task *store.AITask
			err  error
		}
		started := make(chan struct{})
		outcome := make(chan reclaimOutcome, 1)
		previous := aiChannelWriteForTest
		t.Cleanup(func() { aiChannelWriteForTest = previous })
		aiChannelWriteForTest = func() {
			aiChannelWriteForTest = previous
			go func() {
				close(started)
				claimed, err := reclaim()
				outcome <- reclaimOutcome{claimed, err}
			}()
			<-started
			select {
			case early := <-outcome:
				t.Errorf("受控发帧前回收已越过共同锁：task=%+v err=%v", early.task, early.err)
				outcome <- early
			case <-time.After(100 * time.Millisecond):
				t.Log("发帧暂停期间，独立连接上的回收仍在等待")
			}
		}
		rt.poll()
		rt.expectFrame(oldText)
		oldFrames++
		select {
		case reclaimed := <-outcome:
			if reclaimed.err != nil {
				t.Fatal(reclaimed.err)
			}
			second = reclaimed.task
		case <-time.After(5 * time.Second):
			t.Fatal("发帧结束后回收未完成")
		}
	} else {
		barrier := &aiDeliveryBarrierForTest
		if order == "after-authorization" {
			barrier = &aiChannelAuthorizedForTest
		}
		previous := *barrier
		t.Cleanup(func() { *barrier = previous })
		*barrier = func() {
			*barrier = previous
			if order == "after-authorization" {
				// R-158 的同名探针插在实际授权后。修复后在受保护授权之前
				// 确认这份原有只读授权仍会放行，随后才让回收赢得实际顺序。
				auth, err := rt.db.AIDeliveryAuthorization(ctx, oldID)
				if err != nil || !auth.Allowed || !auth.ResultPending {
					t.Fatalf("回收前的 R-158 授权应放行: %+v err=%v", auth, err)
				}
			}
			var err error
			second, err = reclaim()
			if err != nil {
				t.Fatal(err)
			}
			old := assertOld()
			t.Logf("释放发送前：task=%d attempt=%d result=%d delivered=%v err=%q",
				task.ID, second.Attempt, old.ID, old.Delivered, old.Err)
		}
		rt.poll()
		rt.expectNoFrame()
		if second == nil {
			t.Fatal("发送没有经过指定屏障")
		}
		if err := rt.db.AIMarkDelivered(ctx, oldID, true); err != nil {
			t.Fatal(err)
		}
	}
	old := assertOld()
	rt.poll()
	rt.expectNoFrame()
	t.Logf("order=%s task=%d attempt=%d→%d task_state=%s old_result=%d status=%s delivered=%v err=%q frames=%d；回收后重轮询补发=0",
		order, task.ID, first.Attempt, second.Attempt, second.State, old.ID, old.Status, old.Delivered, old.Err, oldFrames)

	// 对照：第二次尝试按真实 BR 正常写入，再由 GS 正常投递；旧句不得补发。
	rt.vendor.setReply("第二次尝试的一句")
	rt.runner.HandleTaskForTest(ctx, second)
	newResults := t108EvidenceResults(t, rt.db, task.ID)
	if len(newResults) != 2 {
		t.Fatalf("第二次尝试应再写一条结果（共 2 行），实得 %d: %+v", len(newResults), newResults)
	}
	rt.poll()
	rec := rt.expectFrame("第二次尝试的一句")
	if strings.Contains(rec.text, oldText) {
		t.Fatalf("新帧混入了旧句: %q", rec.text)
	}
	// 再轮询一次：旧句不补发、新句不重复。
	rt.poll()
	rt.expectNoFrame()

	after := t108EvidenceResults(t, rt.db, task.ID)
	if current := assertOld(); current != old {
		t.Fatalf("旧结果在对照流程后被改写: before=%+v after=%+v", old, current)
	}
	freshID := int64(0)
	deliveredNew := 0
	for _, r := range after {
		if r.ID == oldID {
			continue
		}
		freshID = r.ID
		if r.Delivered >= 0 {
			deliveredNew++
		}
	}
	if freshID == 0 || deliveredNew != 1 {
		t.Fatalf("新尝试应恰好投递一次: results=%+v", after)
	}
	var state string
	var attempt int
	if err := rt.db.SQL.QueryRow(`SELECT state, attempt FROM ai_task WHERE id=?`, task.ID).
		Scan(&state, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != "done" || attempt != 2 {
		t.Fatalf("第二次尝试终态 = %s/attempt=%d，期望 done/2", state, attempt)
	}
	fresh := t108ResultByID(t, after, freshID)
	t.Logf("task=%d state=%s attempt=%d new_result=%d status=%s delivered=%v err=%q frames=1；再次轮询补发=0",
		task.ID, state, attempt, freshID, fresh.Status, fresh.Delivered, fresh.Err)
}

// t108ResultByID 在结果列表里按 ID 取一条。
func t108ResultByID(t *testing.T, list []t108EvidenceResult, id int64) t108EvidenceResult {
	t.Helper()
	for _, r := range list {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("没有结果 %d：%+v", id, list)
	return t108EvidenceResult{}
}

// agentReport 读一次「按角色当前版本」的只读投影。
func (rt *t108EvidenceRuntime) agentReport(key string) store.AIAgentEvidenceReport {
	rt.t.Helper()
	rep, err := rt.db.AIAgentEvidenceReport(rt.ctx, key)
	if err != nil {
		rt.t.Fatalf("读按角色证据 %s: %v", key, err)
	}
	return rep
}

// TestT108AgentEvidenceCurrentVersion 覆盖 R-157 的按角色同版只读交接：
//
// 用真实 GS 选择（玩家上星链）与真实 BR 正常表达点亮当前版，并记录真实的
// task/result ID；保存 r+1 后旧证据、其他角色同号修订、重建同名角色与已回收
// 结果都不得点亮当前版。AITaskEvidenceReport 的按任务语义不变（既有用例继续）。
func TestT108AgentEvidenceCurrentVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("MatchedOnRealSelection", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_agent_ok", "这句话进了频道")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revP := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))

		rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		rt.processOne()
		rt.poll()
		rt.expectFrame("这句话进了频道")
		results := t108EvidenceResults(t, rt.db, task.ID)
		if len(results) != 1 || results[0].Delivered < 0 || !results[0].HasExpr {
			t.Fatalf("真实链应落一条已投递的同版结果: %+v", results)
		}

		rep := rt.agentReport("companion")
		if !rep.AgentFound || rep.Generation != 1 || !rep.PersonalityPresent ||
			rep.CurrentRevision != revP || !rep.Matched || rep.Reason != store.AIEvidenceReasonMatched {
			t.Fatalf("真实 GS 选择 + BR 表达应点亮当前版: %+v", rep)
		}
		if rep.Evidence.TaskID != task.ID || rep.Evidence.ResultID != results[0].ID ||
			!rep.Evidence.Delivered || rep.Evidence.Generation != 1 ||
			rep.Evidence.BRRevision != revP || rep.Evidence.GSRevision == nil ||
			*rep.Evidence.GSRevision != revP {
			t.Fatalf("点亮证据的 task/result/版本不符: %+v", rep)
		}

		// 同角色保存 r+1：旧证据立即回到等待，但最近一条仍显示旧版。
		revP2 := t108SavePersonality(t, rt.db, "companion", 20, 80, 30, 70)
		rep = rt.agentReport("companion")
		if rep.Matched || rep.CurrentRevision != revP2 ||
			rep.Reason != store.AITaskEvidenceReasonPersonaDiff ||
			rep.Evidence.ResultID != 0 || rep.Latest.ResultID != results[0].ID ||
			rep.Latest.BRRevision != revP || rep.Latest.GSRevision == nil ||
			*rep.Latest.GSRevision != revP {
			t.Fatalf("保存 r+1 后旧证据不得点亮当前版: %+v", rep)
		}

		// 新任务真正按 r+1 运行后才生效。
		aiChannelCooldown.Delete(rt.channel + "|catch")
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
		fresh := rt.onlyTaskWithKind(t, "channel")
		if fresh.ID == task.ID {
			t.Fatal("新任务应另起一行")
		}
		rt.processOne()
		rt.poll()
		rt.expectFrame("这句话进了频道")
		freshResults := t108EvidenceResults(t, rt.db, fresh.ID)
		if len(freshResults) != 1 {
			t.Fatalf("新任务应落一条结果: %+v", freshResults)
		}
		rep = rt.agentReport("companion")
		if !rep.Matched || rep.CurrentRevision != revP2 ||
			rep.Evidence.TaskID != fresh.ID || rep.Evidence.ResultID != freshResults[0].ID ||
			rep.Evidence.BRRevision != revP2 || rep.Evidence.GSRevision == nil ||
			*rep.Evidence.GSRevision != revP2 {
			t.Fatalf("按 r+1 运行的新任务应点亮新版: %+v", rep)
		}
		t.Logf("R-157 agent evidence agent=companion gen=1 task=%d result=%d rev=%d matched=true；保存 r+1=%d 后等待；新任务 task=%d result=%d 点亮 r+1",
			task.ID, results[0].ID, revP, revP2, fresh.ID, freshResults[0].ID)
	})

	t.Run("OtherAgentSameRevision", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_agent_other", "换个人说")
		// 只让 fishguide 具备能力并产生证据。
		rt.addAgent("fishguide", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revF := t108SavePersonality(t, rt.db, "fishguide", 12, 88, 30, 70)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "fishguide", activityReplyFeature(3),
			store.AIActorSeed(0, "fishguide")))
		rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		if task.Agent != "fishguide" {
			t.Fatalf("证据应由 fishguide 产生，实得 %s", task.Agent)
		}
		rt.processOne()
		rt.poll()
		rt.expectFrame("换个人说")

		// companion 保存到同一个数字修订，但没有任何自己的证据。
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revC := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
		if revC != revF {
			t.Fatalf("两角色应保存到同一修订号: %d/%d", revC, revF)
		}
		repC := rt.agentReport("companion")
		if repC.Matched || repC.CurrentRevision != revC ||
			repC.Reason != store.AITaskEvidenceReasonNoExpression ||
			repC.Evidence.ResultID != 0 {
			t.Fatalf("其他角色同号修订不得点亮本角色: %+v", repC)
		}
		repF := rt.agentReport("fishguide")
		if !repF.Matched || repF.Evidence.TaskID != task.ID {
			t.Fatalf("证据归属角色应正常点亮: %+v", repF)
		}
		t.Logf("R-157 other-agent revision=%d companion matched=false reason=%q；fishguide task=%d matched=true",
			revC, repC.Reason, task.ID)
	})

	t.Run("RebuiltAgentGeneration", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_agent_rebuilt", "重建前的一句")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revP := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		rt.processOne()
		rt.poll()
		rt.expectFrame("重建前的一句")
		if rep := rt.agentReport("companion"); !rep.Matched || rep.Generation != 1 {
			t.Fatalf("重建前应点亮: %+v", rep)
		}

		// 逻辑删除 → 同 key 重建 → 代次 +1（Role 重建的真实语义：旧代次证据不再生效）。
		if err := rt.db.AIDeleteAgent(ctx, "companion"); err != nil {
			t.Fatalf("删除角色: %v", err)
		}
		if _, _, err := rt.db.AICreateAgent(ctx, store.AIAgent{
			Key: "companion", Display: "AI 伙伴", Role: "陪聊", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("重建角色: %v", err)
		}
		if _, err := rt.db.SQL.Exec(
			`UPDATE ai_runtime_control SET generation=generation+1 WHERE agent_key='companion'`); err != nil {
			t.Fatal(err)
		}
		rep := rt.agentReport("companion")
		if !rep.AgentFound || rep.Generation != 2 || rep.CurrentRevision != revP ||
			rep.Matched || rep.Reason != store.AITaskEvidenceReasonGeneration ||
			rep.Latest.TaskID != task.ID || rep.Latest.Generation != 1 ||
			rep.Latest.GSRevision == nil || *rep.Latest.GSRevision != revP {
			t.Fatalf("重建同名角色后旧代次证据不得点亮: %+v", rep)
		}
		t.Logf("R-157 rebuilt agent generation 1→2 task=%d rev=%d matched=false reason=%q",
			task.ID, revP, rep.Reason)
	})

	t.Run("ReclaimedResultNotEffective", func(t *testing.T) {
		rt := newT108EvidenceRuntime(t, "t108_ev_agent_reclaimed", "不该点亮")
		rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		revP := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
		t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
			store.AIActorSeed(0, "companion")))
		rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
		task := rt.onlyTask()
		first, ok, err := rt.db.AIPopTask(ctx)
		if err != nil || !ok || first.ID != task.ID || first.Attempt != 1 {
			t.Fatalf("认领: %+v ok=%v err=%v", first, ok, err)
		}
		if err := rt.db.AIAddChannelResultWithExpression(ctx, task.ID, first.Attempt, 2147483645,
			"回收前写的一句", &store.AIResultExpression{
				AgentKey: "companion", Generation: 1, BRPersonalityRevision: revP,
			}); err != nil {
			t.Fatalf("写待发结果: %v", err)
		}
		resultID := t108EvidenceResults(t, rt.db, task.ID)[0].ID
		if _, err := rt.db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
			float64(time.Now().Unix())-10, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, recovered := t108Reclaim(t, rt.db, float64(time.Now().Unix())); recovered != 1 {
			t.Fatalf("回收计数 = %d，期望 1", recovered)
		}
		rep := rt.agentReport("companion")
		if rep.Matched || rep.Reason != store.AIEvidenceReasonAttemptReclaimed ||
			rep.Latest.ResultID != resultID || rep.Latest.Delivered ||
			rep.Latest.BRRevision != revP || rep.Latest.GSRevision == nil ||
			*rep.Latest.GSRevision != revP {
			t.Fatalf("已回收结果不得点亮当前版: %+v", rep)
		}
		t.Logf("R-157 reclaimed agent=companion task=%d result=%d rev=%d matched=false reason=%q",
			task.ID, resultID, revP, rep.Reason)
	})
}

// TestT108AgentEvidenceFixtureDB 仅当 T108_EVIDENCE_DB 指向一个尚不存在的路径时运行：
// 用真实 GS 选择（玩家上星链）与真实 BR 表达把一份「同版运行」写进保留在磁盘上的
// 夹具库，供 T-107 的 P4 管理 GET 直接消费。库内只有合成角色、合成厂商与测试玩家，
// 不含真实凭据；生成后不再改写，也不写产品树。
func TestT108AgentEvidenceFixtureDB(t *testing.T) {
	path := os.Getenv("T108_EVIDENCE_DB")
	if path == "" {
		t.Skip("未指定 T108_EVIDENCE_DB（同版运行夹具库只在交接取证时生成）")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("解析库路径: %v", err)
	}
	if _, err := os.Stat(abs); err == nil {
		t.Fatalf("目标库已存在（不覆盖既有证据）: %s", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	template, err := testDBTemplatePath(testDBMigrated)
	if err != nil {
		t.Fatalf("准备迁移模板: %v", err)
	}
	copyTestDBFile(t, template, abs)
	db := openStaticDBAtPath(t, abs)

	rt := newT108EvidenceRuntimeWithDB(t, "t108_ev_fixture", "这句话进了频道", db)
	rt.addAgent("companion", aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
	revP := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
	t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
		store.AIActorSeed(0, "companion")))
	rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
	task := rt.onlyTask()
	rt.processOne()
	rt.poll()
	rt.expectFrame("这句话进了频道")
	results := t108EvidenceResults(t, rt.db, task.ID)
	if len(results) != 1 || results[0].Delivered < 0 || !results[0].HasExpr {
		t.Fatalf("夹具库应落一条已投递的同版结果: %+v", results)
	}
	rep := rt.agentReport("companion")
	if !rep.Matched || rep.Evidence.TaskID != task.ID || rep.Evidence.ResultID != results[0].ID ||
		rep.CurrentRevision != revP || rep.Evidence.BRRevision != revP ||
		rep.Evidence.GSRevision == nil || *rep.Evidence.GSRevision != revP {
		t.Fatalf("夹具库的按角色报告不符: %+v", rep)
	}
	t.Logf("T108_EVIDENCE_DB=%s agent=companion generation=%d revision=%d task=%d result=%d channel=%s matched=true",
		abs, rep.Generation, rep.CurrentRevision, task.ID, results[0].ID, rt.channel)
}

func TestT108AgentEvidenceSurvivesPrivateReplies(t *testing.T) {
	rt := newT108EvidenceRuntime(t, "t108_ev_dm_history", "同版频道消息")
	rt.addAgent("companion", aiconfig.CapabilityReplyStar)
	revision := t108SavePersonality(t, rt.db, "companion", 88, 12, 64, 31)
	t108SeedFixed(t, t108WantSeed(t, rt.db, "companion", activityReplyFeature(3),
		store.AIActorSeed(0, "companion")))
	rt.srv.maybeAIPlayerCatch(rt.ctx, t013SelfUID, playerStarFish())
	original := rt.processOne()
	rt.poll()
	rt.expectFrame("同版频道消息")
	before := rt.agentReport("companion")
	if !before.Matched || before.CurrentRevision != revision || before.Evidence.TaskID != original.ID ||
		before.Evidence.GSRevision == nil || *before.Evidence.GSRevision != revision ||
		before.Evidence.BRRevision != revision || !before.Evidence.Delivered {
		t.Fatalf("频道正例未点亮: %+v", before)
	}
	resultID := before.Evidence.ResultID

	rt.vendor.setReply("正常私聊回复")
	private := &Ctx{Sess: rt.probe.sess, ExpectResponse: true}
	var newestTask, newestResult int64
	for i := 1; i <= 32; i++ {
		if _, ok := rt.srv.receiveAIChat(private, original.ContactUID, "companion", fmt.Sprintf("私聊 %d", i)); !ok {
			t.Fatalf("私聊 %d 未受理", i)
		}
		task := rt.processOne()
		results := t108EvidenceResults(t, rt.db, task.ID)
		if task.Kind != "dm" || task.Attempt != 1 || len(results) != 1 || results[0].Status != "done" ||
			!results[0].HasExpr || results[0].BRRevision != revision {
			t.Fatalf("私聊 %d 未生成正常表达：task=%+v results=%+v", i, task, results)
		}
		for _, bound := range rt.evidence(task.ID).Participants {
			if bound.GSPersonalityRevision != nil {
				t.Fatalf("私聊 %d 不应使用四维做 GS 选择: %+v", i, bound)
			}
		}
		newestTask, newestResult = task.ID, results[0].ID
	}
	if rt.vendor.calls() != 33 {
		t.Fatalf("厂商调用=%d，期望频道 1 + 私聊 32", rt.vendor.calls())
	}
	if originalResults := t108EvidenceResults(t, rt.db, original.ID); len(originalResults) != 1 ||
		originalResults[0].ID != resultID || !originalResults[0].HasExpr || originalResults[0].Delivered <= 0 {
		t.Fatalf("原结果应仍存在且有效: %+v", originalResults)
	}
	rep := rt.agentReport("companion")
	if !rep.Matched || rep.Generation != before.Generation || rep.CurrentRevision != revision ||
		rep.Evidence.TaskID != original.ID || rep.Evidence.ResultID != resultID ||
		rep.Evidence.GSRevision == nil || *rep.Evidence.GSRevision != revision ||
		rep.Evidence.BRRevision != revision || !rep.Evidence.Delivered {
		t.Fatalf("私聊历史挤出有效同版证据：期望 task=%d/result=%d，报告=%+v", original.ID, resultID, rep)
	}
	if rep.Latest.TaskID != newestTask || rep.Latest.ResultID != newestResult ||
		rep.Latest.GSRevision != nil || rep.Latest.BRRevision != revision {
		t.Fatalf("Latest 应归属最新私聊：期望 task=%d/result=%d，报告=%+v", newestTask, newestResult, rep)
	}
	t.Logf("agent=companion generation=%d revision=%d private=32 Evidence=%d/%d Latest=%d/%d",
		rep.Generation, revision, original.ID, resultID, newestTask, newestResult)

	// 用原清理入口移除已投递的原结果。32 条待投递的私聊结果仍留在库里。
	if err := rt.db.AIPrune(rt.ctx, float64(time.Now().Add(time.Hour).Unix())); err != nil {
		t.Fatal(err)
	}
	if results := t108EvidenceResults(t, rt.db, original.ID); len(results) != 0 {
		t.Fatalf("原结果尚未清理: %+v", results)
	}
	rep = rt.agentReport("companion")
	if rep.Matched || rep.Evidence.ResultID != 0 || rep.CurrentRevision != revision ||
		rep.Reason != store.AITaskEvidenceReasonNoGSPersona || rep.Latest.ResultID != newestResult {
		t.Fatalf("失去唯一匹配证据后应为未确认: %+v", rep)
	}
	t.Logf("清理原结果后 matched=%v reason=%q Latest=%d/%d", rep.Matched, rep.Reason, rep.Latest.TaskID, rep.Latest.ResultID)
}
