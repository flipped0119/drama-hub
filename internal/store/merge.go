package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// EmbyItem 是 emby_item 派生表的一行：Emby 看到的一条剧集（单集或自动分块后的合并集）。
//
// 派生表由 RebuildEmbyItems 幂等重建，uid 是 `/video/{id}` 里 `{id}` 的 uid 段：
// 单集 `e<episode.id>`、合并集 `m<dramaID>_<epFrom>_<epTo>`（两个空间互不匹配，
// docs/adr/0010）。
type EmbyItem struct {
	ID         int64
	DramaID    int64
	Season     int
	EpNo       int
	Kind       string // single | merged
	UID        string
	RuntimeSec int
	// EpisodeID 是单集对应的源集主键；合并集为 0。
	EpisodeID int64
	// EpFrom / EpTo 是这条对外剧集覆盖的源集号：单集两者都等于自身 ep_no，
	// 合并集是块内最小与最大 ep_no（渲染「第 A-B 集」标签用）。
	EpFrom int
	EpTo   int
	// Title 是源集标题（仅单集有值）；合并集标签由 EpFrom/EpTo 派生「第 A-B 集」。
	Title string
}

// chunkEpisodes 把按 ep_no 升序的集列表切成连续块，每块至多 n 集（纯函数）。
//
// 切的是**列表切片**而不是 `ep_no` 数值区间：库里集号可能有缺号（源侧下架不删行），
// 数值区间会切出空块或让某块只剩 1 集。n ≤ 0 时按 1 处理，只为不除零 panic；
// 注册表（settings 的 merge_chunk_size）已经保证 n ∈ [1,500]。
//
// 返回值是入参的子切片（不拷贝）：调用方只读，不要就地改写返回的块。
func chunkEpisodes(eps []Episode, n int) [][]Episode {
	if n <= 0 {
		n = 1
	}
	blocks := make([][]Episode, 0, (len(eps)+n-1)/n)
	for start := 0; start < len(eps); start += n {
		blocks = append(blocks, eps[start:min(start+n, len(eps))])
	}
	return blocks
}

// RebuildEmbyItems 幂等重建某部剧的对外剧集派生表，并返回重建后的行。
//
// 派生规则（docs/adr/0010）：该剧 episode 按 ep_no 升序，每 merge_chunk_size 集切一块，
// 第 i 块占对外集号 i；块内 1 集是单集、≥2 集是合并集。0 集 → 0 行，不留空组。
// 重建语义是「整体替换」：先删该剧全部 emby_item 行再插入，重复调用结果一致。
//
// uid 生成规则只在 rebuildEmbyItems 一处：单集 `e<episode.id>`、合并集
// `m<dramaID>_<epFrom>_<epTo>`；runtime_sec 单集取该集时长、合并集取块内时长之和。
//
// 根 Store 调用时自带事务；事务内 Store 调用时沿用外层事务（同步完成是第二个
// 调用点，它需要与写剧集同一事务）。
func (s *Store) RebuildEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	if s.root == nil {
		if err := s.rebuildEmbyItems(ctx, dramaID); err != nil {
			return nil, err
		}
		return s.ListEmbyItems(ctx, dramaID)
	}
	var out []EmbyItem
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.rebuildEmbyItems(ctx, dramaID); err != nil {
			return err
		}
		items, err := tx.ListEmbyItems(ctx, dramaID)
		out = items
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// outwardItem 是重建过程中的中间态：一条对外剧集。
type outwardItem struct {
	epNo      int
	kind      string
	uid       string
	episodeID int64
	epFrom    int
	epTo      int
	runtime   int
}

