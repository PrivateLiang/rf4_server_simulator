package aibridge

// 活动配置快照与不可变密文对象的独立真值（DEV-114 A / T-102）。
//
// 这些用例锁定发布合同里读者必须遵守的部分：
//   - 地址、模型、修订、凭据引用在单次读取中绑定；
//   - 活动引用缺失/指纹不符/对象与厂商不符 → 锁定，不回退同名文件或旧库；
//   - 发布历史与活动指针矛盾 → 锁定；
//   - 提交后新读取只见新套，旧对象仍在（在途传输可完成）；
//   - 旧安装（无活动修订、无发布历史）才走只读兼容：外部旧文件优先、旧库后备。

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

type snapshotFixture struct {
	d          *store.DB
	keyFile    string
	masterKey  []byte
	providerID int64
	modelRowID int64
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	t.Helper()
	// 测试模拟支持目录 fsync 的平台；真实平台的拒绝发布见 TestCredentialInstallDurability。
	t.Cleanup(SetCredentialDirSyncForTest(func(string) error { return nil }))
	dir := t.TempDir()
	d, err := store.Open(filepath.Join(dir, "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "ai_master.key")
	masterKey, err := InitMasterKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotFixture{d: d, keyFile: keyFile, masterKey: masterKey}
	ctx := context.Background()
	if err := d.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_provider WHERE name='deepseek'`).Scan(&f.providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_provider SET base_url='https://a.example.com/v1' WHERE id=?`, f.providerID); err != nil {
		t.Fatal(err)
	}
	if err := d.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_model WHERE provider_id=? ORDER BY id LIMIT 1`, f.providerID).Scan(&f.modelRowID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_model SET model_id='model-a' WHERE id=?`, f.modelRowID); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedLegacySecret 写入旧库密文（旧安装兼容路径）。
func (f *snapshotFixture) seedLegacySecret(t *testing.T, plain string) {
	t.Helper()
	ct, nonce, err := Encrypt(f.masterKey, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.SQL.ExecContext(context.Background(), `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES(?,?,?,'default',?,0)
		ON CONFLICT(provider_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,masked=excluded.masked`,
		f.providerID, ct, nonce, Masked(plain)); err != nil {
		t.Fatal(err)
	}
}

// publish 先创建不可变对象，再在事务里发布。返回新对象引用与发布结果。
func (f *snapshotFixture) publish(t *testing.T, base, model, key string, expect int64) (string, store.AIConfigPublication) {
	t.Helper()
	obj, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey, f.providerID, "deepseek", key)
	if err != nil {
		t.Fatalf("创建凭据对象: %v", err)
	}
	pub, err := f.d.AIPublishConfig(context.Background(), store.AIPublishRequest{
		ExpectedRevision: expect, PriorRevision: expect,
		PriorBaseURL: "https://a.example.com/v1",
		ProviderID:   f.providerID, ModelRowID: f.modelRowID,
		BaseURL: base, ModelID: model,
		CredentialRef: obj.Ref, CredentialChanged: true, CredentialMasked: obj.Masked,
		Source: "admin_save", Actor: "tester",
		Purpose: []string{"chat"}, Contents: []string{"chat_text"},
	})
	if err != nil {
		t.Fatalf("发布配置: %v", err)
	}
	return obj.Ref, pub
}

// TestProviderSnapshot 覆盖读者合同的正常路径与修订绑定。
func TestProviderSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")

	// 1) 旧安装：没有活动修订、没有发布历史 → 只读兼容行 + 旧库密文。
	snap, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatalf("旧安装应可读: %v", err)
	}
	if !snap.Legacy || snap.Revision() != 0 {
		t.Fatalf("旧安装快照 = %+v，期望 Legacy/rev0", snap)
	}
	if snap.Spec.BaseURL != "https://a.example.com/v1" || snap.Spec.ModelID != "model-a" ||
		snap.Spec.APIKey != "sk-legacy-db" {
		t.Fatalf("旧套字段 = %+v", snap.Spec)
	}

	// 2) 发布 B：同一事务写活动修订、记录与审计。
	refB, pubB := f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
	if pubB.Revision != 1 {
		t.Fatalf("首个发布修订 = %d，期望 1", pubB.Revision)
	}
	snap, err = LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatalf("发布后读取: %v", err)
	}
	if snap.Legacy || snap.Revision() != 1 ||
		snap.Spec.BaseURL != "https://b.example.com/v1" || snap.Spec.ModelID != "model-b" ||
		snap.Spec.APIKey != "sk-new-b" {
		t.Fatalf("发布后快照 = %+v / %+v", snap, snap.Spec)
	}
	if snap.Masked == "" || strings.Contains(snap.Masked, "sk-new-b") {
		t.Fatalf("遮蔽值不合法: %q", snap.Masked)
	}
	if refB == "" || !ValidCredentialRef(refB) {
		t.Fatalf("凭据引用不合法: %q", refB)
	}
	// 旧对象仍可解密（在途传输可完成）。
	if _, plain, err := ResolveCredentialObject(CredentialDir(f.keyFile), refB, f.masterKey, f.providerID); err != nil || plain != "sk-new-b" {
		t.Fatalf("已发布对象不可读: %v", err)
	}
	if got, err := ActiveRevision(ctx, f.d); err != nil || got != 1 {
		t.Fatalf("ActiveRevision = %d，err=%v，期望 1", got, err)
	}
	if err := Gate(ctx, f.d, 1); err != nil {
		t.Fatalf("同一修订的发送前检查应放行: %v", err)
	}

	// 3) 再发布 C：持旧修订的尝试必须作废，新读取只见 C。
	f.publish(t, "https://c.example.com/v1", "model-c", "sk-new-c", 1)
	if err := Gate(ctx, f.d, 1); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("旧修订的发送前检查应返回 stale，得到 %v", err)
	}
	snap, err = LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil || snap.Revision() != 2 || snap.Spec.BaseURL != "https://c.example.com/v1" {
		t.Fatalf("第二次发布后快照 = %+v，err=%v", snap, err)
	}
	// 同一 DB 里旧对象仍保留（未引用对象不回收）。
	entries, err := os.ReadDir(CredentialDir(f.keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("凭据对象数量 = %d，期望至少 2（B/C 各自保留，旧安装 A 不生成对象）", len(entries))
	}

	// 4) 急停优先于一切：Gate 返回急停拒绝。
	if err := f.d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	var denied *EgressDeniedError
	if err := Gate(ctx, f.d, 2); !errors.As(err, &denied) || denied.State != aiconfig.StateEmergency {
		t.Fatalf("急停时 Gate = %v，期望 EgressDeniedError(emergency)", err)
	}
}

