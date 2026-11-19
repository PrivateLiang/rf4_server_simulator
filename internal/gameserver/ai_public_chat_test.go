package gameserver

// T-013：公聊命令保留官方广播。
//
// 覆盖票 §3 的可判定场景：AI 关闭（总闸紧急停止 / AI 子系统未接入）、零接入
// （Agent 存在但 joined=0）、普通 @邀请 前缀句、非法目标、合法邀请/移出，
// 以及「合法命令的 AI 侧效果只执行一次、原消息不再送模型」。
//
// 「无权限」场景无法从既有代码判定：Session/Ctx/Server 没有频道角色或权限位，
// hSocialPublic 也不读取任何权限状态。按票面要求不自行发明权限模型，已登记
// DR-002（L1，台账 §A）；本文件不伪造该场景的测试。

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/tunnel"
)

const t013SelfUID = 1000001

// t013ChatRecord 是隧道客户端侧读到的一条推送。
type t013ChatRecord struct {
	main, sub uint8
	text      string
	fromUID   uint32
	err       error
}

// t013ChatProbe 一条带真实隧道的最小公聊会话。
type t013ChatProbe struct {
	srv     *Server
	db      *store.DB
	sess    *Session
	records chan t013ChatRecord
}

func newT013ChatProbe(t *testing.T, channel string) *t013ChatProbe {
	t.Helper()
	return newT013ChatProbeWithDB(t, channel, openMigratedTestDB(t))
}

// newT013ChatProbeWithoutDB 模拟 AI 子系统未接入：服务端没有共享库。
func newT013ChatProbeWithoutDB(t *testing.T, channel string) *t013ChatProbe {
	t.Helper()
	return newT013ChatProbeWithDB(t, channel, nil)
}

func newT013ChatProbeWithDB(t *testing.T, channel string, db *store.DB) *t013ChatProbe {
	t.Helper()
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "t013-chat"}}, logging.Discard(), nil)
	if db != nil {
		srv.SetDB(db)
	}
	sess := &Session{ID: 1, Userid: t013SelfUID, Srv: srv, log: logging.Discard()}
	sess.SetState("channel", map[string]any{"name": channel, "region": "CN", "users": 1})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听测试隧道: %v", err)
	}
	type hsRes struct {
		c   *tunnel.Conn
		err error
	}
	scCh := make(chan hsRes, 1)
	go func() {
		nc, aerr := ln.Accept()
		if aerr != nil {
			scCh <- hsRes{err: aerr}
			return
		}
		c, herr := tunnel.HandshakeServer(nc, "t013-chat")
		scCh <- hsRes{c: c, err: herr}
	}()
	nc, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("连接测试隧道: %v", err)
	}
	gc, err := tunnel.HandshakeClient(nc, "t013-chat")
	if err != nil {
		t.Fatalf("客户端握手: %v", err)
	}
	sr := <-scCh
	if sr.err != nil {
		t.Fatalf("服务端握手: %v", sr.err)
	}
	sess.tunnel = sr.c
	srv.sessions[sess.ID] = sess
	t.Cleanup(func() {
		_ = gc.Close()
		_ = sr.c.Close()
		_ = ln.Close()
	})

	records := make(chan t013ChatRecord, 32)
	go func() {
		for {
			_, _, payload, rerr := gc.ReadRecord()
			if rerr != nil {
				records <- t013ChatRecord{err: rerr}
				return
			}
			env, ok := proto.ParseEnvelope(payload)
			if !ok {
				records <- t013ChatRecord{err: fmt.Errorf("信封解析失败")}
				return
			}
			r := t013ChatRecord{main: env.Main, sub: env.Sub}
			dec, _, derr := proto.DecodeValue(env.Body, 0, srv.Table())
			if derr != nil {
				r.err = fmt.Errorf("推送 body 不可解码: %w", derr)
			} else if dec.Kind != proto.KindArgs || len(dec.Items) != 3 {
				r.err = fmt.Errorf("24/27 body 应 args[3]，实得 kind=%v len=%d", dec.Kind, len(dec.Items))
			} else {
				f1 := dec.Items[0].Field(1)
				f2 := dec.Items[0].Field(2)
				r.text = f1.StrOr()
				r.fromUID = f2.U32()
			}
			records <- r
		}
	}()

	return &t013ChatProbe{srv: srv, db: db, sess: sess, records: records}
}

// send 走 24/10 公聊入口；该指令本身必须 fire-and-forget（handled, nil body）。
func (p *t013ChatProbe) send(t *testing.T, msg string) {
	t.Helper()
	body, ok := hSocialPublic(miscCtx(p.sess, 24, 10, proto.Args524(proto.StrVal(msg))))
	if !ok || body != nil {
		t.Fatalf("24/10 应 handled+nil（fire-and-forget），ok=%v body=%x", ok, body)
	}
}

