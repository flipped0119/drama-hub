package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// TestAPI_JellyfinLibraries 覆盖 POST /api/jellyfin/libraries 的三种结局（AC-11）：
// 202 已派出 / 409 上一轮还在跑 / 400 未配置且指出缺哪个键；prune 只由请求体给。
func TestAPI_JellyfinLibraries(t *testing.T) {
	t.Run("202 已派出", func(t *testing.T) {
		jf := &fakeJellyfin{}
		srv, _, _ := newTestServerWith(t, jf)
		// 请求体可省：curl 不带 body 也要能触发，按 prune=false 处理。
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("状态码 = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
		}
		if got := decode[jellyfinLibrariesResponse](t, rec); !got.Started {
			t.Errorf("响应 = %+v, 期望 started=true", got)
		}
		if jf.calls != 1 || jf.prune {
			t.Errorf("触发 %d 次, prune=%v, 期望 1 次且 prune=false", jf.calls, jf.prune)
		}
	})

	t.Run("prune 由请求体给", func(t *testing.T) {
		jf := &fakeJellyfin{}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", `{"prune":true}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("状态码 = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
		}
		if !jf.prune {
			t.Error("prune=true 没有传到 Manager")
		}
	})

	t.Run("409 上一轮还在跑", func(t *testing.T) {
		jf := &fakeJellyfin{startErr: fmt.Errorf("%w: 上一次还没结束", jellyfin.ErrBusy)}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("状态码 = %d, 期望 409（body=%s）", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 未配置且点名缺的键", func(t *testing.T) {
		jf := &fakeJellyfin{startErr: fmt.Errorf("%w: 缺少 jellyfin_url、jellyfin_api_key", jellyfin.ErrNotConfigured)}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "jellyfin_url") {
			t.Errorf("响应体 = %s, 期望指出缺哪个键", body)
		}
	})

	t.Run("400 请求体不是合法 JSON", func(t *testing.T) {
		jf := &fakeJellyfin{}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", `{"prune":`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d, 期望 400", rec.Code)
		}
		if jf.calls != 0 {
			t.Errorf("请求体非法时不该触发，实际触发了 %d 次", jf.calls)
		}
	})
}

// TestAPI_JellyfinLibrariesNeedsToken 锁定写操作鉴权对新增端点同样生效：
// 没有 X-Admin-Token 一律 401 且不触发。
func TestAPI_JellyfinLibrariesNeedsToken(t *testing.T) {
	jf := &fakeJellyfin{}
	srv, _, _ := newTestServerWith(t, jf)
	req := newRequest(http.MethodPost, "/api/jellyfin/libraries", "")
	req.Header.Del(AdminTokenHeader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 = %d, 期望 401", rec.Code)
	}
	if jf.calls != 0 {
		t.Errorf("未授权请求触发了 %d 次媒体库同步", jf.calls)
	}
}

