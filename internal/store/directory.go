package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是「目录列举」的几条查询：虚拟文件树的每一层只查一层、
// 每次请求常数条 SQL（docs/DIRECTORY-CONTRACT.md「MUST：目录列举必须是
// 「一个目录一次查询」」）。四条查询都必须走索引：
//
//	ListGenres          → idx_drama_genre_title（覆盖 DISTINCT genre）
//	GenreExists         → idx_drama_genre_title（genre 点查 EXISTS）
//	ListDramaDirsByGenre → idx_drama_genre_title（genre, title）
//	DramaByTitleYear    → idx_drama_title
//
// 只有 ListGenres（根目录）会扫整条覆盖索引；其余三条都是点查 / 区间查询。
// 计划由 TestDirectoryQueryPlans 钉住（EXPLAIN 断言索引名）。

// ListGenres 返回库中出现过的全部分类（genre 原值，含表示「未声明」的空串），
// 按字节序升序。
//
// 只有根目录用它，必须一次拿到全部 68 个分类，因此这是一条覆盖索引全扫
// （60 万行约 74ms，随行数线性增长）。其它层不要用它，用 GenreExists 点查。
func (s *Store) ListGenres(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT genre FROM drama ORDER BY genre`)
	if err != nil {
		return nil, fmt.Errorf("列分类: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 8)
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("读取分类行: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分类: %w", err)
	}
	return out, nil
}

// GenreExists 报告某个 genre 原值在库里是否出现过。
//
// 供 vfs 解析「一个分类目录名对应哪些 genre 值」：目录名 → 取值集合是纯映射
// （`其他` → `{"", "其他"}`，其余就是目录名本身），所以只需要存在性判断。
//
// 必须是 `EXISTS (SELECT 1 … WHERE genre = $1)` 这种点查（O(log n)，实测微秒级）：
// 换成 `SELECT DISTINCT genre FROM drama` 是覆盖索引全扫（60 万行约 74ms），
// 换成 `SELECT DISTINCT genre WHERE genre IN (…)` 也要扫该分类的全部索引行
// （183k 行约 44ms）——两者都会让每层列举随库规模增长。
func (s *Store) GenreExists(ctx context.Context, genre string) (bool, error) {
	var ok bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM drama WHERE genre = $1)`, genre).Scan(&ok); err != nil {
		return false, fmt.Errorf("查分类是否存在 %q: %w", genre, err)
	}
	return ok, nil
}

// DramaDir 是一部剧在目录列举里需要的**全部**字段：只够算出它的目录名，
// 不含 plot / cover_url / episode_total / sync_at / tags 这些只在渲染 NFO 时才用的大列
// （Genre 留着：归属校验要用它）。
//
// 为什么单独一个类型（MKACG-651 实测）：分类目录列举是唯一会一次物化整个分类的查询，
// 216k 子项时 []Drama 的堆峰值 ≈96 MB，其中 plot/tags 物化占 ≈45 MB（47%）；
// 换成窄列后堆峰值 ≈51 MB。渲染路径不受影响——它本来就走 (title, year) /
// (provider, provider_id) 点查拿完整 Drama。
type DramaDir struct {
	ID         int64
	Provider   string
	ProviderID string
	Title      string
	Year       int
	// Genre 只用于归属校验（目录名 → 该剧确实属于这个分类目录），不参与命名。
	Genre string
}

// ListDramaDirsByGenre 返回给定分类值集合下全部剧目的目录字段，按 title 升序。
//
// 这是「分类目录一次查询」：结果集就是一个分类目录的子项数（无分片层摊薄，
// docs/DIRECTORY-CONTRACT.md「单目录子项上限」），不分页、不截断。
// 走 idx_drama_genre_title，代价随该分类的剧数线性增长——这是去掉分片层后
// 接受的唯一代价，实测数字见契约文档。
func (s *Store) ListDramaDirsByGenre(ctx context.Context, genres []string) ([]DramaDir, error) {
	if len(genres) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(genres))
	for _, g := range genres {
		args = append(args, g)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider, provider_id, title, year, genre FROM drama WHERE genre IN (`+
			placeholders(1, len(genres))+`) ORDER BY title`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("列分类 %v 的剧目: %w", genres, err)
	}
	defer rows.Close()

	out := make([]DramaDir, 0, 32)
	for rows.Next() {
		var d DramaDir
		if err := rows.Scan(&d.ID, &d.Provider, &d.ProviderID, &d.Title, &d.Year, &d.Genre); err != nil {
			return nil, fmt.Errorf("读取剧目行: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分类 %v 的剧目: %w", genres, err)
	}
	return out, nil
}

// DramaByTitleYear 按 (title, year) 点查一部剧，走 idx_drama_title。
//
// 无行返回 ErrNotFound。返回值不唯一时（同名同年的剧可以有两条）取 id 最小的
// 一条，调用方必须用它算出的目录名反查一次（vfs 的 locateDrama 会比对，不匹配
// 则退到分类内查询），因此这里不承诺「返回的就是你要的那部」。
func (s *Store) DramaByTitleYear(ctx context.Context, title string, year int) (Drama, error) {
	row := s.db.QueryRowContext(ctx,
		dramaColumns+` WHERE title = $1 AND year = $2 ORDER BY id LIMIT 1`, title, year)
	d, err := scanDrama(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Drama{}, ErrNotFound
	}
	if err != nil {
		return Drama{}, fmt.Errorf("按 title/year 查剧目: %w", err)
	}
	return d, nil
}

// placeholders 返回从 from 起、共 n 个逗号分隔的位置占位符（$from,$from+1,…），
// 供 IN (…) 拼接。PostgreSQL 的位置占位符带序号，所以起点必须是调用方算好的。
//
// 只拼占位符个数，参数一律走绑定（值不进 SQL 文本）。
func placeholders(from, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(from+i)
	}
	return strings.Join(parts, ",")
}
