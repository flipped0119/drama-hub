package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// coverBody 是假 CDN 返回的「海报」字节。内容只要非空且可断言即可。
var coverBody = []byte("\xff\xd8\xff\xe0 fake jpeg bytes")

const (
	testProvider   = "web"
	testProviderID = "7503745441910017049"
)

// fakeCDN 是可切换响应的假封面源，并记录被请求次数。
type fakeCDN struct {
	mu       sync.Mutex
	status   int
	body     []byte
	delay    time.Duration // 回包前的等待，只用于「让并发请求真的重叠」
	requests int
}

func (f *fakeCDN) set(status int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeCDN) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func newFakeCDN(t *testing.T) (*fakeCDN, *httptest.Server) {
	t.Helper()
	f := &fakeCDN{status: http.StatusOK, body: coverBody}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.requests++
		status, body, delay := f.status, f.body, f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "image/jpeg")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// newTestCoverCache 建临时库 + 临时 cache_dir，时钟固定在固定值上。
func newTestCoverCache(t *testing.T, srv *httptest.Server, cacheDir string) (*CoverCache, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.KeyCacheDir, cacheDir); err != nil {
		t.Fatalf("设置 cache_dir: %v", err)
	}
	c := NewCoverCache(st, settings, srv.Client(), slog.New(slog.DiscardHandler))
	c.now = func() time.Time { return time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC) }
	return c, st
}

// AC：同步后 artifact 有 kind='cover' 行，文件落在 <cache_dir>/cover/…，字段与实际
// 文件一致；再次本地化不回源（源图 404 也仍能取到本地海报）。
func TestLocalizeCoverWritesArtifactAndFile(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg"); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}

	key := testProvider + "/" + testProviderID
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if row.Kind != store.ArtifactKindCover || row.Key != key {
		t.Fatalf("artifact = %+v, 期望 kind=cover key=%s", row, key)
	}
	if row.Bytes != int64(len(coverBody)) {
		t.Fatalf("artifact.Bytes = %d, 期望 %d", row.Bytes, len(coverBody))
	}
	wantSum := sha256.Sum256(coverBody)
	if row.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("artifact.SHA256 = %q, 期望内容哈希", row.SHA256)
	}
	if row.LastUsedAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("artifact.LastUsedAt = %q, 期望注入时钟的 UTC RFC3339", row.LastUsedAt)
	}

	// 落盘布局：<cache_dir>/cover/<hash 前 2 位>/<hash>.jpg（docs/CACHE-MODEL.md）。
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	wantPath := filepath.Join(cacheDir, "cover", hash[:2], hash+".jpg")
	if row.Path != wantPath {
		t.Fatalf("artifact.Path = %q, 期望 %q", row.Path, wantPath)
	}
	onDisk, err := os.ReadFile(row.Path)
	if err != nil {
		t.Fatalf("读本地海报: %v", err)
	}
	if string(onDisk) != string(coverBody) {
		t.Fatalf("本地海报字节与上游不一致: %q", onDisk)
	}

	// 源图改成 404 后重跑：命中的是本地文件，不发请求、字段不变。
	cdn.set(http.StatusNotFound, nil)
	before := cdn.count()
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg"); err != nil {
		t.Fatalf("第二次 LocalizeCover（源图 404）: %v", err)
	}
	if got := cdn.count(); got != before {
		t.Fatalf("命中本地后仍回源：请求数 %d → %d", before, got)
	}
	after, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("第二次 ArtifactByKey: %v", err)
	}
	if after != row {
		t.Fatalf("命中后索引行变了：%+v → %+v", row, after)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("命中后本地文件不可用: %v", err)
	}
}

// 本地文件被删（索引行还在）时重新下载；源图 404 则报 ErrCoverFetch 且不写索引行。
func TestLocalizeCoverMissingFileRefetches(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删本地海报: %v", err)
	}

	cdn.set(http.StatusNotFound, []byte("nope"))
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); !errors.Is(err, ErrCoverFetch) {
		t.Fatalf("文件缺失且源图 404 的错误 = %v, 期望 ErrCoverFetch", err)
	}
	if got := cdn.count(); got != 2 {
		t.Fatalf("请求数 = %d, 期望 2（首次 + 文件缺失后重取）", got)
	}
	if _, err := os.Stat(row.Path); !os.IsNotExist(err) {
		t.Fatalf("下载失败却留下了文件: %v", err)
	}

	// 源图恢复后重取成功，索引行可读。
	cdn.set(http.StatusOK, coverBody)
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("恢复后 LocalizeCover: %v", err)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("重取后文件不在: %v", err)
	}
}

