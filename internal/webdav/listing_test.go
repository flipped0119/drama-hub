package webdav

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 本文件是「集合 GET 目录页」的测试（MKACG-671）：
// 上半部分用 fakeTree 验渲染本身（不碰数据库、不起 HTTP），
// 下半部分用真 handler + 真库验协议契约（认证、跳转、404、405、单页条数热改）。

// readAll 读完响应体并关闭。
func readListing(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return string(body)
}

// seasonBase 是 fixture 季目录的转义形式，用来拼期望的 href。
func seasonBase() string {
	return MountPath +
		url.PathEscape("爱情") + "/" +
		url.PathEscape("测试剧 (2024)") + "/" +
		url.PathEscape("Season 01") + "/"
}

// AC-1/AC-2：一季的目录页列出直接子项，.strm 指向播放地址，目录带尾斜杠。
func TestBuildListingSeason(t *testing.T) {
	ft := newFakeTree()
	nodes, err := ft.List(testCtx(t), fakeSeason)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	view, err := buildListing(testCtx(t), NewFS(ft), seasonBase(), nodes, 1000)
	if err != nil {
		t.Fatalf("buildListing: %v", err)
	}

	if view.Title != "Season 01" {
		t.Errorf("Title = %q, 期望 Season 01", view.Title)
	}
	if view.Total != 2 || len(view.Entries) != 2 {
		t.Fatalf("Total = %d, Entries = %d, 期望各 2", view.Total, len(view.Entries))
	}
	if view.Summary != "共 2 项" {
		t.Errorf("Summary = %q, 期望 \"共 2 项\"", view.Summary)
	}
	if len(view.Crumbs) != 4 || view.Crumbs[0].Href != MountPath {
		t.Errorf("Crumbs = %+v, 期望 4 格且首格指 MountPath", view.Crumbs)
	}

	strm := view.Entries[0]
	if name := baseOf(fakeStrm); strm.Name != name {
		t.Errorf("Name = %q, 期望 %q", strm.Name, name)
	}
	// .strm 的链接是播放地址，不是 WebDAV 自身路径：媒体字节只走 /video。
	if strm.Href != "http://example/video/e1-abcdef012345.m3u8" {
		t.Errorf("strm Href = %q, 期望播放地址", strm.Href)
	}
	if want := humanSize(int64(len(ft.bodies[fakeStrm]))); strm.Size != want {
		t.Errorf("strm Size = %q, 期望 %q", strm.Size, want)
	}
	if strm.ModTime != "—" {
		t.Errorf("strm ModTime = %q, 期望破折号（库里没有可靠修改时间）", strm.ModTime)
	}

	nfo := view.Entries[1]
	if want := seasonBase() + url.PathEscape(baseOf(fakeNFO)); nfo.Href != want {
		t.Errorf("nfo Href = %q, 期望 %q", nfo.Href, want)
	}
}

// 单页条数：超限截断，并写明总数与实际显示数。
func TestBuildListingTruncates(t *testing.T) {
	ft := newFakeTree()
	nodes, err := ft.List(testCtx(t), fakeSeason)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	view, err := buildListing(testCtx(t), NewFS(ft), seasonBase(), nodes, 1)
	if err != nil {
		t.Fatalf("buildListing: %v", err)
	}
	if len(view.Entries) != 1 {
		t.Fatalf("Entries = %d, 期望 1", len(view.Entries))
	}
	if view.Summary != "共 2 项，已显示前 1" {
		t.Errorf("Summary = %q, 期望 \"共 2 项，已显示前 1\"", view.Summary)
	}
}

// 子项在列举后被删（渲染 404）时跳过该行，不让一条坏数据挡住整页，
// 与 PROPFIND 的 children 处理一致。
func TestBuildListingSkipsVanishedEntry(t *testing.T) {
	ft := newFakeTree()
	ft.renderErr[fakeNFO] = vfs.ErrNotFound
	nodes, err := ft.List(testCtx(t), fakeSeason)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	view, err := buildListing(testCtx(t), NewFS(ft), seasonBase(), nodes, 1000)
	if err != nil {
		t.Fatalf("buildListing: %v", err)
	}
	if len(view.Entries) != 1 || view.Entries[0].Name != baseOf(fakeStrm) {
		t.Fatalf("Entries = %+v, 期望只剩 .strm 一行", view.Entries)
	}
}