// rebuildEmbyItems 是 RebuildEmbyItems 的事务内实现（只碰 s.db，不开关事务）。
//
// 两条约束都在 lockDrama：
//   - 同一部剧的「先删后插」必须串行。READ COMMITTED 下两个并发重建会各自按自己的
//     快照删（删不到对方刚插入的行），再各自插入同一批 uid → 唯一约束冲突；
//   - 这把锁同时是全局锁序（drama → 子表）的入口，锁在调用方的事务结束时释放，
//     所以所有调用点都必须已经在事务里（RebuildEmbyItems / BackfillEpisodeDuration）。
func (s *Store) rebuildEmbyItems(ctx context.Context, dramaID int64) error {
	if err := s.lockDrama(ctx, dramaID); err != nil {
		return err
	}
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return err
	}

	blocks := chunkEpisodes(eps, n)
	items := make([]outwardItem, 0, len(blocks))
	for i, block := range blocks {
		it := outwardItem{
			epNo:   i + 1,
			epFrom: block[0].EpNo,
			epTo:   block[len(block)-1].EpNo,
		}
		for _, ep := range block {
			it.runtime += ep.DurationSec
		}
		if len(block) == 1 {
			it.kind = "single"
			it.uid = singleUID(block[0].ID)
			it.episodeID = block[0].ID
		} else {
			it.kind = "merged"
			it.uid = mergedUID(dramaID, it.epFrom, it.epTo)
		}
		items = append(items, it)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM emby_item WHERE drama_id = $1`, dramaID); err != nil {
		return wrapWriteErr("清空派生表", err)
	}
	for _, it := range items {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO emby_item(drama_id, season, ep_no, kind, uid, runtime_sec)
			 VALUES($1, 1, $2, $3, $4, $5)`,
			dramaID, it.epNo, it.kind, it.uid, it.runtime,
		); err != nil {
			return wrapWriteErr("写入派生表", err)
		}
	}
	return nil
}

// mergeChunkSize 现读 settings 的 merge_chunk_size（每次重建都读，不缓存）。
//
// 库中无行取注册表默认值，所以读的是 settingValue 而不是 Settings —— 后者持有
// *sql.DB，而事务内的 Store 只有 dbtx。校验规则仍只有 specFor 一份。
func (s *Store) mergeChunkSize(ctx context.Context) (int, error) {
	value, err := settingValue(ctx, s.db, KeyMergeChunkSize)
	if err != nil {
		return 0, fmt.Errorf("读 merge_chunk_size: %w", err)
	}
	n, ok := value.(int)
	if !ok {
		return 0, fmt.Errorf("%w: merge_chunk_size 不是整型", ErrInvalidSetting)
	}
	return n, nil
}

// embyItemColumns 是 emby_item 表的显式列清单（不含渲染辅助的 EpFrom/EpTo）。
const embyItemColumns = `SELECT id, drama_id, season, ep_no, kind, uid, runtime_sec FROM emby_item`

// EmbyItemByUID 按 uid 查一条对外剧集（单集 `e<episode.id>` / 合并集
// `m<dramaID>_<epFrom>_<epTo>`）。
//
// 播放面的 `/video/{id}` 用它判断「这个播放单元现在还在不在」：uid 只在派生表里
// 存在才算合法（docs/PLAYBACK-CONTRACT.md「不飘移」第 3 条 —— 改 N 之后旧 uid
// 立刻不再是播放单元，旧 URL 得到 404 而不是播到别的形态）。无行返回 ErrNotFound。
func (s *Store) EmbyItemByUID(ctx context.Context, uid string) (EmbyItem, error) {
	var it EmbyItem
	err := s.db.QueryRowContext(ctx, embyItemColumns+` WHERE uid = $1`, uid).
		Scan(&it.ID, &it.DramaID, &it.Season, &it.EpNo, &it.Kind, &it.UID, &it.RuntimeSec)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbyItem{}, ErrNotFound
	}
	if err != nil {
		return EmbyItem{}, fmt.Errorf("按 uid 查对外剧集 %q: %w", uid, err)
	}
	return it, nil
}

