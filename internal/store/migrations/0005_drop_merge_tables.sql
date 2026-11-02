-- 0005_drop_merge_tables.sql（PostgreSQL 版）

-- 合并集改成「按 merge_chunk_size 自动分块、只落 emby_item 派生表」
-- （docs/adr/0010-auto-merge-chunking.md）。手工编排的两张表退出主流程，
-- 自动分组是唯一来源：留着它们只会留下「两套真相，谁优先」的问题。
--
-- 先删成员表再删组表：merge_item.group_id 有 ON DELETE CASCADE，顺序反了也能删，
-- 但显式从子表开始读起来更清楚。
--
-- 不可逆：人工编排的历史数据随表丢失（ADR-0010「后果」）。
DROP TABLE IF EXISTS merge_item;
DROP TABLE IF EXISTS merge_group;

-- 派生表不迁移数据：旧合并行的 uid 是 m<merge_group.id>，新代码的形态真值
-- （store.ParseMergedUID）解不了它，留着会让该剧 Season 01 的列举直接报错
-- （vfs → webdav PROPFIND 失败），而自愈点只有下一次重建。
-- 所以顺手清掉「新代码读不动的合并行」：派生表本来就是重建的产物，下次重建
-- （同步完成 / 首播回填）会按当前 merge_chunk_size 补回正确的块。
-- 单集行 e<episode.id> 形态未变，保留不动。
DELETE FROM emby_item WHERE kind = 'merged' AND uid !~ '^m[0-9]+_[0-9]+_[0-9]+$';
