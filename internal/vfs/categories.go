package vfs

import (
	"context"
	"strings"
)

// CategoryDirs 列出根目录下的分类目录名（不带 `短剧/` 前缀），给 Jellyfin 建库用。
//
// 口径与根目录列举完全一致（转义 + 「其他」兜底 + 去重），不复制第二份规则：库名与库
// 路径必须与真实目录名逐字相等，差一个转义字符库就指向一个不存在的目录（ADR-0012）。
// 所以这里直接复用 listGenres，不自己读 drama.genre。
//
// 单独成文件是刻意的：本函数是 MKACG-654 对 vfs 的唯一增量，而 MKACG-651 正在大改
// tree.go（树形从四层变三层）。放同一个文件会让两条分支在同一个 hunk 上冲突，
// 合入顺序被一个纯格式冲突挡住；分文件后两边都能独立合入。
func (t *Tree) CategoryDirs(ctx context.Context) ([]string, error) {
	nodes, err := t.listGenres(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, strings.TrimPrefix(node.Path, Root+"/"))
	}
	return out, nil
}
