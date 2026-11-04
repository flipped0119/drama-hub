package webdav

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 集合 GET 的目录页（MKACG-671）。
//
// x/net/webdav 对集合的 GET/HEAD 一律 405，浏览器与文件管理器因此打不开目录；
// 这里只接管「GET/HEAD 且目标是集合」，字节仍只从 /video 流出（硬约束 7）。
// 单页条数由 settings 的 webdav_page_size 决定，见 docs/DIRECTORY-CONTRACT.md。

// entryView 是目录页的一行。
type entryView struct {
	Name string
	Href string
	Size string
	// Bytes 是文件的精确字节数，给 title 属性用（人读的是 Size）。
	Bytes   int64
	ModTime string
	IsDir   bool
}

// crumbView 是面包屑的一格。
type crumbView struct {
	Name string
	Href string
}

// listingView 是整页的数据：模板只负责排版。
type listingView struct {
	Title   string
	Crumbs  []crumbView
	Entries []entryView
	Total   int
	Limit   int
	Summary string
}

// buildListing 把一层的子项变成可渲染视图。
//
// davPath 是请求路径的转义形式（用来拼链接），limit 是调用方从 settings 读出的
// 单页条数。超出一律截断，页面上写明总数与实际显示数。
func buildListing(ctx context.Context, fsys *FS, davPath string, nodes []vfs.Node, limit int) (listingView, error) {
	base := davPath
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	crumbs := listingCrumbs(base)

	view := listingView{
		Title:  crumbs[len(crumbs)-1].Name,
		Crumbs: crumbs,
		Total:  len(nodes),
		Limit:  limit,
	}

	shown := limit
	if shown > len(nodes) {
		shown = len(nodes)
	}
	if shown < 0 {
		shown = 0
	}
	for _, node := range nodes[:shown] {
		entry, ok, err := buildEntry(ctx, fsys, base, node)
		if err != nil {
			return listingView{}, err
		}
		if ok {
			view.Entries = append(view.Entries, entry)
		}
	}

	if len(nodes) > shown {
		view.Summary = fmt.Sprintf("共 %s 项，已显示前 %s", groupDigits(view.Total), groupDigits(len(view.Entries)))
	} else {
		view.Summary = fmt.Sprintf("共 %s 项", groupDigits(view.Total))
	}
	return view, nil
}

// buildEntry 渲染一行。ok=false 表示子项在列举后被删（海报回源失败等），
// 与 PROPFIND 的处理一致：跳过，不让一条坏数据挡住整页。
func buildEntry(ctx context.Context, fsys *FS, base string, node vfs.Node) (entryView, bool, error) {
	name := baseOf(node.Path)
	href := base + url.PathEscape(name)
	if node.IsDir {
		return entryView{
			Name:    name + "/",
			Href:    href + "/",
			Size:    "—",
			ModTime: "—",
			IsDir:   true,
		}, true, nil
	}

	info, err := fsys.stat(ctx, node)
	if errors.Is(err, vfs.ErrNotFound) {
		return entryView{}, false, nil
	}
	if err != nil {
		return entryView{}, false, err
	}

	entry := entryView{
		Name:    name,
		Href:    href,
		Size:    humanSize(info.size),
		Bytes:   info.size,
		ModTime: formatModTime(info.ModTime()),
	}
	// .strm 的直链指向播放地址（strm_prefix + /video/<id>.m3u8），
	// 不把虚拟文件的字节从 /webdav 流出去；渲染结果不是绝对 URL 时退回自身路径。
	if strings.HasSuffix(name, ".strm") {
		if body, err := fsys.render(ctx, node.Path); err == nil {
			if target := strings.TrimSpace(string(body)); isAbsoluteHTTPURL(target) {
				entry.Href = target
			}
		}
	}
	return entry, true, nil
}

// isAbsoluteHTTPURL 判断 .strm 的内容能否直接当链接：只认 http/https，
// 其它形式（相对路径、空串）都退回 WebDAV 自身路径。
func isAbsoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.IsAbs() && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// listingCrumbs 按路径段生成面包屑，第一格是树根。
func listingCrumbs(davPath string) []crumbView {
	crumbs := []crumbView{{Name: vfs.Root, Href: MountPath}}
	rest := strings.Trim(strings.TrimPrefix(davPath, MountPathNoSlash), "/")
	if rest == "" {
		return crumbs
	}
	var href strings.Builder
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" {
			continue
		}
		href.WriteString("/")
		href.WriteString(seg)
		crumbs = append(crumbs, crumbView{
			Name: unescapeSegment(seg),
			Href: MountPathNoSlash + href.String() + "/",
		})
	}
	return crumbs
}

// unescapeSegment 把路径段还原成人读的名字；非法转义按原样显示。
func unescapeSegment(seg string) string {
	if name, err := url.PathUnescape(seg); err == nil {
		return name
	}
	return seg
}

// listingTemplate 是目录页。只在包级解析一次（硬约束 4：模板 embed）。
var listingTemplate = template.Must(template.New("listing").Parse(listingTemplateText))

const listingTemplateText = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:system-ui,-apple-system,"Noto Sans CJK SC",sans-serif;margin:2rem;color:#1f2328}
nav{font-size:.9rem;color:#57606a}
nav a{color:#0969da;text-decoration:none}
h1{font-size:1.4rem;margin:.6rem 0}
.summary{color:#57606a;font-size:.9rem;margin:0 0 1rem}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.35rem .6rem;border-bottom:1px solid #d0d7de}
th{font-size:.85rem;color:#57606a;font-weight:600}
td.size,th.size{text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums}
td.mtime{white-space:nowrap;color:#57606a}
td a{color:#0969da;text-decoration:none}
td a:hover{text-decoration:underline}
</style>
</head>
<body>
<nav>{{range $i, $c := .Crumbs}}{{if $i}} / {{end}}<a href="{{$c.Href}}">{{$c.Name}}</a>{{end}}</nav>
<h1>{{.Title}}</h1>
<p class="summary">{{.Summary}}</p>
<table>
<thead><tr><th>名称</th><th class="size">大小</th><th class="mtime">修改时间</th></tr></thead>
<tbody>
{{range .Entries}}<tr><td><a href="{{.Href}}">{{.Name}}</a></td><td class="size"{{if not .IsDir}} title="{{.Bytes}} 字节"{{end}}>{{.Size}}</td><td class="mtime">{{.ModTime}}</td></tr>
{{else}}<tr><td colspan="3">（空目录）</td></tr>
{{end}}</tbody>
</table>
</body>
</html>
`

// renderListing 把视图排成 HTML。
func renderListing(view listingView) ([]byte, error) {
	var buf strings.Builder
	if err := listingTemplate.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("渲染目录页: %w", err)
	}
	return []byte(buf.String()), nil
}

// humanSize 把字节数写成 B/KB/MB/GB/TB。
func humanSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	for i, unit := range units {
		value /= 1024
		if value < 1024 || i == len(units)-1 {
			return strconv.FormatFloat(value, 'f', 1, 64) + " " + unit
		}
	}
	return strconv.FormatInt(n, 10) + " B"
}

// groupDigits 加千分位：21.8 万项的总数一眼可读。
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// formatModTime 写修改时间；零值表示库里没有可靠时间，写成破折号而不是 1970。
func formatModTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}
