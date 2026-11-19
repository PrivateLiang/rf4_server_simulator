package store

// T-012 存储层回归：旧库凭据写入停用；旧库密文只读（导出用），不改任何行。

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestAISecretWriteRetiredAndLegacyReadOnly 旧写路径必须显式失败且不落行；
// 只读导出接口返回原始密文、厂商关联与遮蔽值，可重复调用不改变库。
func TestAISecretWriteRetiredAndLegacyReadOnly(t *testing.T) {
	ctx := context.Background()
	d := openDirtyAIDB(t, "ai-secret-retired.db")
	var providerID int64
	if err := d.SQL.QueryRow(`SELECT id FROM ai_provider WHERE name='deepseek'`).Scan(&providerID); err != nil {
		t.Fatalf("读取种子厂商: %v", err)
	}

	// 停用写路径：显式错误、零行写入。
	if err := d.AISaveSecret(ctx, providerID, []byte{1, 2, 3}, []byte{4, 5, 6}, "sk-****1234"); !errors.Is(err, ErrAISecretWriteRetired) {
		t.Fatalf("AISaveSecret 错误 = %v，期望 ErrAISecretWriteRetired", err)
	}
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_secret`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停用的写路径仍写入 %d 行", n)
	}

	// 手工写入历史行（模拟旧库）：只读接口原样读出。
	ct := []byte{0xde, 0xad, 0xbe, 0xef}
	nonce := []byte{0x01, 0x02, 0x03}
	if _, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES(?,?,?,'default','sk-****5678',0)`, providerID, ct, nonce); err != nil {
		t.Fatal(err)
	}
	rows, err := d.AIListSecrets(ctx)
	if err != nil {
		t.Fatalf("列出旧密文: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("旧密文行 = %d，期望 1", len(rows))
	}
	row := rows[0]
	if row.ProviderID != providerID || row.Provider != "deepseek" ||
		!bytes.Equal(row.Ciphertext, ct) || !bytes.Equal(row.Nonce, nonce) || row.Masked != "sk-****5678" {
		t.Fatalf("只读导出行 = %+v", row)
	}
	gotCT, gotNonce, found, err := d.AISecretByName(ctx, "deepseek")
	if err != nil || !found || !bytes.Equal(gotCT, ct) || !bytes.Equal(gotNonce, nonce) {
		t.Fatalf("按厂商名读旧密文 = (%d, %d, %v, %v)", len(gotCT), len(gotNonce), found, err)
	}
	if _, _, found, err := d.AISecretByName(ctx, "no-such-provider"); err != nil || found {
		t.Fatalf("未知厂商 = (%v, %v)，期望 (false, nil)", found, err)
	}

	// 只读语义：再列一次，行数与密文都不变。
	rows2, err := d.AIListSecrets(ctx)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("第二次列出 = %d, %v", len(rows2), err)
	}
	if !bytes.Equal(rows2[0].Ciphertext, ct) || !bytes.Equal(rows2[0].Nonce, nonce) {
		t.Fatal("只读接口改动了旧密文")
	}
}
