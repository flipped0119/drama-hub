package webdav

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	xwebdav "golang.org/x/net/webdav"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// MountPath 是挂载点。cmd 用它注册路由，Handler 用它生成 PROPFIND 的 href
// （RFC 4918 要求 href 是绝对路径；Prefix 交给 x/net/webdav 处理，不做 StripPrefix）。
const MountPath = "/webdav/"

// MountPathNoSlash 是同一挂载点的无尾斜杠形式。它也注册到同一个 Handler：
// http.ServeMux 自己的跳转发生在认证之前，跨跳转会丢掉 Authorization，
// 客户端要重新弹一次认证（MKACG-671）。
const MountPathNoSlash = "/webdav"

// allowMethods 是 405 的 Allow 头。x/net/webdav 只在 OPTIONS 里写 Allow，
// 405 响应本身不带，客户端因此看不出「这里能用哪些方法」。
const allowMethods = "OPTIONS, GET, HEAD, PROPFIND, LOCK, UNLOCK"

// Handler 是只读 WebDAV 端点的 http.Handler：Basic 认证 → x/net/webdav → vfs.Tree。
//
// 认证用 settings 的 webdav_user / webdav_pass，每请求现读（硬约束 5：改值即时生效，
// 不缓存）。协议部分委托 x/net/webdav，本包只提供 FileSystem 与认证。
type Handler struct {
	settings *store.Settings
	inner    *xwebdav.Handler
	tree     Tree
	// fs 是协议层用的同一份 FileSystem：目录页要用它判断目标是否为集合、
	// 取文件大小与 .strm 的播放地址。
	fs       *FS
	propfind *propfindCache
}

// NewHandler 装配 WebDAV 端点。settings 不可为 nil；logger 只用于协议层错误
// （正常请求不产生第二条日志——访问日志归装配根的 httpx.RequestLogger）。
func NewHandler(tree Tree, settings *store.Settings, logger *slog.Logger) *Handler {
	fsys := NewFS(tree)
	h := &Handler{settings: settings, tree: tree, fs: fsys, propfind: newPropfindCache()}
	h.inner = &xwebdav.Handler{
		Prefix:     MountPath,
		FileSystem: fsys,
		// 锁定是协议层要求（rclone 会发 LOCK/UNLOCK）；本服务只读，内存锁足够，
		// 不落盘、也无跨进程语义（硬约束 4：单二进制单实例）。
		LockSystem: xwebdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			// x/net/webdav 只把状态码写回客户端，失败详情不经它输出；仅在出错时记一条，
			// 避免每个正常请求多一条日志（DEV-STANDARDS：每请求恰一条访问日志）。
			if err != nil {
				logger.Warn("webdav 协议错误", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}
	return h
}

// ServeHTTP 先认证，再挂上请求内树快照转交协议处理器。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		// WWW-Authenticate 是 Basic 方案的要求：缺了它，rclone / 浏览器不会重试带凭据。
		w.Header().Set("WWW-Authenticate", `Basic realm="drama-hub", charset="UTF-8"`)
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	// 无尾斜杠的挂载点在这里补跳转：认证之后才跳，凭据不会丢；308 保住方法与 body。
	if r.URL.Path == MountPathNoSlash {
		target := MountPath
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
	// B1：.strm 在 strm_prefix 为空时要写出绝对地址，基地址只能从请求里来。
	ctx := vfs.WithBaseURL(r.Context(), requestBaseURL(r))
	r = r.WithContext(withSnapshot(ctx))
	// PROPFIND 是本服务最热的请求（Jellyfin 扫库几乎全是它）：命中响应缓存时
	// 整块回放，未命中才走协议层。放在认证之后，缓存不绕过认证。
	if r.Method == methodPropfind {
		h.servePropfind(w, r)
		return
	}
	// 集合的 GET/HEAD 渲染目录页；文件与其它方法原样交给协议层（MKACG-671）。
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		handled, err := h.serveListing(w, r)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		if handled {
			return
		}
	}
	// 包装只为给协议层自己产生的 405 补 Allow。
	h.inner.ServeHTTP(allowHeader{w}, r)
}

// serveListing 在目标是集合时渲染目录页。handled=false 表示「不是集合」，
// 交回协议层处理（文件 200、不存在 404 的语义都不变）。
func (h *Handler) serveListing(w http.ResponseWriter, r *http.Request) (bool, error) {
	ctx := r.Context()
	// FileSystem 收的是「相对挂载点根」的路径（x/net/webdav 已剥 Prefix，这里自己剥）。
	fsPath := strings.TrimPrefix(r.URL.Path, MountPathNoSlash)
	fi, err := h.fs.Stat(ctx, fsPath)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	limit, err := h.settings.Int(ctx, store.KeyWebDAVPageSize)
	if err != nil {
		return false, fmt.Errorf("读取 %s: %w", store.KeyWebDAVPageSize, err)
	}
	dir := vfsDir(strings.Trim(fsPath, "/"))
	nodes, err := h.tree.List(ctx, dir)
	if err != nil {
		return false, err
	}
	view, err := buildListing(ctx, h.fs, r.URL.EscapedPath(), nodes, limit)
	if err != nil {
		return false, err
	}
	body, err := renderListing(view)
	if err != nil {
		return false, err
	}

	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	// 目录内容是同步后随时会变的视图，别让浏览器拿旧页面骗人。
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return true, nil
}

// allowHeader 给 405 补 Allow，其它状态码原样透传。
type allowHeader struct {
	http.ResponseWriter
}

func (w allowHeader) WriteHeader(code int) {
	if code == http.StatusMethodNotAllowed && w.Header().Get("Allow") == "" {
		w.Header().Set("Allow", allowMethods)
	}
	w.ResponseWriter.WriteHeader(code)
}

// authorized 校验 Basic 凭据。
//
// 凭据取 settings（每请求现读，改值即时生效）。读失败（必填项缺失）不算「认证通过」，
// 一律 401 而不是 500：库没配好时不应把挂载点敞开。比较用 constant-time，避免按字节
// 短路泄漏长度/前缀信息。
func (h *Handler) authorized(r *http.Request) bool {
	user, err := h.settings.String(r.Context(), store.KeyWebDAVUser)
	if err != nil {
		return false
	}
	pass, err := h.settings.String(r.Context(), store.KeyWebDAVPass)
	if err != nil {
		return false
	}
	gotUser, gotPass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
	return userOK && passOK
}

// requestBaseURL 由请求推导本服务的对外基地址 `scheme://host`。
//
// 反向代理后 Host 是外部域名、真实 scheme 在 X-Forwarded-Proto 里；两者都没有时按
// 连接是否 TLS 判断。Host 为空（不合法的 HTTP/1.0 请求）返回空串，渲染退回相对路径。
func requestBaseURL(r *http.Request) string {
	if r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = normalizeProto(proto)
	}
	return scheme + "://" + r.Host
}

// normalizeProto 取 X-Forwarded-Proto 的第一个值（代理链上会是 `https,http`）。
func normalizeProto(raw string) string {
	proto, _, _ := strings.Cut(raw, ",")
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		return "http"
	}
	return proto
}
