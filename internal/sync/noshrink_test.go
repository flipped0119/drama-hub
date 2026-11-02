package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 本文件覆盖「刷新只增改不删」（docs/DATA-MODEL.md「增删改语义」、
// docs/SCHEDULER-MODEL.md「幂等」）：源侧集数变少、剧目下架都不许删库，
// 自动分块的派生结果也不许被刷新冲掉（块成员与 uid 在两次同步之间不变）。

// workDetail 造一部有 n 集的剧（集号 1..n，集标识 vid-1..vid-n）。
func workDetail(n int) plugin.WorkDetail {
	eps := make([]plugin.Episode, n)
	for i := range eps {
		eps[i] = plugin.Episode{No: i + 1}
	}
	return plugin.WorkDetail{
		Work: plugin.Work{
			Ref:         plugin.WorkRef{Plugin: "web", ID: "w1"},
			Title:       "假剧",
			EpisodeDecl: n,
		},
		Episodes: eps,
	}
}

// newNoshrinkSyncer 构造一个可改数据的假插件 + 可控时钟的写库编排。
func newNoshrinkSyncer(t *testing.T) (*fakePlugin, *Syncer, *fakeClock) {
	t.Helper()
	p := &fakePlugin{name: "web", detail: workDetail(5)}
	clock := &fakeClock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	s := New(p, nil, nil)
	s.now = clock.now
	return p, s, clock
}

// 集数变少：剧照常在，只是上游声明从 5 集变成 3 集。
// 行为：drama.episode_total 跟着变小、sync_at 更新；已入库的第 4、5 集行不删。
func TestResyncKeepsEpisodesWhenDeclaredShrinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	first, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	clock.advance(time.Hour)

	p.detail = workDetail(3) // 源侧集数变少
	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("重跑同步: %v", err)
	}

	second, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("重跑后 DramaByProviderID: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("剧目 id 变了：%d → %d", first.ID, second.ID)
	}
	if second.EpisodeTotal != 3 {
		t.Errorf("episode_total = %d, 期望跟着声明走 = 3", second.EpisodeTotal)
	}
	if second.SyncAt == first.SyncAt {
		t.Errorf("sync_at 没更新（%q）：刷新必须留痕", second.SyncAt)
	}

	eps, err := st.ListEpisodes(ctx, second.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != 5 {
		t.Fatalf("集行 %d 条, 期望 5 条（第 4、5 集不删；实际 %v）", len(eps), epNos(eps))
	}
	for i, ep := range eps {
		if ep.EpNo != i+1 {
			t.Fatalf("第 %d 行 ep_no = %d, 期望 %d", i+1, ep.EpNo, i+1)
		}
	}
}

// 剧目下架：上游对 Work 回 ErrNotFound。
// 行为：本次同步返回 ErrNotFound，剧与集一行不删、sync_at 不动。
func TestResyncIgnoresDelistedWork(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	before, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	dramas, episodes := dbCounts(t, st)

	clock.advance(time.Hour)
	p.err = fmt.Errorf("%w: 上游 404", plugin.ErrNotFound)
	err = s.SyncSeries(ctx, st, "w1")
	if !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("err = %v, 期望 ErrNotFound（下架不是故障）", err)
	}

	afterDramas, afterEpisodes := dbCounts(t, st)
	if afterDramas != dramas || afterEpisodes != episodes {
		t.Fatalf("下架后库变了：(剧,集) %d/%d → %d/%d", dramas, episodes, afterDramas, afterEpisodes)
	}
	after, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("下架后查不到剧了: %v", err)
	}
	if after.SyncAt != before.SyncAt {
		t.Errorf("下架不该改 sync_at：%q → %q", before.SyncAt, after.SyncAt)
	}
}

// 自动分块不被刷新冲掉：分块按当前 N 在派生表里重算，刷新（只增改 drama/episode）
// 不得删除任何集行，因此块成员与 uid 在两次同步之间逐字符不变。
func TestResyncKeepsChunking(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	drama, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	eps, err := st.ListEpisodes(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != 5 {
		t.Fatalf("集行 %d 条, 期望 5 条", len(eps))
	}

	// 默认 N=10：5 集只有一块，uid 是 m<dramaID>_<首集>_<末集>。
	wantUID := fmt.Sprintf("m%d_1_5", drama.ID)
	items, err := st.ListEmbyItems(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 1 || items[0].Kind != "merged" || items[0].UID != wantUID {
		t.Fatalf("派生表 = %+v, 期望 1 条 merged uid=%s", items, wantUID)
	}

	clock.advance(time.Hour)
	p.detail = workDetail(3) // 集数变少：只改 episode_total，不删集行
	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("重跑同步: %v", err)
	}

	after, err := st.ListEpisodes(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(after) != 5 {
		t.Fatalf("集行 %d 条, 期望 5 条（刷新不许删集）", len(after))
	}
	itemsAfter, err := st.ListEmbyItems(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEmbyItems(刷新后): %v", err)
	}
	if len(itemsAfter) != 1 || itemsAfter[0].UID != wantUID || itemsAfter[0].Kind != "merged" {
		t.Fatalf("刷新后派生表 = %+v, 期望仍是 1 条 merged uid=%s", itemsAfter, wantUID)
	}
	if itemsAfter[0].EpFrom != 1 || itemsAfter[0].EpTo != 5 {
		t.Fatalf("刷新后块覆盖 %d-%d, 期望 1-5", itemsAfter[0].EpFrom, itemsAfter[0].EpTo)
	}
}

// epNos 取集号列表，只为失败信息可读。
func epNos(eps []store.Episode) []int {
	out := make([]int, 0, len(eps))
	for _, ep := range eps {
		out = append(out, ep.EpNo)
	}
	return out
}
