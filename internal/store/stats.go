package store

import (
	"context"
	"fmt"
)

// DramaStats 是一部剧的派生计数，供管理面的剧目列表与看板使用。
//
// 各项与表的关系：Episodes 是 episode 行数；EmbyItems 是按当前 merge_chunk_size
// 派生出的对外集数（= ceil(Episodes / N)，0 集 → 0），与 vfs / 播放面同一份规则
// （deriveEmbyItems），库里没有可数的派生表（docs/adr/0014）。
//
// 不再有「合并组数」：合并集是自动分块的派生结果（docs/adr/0010），没有可数的编排表。
// 也不再有「可播集数」：直链不落库（0004 已删 play_url），「这一集现在能不能播」
// 只有请求时向插件解析才知道，库里没有可数的真值。
type DramaStats struct {
	DramaID   int64
	Episodes  int
	EmbyItems int
}

// ListDramaStats 返回全部剧的派生计数，按 drama.id 升序。
//
// 一次聚合查询取全库的集数，再按当前 merge_chunk_size 在内存里算对外集数：
// 管理面列表页不做「每剧一条」的 N+1，也不再需要第二张表。
func (s *Store) ListDramaStats(ctx context.Context) ([]DramaStats, error) {
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id,
		       (SELECT COUNT(*) FROM episode e WHERE e.drama_id = d.id)
		FROM drama d
		ORDER BY d.id`)
	if err != nil {
		return nil, fmt.Errorf("统计剧目计数: %w", err)
	}
	defer rows.Close()

	out := make([]DramaStats, 0)
	for rows.Next() {
		var st DramaStats
		if err := rows.Scan(&st.DramaID, &st.Episodes); err != nil {
			return nil, fmt.Errorf("读取剧目计数行: %w", err)
		}
		if st.Episodes > 0 {
			st.EmbyItems = (st.Episodes + n - 1) / n
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历剧目计数: %w", err)
	}
	return out, nil
}
