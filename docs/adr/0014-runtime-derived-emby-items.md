# ADR-0014：合并集在运行时派生，不再落库

状态：**已接受**（2026-11-03，MKACG-666）。取代 [ADR-0010](0010-auto-merge-chunking.md)
里「合并结果落 `emby_item` 派生表」的部分；分块语义本身不变。

## 背景

ADR-0010 把合并集做成「按 `merge_chunk_size` 自动分块，结果写进 `emby_item` 派生表」，
重建点只有两个：同步完成（`internal/sync`）与首播回填时长
（`store.BackfillEpisodeDuration`）。

问题：**改配置之后要等下一次重建才生效**。实测（22 集，N=10）：

1. 重建后 `emby_item` 3 行（对外 3 集）；
2. 把 N 改成 5、**不重建** → 仍是 3 行，`ls` 与 `.m3u8` 都不变；
3. 再重建 → 5 行。

所以用户看到的是「先一集一个、后来变十合一」——库里存的不是原始集，而是某一时刻按当时 N
算出的合并结果。lxz 2026-11-03 定：**数据库只存原始集，合并在运行时合成**，改
`merge_chunk_size` 必须立即影响对外集号、文件名与 playlist，不需要同步、不需要重建。

## 决策

**方案 B：完全运行时派生。** 删掉 `emby_item` 表，对外形态只由 `episode` + 当前 N 算出：

- `store.deriveEmbyItems(dramaID, eps, n)` 是唯一派生实现（纯函数）：`episode` 按
  `ep_no` 升序切连续列表切片，第 i 块的对外集号 = i；块内 1 集 → `kind=single`、
  `uid=e<episode.id>`；≥2 集 → `kind=merged`、`uid=m<dramaID>_<epFrom>_<epTo>`；
  `runtime_sec` 单集取本集、合并集取块内之和。
- `store.ListEmbyItems` = 现读 `merge_chunk_size` + `ListEpisodes` + `deriveEmbyItems`。
  vfs 的文件名/NFO 与播放面共用它，所以「对外集号、文件名、playlist」永远同一份规则。
- `store.PlaybackEpisodes(ctx, uid)` 用同一套规则判 uid 合法性并展开成员集：合并集必须
  匹配某个块的首尾集号，单集必须是当前 N 下自成一块的那一集。不匹配一律 `ErrNotFound`
  → 404。
- 删除 `RebuildEmbyItems` / `rebuildEmbyItems` / `EmbyItemByUID` / `ListEpisodesInRange`；
  同步只写 `drama` / `episode`，回填只写 `episode.duration_sec`。
- 迁移 `0008_drop_emby_item.sql`：`DROP TABLE IF EXISTS emby_item;`，不迁移数据
  （表里本来就只是派生结果，原始数据都在 `episode`）。

## 理由

- 库里只存原始集，符合「数据库是事实、形态是视图」的分层；没有「什么时候重建派生表」
  这个问题，也就没有滞后、没有漏重建的写路径。
- 读路径不写库：不存在「读时发现配置版本变了就先重建」的写放大，也不需要配置版本列、
  并发重建锁。
- 代价可控：每次列举多读一次 `settings` 单行（本来就要读 `episode`），播放路径同样是
  一次 settings 读 + 一次该剧集列表读；没有一集一查的 N 次查询。
- 旧 uid 立即 404 是需求本身要的语义（改 N 后旧合并 id 不该再能播），不是副作用。

## 被否方案

**方案 A：派生表按需重建**（`emby_item` 仍落库，读时发现配置版本变了就先重建该剧）。
否决理由：读路径写库；需要配置版本簿记与并发重建串行；仍然存着「某一时刻的合并结果」，
和「库里只存原始集」的要求相悖。ADR-0010 也已经因为同样的「多一层派生表 + 什么时候重建它」
否决过 `merge_block` 表。

## 后果

- 改 N 会立即重排全库对外集号，旧合并 uid 全部失效（Emby 观看进度对不上，lxz 已接受，
  同 ADR-0010）。
- `emby_item` 从表清单消失；`/api/dramas` 的 `emby_items` 字段仍在，值改为运行时算出的
  对外集数。
- 不新增全库重建入口：2847 万集在单个请求里做不完，也不需要——派生是读时算的。
- 运维上的 `TRUNCATE`/备份清单不再包含该表（`docs/DEPLOY.md` 已同步）。

## 相关

- [ADR-0010](0010-auto-merge-chunking.md)：自动分块的语义来源（本 ADR 只改「存哪里」）。
- `docs/DEV-STANDARDS.md`「数据库」「时长探测」、`docs/PLAYBACK-CONTRACT.md`
  「不飘移」、`docs/DATA-MODEL.md`「合并数据」。