// TestProviderSnapshotLegacyFilePriority 旧安装解析保持原优先级：
// 原外部凭据文件优先，旧库密文只读后备。
func TestProviderSnapshotLegacyFilePriority(t *testing.T) {
	ctx := context.Background()
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")
	if err := SaveCredential(CredentialsPath(f.keyFile), f.masterKey, f.providerID, "deepseek", "sk-legacy-file"); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Spec.APIKey != "sk-legacy-file" {
		t.Fatalf("外部旧文件应优先，得到 %q", snap.Spec.APIKey)
	}
	if err := os.Remove(CredentialsPath(f.keyFile)); err != nil {
		t.Fatal(err)
	}
	snap, err = LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Spec.APIKey != "sk-legacy-db" {
		t.Fatalf("外部文件删除后应回退旧库密文，得到 %q", snap.Spec.APIKey)
	}
}

// TestProviderSnapshotLocks 活动引用损坏/不匹配/历史矛盾一律锁定，
// 不得回退同名文件或旧库明文。
func TestProviderSnapshotLocks(t *testing.T) {
	ctx := context.Background()

	t.Run("活动对象缺失", func(t *testing.T) {
		f := newSnapshotFixture(t)
		f.seedLegacySecret(t, "sk-legacy-db") // 旧库有可用 Key，但不得被回退使用
		ref, _ := f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
		if err := os.Remove(filepath.Join(CredentialDir(f.keyFile), ref+".json")); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
		if !errors.Is(err, ErrConfigLocked) {
			t.Fatalf("应锁定，得到 %v", err)
		}
		if !errors.Is(err, ErrCredentialObjectMissing) {
			t.Fatalf("应带对象缺失原因，得到 %v", err)
		}
	})

	t.Run("主密钥不匹配", func(t *testing.T) {
		f := newSnapshotFixture(t)
		f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
		wrong := []byte("ffffffffffffffffffffffffffffffff")
		_, err := LoadSnapshot(ctx, f.d, f.keyFile, wrong)
		if !errors.Is(err, ErrConfigLocked) || !errors.Is(err, ErrCredentialKeyMismatch) {
			t.Fatalf("应锁定+指纹不符，得到 %v", err)
		}
	})

	t.Run("对象厂商不匹配", func(t *testing.T) {
		f := newSnapshotFixture(t)
		obj, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey, 999, "other", "sk-other")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.AIPublishConfig(ctx, store.AIPublishRequest{
			ProviderID: f.providerID, ModelRowID: f.modelRowID,
			BaseURL: "https://b.example.com/v1", ModelID: "model-b",
			CredentialRef: obj.Ref, CredentialChanged: true, Source: "admin_save",
		}); err != nil {
			t.Fatal(err)
		}
		_, err = LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
		if !errors.Is(err, ErrConfigLocked) || !errors.Is(err, ErrCredentialObjectMismatch) {
			t.Fatalf("应锁定+厂商不匹配，得到 %v", err)
		}
	})

	t.Run("历史与指针矛盾", func(t *testing.T) {
		f := newSnapshotFixture(t)
		// 只有发布记录、没有活动指针：不能当旧安装。
		raw, _ := aiconfig.MarshalConfigRecord(aiconfig.ConfigRecord{Schema: 1, Revision: 1, Kind: "model"})
		if _, err := f.d.SQL.ExecContext(ctx,
			`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
			aiconfig.ConfigRecordKey(1), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey); !errors.Is(err, store.ErrAIConfigHistoryConflict) {
			t.Fatalf("应返回历史矛盾，得到 %v", err)
		}
	})

	t.Run("活动指针损坏", func(t *testing.T) {
		f := newSnapshotFixture(t)
		if _, err := f.d.SQL.ExecContext(ctx,
			`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
			aiconfig.KeyConfigActive, `{"schema":1,"revision":1}`); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey); !errors.Is(err, store.ErrAIConfigSnapshotInvalid) {
			t.Fatalf("应返回记录无效，得到 %v", err)
		}
	})
}

