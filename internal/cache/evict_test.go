package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// blob 生成 n 字节的可断言内容：首字节标记来源，其余填同一个值。
func blob(n int, marker byte) []byte {
	body := bytes.Repeat([]byte{marker}, n)
	return body
}

// AC：cache_max_mb 调到 1 MB 造超限 → 淘汰后容量落回上限内，且被淘汰的是
// last_used_at 最旧的那些（docs/CACHE-MODEL.md「测试要求」第 4 条）。
func TestEvictBySizeLimit(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	if err := tc.settings.Set(ctx, store.KeyCacheMaxMB, "1"); err != nil {
		t.Fatalf("设置 cache_max_mb: %v", err)
	}

	// 三个 400 KB 的分片（共 1.2 MB > 1 MB），写入时间依次前进。
	const chunk = 400 << 10
	specs := make([]Spec, 0, 3)
	for seq := 0; seq < 3; seq++ {
		spec := segSpec("e" + strconv.Itoa(seq))
		if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
			return blob(chunk, byte('a'+seq)), nil
		}); err != nil {
			t.Fatalf("写第 %d 个分片: %v", seq, err)
		}
		specs = append(specs, spec)
		tc.clock.advance(time.Minute)
	}

	before, err := tc.st.SumArtifactBytes(ctx, store.ArtifactKindSegment)
	if err != nil {
		t.Fatalf("SumArtifactBytes: %v", err)
	}
	if before != 3*chunk {
		t.Fatalf("淘汰前总量 = %d, 期望 %d", before, 3*chunk)
	}

	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	after, err := tc.st.SumArtifactBytes(ctx, store.ArtifactKindSegment)
	if err != nil {
		t.Fatalf("SumArtifactBytes: %v", err)
	}
	t.Logf("淘汰前 %d 字节 → 淘汰后 %d 字节（上限 %d）", before, after, 1<<20)
	if after > 1<<20 {
		t.Fatalf("淘汰后总量 = %d, 仍超过上限 1 MB", after)
	}

	// 最久未用的 seq 0 先走：行与文件都不在。
	if _, err := tc.st.ArtifactByKey(ctx, specs[0].Kind, specs[0].Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("最久未用的分片行 = %v, 期望 ErrNotFound", err)
	}
	for _, spec := range specs[1:] {
		row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
		if err != nil {
			t.Fatalf("较新的分片行被误删: %v", err)
		}
		if _, err := os.Stat(row.Path); err != nil {
			t.Fatalf("较新的分片文件被误删: %v", err)
		}
	}
	if files := contentFiles(t, tc.root); len(files) != 2 {
		t.Fatalf("淘汰后内容文件数 = %d, 期望 2：%v", len(files), files)
	}

	// 再跑一轮：已在上限内，不该再删东西。
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("第二轮 Evict: %v", err)
	}
	if files := contentFiles(t, tc.root); len(files) != 2 {
		t.Fatalf("第二轮淘汰删多了：%v", files)
	}
}

// cache_max_mb = 0 表示不按大小淘汰：只受 cache_lru_days 约束。
func TestEvictSizeDisabled(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	if err := tc.settings.Set(ctx, store.KeyCacheMaxMB, "0"); err != nil {
		t.Fatalf("设置 cache_max_mb: %v", err)
	}
	const chunk = 2 << 20 // 2 MB，已远超「0 MB 上限」的字面意思
	spec := segSpec(testUID)
	if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		return blob(chunk, 'z'), nil
	}); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); err != nil {
		t.Fatalf("cache_max_mb=0 仍做了大小淘汰: %v", err)
	}
}