// 首次下载失败（500 / 空响应体）不写索引行，也不留半截文件。
func TestLocalizeCoverFailureLeavesNothing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
	}{
		{"上游 500", http.StatusInternalServerError, []byte("boom")},
		{"空响应体", http.StatusOK, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cdn, srv := newFakeCDN(t)
			cdn.set(tc.status, tc.body)
			cacheDir := filepath.Join(t.TempDir(), "cache")
			c, st := newTestCoverCache(t, srv, cacheDir)
			ctx := context.Background()

			err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg")
			if !errors.Is(err, ErrCoverFetch) {
				t.Fatalf("err = %v, 期望 ErrCoverFetch", err)
			}
			if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("失败后 artifact 行 = %v, 期望 ErrNotFound", err)
			}
			if entries := listFiles(t, cacheDir); len(entries) != 0 {
				t.Fatalf("失败后在缓存目录留下文件: %v", entries)
			}
		})
	}
}

// 上游未声明封面（url 为空）时无事可做：不发请求、不写行。
func TestLocalizeCoverEmptyURL(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	c, st := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, ""); err != nil {
		t.Fatalf("空 url 的 LocalizeCover = %v, 期望 nil", err)
	}
	if got := cdn.count(); got != 0 {
		t.Fatalf("空 url 仍发了 %d 次请求", got)
	}
	if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("空 url 后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
}

// AC（MKACG-620）：单次下载等多久由 settings 的 cover_timeout_sec 现读决定，
// 不再走代码常量——配 1 s 时，封面域名不回包最多等 1 s 就放弃。
func TestLocalizeCoverUsesConfiguredTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// 只回包不给响应头：客户端等到 ctx 超时。release 让 handler 在客户端放弃后
		// 也能立刻退出（srv.Close 会等所有 handler 结束）。
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	c, st := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()
	if err := c.settings.Set(ctx, store.KeyCoverTimeoutSec, "1"); err != nil {
		t.Fatalf("设置 cover_timeout_sec: %v", err)
	}

	start := time.Now()
	err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCoverFetch) {
		t.Fatalf("超时的错误 = %v, 期望 ErrCoverFetch", err)
	}
	// 上限：配置值 1 s。用默认值 5 s 或旧常量 15 s 都会超这条。
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("等待 %v, 超过 cover_timeout_sec = 1 s", elapsed)
	}
	// 下限：确实按配置值等，而不是立刻失败。
	if elapsed < 900*time.Millisecond {
		t.Fatalf("等待 %v, 期望接近配置的 1 s", elapsed)
	}
	t.Logf("封面下载等待 %v（cover_timeout_sec = 1 s）", elapsed.Round(time.Millisecond))
	if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("超时后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
}

// coverExt 只影响本地文件名：Content-Type 优先，其次 URL 后缀，最后 .img。
func TestCoverExt(t *testing.T) {
	cases := []struct{ contentType, url, want string }{
		{"image/jpeg", "https://cdn/x", ".jpg"},
		{"image/png; charset=binary", "https://cdn/x", ".png"},
		{"image/webp", "https://cdn/x", ".webp"},
		{"image/gif", "https://cdn/x", ".gif"},
		{"application/octet-stream", "https://cdn/x.webp", ".webp"},
		{"application/octet-stream", "https://cdn/x?sign=1", ".img"},
		{"", "https://cdn/very.long.suffix", ".img"},
	}
	for _, tc := range cases {
		if got := coverExt(tc.contentType, tc.url); got != tc.want {
			t.Errorf("coverExt(%q, %q) = %q, 期望 %q", tc.contentType, tc.url, got, tc.want)
		}
	}
}

// listFiles 返回目录树下的普通文件相对路径（不存在目录视为空）。
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历缓存目录: %v", err)
	}
	return out
}

// MKACG-650：poster.jpg 的内容取自本地封面缓存——命中时一个字节都不回源。
func TestPosterReadsLocalWithoutRefetch(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	c, _ := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}
	before := cdn.count()

	body, available, err := c.Poster(ctx, testProvider, testProviderID, url)
	if err != nil {
		t.Fatalf("Poster: %v", err)
	}
	if !available {
		t.Fatal("本地已有封面，Poster 却报不可用")
	}
	if string(body) != string(coverBody) {
		t.Fatalf("poster 字节 = %q, 期望上游字节 %q", body, coverBody)
	}
	if got := cdn.count(); got != before {
		t.Fatalf("命中本地后仍回源：请求数 %d → %d", before, got)
	}
}

