-- 0007_drop_shard.sql（PostgreSQL 版）：去掉目录分片层（MKACG-651 方案 A）。
--
-- 为什么删：Jellyfin 把「分类目录」当 Series、把「分片目录」当 Season（`东方仙侠/66`
-- → 「第 66 季」），真正的剧目录退化成普通 Folder —— 分片层让 Emby 认不出剧。
-- 目录形状改为 /<分类>/<剧名> (<年份>)/Season 01，分片键不再有任何读者。
--
-- 不可回滚：文件一经提交不再修改（迁移只向前）。DROP COLUMN 会一并删掉
-- idx_drama_genre_shard_title（它依赖 shard 列），所以旧索引不必单独 DROP；
-- 但显式写一条 IF EXISTS 让意图可读，也容忍「索引已被人工删掉」的库。
--
-- 同一二进制既迁移也不再读该列：服务启动即迁移，代码里已经没有 shard，
-- 不存在「先删列后发代码」的窗口。
ALTER TABLE drama DROP COLUMN IF EXISTS shard;

-- 分类目录一次列举 = 一次 WHERE genre IN (…) ORDER BY title。
-- (genre, title) 两列都是 COLLATE "C"，与查询的排序口径一致；同时覆盖
-- SELECT DISTINCT genre（覆盖索引前缀）。
CREATE INDEX IF NOT EXISTS idx_drama_genre_title ON drama (genre, title);
