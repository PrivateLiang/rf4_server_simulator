package admin

// T-020 / DEV-004：后台只提交意图。
//
// 管理动作返回 202 pending，不直接改运行态；GET /api/ai/commands/{id}
// 只回脱敏状态。这里用真实迁移 SQLite 验证：
//   - 202 语义与 command_id/request_id；
//   - 记忆清空/删除/频道在 GS 应用前不生效；
//   - 过期代次原样入队，由 GS 判定拒绝；
//   - 普通整卡保存不偷偷提交 runtime 命令；
//   - admin 包不再直接调用运行态写接口。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func adminRuntimeDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "admin-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatalf("迁移: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func adminRuntimeServer(t *testing.T, db *store.DB) *Server {
	t.Helper()
	return NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
}

func adminPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	switch path {
	case "/api/ai/agent/actor":
		srv.handleAIAgentActor(rec, req)
	case "/api/ai/agent/delete":
		srv.handleAIAgentDelete(rec, req)
	case "/api/ai/memory/clear":
		srv.handleAIMemoryClear(rec, req)
	case "/api/ai/channel":
		srv.handleAIChannelSave(rec, req)
	default:
		t.Fatalf("未知测试路径 %s", path)
	}
	return rec
}

func adminGetCommand(t *testing.T, srv *Server, id int64) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/ai/commands/"+itoaAdmin(id), nil)
	req.SetPathValue("id", itoaAdmin(id))
	rec := httptest.NewRecorder()
	srv.handleAICommandGet(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析命令响应: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func itoaAdmin(n int64) string {
	return strconv.FormatInt(n, 10)
}

func adminCommandCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAIRuntimeCommandAdminActorAccepted：角色开关只入队，202 + pending。
func TestAIRuntimeCommandAdminActorAccepted(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	ctx := context.Background()

	rec := adminPost(t, srv, "/api/ai/agent/actor", `{"key":"companion","enabled":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	var accepted struct {
		OK        bool   `json:"ok"`
		Status    string `json:"status"`
		CommandID int64  `json:"command_id"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("解析 202: %v", err)
	}
	if !accepted.OK || accepted.Status != store.AIRuntimeStatusPending ||
		accepted.CommandID <= 0 || accepted.RequestID == "" {
		t.Fatalf("202 响应 = %+v", accepted)
	}
	// 运行态还没变：actor_enabled 仍为 0，也没有 actor 行。
	var actorEnabled int
	if err := db.SQL.QueryRow(`SELECT actor_enabled FROM ai_agent WHERE key='companion'`).Scan(&actorEnabled); err != nil {
		t.Fatal(err)
	}
	if actorEnabled != 0 {
		t.Fatal("入队阶段就改了 actor_enabled")
	}
	var actorRows int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_actor_state`).Scan(&actorRows); err != nil {
		t.Fatal(err)
	}
	if actorRows != 0 {
		t.Fatal("入队阶段就建了角色状态")
	}
	// GET 查询：pending 返回 202，只含脱敏字段，不回显 payload。
	rec, body := adminGetCommand(t, srv, accepted.CommandID)
	if rec.Code != http.StatusAccepted || body["status"] != store.AIRuntimeStatusPending {
		t.Fatalf("pending 查询 = %d %+v", rec.Code, body)
	}
	if _, leaked := body["payload"]; leaked {
		t.Fatal("命令查询泄露 payload")
	}
	// 模拟 GS 应用后：GET 返回 200 applied。
	cmd, err := db.AIApplyRuntimeCommand(ctx, accepted.CommandID, 0, func(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
		if _, err := tx.SQL().ExecContext(tx.Context(),
			`UPDATE ai_agent SET actor_enabled=1 WHERE key=?`, cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	})
	if err != nil || cmd.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("应用 = %+v err=%v", cmd, err)
	}
	rec, body = adminGetCommand(t, srv, accepted.CommandID)
	if rec.Code != http.StatusOK || body["status"] != store.AIRuntimeStatusApplied {
		t.Fatalf("终态查询 = %d %+v", rec.Code, body)
	}
	if _, ok := body["result_revision"]; !ok {
		t.Fatal("终态查询缺 result_revision")
	}
}

// TestAIRuntimeCommandAdminMemoryClearIsIntent：清记忆在 GS 应用前不生效。
func TestAIRuntimeCommandAdminMemoryClearIsIntent(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.AIAddMemory(ctx, "companion", "star", "perch", "鲈鱼", 1200, "x"); err != nil {
			t.Fatal(err)
		}
	}
	rec := adminPost(t, srv, "/api/ai/memory/clear", `{"key":"companion"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", rec.Code)
	}
	var memories int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_memory WHERE agent_key='companion'`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 2 {
		t.Fatalf("入队即清了记忆: 剩 %d 条", memories)
	}
	var id int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_runtime_command WHERE kind='memory_clear' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AIApplyRuntimeCommand(ctx, id, 0, func(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
		if err := tx.ClearMemories(cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_memory WHERE agent_key='companion'`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 0 {
		t.Fatalf("应用后记忆 = %d", memories)
	}
}

// TestAIRuntimeCommandAdminDeleteIsIntent：删除只入队，Agent 在应用前仍存在。
func TestAIRuntimeCommandAdminDeleteIsIntent(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	rec := adminPost(t, srv, "/api/ai/agent/delete", `{"key":"companion"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", rec.Code)
	}
	var deleted float64
	if err := db.SQL.QueryRow(`SELECT deleted_ts FROM ai_agent WHERE key='companion'`).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatal("入队阶段就逻辑删除了 Agent")
	}
	if n := adminCommandCount(t, db); n != 1 {
		t.Fatalf("命令 = %d 行，期望 1", n)
	}
}

// TestAIRuntimeCommandAdminChannelIsIntent：频道勾选改意图，应用前成员不变。
func TestAIRuntimeCommandAdminChannelIsIntent(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	rec := adminPost(t, srv, "/api/ai/channel",
		`{"channel":"runtime-admin-room","agent":"companion","enabled":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	var members int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_channel_agent WHERE channel='runtime-admin-room'`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 0 {
		t.Fatalf("入队阶段就写了频道成员: %d", members)
	}
	// 地图订阅仍是 AD 直接配置（非运行态），保持同步写。
	rec = adminPost(t, srv, "/api/ai/channel",
		`{"map_key":"*","agent":"companion","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("地图订阅状态码 = %d，期望 200", rec.Code)
	}
	if n := adminCommandCount(t, db); n != 1 {
		t.Fatalf("地图订阅不应产生 runtime 命令，命令数 = %d", n)
	}
}

// TestAIRuntimeCommandAdminStaleGenerationPreserved：页面上报的过期代次原样入队，
// 由 GS 拒绝；后台不能偷偷改成当前代次。
func TestAIRuntimeCommandAdminStaleGenerationPreserved(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	ctx := context.Background()
	if _, err := db.SQL.Exec(`UPDATE ai_runtime_control SET generation=5 WHERE agent_key='companion'`); err != nil {
		t.Fatal(err)
	}
	rec := adminPost(t, srv, "/api/ai/agent/actor",
		`{"key":"companion","enabled":false,"generation":2,"revision":3}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", rec.Code)
	}
	var gen int64
	if err := db.SQL.QueryRow(`SELECT expected_generation FROM ai_runtime_command ORDER BY id DESC LIMIT 1`).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if gen != 2 {
		t.Fatalf("expected_generation = %d，期望页面上报的 2（GS 才会拒绝）", gen)
	}
	ctl, err := db.AIGetRuntimeControl(ctx, "companion")
	if err != nil || ctl.Generation != 5 {
		t.Fatalf("后台偷改了代次: %+v err=%v", ctl, err)
	}
}

// TestAIRuntimeCommandAdminQueueFull429：队列满映射为 429。
func TestAIRuntimeCommandAdminQueueFull429(t *testing.T) {
	db := adminRuntimeDB(t)
	srv := adminRuntimeServer(t, db)
	now := float64(time.Now().Unix())
	tx, err := db.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.AIRuntimeMaxPendingAgent; i++ {
		if _, err := tx.Exec(`
			INSERT INTO ai_runtime_command(request_id, source, kind, agent_key, payload,
				payload_hash, created_ts, expires_ts, status)
			VALUES(?,'admin','memory_clear','companion','{}','h',?,?,'pending')`,
			"admin-fill-"+itoaAdmin(int64(i)), now, now+600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rec := adminPost(t, srv, "/api/ai/memory/clear", `{"key":"companion"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("队列满状态码 = %d，期望 429（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestAIRuntimeCommandAdminNoDirectRuntimeWriter：admin 包不得再直接写运行态。
func TestAIRuntimeCommandAdminNoDirectRuntimeWriter(t *testing.T) {
	raw, err := os.ReadFile("ai.go")
	if err != nil {
		t.Fatalf("读取 admin/ai.go: %v", err)
	}
	src := string(raw)
	for _, banned := range []string{
		"AISaveActor", "AIDeleteActor", "AISetActorEnabled", "AIClearMemories",
		"AISetChannelAgent", "AIAddMemory", "AIPruneMemories", "AIAddCatch",
		"AIDeleteAgent",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("admin/ai.go 仍直接调用运行态写接口 %s（应只入队）", banned)
		}
	}
	// 普通整卡保存不得偷偷提交 runtime 命令。
	for _, fn := range []string{"handleAIConfigSave", "handleAISettingsSave"} {
		body := adminFuncBody(t, src, fn)
		if body == "" {
			t.Fatalf("未找到 %s", fn)
		}
		if strings.Contains(body, "AIEnqueueRuntimeCommand") || strings.Contains(body, "enqueueAIIntent") {
			t.Errorf("%s 提交了 runtime 命令（整卡保存只能写配置）", fn)
		}
	}
}

// adminFuncBody 粗略截取一个函数体：从 func 声明到下一个顶层 func。
func adminFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func (s *Server) "+name+"(")
	if start < 0 {
		return ""
	}
	rest := src[start+1:]
	if next := strings.Index(rest, "\nfunc "); next >= 0 {
		return rest[:next]
	}
	return rest
}
