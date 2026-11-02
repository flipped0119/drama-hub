# ADR-0013：删除目录分片层（分类目录下直接是剧）

状态：已接受（2026-11-02，lxz 定稿；MKACG-651 方案 A）
取代：`docs/DIRECTORY-CONTRACT.md` v3 的「D2：分类目录下必须有分片层」

## 背景

v3 在分类目录与剧目录之间加了一层纯函数分片（`shard = fnv32a(provider_id) % 128`，
目录名 `00`–`7f`），目的是让单个目录的子项数落回 `PROPFIND` 能列出来的量级
（最大分类外推 21.6 万）。

2026-11-02 在 nas 上直接读 `jellyfin.db` 实测，结论是这层**帮了倒忙**：

| 观测 | 实测结果 |
|---|---|
| Jellyfin 扫出的 Series 数 | **59**（= 分类目录数），而真实剧目录有 1436 个 |
| 每一层的角色 | `<分类>` 被当 Series、`<分片>` 被当 Season（`东方仙侠/66` → 「第 66 季」） |
| 真正的剧目录 | 退化成普通 Folder，Emby/Jellyfin 完全不认 |

也就是说分片层不但没解决「扫不进去」，反而让两个客户端都认不出剧。

同时必须回答的是：**去掉分层之后，一个分类目录的 `PROPFIND` 到底付多少代价？**
这不能靠外推，必须实测（见「实测结论」）。

## 决定

1. **树形固定四层**：`/<分类>/<剧名> (<年份>)/{tvshow.nfo, Season 01/*.strm|*.nfo}`。
   分类与剧之间不再有任何中间目录层，且**以后也不许加**（写进 `AGENTS.md` 硬约束 10）。
2. **分片机制整体下线**：`store.ShardOf` / `store.ShardCount` / `shardDirName` /
   `parseShard` / `drama.shard` 列 / `idx_drama_genre_shard_title` 全部删除；
   迁移 `0007_drop_shard.sql` 删列并建 `idx_drama_genre_title (genre, title)`。
3. **分类目录列举 = 一次 `WHERE genre IN (…) ORDER BY title`**，不分页、不截断、不缓存。
   该查询只取窄列（`store.DramaDir`：`id/provider/provider_id/title/year/genre`）。
4. `.strm` / `.nfo` 的字节与文件名算法、`/video` 出流链路、合并集虚拟 HLS、
   `internal/webdav` 的生产代码**一律不改**。
5. 不做路径兼容层。老路径与新路径不会同时可用：Emby/Jellyfin 库必须重扫，
   这是 2.0 的一次性代价。

## 实测结论（复现方式与完整表格见 `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」）

真 PostgreSQL 16 + 真二进制容器（冷启动，读数取 `/proc/1/status` 的 `VmHWM`），
`PROPFIND Depth: 1`：

| 最大分类 N | 响应体 | 墙钟 | RSS 增量 |
|---|---|---|---|
| 100（当前真实规模） | 0.1 MB | 3–4 ms | 0 MB |
| 10,000 | 5.4 MB | 0.20–0.31 s | 1–2 MB |
| 100,000 | 52.9 MB | 1.99 s | 68.0 MB |
| 216,000（L1 外推） | 114.7 MB | 4.06 s | 136.9 MB |

判据线是墙钟 ≤5 s、RSS 增量 ≤200 MB、响应体 ≤100 MB。`Depth: 1` 越线的是**响应体**
（216k 时 114.7 MB，超 15%），另两项有余量但不大（墙钟剩 ~19%）。

**省略 `Depth` 头 / `Depth: infinity`（= 递归整棵子树）三项全越线**：216k 剧
369.0 MB / 115.9–120.2 s / RSS +501 MB。这是 `x/net/webdav` 的既有语义（缺头默认
`infiniteDepth`，RFC 4918 §9.1），不是本 ADR 引入的；本服务不拦截它，只把代价写进上限。

响应体大小是 `PROPFIND` 协议本身的下限（无分页），服务端只能控制「别为它多花内存」
——已用窄列把 216k 的堆峰值从 100–130 MB 压到 47–51 MB。

升级路径（按证据强度）：**客户端带 `Depth: 1`**（rclone/Emby/Jellyfin 本来就这么扫）；
**Emby/Jellyfin 一个库挂多个 media folder，每个分类一个**（纯客户端配置）；其次给超大
分类单独限库；**不恢复分片层**（那会让剧彻底认不出来）。若要挡住整树递归，需改
`Depth` 处理，属另一件决策（另开 ADR）。

## 后果

- 分类目录的 `PROPFIND` 从「常数 128 个子项」变成「该分类的全部剧目录」：
  当前规模 4 ms，L1 最坏假设 4.06 s。服务端成本对子项数线性，没有隐藏的 O(n²)
  ——前提是请求内快照（`withSnapshot`）不被绕过。
- 同名同年的剧现在在**同一分类内**必定冲突，部分剧目录名会带上
  `[provider-provider_id]` 后缀（原先只有落在同一分片才冲突）。这是契约要求的必然变化。
- `Depth: 1` 的成本对子项数线性（真服务冷启动实测：21.6 万剧的分类目录 114.7 MB / 4.06 s / RSS +137 MB）；**省略 `Depth` 头或 `Depth: infinity` 会递归整棵子树**，同一目录 369 MB / 116 s / RSS +501 MB，三项判据全越线。这是 `x/net/webdav` 的既有语义（缺头默认 `infiniteDepth`），不是本 ADR 引入的；本服务刻意不拦截它（拦截要改 `Depth` 支持，超出本任务契约），只把代价写进上限与排障口径。
- 脏标题（含 `/ : * ?`）的单文件 `Render` 回退路径从「扫一个分片」变成「扫一个分类」：
  21.6 万部时实测 0.77–1.41 s/文件。这类剧是少数；若量大到影响扫描，升级路径是加
  派生列或函数索引，**不是**恢复分片层。
- `rclone --max-depth` 从 5 降到 4；Emby/Jellyfin 的库根必须是**分类目录**而不是
  挂载点根——挂载点根会让分类目录自己被当 Series（这与分片层是否存在无关，
  是「库根挂在哪」的问题，实测记录在同名文档）。
- 迁移 `0007` 的 `DROP COLUMN` 不可回滚：单实例部署、迁移随启动执行、同一个二进制
  既迁移也不再读该列，因此不存在「先删列后发代码」的窗口。不要拆成两步发布。

## 相关

- `docs/DIRECTORY-CONTRACT.md`（v5，树形与单目录子项上限的真值）
- `docs/DATA-MODEL.md`（`0007_drop_shard.sql`）
- `internal/webdav/bench_propfind_test.go`（`DRAMA_HUB_BENCH=1` 的实测用例）
