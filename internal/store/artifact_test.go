package store

import (
	"context"
	"errors"
	"testing"
)

// AC：artifact 索引行按 (kind, key) 读写；未命中原样返回 ErrNotFound，同键 upsert 覆盖。
func TestArtifactRoundTrip(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	if _, err := s.ArtifactByKey(ctx, ArtifactKindCover, "web/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中的错误 = %v, 期望 ErrNotFound", err)
	}

	want := Artifact{
		Kind: ArtifactKindCover, Key: "web/p1",
		Path: "/data/cache/cover/aa/aa.jpg", Bytes: 100,
		SHA256: "deadbeef", LastUsedAt: "2026-10-30T09:00:00Z",
	}
	if err := s.PutArtifact(ctx, want); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	got, err := s.ArtifactByKey(ctx, want.Kind, want.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if got != want {
		t.Fatalf("读回 = %+v, 期望 %+v", got, want)
	}

	// 同键（同内容）重写：覆盖为新的 path/last_used_at，行数不变。
	updated := want
	updated.Bytes = 200
	updated.LastUsedAt = "2026-10-30T10:00:00Z"
	if err := s.PutArtifact(ctx, updated); err != nil {
		t.Fatalf("PutArtifact(覆盖): %v", err)
	}
	got, err = s.ArtifactByKey(ctx, updated.Kind, updated.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey(覆盖后): %v", err)
	}
	if got != updated {
		t.Fatalf("覆盖后读回 = %+v, 期望 %+v", got, updated)
	}
	if n := countRows(t, s.db, "artifact"); n != 1 {
		t.Fatalf("artifact 行数 = %d, 期望 1", n)
	}

	// 同一个 key 不同 kind 是两行（主键是 (kind, key)）。
	index := Artifact{Kind: ArtifactKindIndex, Key: want.Key, Path: "/data/cache/index/aa/aa.json",
		Bytes: 1, LastUsedAt: "2026-10-30T10:00:00Z"}
	if err := s.PutArtifact(ctx, index); err != nil {
		t.Fatalf("PutArtifact(index): %v", err)
	}
	if n := countRows(t, s.db, "artifact"); n != 2 {
		t.Fatalf("artifact 行数 = %d, 期望 2", n)
	}
}

// MKACG-650：ReleaseArtifact 把行降级为「只有登记、没有本地字节」——path / bytes /
// sha256 清空、last_used_at 推到给定时刻，行留着。行不存在返回 ErrNotFound。
//
// 这一行不能删：它是「这部剧有封面」的唯一真值（`vfs.HasCover` 只看行），
// 删掉 poster.jpg 就永远从剧目录里消失，「淘汰后按需回源」没有入口。
func TestReleaseArtifactKeepsRow(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	want := Artifact{
		Kind: ArtifactKindCover, Key: "web/p1",
		Path: "/data/cache/cover/aa/aa.jpg", Bytes: 100,
		SHA256: "deadbeef", LastUsedAt: "2026-10-30T09:00:00Z",
	}
	if err := s.PutArtifact(ctx, want); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	const at = "2026-11-02T05:00:00Z"
	if err := s.ReleaseArtifact(ctx, want.Kind, want.Key, at); err != nil {
		t.Fatalf("ReleaseArtifact: %v", err)
	}

	got, err := s.ArtifactByKey(ctx, want.Kind, want.Key)
	if err != nil {
		t.Fatalf("释放后行 = %v, 期望保留", err)
	}
	if got.Path != "" || got.Bytes != 0 || got.SHA256 != "" {
		t.Fatalf("释放后行 = %+v, 期望 path/bytes/sha256 清空", got)
	}
	if got.LastUsedAt != at {
		t.Fatalf("释放后 last_used_at = %q, 期望 %q", got.LastUsedAt, at)
	}
	if n := countRows(t, s.db, "artifact"); n != 1 {
		t.Fatalf("artifact 行数 = %d, 期望 1（行必须留着）", n)
	}

	// 释放过的行还能被 PutArtifact 覆盖回真实值（按需回源成功后写回）。
	if err := s.PutArtifact(ctx, want); err != nil {
		t.Fatalf("释放后 PutArtifact: %v", err)
	}
	if back, err := s.ArtifactByKey(ctx, want.Kind, want.Key); err != nil || back != want {
		t.Fatalf("写回后行 = (%+v, %v), 期望 %+v", back, err, want)
	}

	// 行不存在时返回 ErrNotFound。
	if err := s.ReleaseArtifact(ctx, ArtifactKindCover, "web/missing", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在行的 ReleaseArtifact = %v, 期望 ErrNotFound", err)
	}
}