// TestAPI_StatusJellyfin 覆盖 /api/status 的 jellyfin 块（AC-1 / AC-8）：
// 配置现状、缺哪个键、上次结果都可见；密钥只有 api_key_set。
func TestAPI_StatusJellyfin(t *testing.T) {
	lastRun := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	jf := &fakeJellyfin{state: jellyfin.State{
		Configured:    true,
		Missing:       []string{},
		URL:           "https://jellyfin.example",
		LibraryRoot:   "/data/video/dramahub",
		LibraryPrefix: "短剧 - ",
		APIKeySet:     true,
		Running:       false,
		RunSeq:        3,
		StartedAt:     lastRun.Add(-2 * time.Second),
		Phase:         jellyfin.PhaseFinished,
		PhaseDone:     2,
		PhaseTotal:    2,
		DurationMS:    2345,
		LastRunAt:     lastRun,
		LastOKAt:      lastRun,
		Created:       58,
		Unchanged:     12,
		Uncertain:     2,
		Failed:        []string{"不存在的分类"},
		Orphans:       []string{"短剧 - 已下架"},
		Conflicts:     []string{},
	}}
	srv, _, _ := newTestServerWith(t, jf)

	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if !got.Jellyfin.Configured || !got.Jellyfin.APIKeySet {
		t.Errorf("jellyfin 块 = %+v, 期望 configured 与 api_key_set 都为 true", got.Jellyfin)
	}
	if got.Jellyfin.LibraryRoot != "/data/video/dramahub" {
		t.Errorf("library_root = %q", got.Jellyfin.LibraryRoot)
	}
	if want := lastRun.Format(time.RFC3339); got.Jellyfin.LastRunAt != want {
		t.Errorf("last_run_at = %q, 期望 %q（UTC RFC3339）", got.Jellyfin.LastRunAt, want)
	}
	if got.Jellyfin.Created != 58 || len(got.Jellyfin.Orphans) != 1 {
		t.Errorf("上次结果 = created %d / orphans %v, 期望 58 / 1 个", got.Jellyfin.Created, got.Jellyfin.Orphans)
	}
	if got.Jellyfin.Uncertain != 2 {
		t.Errorf("uncertain = %d, 期望 2（超时结果未知不与 created 混算）", got.Jellyfin.Uncertain)
	}
	if len(got.Jellyfin.Failed) != 1 || got.Jellyfin.Failed[0] != "不存在的分类" {
		t.Errorf("failed = %v, 期望 [不存在的分类]（被 Jellyfin 拒掉的分类要点名）", got.Jellyfin.Failed)
	}
	// 进度字段：页面点完 202 后靠这几个轮询（MKACG-664）。run_seq 是「认领我点的那一轮」
	// 的依据，没有它页面只能看 running 猜。
	if got.Jellyfin.RunSeq != 3 {
		t.Errorf("run_seq = %d, 期望 3", got.Jellyfin.RunSeq)
	}
	if want := lastRun.Add(-2 * time.Second).Format(time.RFC3339); got.Jellyfin.StartedAt != want {
		t.Errorf("started_at = %q, 期望 %q", got.Jellyfin.StartedAt, want)
	}
	if got.Jellyfin.Phase != jellyfin.PhaseFinished || got.Jellyfin.PhaseDone != 2 || got.Jellyfin.PhaseTotal != 2 {
		t.Errorf("phase = %q %d/%d, 期望 %q 2/2",
			got.Jellyfin.Phase, got.Jellyfin.PhaseDone, got.Jellyfin.PhaseTotal, jellyfin.PhaseFinished)
	}
	if got.Jellyfin.DurationMS != 2345 {
		t.Errorf("duration_ms = %d, 期望 2345", got.Jellyfin.DurationMS)
	}
	if got.Jellyfin.Unchanged != 12 {
		t.Errorf("unchanged = %d, 期望 12", got.Jellyfin.Unchanged)
	}
	// 空时间写成空串（页面显示「—」），不能是 0001-01-01。
	srvEmpty, _, _ := newTestServerWith(t, &fakeJellyfin{})
	zero := decode[statusView](t, do(t, srvEmpty, http.MethodGet, "/api/status", ""))
	if zero.Jellyfin.StartedAt != "" || zero.Jellyfin.Phase != "" {
		t.Errorf("未跑过时 started_at = %q / phase = %q, 期望都是空串",
			zero.Jellyfin.StartedAt, zero.Jellyfin.Phase)
	}
	if body := do(t, srv, http.MethodGet, "/api/status", "").Body.String(); strings.Contains(body, "api_key\":") {
		t.Errorf("响应里出现了 api_key 字段：%s", body)
	}
}

// TestAPI_SettingsJellyfin 覆盖五项配置的写入面（AC-1 / AC-8）：
// api_key 只回 null、library_root 归一化去尾 /、越界值 400 且不落库。
func TestAPI_SettingsJellyfin(t *testing.T) {
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	settings := store.NewSettings(db.DB)
	srv := NewServer(store.NewStore(db.DB), settings, &fakeSyncer{}, &fakeSchedule{}, &fakeJellyfin{},
		[]Source{{Name: "web", Label: "红果官网 SSR"}}, testAdminToken, slog.New(slog.DiscardHandler))

	rec := do(t, srv, http.MethodPatch, "/api/settings",
		`{"settings":{"jellyfin_url":"https://jellyfin.example/","jellyfin_api_key":"abcd1234","jellyfin_library_root":"/data/video/dramahub/","jellyfin_library_prefix":"库 - ","jellyfin_timeout_sec":"15"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[settingsView](t, rec)
	if value, ok := got.Settings["jellyfin_api_key"]; !ok || value != nil {
		t.Errorf("jellyfin_api_key 响应 = %v（存在 %v），期望 null（不回显）", value, ok)
	}
	if got.Settings["jellyfin_library_root"] != "/data/video/dramahub" {
		t.Errorf("jellyfin_library_root 归一化后 = %v, 期望去尾 /", got.Settings["jellyfin_library_root"])
	}
	if got.Settings["jellyfin_url"] != "https://jellyfin.example" {
		t.Errorf("jellyfin_url 归一化后 = %v", got.Settings["jellyfin_url"])
	}
	if kept, err := settings.String(t.Context(), store.KeyJellyfinAPIKey); err != nil || kept != "abcd1234" {
		t.Errorf("库里的 api key = %q, %v，期望 abcd1234", kept, err)
	}

	rec = do(t, srv, http.MethodPatch, "/api/settings", `{"settings":{"jellyfin_timeout_sec":"61"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	if kept, err := settings.Int(t.Context(), store.KeyJellyfinTimeoutSec); err != nil || kept != 15 {
		t.Errorf("越界写入后 timeout = %d, %v，期望仍是 15", kept, err)
	}
}