// TestProviderSnapshotTwoHandles 模拟两个进程共用一个 SQLite：
// 一方发布后，另一方下一次读取只见新套；它手里的旧修订尝试直接作废。
func TestProviderSnapshotTwoHandles(t *testing.T) {
	ctx := context.Background()
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")
	// 第二个独立连接 = 第二个进程的视角（同一个 SQLite 文件）。
	dbPath := filepath.Join(filepath.Dir(f.keyFile), "snapshot.db")
	d2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Close() })

	f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
	snap1, err := LoadSnapshot(ctx, d2, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatalf("第二个句柄读取: %v", err)
	}
	if snap1.Revision() != 1 || snap1.Spec.BaseURL != "https://b.example.com/v1" {
		t.Fatalf("第二个句柄看到 %+v", snap1)
	}
	// 第一个句柄发布 C；第二个句柄的旧修订检查必须 stale。
	f.publish(t, "https://c.example.com/v1", "model-c", "sk-new-c", 1)
	if err := Gate(ctx, d2, snap1.Revision()); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("旧修订在另一进程应 stale，得到 %v", err)
	}
	snap2, err := LoadSnapshot(ctx, d2, f.keyFile, f.masterKey)
	if err != nil || snap2.Revision() != 2 || snap2.Spec.ModelID != "model-c" {
		t.Fatalf("第二次读取 = %+v，err=%v", snap2, err)
	}
}