// 目录项带尾斜杠、大小与修改时间留破折号。
func TestBuildListingDirectoryEntry(t *testing.T) {
	ft := newFakeTree()
	nodes, err := ft.List(testCtx(t), fakeDramaDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	view, err := buildListing(testCtx(t), NewFS(ft), MountPath, nodes, 1000)
	if err != nil {
		t.Fatalf("buildListing: %v", err)
	}
	var dir *entryView
	for i := range view.Entries {
		if view.Entries[i].IsDir {
			dir = &view.Entries[i]
		}
	}
	if dir == nil {
		t.Fatalf("没有目录行: %+v", view.Entries)
	}
	if !strings.HasSuffix(dir.Name, "/") || !strings.HasSuffix(dir.Href, "/") {
		t.Errorf("目录行 = %+v, 期望名称与链接都带尾斜杠", dir)
	}
	if dir.Size != "—" || dir.ModTime != "—" {
		t.Errorf("目录行大小/时间 = %q/%q, 期望都是破折号", dir.Size, dir.ModTime)
	}
}

// AC-2：条目名里的 HTML 元字符必须转义（剧名来自上游源，不可信）。
func TestRenderListingEscapes(t *testing.T) {
	view := listingView{
		Title:   `爱情 & <script>`,
		Crumbs:  []crumbView{{Name: "短剧", Href: MountPath}},
		Summary: "共 1 项",
		Entries: []entryView{{
			Name:  `剧<b>&"x"`,
			Href:  MountPath + "a?b=1&c=2",
			Size:  "1.0 KB",
			Bytes: 1024,
		}},
	}
	body, err := renderListing(view)
	if err != nil {
		t.Fatalf("renderListing: %v", err)
	}
	html := string(body)
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>") {
		t.Errorf("HTML 未转义:\n%s", html)
	}
	for _, want := range []string{"&amp;", "&lt;script&gt;", "&lt;b&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少转义结果 %q:\n%s", want, html)
		}
	}
}

// 空目录给一行提示，而不是一张没有行的表。
func TestRenderListingEmpty(t *testing.T) {
	body, err := renderListing(listingView{
		Title:   vfs.Root,
		Crumbs:  []crumbView{{Name: vfs.Root, Href: MountPath}},
		Summary: "共 0 项",
	})
	if err != nil {
		t.Fatalf("renderListing: %v", err)
	}
	if !strings.Contains(string(body), "（空目录）") {
		t.Errorf("空目录页缺少提示:\n%s", body)
	}
}

// AC-9：21.8 万项的目录也只渲染一页——body 大小与总数无关。
func TestBuildListingCapsHugeDirectory(t *testing.T) {
	const total = 218_000
	nodes := make([]vfs.Node, total)
	for i := range nodes {
		nodes[i] = vfs.Node{Path: fakeGenreDir + "/剧 " + strconv.Itoa(i) + " (2024)", IsDir: true}
	}
	ft := &fakeTree{
		dirs:      map[string][]vfs.Node{},
		bodies:    map[string][]byte{},
		renderErr: map[string]error{},
		listErr:   map[string]error{},
	}
	view, err := buildListing(testCtx(t), NewFS(ft), davPath(fakeGenreDir)+"/", nodes, 1000)
	if err != nil {
		t.Fatalf("buildListing: %v", err)
	}
	body, err := renderListing(view)
	if err != nil {
		t.Fatalf("renderListing: %v", err)
	}
	if len(view.Entries) != 1000 {
		t.Fatalf("Entries = %d, 期望 1000", len(view.Entries))
	}
	if len(body) > 2<<20 {
		t.Errorf("body = %d 字节, 期望 ≤2 MB", len(body))
	}
	if want := "共 218,000 项，已显示前 1,000"; !strings.Contains(string(body), want) {
		t.Errorf("缺少总数提示 %q", want)
	}
}

// AC-8 的测量段：8,329 项的目录页 HTML 生成要落在与 PROPFIND 同档的门槛内。
// 用 DRAMA_HUB_BENCH=1 打开（本机可跑，不依赖数据库）。
func TestListingRenderBudget(t *testing.T) {
	if os.Getenv("DRAMA_HUB_BENCH") != "1" {
		t.Skip("设置 DRAMA_HUB_BENCH=1 打开目录页生成预算测量（AC-8）")
	}
	const total = 8_329
	nodes := make([]vfs.Node, total)
	for i := range nodes {
		nodes[i] = vfs.Node{Path: fakeGenreDir + "/剧 " + strconv.Itoa(i) + " (2024)", IsDir: true}
	}
	ft := &fakeTree{
		dirs:      map[string][]vfs.Node{},
		bodies:    map[string][]byte{},
		renderErr: map[string]error{},
		listErr:   map[string]error{},
	}
	fsys := NewFS(ft)
	ctx := testCtx(t)

	const rounds = 20
	costs := make([]time.Duration, 0, rounds)
	var bodyLen int
	for i := 0; i < rounds; i++ {
		start := time.Now()
		view, err := buildListing(ctx, fsys, davPath(fakeGenreDir)+"/", nodes, 1000)
		if err != nil {
			t.Fatalf("buildListing: %v", err)
		}
		body, err := renderListing(view)
		if err != nil {
			t.Fatalf("renderListing: %v", err)
		}
		costs = append(costs, time.Since(start))
		bodyLen = len(body)
	}
	p50 := median(costs)
	t.Logf("8,329 项目录页：P50 = %s，body = %d 字节", p50, bodyLen)
	if p50 > 50*time.Millisecond {
		t.Errorf("P50 = %s, 期望 ≤50ms", p50)
	}
	if bodyLen > 2<<20 {
		t.Errorf("body = %d 字节, 期望 ≤2 MB", bodyLen)
	}
}

