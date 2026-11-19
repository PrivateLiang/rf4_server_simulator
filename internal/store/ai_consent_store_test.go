package store

import (
	"context"
	"path/filepath"
	"testing"

	"rf4server/internal/aiconfig"
)

func openConsentDB(t *testing.T, name string) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func providerState(t *testing.T, d *DB, id int64) (base, model, dest string) {
	t.Helper()
	if err := d.SQL.QueryRow(`
		SELECT p.base_url, COALESCE(m.model_id,'')
		FROM ai_provider p LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.id=? ORDER BY m.id LIMIT 1`, id).Scan(&base, &model); err != nil {
		t.Fatalf("读取 provider 状态: %v", err)
	}
	dest = d.AISetting(context.Background(), aiconfig.KeyEgressDestination, "")
	return base, model, dest
}

// TestAISaveProviderDestinationAtomic 目的地快照写入失败时，
// 厂商地址/模型与快照必须一起回滚，不能留下「新目的地 + 旧授权」。
func TestAISaveProviderDestinationAtomic(t *testing.T) {
	d := openConsentDB(t, "consent-tx.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()
	id := seed.ProviderID
	destA, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://a.invalid/v1", ModelID: "m1",
	})
	if err := d.AISaveProviderDestination(ctx, id, seed.ModelRowID, "https://a.invalid/v1", "m1", destA); err != nil {
		t.Fatalf("初始化目的地失败: %v", err)
	}
	if _, err := d.SQL.Exec(`
		CREATE TRIGGER block_dest_update BEFORE UPDATE ON ai_setting
		WHEN NEW.key='ai_egress_destination'
		BEGIN SELECT RAISE(ABORT,'blocked destination'); END;`); err != nil {
		t.Fatalf("创建触发器: %v", err)
	}
	destB, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://b.invalid/v2", ModelID: "m2",
	})
	if err := d.AISaveProviderDestination(ctx, id, seed.ModelRowID, "https://b.invalid/v2", "m2", destB); err == nil {
		t.Fatal("快照写入被触发器阻断，事务应失败")
	}
	base, model, dest := providerState(t, d, id)
	if base != "https://a.invalid/v1" || model != "m1" {
		t.Fatalf("事务失败后厂商状态被部分修改: base=%q model=%q", base, model)
	}
	if dest != destA {
		t.Fatalf("事务失败后快照被部分修改: %q", dest)
	}
}

// TestAISetSettingsNoPartialConsent 授权与目的地快照同事务写入；
// 任一键失败时不得留下半套授权。
func TestAISetSettingsNoPartialConsent(t *testing.T) {
	d := openConsentDB(t, "consent-partial.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()
	id := seed.ProviderID
	destA, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://a.invalid/v1", ModelID: "m1",
	})
	if err := d.AISaveProviderDestination(ctx, id, seed.ModelRowID, "https://a.invalid/v1", "m1", destA); err != nil {
		t.Fatalf("初始化快照失败: %v", err)
	}
	if _, err := d.SQL.Exec(`
		CREATE TRIGGER block_dest_update2 BEFORE UPDATE ON ai_setting
		WHEN NEW.key='ai_egress_destination'
		BEGIN SELECT RAISE(ABORT,'blocked destination'); END;`); err != nil {
		t.Fatalf("创建触发器: %v", err)
	}
	key, _ := aiconfig.ConsentKeyFor(aiconfig.ConsentItemOutbound)
	rec, _ := aiconfig.NewConsentRecord(aiconfig.ConsentItemOutbound, "tester", 1700000000,
		aiconfig.ConsentDestination{BaseURL: "https://a.invalid/v1", ModelID: "m1"})
	raw, _ := aiconfig.MarshalConsent(rec)
	if err := d.AISetSettings(ctx, map[string]string{
		key: raw, aiconfig.KeyEgressDestination: destA,
	}); err == nil {
		t.Fatal("快照更新被触发器阻断，事务应失败")
	}
	if got := d.AISetting(ctx, key, ""); got != "" {
		t.Fatalf("事务失败后仍留下授权记录: %q", got)
	}
	if got := d.AISetting(ctx, aiconfig.KeyEgressDestination, ""); got != destA {
		t.Fatalf("事务失败后快照被修改: %q", got)
	}
}

// TestProviderDestinationSnapshotRoundTrip 快照可被 aiconfig 解析；
// V2 起目的地变化不再影响运行资格（只留急停），旧记录仅供兼容读取。
func TestProviderDestinationSnapshotRoundTrip(t *testing.T) {
	d := openConsentDB(t, "consent-scope.db")
	seed := seedAIDirtyData(t, d)
	ctx := context.Background()
	id := seed.ProviderID
	destA, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://a.invalid/v1", ModelID: "m1",
	})
	if err := d.AISaveProviderDestination(ctx, id, seed.ModelRowID, "https://a.invalid/v1", "m1", destA); err != nil {
		t.Fatal(err)
	}
	if _, ok := aiconfig.ParseDestination(d.AISetting(ctx, aiconfig.KeyEgressDestination, "")); !ok {
		t.Fatal("快照无法解析")
	}
	key, _ := aiconfig.ConsentKeyFor(aiconfig.ConsentItemOutbound)
	rec, _ := aiconfig.NewConsentRecord(aiconfig.ConsentItemOutbound, "tester", 1700000000,
		aiconfig.ConsentDestination{BaseURL: "https://a.invalid/v1", ModelID: "m1"})
	raw, _ := aiconfig.MarshalConsent(rec)
	if err := d.AISetSettings(ctx, map[string]string{
		key: raw, aiconfig.KeyOutboundEnabled: "1",
	}); err != nil {
		t.Fatal(err)
	}
	lookup := func(k string) string { return d.AISetting(ctx, k, "") }
	if got := aiconfig.Egress(lookup); !got.Allowed {
		t.Fatalf("同目的地应放行，得到 (%v,%q)", got.Allowed, got.State)
	}
	destB, _ := aiconfig.MarshalDestination(aiconfig.ConsentDestination{
		BaseURL: "https://b.invalid/v2", ModelID: "m2",
	})
	if err := d.AISaveProviderDestination(ctx, id, seed.ModelRowID, "https://b.invalid/v2", "m2", destB); err != nil {
		t.Fatal(err)
	}
	// 旧 consent 记录此时会显示范围已变化，但这不再阻止发送。
	if got := aiconfig.Egress(lookup); !got.Allowed {
		t.Fatalf("目的地变化不得阻止发送，得到 (%v,%q)", got.Allowed, got.State)
	}
	if got := aiconfig.EvaluateConsent(lookup, aiconfig.ConsentItemOutbound); got.Valid {
		t.Fatalf("旧记录应显示范围已变化（仅供展示）: %+v", got)
	}
}