// AC：cache_lru_days 过期 —— last_used_at 改到 40 天前的条目，淘汰后行与文件都不在，
// 没到期的条目原样保留（docs/CACHE-MODEL.md「测试要求」第 5 条）。
func TestEvictExpired(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	now := tc.clock.now()

	// 40 天前写下的两种 kind 各一条（过期是「多久没命中」，与 kind 无关）。
	oldTime := now.AddDate(0, 0, -40)
	tc.clock.set(oldTime)
	oldSpecs := []Spec{
		segSpec(testUID),
		{Kind: store.ArtifactKindCover, Key: CoverKey("web", "p1"), Ext: ".jpg"},
	}
	for _, spec := range oldSpecs {
		if _, err := tc.c.GetOrGenerate(ctx, spec, genOf("old")); err != nil {
			t.Fatalf("写 %s: %v", spec.Kind, err)
		}
	}
	// 10 天前写下的一条分片：不该被删。
	tc.clock.set(now.AddDate(0, 0, -10))
	fresh := segSpec("e2")
	if _, err := tc.c.GetOrGenerate(ctx, fresh, genOf("fresh")); err != nil {
		t.Fatalf("写新分片: %v", err)
	}
	freshRow, err := tc.st.ArtifactByKey(ctx, fresh.Kind, fresh.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}

	tc.clock.set(now)
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	// 分片：行与文件都不在。
	for _, spec := range oldSpecs[:1] {
		if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s 过期后行 = %v, 期望 ErrNotFound", spec.Kind, err)
		}
	}
	// 海报：文件删掉，**行留着**（行是「这部剧有封面」的真值，MKACG-650）；
	// path 清空表示本地没有字节，下一轮过期查询不会又选中它（last_used_at 已推到当下）。
	coverSpec := oldSpecs[1]
	coverRow, err := tc.st.ArtifactByKey(ctx, coverSpec.Kind, coverSpec.Key)
	if err != nil {
		t.Fatalf("海报过期后行 = %v, 期望保留（MKACG-650）", err)
	}
	if coverRow.Path != "" || coverRow.Bytes != 0 || coverRow.SHA256 != "" {
		t.Fatalf("过期海报行 = %+v, 期望 path/bytes/sha256 都清空", coverRow)
	}
	if coverRow.LastUsedAt != stamp(now) {
		t.Fatalf("过期海报行 last_used_at = %q, 期望 %q（推到当下，避免下一轮再选中）",
			coverRow.LastUsedAt, stamp(now))
	}
	if _, err := os.Stat(freshRow.Path); err != nil {
		t.Fatalf("未过期的分片文件被删了: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, fresh.Kind, fresh.Key); err != nil {
		t.Fatalf("未过期的分片行被删了: %v", err)
	}
	if files := contentFiles(t, tc.root); len(files) != 1 {
		t.Fatalf("过期淘汰后内容文件数 = %d, 期望 1：%v", len(files), files)
	}
}

// RunEviction 启动即跑一轮，ctx 取消后退出（不留 goroutine）。
func TestRunEvictionRunsThenStops(t *testing.T) {
	tc := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 先造一条 40 天前写入的分片，循环的第一轮就该清掉它。
	tc.clock.set(tc.clock.now().AddDate(0, 0, -40))
	spec := segSpec(testUID)
	if _, err := tc.c.GetOrGenerate(context.Background(), spec, genOf(testSegmentBody)); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	tc.clock.set(tc.clock.now().AddDate(0, 0, 40)) // 回到「现在」

	done := make(chan struct{})
	go func() {
		defer close(done)
		tc.c.RunEviction(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := tc.st.ArtifactByKey(context.Background(), spec.Kind, spec.Key)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第一轮淘汰没有清掉过期条目（最后错误：%v）", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunEviction 在 ctx 取消后没有退出")
	}
}

// 淘汰先删文件再删行；文件已经不在时算成功（自愈），行也一并清掉。
func TestEvictSelfHealsMissingFile(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID)
	if _, err := tc.c.GetOrGenerate(ctx, spec, genOf(testSegmentBody)); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删文件: %v", err)
	}

	tc.clock.advance(40 * 24 * time.Hour)
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("文件缺失的过期行 = %v, 期望 ErrNotFound", err)
	}
}

// tmp 里的临时文件超过 tmpMaxAge 由淘汰清扫（写盘被 kill 打断留下的半截文件）。
func TestEvictSweepsStaleTmp(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	tmpDir := filepath.Join(tc.root, dirTmp)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("建 tmp: %v", err)
	}
	now := tc.clock.now()
	stale := filepath.Join(tmpDir, ".tmp-stale")
	if err := os.WriteFile(stale, []byte("half written"), 0o644); err != nil {
		t.Fatalf("写半截文件: %v", err)
	}
	// mtime 显式钉住，不依赖真实时间：stale 比注入时钟早 2 小时（超过 tmpMaxAge），
	// fresh 就是「现在」。
	if err := os.Chtimes(stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("改半截文件的 mtime: %v", err)
	}
	fresh := filepath.Join(tmpDir, ".tmp-fresh")
	if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
		t.Fatalf("写临时文件: %v", err)
	}
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatalf("改临时文件的 mtime: %v", err)
	}

	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期临时文件没被清扫: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("仍在写入的临时文件被误删: %v", err)
	}
}

