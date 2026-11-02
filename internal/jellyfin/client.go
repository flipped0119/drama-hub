package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Jellyfin 管理 API 的四个端点（12.1 实测，见 docs/adr/0012「API」）。
//
// 建库/删库都按 name 操作：POST 回 204 且**没有响应体**，拿不到库 id；需要 id 时只能从
// 列库的 ItemId 取。本包不需要 id（删库按 name、扫描用全局 /Library/Refresh）。
const (
	pathLibraries = "/Library/VirtualFolders"
	pathRefresh   = "/Library/Refresh"
)

// authScheme 是 12.1 唯一可用的认证头。
//
// X-Emby-Token 已被迁移 20260531160000_DisableLegacyAuthorization 关掉（EnableLegacy
// Authorization=false），发了也不生效；`?ApiKey=` 仍可用，但 key 会写进 Jellyfin 自己的
// 访问日志——本任务明确不把 key 放进 query（AC-8）。
const authScheme = `MediaBrowser Token="`

// Client 是 Jellyfin 管理 API 的窄封装：一个配置 = 一个 client，用完即弃。
//
// 不带 http.Client.Timeout：一次 reconcile 要串行发 N+2 个请求，全局 Timeout 会把整轮
// 卡在一个值上；超时按「每次请求一个 ctx」给（Transport 由调用方决定，测试直连 httptest）。
type Client struct {
	baseURL string
	apiKey  string
	timeout time.Duration
	http    *http.Client
}

// NewClient 用一份配置造 client。httpClient 为 nil 时用 http.DefaultClient（无 Timeout）。
func NewClient(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.URL, "/"),
		apiKey:  cfg.APIKey,
		timeout: cfg.Timeout,
		http:    httpClient,
	}
}

// ListLibraries 列全部虚拟媒体库（`GET /Library/VirtualFolders`）。
func (c *Client) ListLibraries(ctx context.Context) ([]Library, error) {
	var out []Library
	if err := c.do(ctx, http.MethodGet, pathLibraries, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLibrary 建一个 tvshows 库（`POST /Library/VirtualFolders`）。
//
// refreshLibrary=false 是刻意的：每个库各触发一次全库扫描，65 个库就是 65 次；扫描统一
// 在整轮末尾调一次（Refresh）。`paths` 由 Jellyfin 的 CommaDelimitedCollectionModelBinder
// 绑定——分类目录名里若有逗号会被拆成两个路径，当前分类名不含逗号，不做处理。
func (c *Client) CreateLibrary(ctx context.Context, name, libraryPath string) error {
	query := url.Values{
		"name":           {name},
		"collectionType": {"tvshows"},
		"paths":          {libraryPath},
		"refreshLibrary": {"false"},
	}
	return c.do(ctx, http.MethodPost, pathLibraries, query, nil)
}

// DeleteLibrary 按库名删一个库（`DELETE /Library/VirtualFolders`）。
//
// 名字不存在时 Jellyfin 会抛异常（可能 500），所以它不能当存在性探测用 —— 调用方只用它
// 删自己刚列出、判定为孤儿的库。
func (c *Client) DeleteLibrary(ctx context.Context, name string) error {
	query := url.Values{"name": {name}, "refreshLibrary": {"false"}}
	return c.do(ctx, http.MethodDelete, pathLibraries, query, nil)
}

// Refresh 触发一次全库扫描（`POST /Library/Refresh`）。
func (c *Client) Refresh(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, pathRefresh, nil, nil)
}

// statusError 是「Jellyfin 回了非 2xx」的错误。
//
// 单独一个类型是为了让调用方能按状态码区分处置：400 是**这一个请求**被拒（实测 12.1
// 的 `The specified path does not exist`），换下一个分类还能成；5xx / 401 则是系统性
// 的，继续打只是复读。错误文本仍然是「方法 + 端点 + 状态码」，不回显响应体（AC-8）。
type statusError struct {
	method   string
	endpoint string
	code     int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("jellyfin %s %s 返回 %d", e.method, e.endpoint, e.code)
}

// do 发一个请求并解出响应。out 为 nil 时只读空响应体。
//
// 错误文本只带方法、端点路径与状态码：不拼接响应体（Jellyfin 的报错可能回显请求参数）
// 也不带 URL —— key 在请求头里，但仍不给它任何外泄通道（AC-8）。
func (c *Client) do(ctx context.Context, method, endpoint string, query url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	target := c.baseURL + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return fmt.Errorf("构造 jellyfin 请求 %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", authScheme+c.apiKey+`"`)
	resp, err := c.http.Do(req)
	if err != nil {
		// 去掉 *url.Error 外层的 `Get "http://host/path":` 包装：错误文本只留端点路径与
		// 原因类别（超时 / 连接被拒 / …）。URL 本身不含 key，但少回显一份配置更稳。
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("请求 jellyfin %s %s 失败: %w", method, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return &statusError{method: method, endpoint: endpoint, code: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 jellyfin %s %s 响应: %w", method, endpoint, err)
	}
	return nil
}