// TestProviderSnapshotRunnerSend Runner 用同一份快照发送：发布 B 后任务只发到 B；
// 发布 C 后旧修订的任务被丢弃，新任务发到 C。
func TestProviderSnapshotRunnerSend(t *testing.T) {
	ctx := context.Background()
	var hitsA, hitsB, hitsC int32
	mkVendor := func(label string, hits *int32) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + label + `"}}]}`))
		}))
	}
	vA := mkVendor("A", &hitsA)
	defer vA.Close()
	vB := mkVendor("B", &hitsB)
	defer vB.Close()
	vC := mkVendor("C", &hitsC)
	defer vC.Close()

	f := newSnapshotFixture(t)
	f.publish(t, vB.URL, "model-b", "sk-new-b", 0)
	r := NewRunner(f.d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5, MasterKeyFile: f.keyFile},
		f.masterKey, logging.Discard())
	r.providerTransport = vB.Client().Transport
	send := func() string {
		t.Helper()
		if _, err := f.d.AIEnqueue(ctx, &store.AITask{
			Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "fishguide",
			ContactUID: 2147483644, SourceKind: "human", Text: "在吗",
		}); err != nil {
			t.Fatal(err)
		}
		task, ok, err := f.d.AIPopTask(ctx)
		if err != nil || !ok {
			t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
		}
		r.handle(ctx, task)
		var status string
		if err := f.d.SQL.QueryRowContext(ctx, `SELECT status FROM ai_result WHERE task_id=?`, task.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if status := send(); status != "done" {
		t.Fatalf("发布 B 后发送状态 = %q，期望 done", status)
	}
	if hitsB != 1 || hitsA != 0 {
		t.Fatalf("出站计数 A=%d B=%d，期望 A=0 B=1", hitsA, hitsB)
	}

	// 发布 C 后旧修订的尝试被拒绝；新任务发到 C。
	f.publish(t, vC.URL, "model-c", "sk-new-c", 1)
	if err := Gate(ctx, f.d, 1); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("旧修订 Gate = %v，期望 stale", err)
	}
	r.providerTransport = vC.Client().Transport
	if status := send(); status != "done" {
		t.Fatalf("发布 C 后发送状态 = %q，期望 done", status)
	}
	if hitsC != 1 || hitsB != 1 {
		t.Fatalf("出站计数 B=%d C=%d，期望 B=1 C=1", hitsB, hitsC)
	}
}

// TestCredentialObjectWriteFailureKeepsOldSet 外部凭据写入失败时：
// 不产生新对象引用，读者仍用上一套完整配置；故障恢复后重试一次成功。
func TestCredentialObjectWriteFailureKeepsOldSet(t *testing.T) {
	ctx := context.Background()
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")
	f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
	if got, err := ActiveRevision(ctx, f.d); err != nil || got != 1 {
		t.Fatalf("初始修订 = %d, %v", got, err)
	}
	oldHook := credentialObjectHook
	credentialObjectHook = func(string) error { return errors.New("disk full") }
	t.Cleanup(func() { credentialObjectHook = oldHook })
	if _, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey, f.providerID,
		"deepseek", "sk-failed-c"); err == nil {
		t.Fatal("注入写入失败后不应创建对象")
	}
	credentialObjectHook = nil
	snap, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatalf("凭据写失败后读者应仍可用 A: %v", err)
	}
	if snap.Revision() != 1 || snap.Spec.APIKey != "sk-new-b" || snap.Spec.ModelID != "model-b" {
		t.Fatalf("失败后快照 = %+v", snap)
	}
	if refs := countObjectFiles(t, f.keyFile); refs != 1 {
		t.Fatalf("失败后对象文件 = %d，期望 1（只保留已发布的 B）", refs)
	}
	// 故障恢复：重试一次即成功。
	f.publish(t, "https://c.example.com/v1", "model-c", "sk-new-c", 1)
	if got, err := ActiveRevision(ctx, f.d); err != nil || got != 2 {
		t.Fatalf("重试后修订 = %d, %v，期望 2", got, err)
	}
}