// MKACG-650 AC-6（系统淘汰子项）：cache_lru_days 把海报淘汰之后，那一行必须留着——
// 文件删掉、path/bytes 清空。行是「这部剧有封面」的唯一真值，连行删掉会让 poster.jpg
// 从剧目录里消失，请求根本到不了 Render，「按需重新下载」永远不发生。这正是功能检测
// 抓到的失败项。
func TestEvictExpiredCoverKeepsRow(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	now := tc.clock.now()

	tc.clock.set(now.AddDate(0, 0, -40))
	spec := Spec{Kind: store.ArtifactKindCover, Key: CoverKey("web", "p1"), Ext: ".jpg"}
	if _, err := tc.c.GetOrGenerate(ctx, spec, genOf("old cover")); err != nil {
		t.Fatalf("写封面: %v", err)
	}
	row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}

	tc.clock.set(now)
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	// 文件删掉。
	if _, err := os.Stat(row.Path); !os.IsNotExist(err) {
		t.Fatalf("过期封面的文件仍在: %v", err)
	}
	// 行留着，且已降级为「本地没有字节」；last_used_at 推到当下，下一轮不会再选中它。
	released, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("过期封面行 = %v, 期望保留（MKACG-650）", err)
	}
	if released.Path != "" || released.Bytes != 0 || released.SHA256 != "" {
		t.Fatalf("过期封面行 = %+v, 期望 path/bytes/sha256 都清空", released)
	}
	if released.LastUsedAt != stamp(now) {
		t.Fatalf("过期封面行 last_used_at = %q, 期望 %q", released.LastUsedAt, stamp(now))
	}

	// 再跑一轮：行已经不在过期区间，不该被第二轮再处理一次（旧实现会原地打转）。
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("第二轮 Evict: %v", err)
	}
	if again, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); err != nil || again != released {
		t.Fatalf("第二轮淘汰后行 = (%+v, %v), 期望与第一轮后一致", again, err)
	}
}

// MKACG-650 AC-6：走完系统淘汰路径后，下一次 Poster 仍然「有封面」，按需回源一次并写回
// 真实行，之后完全命中本地。端到端覆盖「淘汰 → HasCover 仍真 → Poster 未命中 → 回源 →
// 行写回」整条链路（上一条只验淘汰那一半）。
func TestPosterRefetchesAfterSystemEviction(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	tc := newTestCache(t) // 真库 + 临时 cache_dir + 可注入时钟
	c := NewCoverCache(tc.st, tc.settings, srv.Client(), slog.New(slog.DiscardHandler))
	c.now = tc.clock.now
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"
	key := CoverKey(testProvider, testProviderID)

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}

	// 走生产淘汰路径：把行改老 → Cache.Evict（与 5 分钟循环里的动作同一份实现）。
	tc.clock.set(tc.clock.now().AddDate(0, 0, -40))
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("第二次 LocalizeCover（只为 touch 到 40 天前）: %v", err)
	}
	tc.clock.set(tc.clock.now().AddDate(0, 0, 40))
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	// 淘汰之后这部剧**仍然**「有封面」：树里还有 poster.jpg，请求才有入口。
	if has, err := c.HasCover(ctx, testProvider, testProviderID); err != nil || !has {
		t.Fatalf("系统淘汰后 HasCover = (%v, %v), 期望仍 (true, nil)", has, err)
	}
	before := cdn.count()

	body, available, err := c.Poster(ctx, testProvider, testProviderID, url)
	if err != nil || !available {
		t.Fatalf("系统淘汰后 Poster available=%v err=%v, 期望按需回源成功", available, err)
	}
	if string(body) != string(coverBody) {
		t.Fatalf("回源字节 = %q, 期望 %q", body, coverBody)
	}
	if got := cdn.count(); got != before+1 {
		t.Fatalf("回源计数 %d → %d, 期望 +1", before, got)
	}

	after, err := tc.st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("回源后 ArtifactByKey: %v", err)
	}
	if after.Path == "" || after.Bytes != int64(len(coverBody)) {
		t.Fatalf("回源后行 = %+v, 期望 path 与 bytes 都写回", after)
	}
	if onDisk, err := os.ReadFile(after.Path); err != nil || string(onDisk) != string(coverBody) {
		t.Fatalf("回源后落盘字节 = %q（err=%v）, 期望 %q", onDisk, err, coverBody)
	}

	// 再请求一次：完全命中本地，不再回源。
	recount := cdn.count()
	if _, _, err := c.Poster(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("第二次 Poster: %v", err)
	}
	if got := cdn.count(); got != recount {
		t.Fatalf("回源成功后仍再回源：%d → %d", recount, got)
	}
}