// median 返回耗时的中位数（元素少，排序即可）。
func median(d []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), d...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[len(sorted)/2]
}

// AC-1/AC-2：四层都能浏览，页面正文含下一层条目名。
func TestListingBrowsesCollections(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		path string
		want string
	}{
		{"/", f.genre},
		{f.dramaPath() + "/", f.titleA},
		{f.dramaPath(), f.titleA}, // 无尾斜杠也照样出页面
		{f.seasonPath() + "/", f.episodeBase(f.epIDs[0])},
	} {
		resp := f.request(t, http.MethodGet, c.path, nil, nil)
		body := readListing(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, 期望 200（body: %s）", c.path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, 期望 text/html", c.path, ct)
		}
		if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
			t.Errorf("GET %s: Content-Length = %q, body = %d", c.path, cl, len(body))
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("GET %s 正文缺少 %q:\n%s", c.path, c.want, body)
		}
	}
}

// AC-2：分类页的分类名是能点进去的链接，季页有大小与「未知修改时间」两列。
func TestListingLinksAndColumns(t *testing.T) {
	f := newFixture(t)

	body := readListing(t, f.request(t, http.MethodGet, "/", nil, nil))
	if want := `href="` + MountPath + url.PathEscape(f.genre) + `/`; !strings.Contains(body, want) {
		t.Errorf("分类页缺少链接 %q:\n%s", want, body)
	}

	body = readListing(t, f.request(t, http.MethodGet, f.seasonPath()+"/", nil, nil))
	if !strings.Contains(body, "—") {
		t.Errorf("季页缺少「未知修改时间」的破折号:\n%s", body)
	}
	if !strings.Contains(body, "开局") {
		t.Errorf("季页缺少集名:\n%s", body)
	}
}

// AC-5：未认证一律 401，且响应体不含任何条目；无尾斜杠也不再是 307。
func TestListingUnauthenticated(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/webdav/", "/webdav"} {
		resp, err := f.client.Get(f.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readListing(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, 期望 401", path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s 缺少 WWW-Authenticate", path)
		}
		if strings.Contains(body, f.genre) {
			t.Errorf("GET %s 未认证却渲染了条目:\n%s", path, body)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("GET %s 在认证前跳转到了 %q", path, loc)
		}
	}
}

// AC-6：认证后 /webdav（无尾斜杠）→ 308 → /webdav/，query 保留。
func TestListingRedirectsAfterAuth(t *testing.T) {
	f := newFixture(t)
	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, c := range []struct{ path, want string }{
		{"/webdav", "/webdav/"},
		{"/webdav?depth=1", "/webdav/?depth=1"},
	} {
		req, err := http.NewRequest(http.MethodGet, f.srv.URL+c.path, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.SetBasicAuth(f.user, f.pass)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		_ = readListing(t, resp)
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Errorf("GET %s = %d, 期望 308", c.path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != c.want {
			t.Errorf("GET %s Location = %q, 期望 %q", c.path, loc, c.want)
		}
	}
}

// AC-7：不存在的路径 404；集合上不支持的方法 405 且带 Allow。
func TestListingErrors(t *testing.T) {
	f := newFixture(t)

	resp := f.request(t, http.MethodGet, "/无此分类/", nil, nil)
	_ = readListing(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET 不存在的分类 = %d, 期望 404", resp.StatusCode)
	}

	resp = f.request(t, http.MethodPost, "/", nil, nil)
	_ = readListing(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 集合 = %d, 期望 405", resp.StatusCode)
	}
	allow := resp.Header.Get("Allow")
	for _, method := range []string{string(methodPropfind), "GET", "OPTIONS"} {
		if !strings.Contains(allow, method) {
			t.Errorf("405 的 Allow = %q, 缺少 %s", allow, method)
		}
	}
}

// AC-10：webdav_page_size 改完下一次请求即生效（每次请求现读，硬约束 5）。
func TestListingPageSizeIsLive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 季目录有 2 集 × (strm + nfo) = 4 项。
	if err := f.settings.Set(ctx, store.KeyWebDAVPageSize, "2"); err != nil {
		t.Fatalf("Set webdav_page_size: %v", err)
	}
	body := readListing(t, f.request(t, http.MethodGet, f.seasonPath()+"/", nil, nil))
	if want := "共 4 项，已显示前 2"; !strings.Contains(body, want) {
		t.Fatalf("截断后缺少 %q:\n%s", want, body)
	}
	if got := strings.Count(body, "<tr><td>"); got != 2 {
		t.Errorf("页面有 %d 行条目, 期望 2", got)
	}

	if err := f.settings.Set(ctx, store.KeyWebDAVPageSize, "1000"); err != nil {
		t.Fatalf("Set webdav_page_size: %v", err)
	}
	body = readListing(t, f.request(t, http.MethodGet, f.seasonPath()+"/", nil, nil))
	if !strings.Contains(body, "共 4 项") || strings.Contains(body, "已显示前") {
		t.Fatalf("恢复单页条数后不符:\n%s", body)
	}
}