func countObjectFiles(t *testing.T, keyFile string) int {
	t.Helper()
	entries, err := os.ReadDir(CredentialDir(keyFile))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestCredentialObjectImmutable 锁定密文对象的创建/读取语义：
// 引用随机且形状受限、创建不覆盖、临时件会被识别为已有密文。
func TestCredentialObjectImmutable(t *testing.T) {
	t.Cleanup(SetCredentialDirSyncForTest(func(string) error { return nil }))
	dir := filepath.Join(t.TempDir(), "ai_credentials.d")
	key := []byte("0123456789abcdef0123456789abcdef")
	first, err := CreateCredentialObject(dir, key, 7, "deepseek", "sk-first-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateCredentialObject(dir, key, 7, "deepseek", "sk-second-key")
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref == second.Ref || !ValidCredentialRef(first.Ref) || !ValidCredentialRef(second.Ref) {
		t.Fatalf("引用应随机且合法: %q %q", first.Ref, second.Ref)
	}
	// 两个对象都能各自解密，互不影响。
	for _, tc := range []struct {
		ref  string
		want string
	}{{first.Ref, "sk-first-key"}, {second.Ref, "sk-second-key"}} {
		_, plain, err := ResolveCredentialObject(dir, tc.ref, key, 7)
		if err != nil || plain != tc.want {
			t.Fatalf("对象 %s = (%q,%v)，期望 %q", tc.ref, plain, err, tc.want)
		}
	}
	if first.Masked == "" || strings.Contains(first.Masked, "first") {
		t.Fatalf("遮蔽值不得包含明文: %q", first.Masked)
	}
	// 非法引用形状不得读到目录外。
	for _, bad := range []string{"", "../ai_master.key", "ABCDEF", strings.Repeat("a", 31)} {
		if _, err := LoadCredentialObject(dir, bad); err == nil {
			t.Errorf("非法引用 %q 应被拒绝", bad)
		}
	}
	// 临时准备件也算「已有密文」：显式初始化不得新建主密钥。
	if err := os.WriteFile(filepath.Join(dir, ".ai_credential-crash.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err := CredentialDirHasCiphertext(dir)
	if err != nil || !has {
		t.Fatalf("CredentialDirHasCiphertext = %v，err=%v，期望 true", has, err)
	}
	if has, err := CredentialDirHasCiphertext(filepath.Join(dir, "missing")); err != nil || has {
		t.Fatalf("不存在的目录应为 false，得到 %v，err=%v", has, err)
	}
}

// TestProviderSnapshotLegacyGateAfterPublication 首次发布前后：
// rev0 旧快照在发布前可兼容发送；发布 rev1 后，旧快照在实际发送前必须被拒，
// 两个假厂商都不得新增调用；已发出的在途请求可完成；新读取取 rev1 完整套件。
func TestProviderSnapshotLegacyGateAfterPublication(t *testing.T) {
	ctx := context.Background()
	vA, started, release, hitsA := blockingVendor(t, "legacy-inflight-reply")
	vB := newSnapshotVendor(t)
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")
	if _, err := f.d.SQL.ExecContext(ctx,
		`UPDATE ai_provider SET base_url=? WHERE id=?`, vA.URL, f.providerID); err != nil {
		t.Fatal(err)
	}
	snap0, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil {
		t.Fatalf("旧安装快照: %v", err)
	}
	if snap0.Revision() != 0 || snap0.Spec.BaseURL != vA.URL {
		t.Fatalf("旧快照 = %+v", snap0)
	}
	if err := Gate(ctx, f.d, snap0.Revision()); err != nil {
		t.Fatalf("首次发布前旧快照应放行: %v", err)
	}
	pOld, err := NewProviderWithTransport(snap0.Spec, 5*time.Second, vA.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	pOld.SetEgressGate(func(gctx context.Context) error { return Gate(gctx, f.d, snap0.Revision()) })

	// 旧套请求已经发出（在途），此时首次发布 rev1。
	inflight := make(chan error, 1)
	go func() {
		_, cerr := pOld.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "ping"}}})
		inflight <- cerr
	}()
	<-started
	obj, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey, f.providerID, "deepseek", "sk-new-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.AIPublishConfig(ctx, store.AIPublishRequest{
		ProviderID: f.providerID, ModelRowID: f.modelRowID,
		BaseURL: vB.srv.URL, ModelID: "model-b",
		CredentialRef: obj.Ref, CredentialChanged: true, CredentialMasked: obj.Masked,
		Source: "admin_save", Actor: "tester",
	}); err != nil {
		t.Fatalf("首次发布: %v", err)
	}
	close(release)
	if err := <-inflight; err != nil {
		t.Fatalf("已发出的在途请求应能完成: %v", err)
	}
	if got := atomic.LoadInt32(hitsA); got != 1 || vB.hits() != 0 {
		t.Fatalf("在途完成后的出站 A=%d B=%d，期望 1/0", got, vB.hits())
	}

	// 旧快照的最后一跳必须拒绝；旧 provider 不得再发出任何请求。
	if err := Gate(ctx, f.d, snap0.Revision()); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("首发后 rev0 Gate = %v，期望 StaleConfigError", err)
	}
	if err := chatOnce(t, pOld); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("首发后旧套发送 = %v，期望 stale", err)
	}
	if got := atomic.LoadInt32(hitsA); got != 1 || vB.hits() != 0 {
		t.Fatalf("首发后旧套仍发出请求：A=%d B=%d，期望 1/0", got, vB.hits())
	}

	// 新读取取 rev1 完整套件，并能实际发送到 B。
	snap1, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
	if err != nil || snap1.Revision() != 1 || snap1.Spec.BaseURL != vB.srv.URL ||
		snap1.Spec.ModelID != "model-b" || snap1.Spec.APIKey != "sk-new-b" {
		t.Fatalf("首发后新快照 = %+v, err=%v", snap1, err)
	}
	if err := Gate(ctx, f.d, snap1.Revision()); err != nil {
		t.Fatalf("新修订 Gate: %v", err)
	}
	pNew, err := NewProviderWithTransport(snap1.Spec, 5*time.Second, vB.srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	pNew.SetEgressGate(func(gctx context.Context) error { return Gate(gctx, f.d, snap1.Revision()) })
	if err := chatOnce(t, pNew); err != nil {
		t.Fatalf("新套发送: %v", err)
	}
	if vB.hits() != 1 || atomic.LoadInt32(hitsA) != 1 {
		t.Fatalf("新套出站 A=%d B=%d，期望 1/1", atomic.LoadInt32(hitsA), vB.hits())
	}
}