// MKACG-650：封面文件被删（索引行还在）后请求 poster.jpg → 回源一次、返回完整字节，
// 并把索引行更新到新落的文件上（下一次请求不再回源）。
func TestPosterRefetchesWhenFileDeleted(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"
	key := testProvider + "/" + testProviderID

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删本地封面: %v", err)
	}
	before := cdn.count()

	body, available, err := c.Poster(ctx, testProvider, testProviderID, url)
	if err != nil || !available {
		t.Fatalf("Poster = (%q…, available=%v, %v), 期望回源成功", body, available, err)
	}
	if string(body) != string(coverBody) {
		t.Fatalf("回源字节 = %q, 期望 %q", body, coverBody)
	}
	if got := cdn.count(); got != before+1 {
		t.Fatalf("请求数 %d → %d, 期望 +1（按需回源一次）", before, got)
	}
	after, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("回源后 ArtifactByKey: %v", err)
	}
	if after.Bytes != int64(len(coverBody)) {
		t.Fatalf("回源后 artifact.Bytes = %d, 期望 %d", after.Bytes, len(coverBody))
	}
	onDisk, err := os.ReadFile(after.Path)
	if err != nil {
		t.Fatalf("回源后读本地文件 %s: %v", after.Path, err)
	}
	if string(onDisk) != string(coverBody) {
		t.Fatalf("回源后落盘字节 = %q, 期望 %q", onDisk, coverBody)
	}

	// 再请求一次：这一次必须完全命中本地，证明索引行已经指回真实文件。
	recount := cdn.count()
	if _, _, err := c.Poster(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("第二次 Poster: %v", err)
	}
	if got := cdn.count(); got != recount {
		t.Fatalf("回源成功后仍再回源：请求数 %d → %d", recount, got)
	}
}

// MKACG-650：同一个已淘汰封面的并发请求只回源一次（single-flight）。
func TestPosterConcurrentRequestsFetchOnce(t *testing.T) {
	// 上游慢一点，让 3 个请求真的重叠：单飞失效时计数会是 3。
	cdn, srv := newFakeCDN(t)
	cdn.delay = 60 * time.Millisecond

	c, _ := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"

	const n = 3
	start := make(chan struct{})
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	availables := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bodies[i], availables[i], errs[i] = c.Poster(ctx, testProvider, testProviderID, url)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil || !availables[i] {
			t.Fatalf("第 %d 个请求 = (available=%v, %v), 期望拿到同一份封面", i, availables[i], errs[i])
		}
		if string(bodies[i]) != string(coverBody) {
			t.Fatalf("第 %d 个请求的字节 = %q, 期望 %q", i, bodies[i], coverBody)
		}
	}
	if got := cdn.count(); got != 1 {
		t.Fatalf("并发 %d 个请求回源 %d 次, 期望 1（single-flight）", n, got)
	}
}

// MKACG-650：回源失败 → 报「没有可用封面」（不是 0 字节、不是半截文件），
// 也不把坏值写进缓存：索引行与文件都不留。
func TestPosterFetchFailureReportsUnavailable(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()
	cdn.set(http.StatusNotFound, []byte("nope"))

	body, available, err := c.Poster(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg")
	if err != nil {
		t.Fatalf("Poster 基础设施错误 = %v, 期望 nil（回源失败按「没有封面」处理）", err)
	}
	if available {
		t.Fatal("回源 404 却报「有封面」")
	}
	if body != nil {
		t.Fatalf("回源失败返回了 %d 字节", len(body))
	}
	if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("失败后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
	if entries := listFiles(t, cacheDir); len(entries) != 0 {
		t.Fatalf("失败后在缓存目录留下文件: %v", entries)
	}
}

// 源未声明封面（url 为空）时 Poster 直接报不可用：不发请求。
func TestPosterEmptyURLUnavailable(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	c, _ := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))

	body, available, err := c.Poster(context.Background(), testProvider, testProviderID, "")
	if err != nil || available || body != nil {
		t.Fatalf("Poster(空 url) = (%q, %v, %v), 期望 (nil, false, nil)", body, available, err)
	}
	if got := cdn.count(); got != 0 {
		t.Fatalf("空 url 仍发了 %d 次请求", got)
	}
}

// MKACG-650：HasCover 的判据是索引行，不是文件——文件被删后 poster.jpg 仍要在树里，
// 好让下一次 GET 触发按需回源；行没了才是「这部剧没有封面」。
func TestHasCoverFollowsArtifactRow(t *testing.T) {
	_, srv := newFakeCDN(t)
	c, st := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"
	key := testProvider + "/" + testProviderID

	if has, err := c.HasCover(ctx, testProvider, testProviderID); err != nil || has {
		t.Fatalf("本地化之前 HasCover = (%v, %v), 期望 (false, nil)", has, err)
	}
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}
	if has, err := c.HasCover(ctx, testProvider, testProviderID); err != nil || !has {
		t.Fatalf("本地化之后 HasCover = (%v, %v), 期望 (true, nil)", has, err)
	}
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删本地封面: %v", err)
	}
	if has, err := c.HasCover(ctx, testProvider, testProviderID); err != nil || !has {
		t.Fatalf("文件删掉后 HasCover = (%v, %v), 期望仍 (true, nil)", has, err)
	}
	if err := st.DeleteArtifact(ctx, store.ArtifactKindCover, key); err != nil {
		t.Fatalf("DeleteArtifact: %v", err)
	}
	if has, err := c.HasCover(ctx, testProvider, testProviderID); err != nil || has {
		t.Fatalf("索引行删掉后 HasCover = (%v, %v), 期望 (false, nil)", has, err)
	}
}
