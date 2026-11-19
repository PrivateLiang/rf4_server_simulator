package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 新角色必须能分散到多张地图，不能都落在玩家当前图。
func TestAIRandomMapSpreadsAcrossMaps(t *testing.T) {
	static := filepath.Join("..", "..", "server_data", "static.sqlite3")
	if _, err := os.Stat(static); err != nil {
		t.Skip("static.sqlite3 不存在，跳过")
	}
	d, err := OpenWithStatic(filepath.Join(t.TempDir(), "map.db"), static)
	if err != nil {
		t.Fatalf("OpenWithStatic: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		loc, name, err := d.AIRandomMap(ctx)
		if err != nil || loc == "" || name == "" {
			t.Fatalf("AIRandomMap: loc=%q name=%q err=%v", loc, name, err)
		}
		seen[loc] = true
	}
	if len(seen) < 2 {
		t.Fatalf("10 次随机只出现 %d 张地图: %v", len(seen), seen)
	}
}