func (p *t013ChatProbe) next(t *testing.T) t013ChatRecord {
	t.Helper()
	select {
	case r := <-p.records:
		if r.err != nil {
			t.Fatalf("读 24/27 推送失败: %v", r.err)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到 24/27 推送")
		return t013ChatRecord{}
	}
}

func (p *t013ChatProbe) expectNoMore(t *testing.T) {
	t.Helper()
	select {
	case r := <-p.records:
		t.Fatalf("不应再有推送: %d/%d text=%q err=%v", r.main, r.sub, r.text, r.err)
	case <-time.After(150 * time.Millisecond):
	}
}

// t013AssertBroadcast 断言一条推送就是官方公聊广播：24/27 + 原文 + 发送者 uid。
func t013AssertBroadcast(t *testing.T, r t013ChatRecord, want string) {
	t.Helper()
	if r.main != 24 || r.sub != 27 {
		t.Fatalf("广播必须走 24/27，实得 %d/%d", r.main, r.sub)
	}
	if r.text != want {
		t.Fatalf("广播文本=%q，期望原消息 %q", r.text, want)
	}
	if r.fromUID != t013SelfUID {
		t.Fatalf("广播发送者 uid=%d，期望 %d", r.fromUID, t013SelfUID)
	}
}

func t013AssertFeedback(t *testing.T, r t013ChatRecord, wantSub string) {
	t.Helper()
	if r.main != 24 || r.sub != 27 {
		t.Fatalf("命令回执必须走 24/27，实得 %d/%d", r.main, r.sub)
	}
	if !strings.Contains(r.text, wantSub) {
		t.Fatalf("命令回执=%q，期望包含 %q", r.text, wantSub)
	}
	if r.fromUID != t013SelfUID {
		t.Fatalf("命令回执发送者 uid=%d，期望 %d", r.fromUID, t013SelfUID)
	}
}

func t013JoinAgent(t *testing.T, db *store.DB, key string) {
	t.Helper()
	if err := db.AISetAgentJoined(t.Context(), key, true, 0); err != nil {
		t.Fatalf("接入 Agent %q: %v", key, err)
	}
}

func t013ChannelSubs(t *testing.T, db *store.DB, channel, key string) int {
	t.Helper()
	var n int
	err := db.SQL.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM ai_channel_agent
		WHERE channel=? AND agent_id=(SELECT id FROM ai_agent WHERE key=?)`,
		channel, key).Scan(&n)
	if err != nil {
		t.Fatalf("统计频道订阅: %v", err)
	}
	return n
}

func t013AITasks(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM ai_task`).Scan(&n); err != nil {
		t.Fatalf("统计 AI 任务: %v", err)
	}
	return n
}

// TestAIPublicChatBroadcastNotSwallowed 验证这些情况下消息都回到官方公聊路径：
// AI 关闭、零接入、普通前缀句、非法目标。
func TestAIPublicChatBroadcastNotSwallowed(t *testing.T) {
	t.Run("ai_off_emergency_stop", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_ai_off")
		t013JoinAgent(t, p.db, "companion")
		if err := p.db.AISetSetting(t.Context(), "emergency_stop", "1"); err != nil {
			t.Fatal(err)
		}
		msg := "@邀请 AI 伙伴"
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t) // 总闸停止时不产生 AI 回执
		if n := t013ChannelSubs(t, p.db, "t013_ai_off", "companion"); n != 0 {
			t.Fatalf("AI 关闭时不应改频道订阅，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("AI 关闭时不应产生模型任务，实得 %d 条", n)
		}
	})

	t.Run("ai_off_no_db", func(t *testing.T) {
		p := newT013ChatProbeWithoutDB(t, "t013_ai_off_nodb")
		msg := "@邀请 AI 伙伴"
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
	})

	t.Run("zero_joined_agents", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_zero")
		var joined int
		if err := p.db.SQL.QueryRowContext(t.Context(),
			`SELECT joined FROM ai_agent WHERE key='companion'`).Scan(&joined); err != nil {
			t.Fatalf("读种子 Agent: %v", err)
		}
		if joined != 0 {
			t.Fatalf("夹具应为零接入（joined=0），实得 %d", joined)
		}
		msg := "@邀请 AI 伙伴"
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		if n := t013ChannelSubs(t, p.db, "t013_zero", "companion"); n != 0 {
			t.Fatalf("零接入时不应改频道订阅，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("零接入时不应产生模型任务，实得 %d 条", n)
		}
	})

	t.Run("ordinary_prefix_sentence", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_prefix")
		t013JoinAgent(t, p.db, "companion") // AI 是开着的，确保不是靠总闸才放行
		msg := "@邀请码怎么用？"
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		if n := t013ChannelSubs(t, p.db, "t013_prefix", "companion"); n != 0 {
			t.Fatalf("普通前缀句不应改频道订阅，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("普通前缀句没有点名，不应产生模型任务，实得 %d 条", n)
		}
	})

	t.Run("prefix_sentence_with_agent_name_is_ordinary_chat", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_prefix_mention")
		t013JoinAgent(t, p.db, "companion")
		// T-108：点名也要核对频道范围；先把角色放进本频道范围，测的才是
		// 「多出内容 → 不是精确命令 → 按普通点名处理」这条。
		if err := p.db.AISetChannelAgent(t.Context(), "t013_prefix_mention", "companion", true); err != nil {
			t.Fatal(err)
		}
		msg := "@邀请 AI 伙伴 来钓鱼" // 多出内容 → 不是精确命令，按普通公聊处理
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		// 普通公聊点名 Agent 仍应走模型触发路径：这正好反证「不是命令」。
		if n := t013AITasks(t, p.db); n != 1 {
			t.Fatalf("普通公聊点名应产生 1 条模型任务，实得 %d 条", n)
		}
		var text string
		if err := p.db.SQL.QueryRowContext(t.Context(),
			`SELECT text FROM ai_task ORDER BY id DESC LIMIT 1`).Scan(&text); err != nil {
			t.Fatalf("读 AI 任务: %v", err)
		}
		if text != msg {
			t.Fatalf("模型任务文本=%q，期望原消息 %q", text, msg)
		}
	})

	// T-108：点名一个不在本频道范围的角色，不得因为被叫到就在别的频道出现。
	t.Run("mention_out_of_scope_is_silent", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_scope_out")
		t013JoinAgent(t, p.db, "companion")
		msg := "@邀请 AI 伙伴 来钓鱼"
		p.send(t, msg)
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("越界点名不应产生任务，实得 %d 条", n)
		}
	})

	t.Run("invalid_target", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_invalid")
		t013JoinAgent(t, p.db, "companion")
		for _, msg := range []string{"@邀请 不存在的助手", "@邀请", "@移出 不存在的助手"} {
			p.send(t, msg)
			t013AssertBroadcast(t, p.next(t), msg)
			p.expectNoMore(t) // 非法目标不给 AI 回执
		}
		if n := t013ChannelSubs(t, p.db, "t013_invalid", "companion"); n != 0 {
			t.Fatalf("非法目标不应改频道订阅，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("非法目标不应产生模型任务，实得 %d 条", n)
		}
	})
}

