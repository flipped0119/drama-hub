-- 0006_segment_per_episode.sql（PostgreSQL 版）

-- 「每集一片 fMP4」：segment 从「每 10 秒一行的分片索引」收窄成「每集的播放身份」
-- （docs/adr/0011-episode-per-segment-fmp4.md）。一集一行，只留 episode_id 与 channel。
--
-- 派生数据整体清空：切点 / 时长 / 关键帧 / 编码签名 / mode 五个概念都已消失，没有
-- 一行旧数据还有意义。重建只需要一次 ffprobe（不再切片），下次播放时按需完成。
DELETE FROM segment;

ALTER TABLE segment DROP COLUMN start_sec;
ALTER TABLE segment DROP COLUMN dur;
ALTER TABLE segment DROP COLUMN keyframe;
ALTER TABLE segment DROP COLUMN mode;
ALTER TABLE segment DROP COLUMN codec_sig;
ALTER TABLE segment DROP COLUMN id;

-- channel 是「这一集的字节流来自哪个通道」（web 明文 / app CENC 密文），
-- 它决定分片缓存键与 ffmpeg 的出站身份。判据仍然只有 channelOf 一处。
ALTER TABLE segment
    ADD COLUMN channel TEXT NOT NULL CHECK (channel IN ('web','app'));

ALTER TABLE segment ADD PRIMARY KEY (episode_id);

-- 关键帧索引这个缓存类型随关键帧索引一起删除（全仓无写入方，从未落过一行）。
-- 先删行再收窄 CHECK：PostgreSQL 加 CHECK 会校验已有行，留着 kind='index' 就加不上。
DELETE FROM artifact WHERE kind = 'index';
ALTER TABLE artifact DROP CONSTRAINT artifact_kind_check;
ALTER TABLE artifact ADD CONSTRAINT artifact_kind_check CHECK (kind IN ('cover','segment'));
