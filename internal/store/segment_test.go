package store

import (
	"context"
	"errors"
	"testing"
)

// newSegmentTestEpisode 造一部剧 + 一集，返回 episode.id（segment 行的身份）。
func newSegmentTestEpisode(t *testing.T, s *Store, providerID string) int64 {
	t.Helper()
	ctx := context.Background()
	dramaID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: providerID, Title: "分片身份剧"})
	if err != nil {
		t.Fatalf("UpsertDrama %s: %v", providerID, err)
	}
	epID, err := s.UpsertEpisode(ctx, Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode %s: %v", providerID, err)
	}
	return epID
}

// AC：segment 行以 episode_id 为身份，写入即覆盖通道、读不到返回 ErrNotFound。
func TestUpsertSegment(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	s := NewStore(db.DB)
	ep := newSegmentTestEpisode(t, s, "seg-web")

	if _, err := s.GetSegment(ctx, ep); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中的错误 = %v, 期望 ErrNotFound", err)
	}

	if err := s.UpsertSegment(ctx, Segment{EpisodeID: ep, Channel: "web"}); err != nil {
		t.Fatalf("UpsertSegment: %v", err)
	}
	got, err := s.GetSegment(ctx, ep)
	if err != nil {
		t.Fatalf("GetSegment: %v", err)
	}
	if want := (Segment{EpisodeID: ep, Channel: "web"}); got != want {
		t.Fatalf("读回 = %+v, 期望 %+v", got, want)
	}

	// 通道翻转：同一行被覆盖，不新增行。
	if err := s.UpsertSegment(ctx, Segment{EpisodeID: ep, Channel: "app"}); err != nil {
		t.Fatalf("UpsertSegment(翻通道): %v", err)
	}
	got, err = s.GetSegment(ctx, ep)
	if err != nil {
		t.Fatalf("GetSegment(翻通道后): %v", err)
	}
	if want := (Segment{EpisodeID: ep, Channel: "app"}); got != want {
		t.Fatalf("翻通道后读回 = %+v, 期望 %+v", got, want)
	}
	if n := countRows(t, s.db, "segment"); n != 1 {
		t.Fatalf("segment 行数 = %d, 期望 1（一集一行）", n)
	}
}

// AC：channel 只接受 web / app（schema CHECK 兜住，写错值不留行）。
func TestUpsertSegmentRejectsUnknownChannel(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newCRUDTestDB(t).DB)
	ep := newSegmentTestEpisode(t, s, "seg-bad-channel")

	if err := s.UpsertSegment(ctx, Segment{EpisodeID: ep, Channel: "hls"}); err == nil {
		t.Fatal("未知通道写入未报错（CHECK 未生效）")
	}
	if n := countRows(t, s.db, "segment"); n != 0 {
		t.Fatalf("拒绝未知通道后行数 = %d, 期望 0", n)
	}
}

// AC：删集级联删掉它的 segment 行（派生数据不跨集残留）。
func TestSegmentCascadesWithEpisode(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newCRUDTestDB(t).DB)
	ep := newSegmentTestEpisode(t, s, "seg-cascade")
	if err := s.UpsertSegment(ctx, Segment{EpisodeID: ep, Channel: "web"}); err != nil {
		t.Fatalf("UpsertSegment: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM episode WHERE id = $1`, ep); err != nil {
		t.Fatalf("删集: %v", err)
	}
	if n := countRows(t, s.db, "segment"); n != 0 {
		t.Fatalf("删集后 segment 行数 = %d, 期望 0", n)
	}
}