// ListEmbyItems 返回某部剧的对外剧集，按对外集号升序。
//
// 除表内列外还解出 EpisodeID（单集的源集主键，由 uid 的 `e` 前缀解析）、Title
// （单集的源集标题）与 EpFrom/EpTo（合并集的源集号范围，由 uid 直接解析）。
// vfs 渲染文件名与 NFO 都用它，因此这是「对外集号只由 emby_item 决定」的唯一入口
// （硬约束 6）。
func (s *Store) ListEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	rows, err := s.db.QueryContext(ctx, embyItemColumns+` WHERE drama_id = $1 ORDER BY ep_no`, dramaID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", dramaID, err)
	}
	defer rows.Close()

	out := make([]EmbyItem, 0)
	for rows.Next() {
		var it EmbyItem
		if err := rows.Scan(&it.ID, &it.DramaID, &it.Season, &it.EpNo, &it.Kind, &it.UID, &it.RuntimeSec); err != nil {
			return nil, fmt.Errorf("读取对外剧集行: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历对外剧集 drama_id=%d: %w", dramaID, err)
	}

	// 补齐渲染辅助字段：单集的源集（一次 IN 查询取全部）、合并集的源集号范围
	// （uid 里就有，不需要再查块成员）。
	var singleIDs []int64
	for i := range out {
		switch out[i].Kind {
		case "single":
			id, ok := parseUIDNumber(out[i].UID, "e")
			if !ok {
				return nil, fmt.Errorf("单集 uid %q 形态非法", out[i].UID)
			}
			out[i].EpisodeID = id
			singleIDs = append(singleIDs, id)
		case "merged":
			uidDrama, from, to, ok := ParseMergedUID(out[i].UID)
			if !ok || uidDrama != out[i].DramaID {
				return nil, fmt.Errorf("合并集 uid %q 形态非法", out[i].UID)
			}
			out[i].EpFrom, out[i].EpTo = from, to
		default:
			return nil, fmt.Errorf("对外剧集 %d 的 kind %q 非法", out[i].ID, out[i].Kind)
		}
	}
	if len(singleIDs) > 0 {
		placeholders := placeholders(1, len(singleIDs))
		args := make([]any, len(singleIDs))
		for i, id := range singleIDs {
			args[i] = id
		}
		epRows, err := s.db.QueryContext(ctx,
			`SELECT id, ep_no, title FROM episode WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("查派生表对应剧集: %w", err)
		}
		type epInfo struct {
			epNo  int
			title string
		}
		infoByID := make(map[int64]epInfo, len(singleIDs))
		for epRows.Next() {
			var id int64
			var info epInfo
			if err := epRows.Scan(&id, &info.epNo, &info.title); err != nil {
				_ = epRows.Close()
				return nil, fmt.Errorf("读取派生表对应剧集: %w", err)
			}
			infoByID[id] = info
		}
		if err := epRows.Err(); err != nil {
			_ = epRows.Close()
			return nil, fmt.Errorf("遍历派生表对应剧集: %w", err)
		}
		_ = epRows.Close()
		for i := range out {
			if out[i].Kind != "single" {
				continue
			}
			info, ok := infoByID[out[i].EpisodeID]
			if !ok {
				return nil, fmt.Errorf("派生表指向不存在的剧集 %d", out[i].EpisodeID)
			}
			out[i].EpFrom, out[i].EpTo = info.epNo, info.epNo
			out[i].Title = info.title
		}
	}
	return out, nil
}

// singleUID 是单集句柄：`e` + episode 主键十进制（与 internal/stream 的 uid 规则一致）。
func singleUID(episodeID int64) string {
	return "e" + strconv.FormatInt(episodeID, 10)
}

// mergedUID 是合并集句柄：`m<dramaID>_<epFrom>_<epTo>`。
//
// 只由剧 id 与块成员集号区间派生，不含自增主键：emby_item 整体重建、集集合与 N 不变时
// 逐字符不变（PLAYBACK-CONTRACT「不飘移」）。分隔符是 `_` 而不是 `-` —— 播放 id 是
// `<uid>-<签名>`，stream.ParseID 按首个 `-` 切，uid 里带 `-` 会把 404 顶成 401。
func mergedUID(dramaID int64, epFrom, epTo int) string {
	return "m" + strconv.FormatInt(dramaID, 10) + "_" + strconv.Itoa(epFrom) + "_" + strconv.Itoa(epTo)
}

// ParseMergedUID 解析合并集句柄 `m<dramaID>_<epFrom>_<epTo>`。
//
// 形态真值在这里：store 用它补 EpFrom/EpTo，internal/segment 用它把播放单元展开成
// 成员集（`ep_no ∈ [epFrom, epTo]`，列表连续切片的性质保证区间内没有别块的集）。
// 不匹配、剧 id 非正、区间倒置都返回 ok=false。
func ParseMergedUID(uid string) (dramaID int64, epFrom, epTo int, ok bool) {
	rest, ok := strings.CutPrefix(uid, "m")
	if !ok {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	dramaID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || dramaID <= 0 {
		return 0, 0, 0, false
	}
	epFrom, err = strconv.Atoi(parts[1])
	if err != nil || epFrom <= 0 {
		return 0, 0, 0, false
	}
	epTo, err = strconv.Atoi(parts[2])
	if err != nil || epTo < epFrom {
		return 0, 0, 0, false
	}
	return dramaID, epFrom, epTo, true
}

// parseUIDNumber 解析 `<prefix><十进制>` 形式的 uid，返回其中的数值。
func parseUIDNumber(uid, prefix string) (int64, bool) {
	rest, ok := strings.CutPrefix(uid, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
