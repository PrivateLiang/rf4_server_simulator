package aibridge

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFishNameUsesRealNames(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fish.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE loc_fish(loc TEXT, fish_key TEXT, name_zh TEXT, catches INTEGER);
		INSERT INTO loc_fish VALUES('l','a','鲈鱼',1),('l','b','梅花鲈',1),('l','c','常见拟鲤',1);`); err != nil {
		t.Fatal(err)
	}
	k := &Knowledge{static: db}
	ctx := context.Background()
	if got := k.ResolveFishName(ctx, "河鲈在哪钓"); got != "鲈鱼" {
		t.Fatalf("别名未归一化: %q", got)
	}
	if got := k.ResolveFishName(ctx, "常见拟鲤在哪"); got != "常见拟鲤" {
		t.Fatalf("精确名未命中: %q", got)
	}
	if got := (&Knowledge{}).ResolveFishName(ctx, "河鲈"); got != "" {
		t.Fatalf("无静态库时应返回空: %q", got)
	}
}

// 出网检索前去掉长数字与长标识符。
func TestSanitizeSearchQuery(t *testing.T) {
	got := sanitizeSearchQuery("玩家 123456789 的 token=abcdefghijklmnopqrstuvwxyz012345 怎么钓")
	if got == "" || got == "玩家 123456789 的 token=abcdefghijklmnopqrstuvwxyz012345 怎么钓" {
		t.Fatalf("未脱敏: %q", got)
	}
}

// OpenKnowledge 必须只读打开：文件不存在时报错，不能在静态库位置创建空库。
func TestOpenKnowledgeMissingFileDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-static.sqlite3")
	if _, err := OpenKnowledge(path); err == nil {
		t.Fatal("缺失静态库应返回错误")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("不应创建空静态库: err=%v", err)
	}
}