type snapshotVendor struct {
	srv *httptest.Server
	n   int32
}

func newSnapshotVendor(t *testing.T) *snapshotVendor {
	t.Helper()
	v := &snapshotVendor{}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&v.n, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *snapshotVendor) hits() int { return int(atomic.LoadInt32(&v.n)) }

// urlHostTransport 按请求主机分发到不同本地 fixture，模拟「同一 Runner 先后
// 读到 rev0/rev1 两套配置」时的实际出站目标。
type urlHostTransport map[string]http.RoundTripper

func (m urlHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, ok := m[req.URL.Host]
	if !ok {
		return nil, errors.New("未注册的测试主机 " + req.URL.Host)
	}
	return tr.RoundTrip(req)
}

// TestProviderSnapshotRunnerLegacyFirstPublication Runner 与后台复测共用同一批读者：
// 首次发布前已发出的 rev0 请求可完成；同一 Runner 的下一次发送读 rev1 并打到新域；
// 旧快照 Gate 变 stale。后台复测按同一 LoadSnapshot+Gate 读取（见 admin 发布用例）。
func TestProviderSnapshotRunnerLegacyFirstPublication(t *testing.T) {
	ctx := context.Background()
	vA, started, release, hitsA := blockingVendor(t, "legacy-inflight-reply")
	vB := newSnapshotVendor(t)
	f := newSnapshotFixture(t)
	f.seedLegacySecret(t, "sk-legacy-db")
	if _, err := f.d.SQL.ExecContext(ctx,
		`UPDATE ai_provider SET base_url=? WHERE id=?`, vA.URL, f.providerID); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(f.d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5, MasterKeyFile: f.keyFile},
		f.masterKey, logging.Discard())
	r.providerTransport = urlHostTransport{
		hostOfURL(t, vA.URL):     vA.Client().Transport,
		hostOfURL(t, vB.srv.URL): vB.srv.Client().Transport,
	}
	// 第一条：rev0 旧套请求发出后在途；此时首次发布 rev1。
	taskID1, err := f.d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "fishguide",
		ContactUID: 2147483644, SourceKind: "human", Text: "停前的问题",
	})
	if err != nil {
		t.Fatal(err)
	}
	task1, ok, err := f.d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handle(ctx, task1)
	}()
	<-started
	obj, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey, f.providerID, "deepseek", "sk-new-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.AIPublishConfig(ctx, store.AIPublishRequest{
		ProviderID: f.providerID, ModelRowID: f.modelRowID,
		BaseURL: vB.srv.URL, ModelID: "model-b",
		CredentialRef: obj.Ref, CredentialChanged: true, CredentialMasked: obj.Masked,
		Source: "admin_save", Actor: "tester",
	}); err != nil {
		t.Fatalf("首次发布: %v", err)
	}
	close(release)
	<-done
	var status1, reply1 string
	if err := f.d.SQL.QueryRow(`SELECT status, reply_text FROM ai_result WHERE task_id=?`, taskID1).
		Scan(&status1, &reply1); err != nil {
		t.Fatal(err)
	}
	if status1 != "done" || !strings.Contains(reply1, "legacy-inflight-reply") {
		t.Fatalf("已发出的在途请求应完成，得到 (%q,%q)", status1, reply1)
	}
	if atomic.LoadInt32(hitsA) != 1 || vB.hits() != 0 {
		t.Fatalf("在途出站 A=%d B=%d，期望 1/0", atomic.LoadInt32(hitsA), vB.hits())
	}
	// 同一 Runner 的下一条任务：读 rev1 并发送到 B。
	if err := Gate(ctx, f.d, 0); !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("首发后 rev0 Gate = %v，期望 stale", err)
	}
	taskID2, err := f.d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "fishguide",
		ContactUID: 2147483644, SourceKind: "human", Text: "新配置",
	})
	if err != nil {
		t.Fatal(err)
	}
	task2, ok, err := f.d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task2)
	var status2, reply2 string
	if err := f.d.SQL.QueryRow(`SELECT status, reply_text FROM ai_result WHERE task_id=?`, taskID2).
		Scan(&status2, &reply2); err != nil {
		t.Fatal(err)
	}
	if status2 != "done" || !strings.Contains(reply2, "ok") {
		t.Fatalf("首次发布后 Runner 未打到新域: (%q,%q)", status2, reply2)
	}
	if vB.hits() != 1 || atomic.LoadInt32(hitsA) != 1 {
		t.Fatalf("新套出站 A=%d B=%d，期望 1/1", atomic.LoadInt32(hitsA), vB.hits())
	}
}

func hostOfURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("解析测试地址 %q: %v", raw, err)
	}
	return u.Host
}

// TestCredentialInstallDurability 票 §2.1.1：确认安装持久性后才允许发布。
//   - 支持目录 fsync 的平台：对象创建、发布成功；
//   - 目录同步失败（含首次创建父目录）：对象创建返回不可持久错误，活动修订不变；
//   - 故障移除后重试一次成功；
//   - 真实目录 I/O 安装后，以原主密钥及厂商绑定读回。
func TestCredentialInstallDurability(t *testing.T) {
	ctx := context.Background()

	t.Run("目录同步失败保留旧套", func(t *testing.T) {
		f := newSnapshotFixture(t)
		f.seedLegacySecret(t, "sk-legacy-db")
		f.publish(t, "https://b.example.com/v1", "model-b", "sk-new-b", 0)
		SetCredentialDirSyncForTest(func(string) error { return errors.New("sync boom") })
		defer SetCredentialDirSyncForTest(func(string) error { return nil })
		if _, err := CreateCredentialObject(CredentialDir(f.keyFile), f.masterKey,
			f.providerID, "deepseek", "sk-new-c"); !errors.Is(err, ErrCredentialInstallNotDurable) {
			t.Fatalf("目录同步失败应返回不可持久错误，得到 %v", err)
		}
		snap, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
		if err != nil || snap.Revision() != 1 || snap.Spec.ModelID != "model-b" {
			t.Fatalf("失败后旧套 = %+v, err=%v", snap, err)
		}
		if got, err := ActiveRevision(ctx, f.d); err != nil || got != 1 {
			t.Fatalf("失败后活动修订 = %d, err=%v", got, err)
		}
		// 故障移除后重试一次成功。
		SetCredentialDirSyncForTest(func(string) error { return nil })
		f.publish(t, "https://c.example.com/v1", "model-c", "sk-new-c", 1)
		if got, err := ActiveRevision(ctx, f.d); err != nil || got != 2 {
			t.Fatalf("故障移除后修订 = %d, err=%v，期望 2", got, err)
		}
	})

	t.Run("首次创建目录的父目录同步失败", func(t *testing.T) {
		f := newSnapshotFixture(t)
		f.seedLegacySecret(t, "sk-legacy-db")
		credDir := CredentialDir(f.keyFile)
		SetCredentialDirSyncForTest(func(dir string) error {
			if filepath.Clean(dir) == filepath.Clean(filepath.Dir(credDir)) {
				return errors.New("parent sync boom")
			}
			return nil
		})
		defer SetCredentialDirSyncForTest(func(string) error { return nil })
		if _, err := CreateCredentialObject(credDir, f.masterKey, f.providerID,
			"deepseek", "sk-first"); !errors.Is(err, ErrCredentialInstallNotDurable) {
			t.Fatalf("父目录同步失败应返回不可持久错误，得到 %v", err)
		}
		// 目录已被上次失败留下：不能据此跳过父目录确认，故障持续时每次都拒绝。
		if _, err := os.Stat(credDir); err != nil {
			t.Fatalf("失败后凭据目录应存在（复现前提）: %v", err)
		}
		if _, err := CreateCredentialObject(credDir, f.masterKey, f.providerID,
			"deepseek", "sk-retry"); !errors.Is(err, ErrCredentialInstallNotDurable) {
			t.Fatalf("父目录持续失败时重试应仍被拒绝，得到 %v", err)
		}
		if _, found, _ := f.d.AIConfigActiveRaw(ctx); found {
			t.Fatal("父目录同步持续失败后仍发布活动指针")
		}
		snap, err := LoadSnapshot(ctx, f.d, f.keyFile, f.masterKey)
		if err != nil || snap.Revision() != 0 || snap.Spec.APIKey != "sk-legacy-db" {
			t.Fatalf("失败后旧安装快照 = %+v, err=%v", snap, err)
		}
		// 故障移除后重试一次成功。
		SetCredentialDirSyncForTest(func(string) error { return nil })
		obj, err := CreateCredentialObject(credDir, f.masterKey, f.providerID, "deepseek", "sk-recovered")
		if err != nil || obj.Ref == "" {
			t.Fatalf("故障移除后创建对象失败: %+v, %v", obj, err)
		}
	})

	t.Run("真实平台写入密文对象", func(t *testing.T) {
		f := newSnapshotFixture(t)
		defer SetCredentialDirSyncForTest(nil)()
		dir := CredentialDir(f.keyFile)
		obj, err := CreateCredentialObject(dir, f.masterKey, f.providerID, "deepseek", "sk-real-platform")
		if err != nil {
			t.Fatal(err)
		}
		if _, plain, err := ResolveCredentialObject(dir, obj.Ref, f.masterKey, f.providerID); err != nil || plain != "sk-real-platform" {
			t.Fatalf("真实发布对象读回失败: %v", err)
		}
	})
}

// Repeated references must fail atomically even if the random source collides.
func TestT102CredentialObjectCollisionKeepsActiveBytes(t *testing.T) {
	t.Cleanup(SetCredentialDirSyncForTest(func(string) error { return nil }))
	reader := rand.Reader
	rand.Reader = bytes.NewReader(bytes.Repeat([]byte{1}, 256))
	t.Cleanup(func() { rand.Reader = reader })
	dir := filepath.Join(t.TempDir(), "ai_credentials.d")
	key := []byte("0123456789abcdef0123456789abcdef")
	first, err := CreateCredentialObject(dir, key, 7, "deepseek", "synthetic-first")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, first.Ref+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, collision := CreateCredentialObject(dir, key, 7, "deepseek", "synthetic-second")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if collision == nil || !bytes.Equal(after, original) {
		t.Fatalf("reference collision changed active bytes=%v, collision error=%v", !bytes.Equal(after, original), collision)
	}
}