// TestAIPublicChatCommandEffectOnlyOnce 验证合法命令：官方广播仍在，AI 侧效果
// （订阅变更 + 回执）只执行一次，且原消息不会再被送去模型触发第二轮。
func TestAIPublicChatCommandEffectOnlyOnce(t *testing.T) {
	t.Run("valid_invite", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_invite")
		t013JoinAgent(t, p.db, "companion")
		msg := "@邀请 AI 伙伴"
		p.send(t, msg)
		t013AssertFeedback(t, p.next(t), "拉进")
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t) // 一套 AI 侧效果 + 一条广播，没有第二份
		if n := t013ChannelSubs(t, p.db, "t013_invite", "companion"); n != 1 {
			t.Fatalf("合法邀请后频道订阅应恰好 1 行，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("合法命令原消息不应送模型，实得 %d 条任务", n)
		}
	})

	t.Run("valid_remove", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_remove")
		t013JoinAgent(t, p.db, "companion")
		if err := p.db.AISetChannelAgent(t.Context(), "t013_remove", "companion", true); err != nil {
			t.Fatalf("预置频道订阅: %v", err)
		}
		msg := "@移出 AI 伙伴"
		p.send(t, msg)
		t013AssertFeedback(t, p.next(t), "移出")
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		if n := t013ChannelSubs(t, p.db, "t013_remove", "companion"); n != 0 {
			t.Fatalf("合法移出后频道订阅应为 0 行，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("合法命令原消息不应送模型，实得 %d 条任务", n)
		}
	})

	t.Run("write_failure_keeps_broadcast", func(t *testing.T) {
		p := newT013ChatProbe(t, "t013_writefail")
		t013JoinAgent(t, p.db, "companion")
		if _, err := p.db.SQL.ExecContext(t.Context(), `
			CREATE TRIGGER t013_block_channel_agent BEFORE INSERT ON ai_channel_agent
			BEGIN SELECT RAISE(ABORT, 't013 blocked'); END`); err != nil {
			t.Fatalf("安装写失败夹具: %v", err)
		}
		msg := "@邀请 AI 伙伴"
		p.send(t, msg)
		t013AssertFeedback(t, p.next(t), "操作失败")
		t013AssertBroadcast(t, p.next(t), msg)
		p.expectNoMore(t)
		if n := t013ChannelSubs(t, p.db, "t013_writefail", "companion"); n != 0 {
			t.Fatalf("写失败后不应留下订阅，实得 %d 行", n)
		}
		if n := t013AITasks(t, p.db); n != 0 {
			t.Fatalf("写失败的合法命令也不应送模型，实得 %d 条任务", n)
		}
	})
}
