package vfs

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// CategoryDirs 供 Jellyfin 建库：口径必须与根目录列举逐字一致（转义 + 「其他」兜底），
// 否则库会指向一个不存在的目录（MKACG-654）。
//
// 与被测的 CategoryDirs 一样单独成文件：MKACG-651 正在大改 tree_test.go 的树形断言，
// 把本用例留在那里会让两条分支在同一个 hunk（甚至同一行 import）上冲突。
func TestCategoryDirs(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	nodes, err := f.tree.List(ctx, "")
	if err != nil {
		t.Fatalf("List(根): %v", err)
	}
	dirs, err := f.tree.CategoryDirs(ctx)
	if err != nil {
		t.Fatalf("CategoryDirs: %v", err)
	}
	if len(dirs) != len(nodes) {
		t.Fatalf("CategoryDirs 有 %d 个, 根目录有 %d 个：%v vs %v", len(dirs), len(nodes), dirs, nodes)
	}
	for i, node := range nodes {
		if want := strings.TrimPrefix(node.Path, Root+"/"); dirs[i] != want {
			t.Errorf("CategoryDirs[%d] = %q, 期望 %q（与根目录同序同名）", i, dirs[i], want)
		}
	}
	// fixture：剧 A 有分类「爱情」，剧 B 不声明分类 → 兜底目录「其他」。
	for _, want := range []string{genreA, otherGenre} {
		if !slices.Contains(dirs, want) {
			t.Errorf("CategoryDirs 缺少 %q：%v", want, dirs)
		}
	}
}
