package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path"

	"github.com/justforlxz/drama-hub/internal/httpx"
)

// assets 是管理页面所需的模板与静态资源，随二进制嵌入（ADR-0002：单二进制、
// 无前端构建步骤、没有 package.json）。
//
//go:embed templates/*.html static/*
var assets embed.FS

// statusPageFile 是「同步与缓存」页面的模板，与 templates/base.html 一起渲染。
//
// 剧目页删掉后后台只剩这一页（MKACG-665）；`base.html` 的导航随之只剩一个链接。
const statusPageFile = "templates/status.html"

// pageData 是模板的渲染入参：Page 同时用于高亮导航与 body 的 data-page，
// 页面 JS 靠 body[data-page] 决定挂哪一段逻辑。
type pageData struct {
	Page  string
	Title string
}

// handleStatusPage 处理 GET / 与 GET /status（同步与缓存看板）。
//
// 剧目页删除后 `/` 直接就是这一页；`/status` 保留为稳定链接（MKACG-665）。
// 后台首页的重设计在 MKACG-667，届时再决定 `/` 是否另作他用。
//
// 模板每次请求解析：整页几 KB，单用户管理面下这点开销可以忽略，换来的是不持有
// 包级模板对象。
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	tpl, err := template.ParseFS(assets, "templates/base.html", statusPageFile)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("解析页面模板 %s: %w", statusPageFile, err))
		return
	}
	buf := &bytes.Buffer{}
	if err := tpl.ExecuteTemplate(buf, "base", pageData{Page: "status", Title: "同步与缓存"}); err != nil {
		httpx.WriteError(w, fmt.Errorf("渲染页面 %s: %w", statusPageFile, err))
		return
	}
	writeBody(w, http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// handleStatic 提供内嵌的样式与脚本。
//
// `{file}` 只匹配单个路径段（不含 `/`），因此不存在目录穿越；只放行 .css / .js
// 两种后缀，其余按 404 处理 —— 管理面没有其它静态资源。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType := ""
	switch path.Ext(name) {
	case ".css":
		contentType = "text/css; charset=utf-8"
	case ".js":
		contentType = "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeBody(w, http.StatusOK, contentType, body)
}
