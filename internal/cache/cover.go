package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// ErrCoverFetch 表示海报取不回来（非 2xx、读体失败、传输错误）。
// 调用方用 errors.Is 判断；本包不 import httpx（rank 20 < 30），错误分类由边界包做。
var ErrCoverFetch = errors.New("海报下载失败")

// CoverCache 把上游海报落成本地资产，并写 artifact(kind='cover') 索引。
//
// 落盘布局与键见 docs/CACHE-MODEL.md「落盘布局与键」：
// `<cache_dir>/cover/<sha256(key) 前 2 位>/<sha256(key)>.<ext>`，
// key = `<provider>/<provider_id>`。文件内容只由键决定，写用「临时文件 + rename」，
// 因此读到的永远是完整文件。
//
// 两个调用点共用一份实例（装配根注入）：同步的 LocalizeCover（刷新时本地化）与
// poster.jpg 的 Poster（请求时按需回源）。两者共用同一个 flightGroup，所以同一个
// 封面的并发调用只回源一次（MKACG-650）。
type CoverCache struct {
	st       *store.Store
	settings *store.Settings
	client   *http.Client
	logger   *slog.Logger
	flights  *flightGroup
	now      func() time.Time // 取当前时间；测试注入固定值（last_used_at 要可断言）
}

// NewCoverCache 构造海报缓存。client 的超时不由它自己定：每次下载按 settings 的
// cover_timeout_sec 现读，改完下一次下载即生效（docs/DEV-STANDARDS.md「配置」）。
// 装配根注入的 client 只承载传输（Transport、测试用的 httptest 连接）。
// logger 不可为 nil（按需回源的日志是「淘汰后重新下载」的可观测证据）。
func NewCoverCache(st *store.Store, settings *store.Settings, client *http.Client, logger *slog.Logger) *CoverCache {
	return &CoverCache{
		st:       st,
		settings: settings,
		client:   client,
		logger:   logger,
		flights:  newFlightGroup(),
		now:      time.Now,
	}
}

// LocalizeCover 确保某部剧的海报在本地：已有索引行且文件在 → 直接返回，不回源。
//
// 这正是「源图失效不影响已入库海报」的实现：一旦落盘，后续刷新不再访问上游。
// 文件缺失或索引行缺失时才重新下载并覆盖索引行。url 为空（上游未声明封面）
// 视为无事可做，返回 nil。
//
// 失败返回 ErrCoverFetch（下载）或 store / 文件系统错误，都不写索引行：宁可在
// 下一轮重试，也不留一条指向不存在文件的 artifact 行。海报是附带资产——「取不回来
// 算不算同步失败」由调用方决定（internal/sync 记一条 warn 后不上升为失败）。
//
// 单次出站超时取 settings 的 cover_timeout_sec（现读），不跟插件超时走。
func (c *CoverCache) LocalizeCover(ctx context.Context, provider, providerID, url string) error {
	if url == "" {
		return nil
	}
	key := CoverKey(provider, providerID)
	if ok, err := c.localized(ctx, key); err != nil || ok {
		return err
	}
	_, err := c.flights.do(ctx, key, func() ([]byte, error) {
		// 当上 leader 后再查一次：从上面到这里之间，另一个调用点（上一部剧的刷新、
		// 或一个 poster.jpg 请求）可能已经把这封面落盘了。
		if ok, err := c.localized(ctx, key); err != nil || ok {
			return nil, err
		}
		return c.download(ctx, key, provider, providerID, url)
	}, nil)
	return err
}

