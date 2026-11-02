package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Segment 是 segment 表的一行：某集的播放身份。
//
// 「每集一片」之后一集恰好一个片段，行里就只剩「哪一集、哪个通道」：通道决定分片
// 缓存键（同一集的网页源与 App 源字节流不同）与 ffmpeg 的出站身份，别的都不再需要
// （docs/adr/0011-episode-per-segment-fmp4.md）。
type Segment struct {
	EpisodeID int64
	Channel   string
}

// GetSegment 返回某集的播放身份。无行返回 ErrNotFound。
//
// 「这一集还没有通道行」是正常状态（还没人播过，或者上次播放没走到写行那一步），
// 由调用方现解析一次直链补上。
func (s *Store) GetSegment(ctx context.Context, episodeID int64) (Segment, error) {
	var seg Segment
	err := s.db.QueryRowContext(ctx,
		`SELECT episode_id, channel FROM segment WHERE episode_id = $1`, episodeID).
		Scan(&seg.EpisodeID, &seg.Channel)
	if errors.Is(err, sql.ErrNoRows) {
		return Segment{}, ErrNotFound
	}
	if err != nil {
		return Segment{}, fmt.Errorf("读取分片身份 episode_id=%d: %w", episodeID, err)
	}
	return seg, nil
}

// UpsertSegment 写入或刷新某集的播放身份。
//
// 通道翻转（web ↔ app）时覆盖：分片键里带通道，换了通道的片本来就该重新生成，
// 旧片留在缓存里由 LRU 淘汰（键不同，永不命中）。
func (s *Store) UpsertSegment(ctx context.Context, seg Segment) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO segment(episode_id, channel) VALUES($1, $2)
		 ON CONFLICT(episode_id) DO UPDATE SET channel = excluded.channel`,
		seg.EpisodeID, seg.Channel,
	); err != nil {
		return wrapWriteErr("写入分片身份", err)
	}
	return nil
}
