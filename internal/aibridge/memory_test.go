package aibridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/store"
)

func TestMemoryLine(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.AIAddMemory(ctx, "fishguide", "star", "perch", "鲈鱼", 1200, "lake"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIAddMemory(ctx, "fishguide", "super", "pike", "狗鱼", 4800, "lake"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{db: d}
	line := r.memoryLine(ctx, "fishguide")
	// 常驻记忆只记「什么鱼 + 什么档次」，不列精确重量：人记得住上过一条蓝冠狗鱼，
	// 记不住 4.80kg。精确数字归玩家真问时的 catchLine。
	for _, want := range []string{"蓝冠的狗鱼", "上星的鲈鱼"} {
		if !strings.Contains(line, want) {
			t.Fatalf("记忆缺 %q: %s", want, line)
		}
	}
	for _, unwanted := range []string{"4.80kg", "1.20kg", "kg"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("常驻记忆不该出现精确数字 %q: %s", unwanted, line)
		}
	}
	if len([]rune(line)) > 150 {
		t.Fatalf("记忆超长: %d", len([]rune(line)))
	}
	if got := r.memoryLine(ctx, "nobody"); got != "" {
		t.Fatalf("无记忆应返回空: %q", got)
	}
}