// HasCover 报告这部剧有没有可用封面：索引行在就算有，不看文件、不触网。
//
// 判据只看行、不看文件，是为了「封面被淘汰后按需重新下载」（MKACG-650）：
// 文件被删（手删或淘汰删了文件）时 poster.jpg 仍留在树里，下一次 GET 才会触发回源。
// 行不在 = 这部剧没有可用封面（源未声明，或同步时下载失败）——树里就没有这个文件。
func (c *CoverCache) HasCover(ctx context.Context, provider, providerID string) (bool, error) {
	_, err := c.st.ArtifactByKey(ctx, store.ArtifactKindCover, CoverKey(provider, providerID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Poster 返回 poster.jpg 的字节：本地已有就直接读，没有就回源一次。
//
// available=false 表示这部剧当前没有可用的封面（源未声明，或回源失败），调用方按
// 「文件不存在」处理（WebDAV 404）；error 只表示基础设施故障（库 / 磁盘）。
// 返回的永远是完整字节：读的是落盘后 rename 到位的文件，回源失败一个字都不返回。
func (c *CoverCache) Poster(ctx context.Context, provider, providerID, url string) (body []byte, available bool, err error) {
	key := CoverKey(provider, providerID)
	if body, ok, err := c.hitFile(ctx, key); err != nil || ok {
		return body, err == nil, err
	}
	if url == "" {
		return nil, false, nil
	}
	body, err = c.flights.do(ctx, key, func() ([]byte, error) {
		// 同 LocalizeCover：当上 leader 后复查一次，别的调用点可能刚刚落盘。
		if body, ok, err := c.hitFile(ctx, key); err != nil || ok {
			return body, err
		}
		c.logger.InfoContext(ctx, "poster.jpg 本地无封面，按需回源",
			"provider", provider, "id", providerID)
		return c.download(ctx, key, provider, providerID, url)
	}, nil)
	if err != nil {
		if errors.Is(err, ErrCoverFetch) {
			// 按约定返回「不存在」而不是 0 字节或半截文件：下一次请求可以再试一次。
			c.logger.WarnContext(ctx, "poster.jpg 回源失败，按不存在处理",
				"provider", provider, "id", providerID, "err", err)
			return nil, false, nil
		}
		return nil, false, err
	}
	return body, true, nil
}

// hitFile 读本地已落盘的海报：行在、文件在才算命中，命中即 touch 并返回字节。
//
// 行在、文件没了（被手删或**被淘汰**）按未命中处理，但**保留索引行**：这一行就是
// 「这部剧有封面」的真值（HasCover），删掉它 poster.jpg 会从树里消失。淘汰对海报走的就是
// 这条路径：文件删掉、行降级为空 path（`Cache.deleteOne` 调 `ReleaseArtifact`），
// 于是下一次请求按未命中处理并回源——这就是「淘汰后按需重新下载」（MKACG-650）。
func (c *CoverCache) hitFile(ctx context.Context, key string) ([]byte, bool, error) {
	row, err := c.st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if row.Path == "" { // 被淘汰降级过：行还在，字节不在
		return nil, false, nil
	}
	body, err := os.ReadFile(row.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读海报文件 %s: %w", row.Path, err)
	}
	// 命中即 touch（契约 MUST）：不 touch 的海报会被 cache_lru_days 当过期淘汰，
	// 下次请求就得回源——源图失效时直接丢封面。touch 失败只让这条更早过期，
	// 不影响本次结果，因此忽略。
	_ = c.st.TouchArtifact(ctx, store.ArtifactKindCover, key, stamp(c.now()))
	return body, true, nil
}

// localized 报告本地是否已有海报（行在、文件在），命中即 touch；不读文件内容。
//
// 与 hitFile 的差别只是「不把字节读进内存」：同步（LocalizeCover）不需要字节。
// 空 path 表示这条行被淘汰降级过（行在、字节不在），同样算未命中。
func (c *CoverCache) localized(ctx context.Context, key string) (bool, error) {
	row, err := c.st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Path == "" || !fileExists(row.Path) {
		return false, nil
	}
	// 命中即 touch，理由同 hitFile。
	_ = c.st.TouchArtifact(ctx, store.ArtifactKindCover, key, stamp(c.now()))
	return true, nil
}

// download 回源一次并落盘 + 写索引行，返回字节。
//
// 落盘布局与键不变（与 LocalizeCover 一直是同一份实现）：`<cache_dir>/cover/<hash 前 2 位>/<hash>.<ext>`。
// 失败时文件与索引行都不动（既有行保持原样），下一次请求 / 下一轮刷新自然重试。
func (c *CoverCache) download(ctx context.Context, key, provider, providerID, url string) ([]byte, error) {
	body, ext, err := c.fetch(ctx, url)
	if err != nil {
		return nil, err
	}

	root, err := cacheRoot(ctx, c.settings)
	if err != nil {
		return nil, err
	}
	l := layoutOf(root)
	target, err := l.file(store.ArtifactKindCover, cacheHash(store.ArtifactKindCover, key), ext)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(l.tmpDir(), target, body); err != nil {
		return nil, fmt.Errorf("写入海报文件 %s: %w", target, err)
	}

	digest := sha256.Sum256(body)
	if err := c.st.PutArtifact(ctx, store.Artifact{
		Kind:       store.ArtifactKindCover,
		Key:        key,
		Path:       target,
		Bytes:      int64(len(body)),
		SHA256:     hex.EncodeToString(digest[:]),
		LastUsedAt: stamp(c.now()),
	}); err != nil {
		return nil, err
	}
	return body, nil
}

// fetch 取回海报字节并推出文件扩展名（按响应 Content-Type，退化为 URL 后缀）。
//
// 超时来自 cover_timeout_sec（现读，每次下载一次）。读不到配置就按 ErrCoverFetch
// 返回，不猜一个数字代替配置（可调参数只有 settings 一个真值）。
func (c *CoverCache) fetch(ctx context.Context, url string) (body []byte, ext string, err error) {
	timeoutSec, err := c.settings.Int(ctx, store.KeyCoverTimeoutSec)
	if err != nil {
		return nil, "", fmt.Errorf("%w: 读 cover_timeout_sec: %v", ErrCoverFetch, err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCoverFetch, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCoverFetch, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("%w: 上游状态 %d", ErrCoverFetch, resp.StatusCode)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("%w: 读响应体: %v", ErrCoverFetch, err)
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("%w: 空响应体", ErrCoverFetch)
	}
	return body, coverExt(resp.Header.Get("Content-Type"), url), nil
}

// coverExt 由 Content-Type 推扩展名；认不出的 type 退回 URL 后缀，再退回 ".img"。
// 扩展名只影响本地文件名，不影响任何契约。
func coverExt(contentType, url string) string {
	mediatype, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(mediatype)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	if i := strings.LastIndexByte(url, '.'); i >= 0 && i > strings.LastIndexByte(url, '/') {
		if suffix := url[i:]; len(suffix) <= 5 {
			return suffix
		}
	}
	return ".img"
}

// fileExists 判断路径是否是存在的普通文件。
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
