# ADR-0011：播放片段改成「每集一片 fMP4」，缓存按集

状态：**已接受**（2026-11-02，MKACG-652）。编号 0011：本 ADR 起草时为 0010，同期合并的
MKACG-649（自动分块）先占用了 `0010`，故顺延（内容不变）。lxz 2026-11-02 批准：
「播放面统一为 m3u8，但片段改成『每集一片』，不再切 10 秒」。
取代 ADR-0007 的第 3、5 条（分片缓存键、关键帧索引）与 `docs/PLAYBACK-CONTRACT.md`
的「10 秒切片」口径；ADR-0007 的其余决定（单一播放家族、签名 id、m3u8 不缓存、
缓存归本服务）不变。

## 背景：实测到的三件事

1. **源直链支持标准 HTTP Range**：`Range: bytes=0-1023` → `206 content-range:
   bytes 0-1023/6462127`，中间区间 `bytes=500000-500999` 同样 206。既然 seek 可以交给
   源与播放器，「每 10 秒切一片」就不再是 seek 的前提 —— 它原本是为了让 `-c copy`
   的切点落在关键帧上（ADR-0007 第 5 条）。
2. **10 秒切片的成本**：一集 60 秒 = 6 个 `.ts`，每片一次 `ffmpeg` 冷启动
   （300–600 ms），一集 6 次；缓存键里带 `seq`，同一集在单集 / 合并集 / 重新分组后
   是**不同的文件**。
3. **容器形态是最差组合**：产出的是 **MPEG-TS 里包 HEVC**（`ffprobe` 实测
   `hevc 1080x1920 + aac`，`seg0` 1.95 MB / 13.03 秒）。MSE 不认识 TS，播放器
   （hls.js）必须自己把 TS 解复用再重封装成 fMP4 才能交给 MSE；而 HEVC-in-TS 的
   解复用是 hls.js 后来才补的（robwalch，2023-07：「to deliver HEVC via HLS.js your
   HLS segments should be fragmented MP4」；HEVC-in-TS 的解析到 1.5.14-canary 才有）。
   fMP4 则是 **passthrough**：hls.js 的 README 明说 fMP4 段「passed through to MSE
   without remuxing」。
   → 这正是 MKACG-648「只有进度与声音、没有画面」的候选解释之一（该 issue 已取消，
   其现象并入 MKACG-652，见下「黑屏结论」）。

## 决定

1. **片段 = 一整集**。播放单元的分片数 = 成员集数：单集 1 片、合并集 N 片。
   `seq` 就是成员下标，不再有「集内第几片」这一层。
   → `Timeline` / `Slice` 收敛为 `{Seq, EpisodeID, Dur}`。
2. **片段是自包含的 fragmented MP4**：`ffmpeg -c copy -movflags
   +frag_keyframe+empty_moov+default_base_moof -f mp4`。片头自带 `ftyp+moov`，
   因此每一片都能被单独 `ffprobe`（不依赖前一片），playlist **不需要
   `#EXT-X-MAP`**。`#EXT-X-VERSION:7`、后缀 `.m4s`、`Content-Type: video/mp4`。
3. **缓存键按集**：`sha256("e<episode.id>" \x1f <channel>)`，落
   `<cache_dir>/seg/<hash 前 2 位>/<hash>.m4s`。`seq` 不进键：合并集第 k 片、单集播放、
   重新分组后的同一集命中同一个文件。通道（`web` / `app`）必须进键 —— 同一集的
   两条通道字节流不同（App 源是 CENC 密文），只按集算键会串味。
4. **时长唯一真值 = `episode.duration_sec`**。首播时 `ffprobe` 只取
   `format=duration`（不再 `-skip_frame nokey`、不再收关键帧），回填走既有
   `BackfillEpisodeDuration`（只填 0）。playlist 的 `#EXTINF` 与
   `#EXT-X-TARGETDURATION` 都从它来（整数秒，每集 ≤ 0.5 s 误差）。
5. **删除四个概念**，不留死配置、不留别名：
   - 关键帧索引（`ArtifactKindIndex` + `<cache_dir>/index/` + `artifact_kind_check`
     里的 `'index'`；全仓无写入方）；
   - `seg_target_sec`（settings 键与校验范围）；
   - `codec_sig`（`segment` 列与 `cache.SegmentKey` 入参）：每集一片后它的分辨力
     只剩通道一维，多存就是死状态；
   - `mode`（`copy` / `transcode` 列）：全仓从来没有转码实现，是纸面能力。
   `segment` 表就地收窄成 `(episode_id, channel)`，不新增表。
6. **集边界的时间戳：`#EXT-X-DISCONTINUITY` 只表达边界，绝对时间轴由出流时平移 tfdt 提供**
   （本条在 MKACG-652 功能检测后推翻并重写，见下「修正：集边界时间戳」）。
   `-ss/-t` 与 `Slice.OffsetSec` 一起消失，但「每片 PTS 从 0 起」不能直接上线。
7. **子进程超时**：整集 remux 从 60 s 提到 300 s —— 整集要读完 CDN 全程
   （10 分钟集 ≈ 40 MB），60 s 在弱网上会误杀。

## 黑屏结论（MKACG-648 并入本任务）

**本任务能证明的**：

- 新产出的流是合法的、可解码的自包含 fMP4。容器内 ffmpeg 6.1.2 用同一配方
  实测：`hvc1` + `aac`、顶层 box `ftyp,moov,moof,mdat,mfra`、`ffprobe` 报
  `duration=6.08`、`ffmpeg -i … -f null -` 无错误输出。测试侧把这条固化成
  `assertSelfContainedSegment`（片头必须是 `ftyp,moov`、必须有 `moof`、必须能单独
  `ffprobe` 出视频流与时长、必须能无错解码）。
- 因此「我们的流有问题」这个假设在**容器与切分**这一层被排除：TS+HEVC 是 MSE 必须
  自己重封装的组合，fMP4+HEVC 是 passthrough。
- 「播放端解不解 HEVC」是另一回事，取决于浏览器/MSE 的 `isTypeSupported`（hls.js
  自己也是这么判的）：Safari/带硬解的 Edge/Chrome 能放，不支持的则无论容器是什么都
  放不了。**这一条本环境无法验证**（没有真机、没有 Emby、没有真实红果源），
  所以 AC「浏览器与 Emby 各播一集画面与声音都在」在 MKACG-652 里标为未验证，
  需要真机复测；若仍无画面，根因判定为「播放端不支持 HEVC」，转码是独立决策
  （AGENTS.md「不做转码流水线」，本 ADR 不改变它）。


## 修正：集边界时间戳（MKACG-652 功能检测，2026-11-02）

**原判据错了**：D6 曾写「集边界的时间戳重置由 `#EXT-X-DISCONTINUITY` 表达」。功能检测
（赛博富哥）在合并块上量到 `ffprobe -count_frames` 只有 725/2475 帧（29%），我在容器内
用 ffmpeg 6.1.2 独立复现并定位：

- hls demuxer 遇到 `#EXT-X-DISCONTINUITY` 只打印 `Skip`、**不重定时间基准**，包 PTS 在
  集边界回退（12.04 → 0.08），解码器按回退丢帧；
- 自造 3 集（4/6/3 秒、25 fps、共 325 帧）按原实现出片：整条 playlist
  `frames=275, packets=325`（丢 50 帧），三片单独 `ffprobe` 各是 100/150/75（片本身完好）；
- 把每片 `moof/traf/tfdt` 的 `baseMediaDecodeTime` 平移到「前面各集 EXTINF 之和」后，
  同一条 playlist `frames=325, packets=325`，逐片首 PTS `0.08 / 4.08 / 10.08` 严格单调，
  `ffmpeg -i playlist -f null -` 无 stderr。

**为什么不能靠 ffmpeg 参数**（同一源、`-c copy`、看首包 PTS，全部实测）：

| 手段 | 输出 | 首 PTS |
|---|---|---|
| 基线 `-movflags +frag_keyframe+empty_moov+default_base_moof` | fMP4 | 0.080 |
| `-output_ts_offset 4` | fMP4 | 0.080（无效） |
| `-itsoffset 4` / `+ -copyts` / `-avoid_negative_ts disabled` | fMP4 | 0.080（无效） |
| `-bsf:v setts=ts=TS+4/TB` | fMP4 | 0.000（无效） |
| `-movflags +dash+…` | fMP4 | 0.080（无效） |
| `-output_ts_offset 4` | **mpegts** | 5.400（有效） |

mov 封装器把 tfdt 归一化到轨道起点，所以「把偏移写进片段」这条在 fMP4 上无解；TS 时代
`-output_ts_offset` 之所以管用，正是因为 TS 封装不归一。

**新判据（D6'）**：片段的**缓存字节保持 0 基自包含**，时间轴平移放在**出流那一步**做 ——
`ShiftTimeline` 按本片在播放单元里的起点（前面各集 `duration_sec` 之和）给每个
`moof/traf/tfdt` 加 `offset × 轨道 timescale`（每轨 timescale 不同，先读 `mdhd`），
offset=0 时零拷贝。

这样三条同时成立：
1. ffmpeg 系播放端（Emby 转码/DirectStream、mpv）看到单调 PTS，不再丢帧；
2. 缓存键与 D3 一字不改（拆分位置是呈现层的事，缓存里仍是一集一个文件，改
   `merge_chunk_size` 不会产生第二份缓存，也不会因缓存里躺着旧起点而串味）；
3. 片段仍自包含（片头 `ftyp+moov`、单独可 `ffprobe`），`Content-Length` 与 Range 不变
   （平移只改 4/8 字节字段，长度不变）。

代价：非首片请求多一次整片拷贝（12–40 MB）。回归钉子：
`TestMergedPlaylist_DecodesEveryFrame`（整条 playlist 的 `frames == packets`）+
`mp4time_test.go`（每轨加数、入参不被改写、结构不合规报错）。
**前置集时长缺失时必须先补齐，不能按 0 出流**（同一轮功能检测的第二个阻断项）：只请求
分片、不取 playlist（或首播时 playlist 与分片并发到达）时 `duration_sec` 仍是 0，起点恒 0，
出站片又回到「每片从 0 起」——检测现场 4 片首 PTS 全 0，拼成 playlist 后 1550 包只解出
800 帧（丢 48%）。所以 `timelineStart` 在前置集时长为 0 时现场补齐：解析一次直链 +
`ffprobe format=duration` + 回填（与 playlist 路径同一份 `ensureDuration`），补齐后写库，
后续请求回到零外呼。代价边界：只有这种情形会外呼，次数 = 前面还没回填的集数（≤ 一个合并
块的成员数，默认 10）且只发生一次；补齐失败按播放面错误矩阵返回（502/503），绝不出时间轴
错误的字节。回归钉子：`TestSegmentDirectRequestFillsDuration`（不取 playlist 直接请求第 2 片，
首 PTS 必须 ≈ 前一集时长、且补齐后第二次请求零解析）。
## 被否决的方案

1. **保留 10 秒切片，只把 TS 换成 fMP4。**
   否决理由：每集仍然 6 次 `ffmpeg` 冷启动，缓存键仍然带 `seq`（同集在单集/合并集里
   是两份文件），收益只有「容器变好」；而「每集一片」把每集的 ffmpeg 次数从 6 降到 1、
   让缓存按集复用，且片段自包含天然可逐片 ffprobe。
2. **给 playlist 加 `#EXT-X-MAP` 指向一个共享 init 文件。**
   否决理由：合并集每集编码参数可能不同（集间 `extradata` 不一致），共享 init 要么
   强制所有集同参（需转码），要么按集给 init（等于每集多一个文件、多一次请求）。
   自包含片段把 init 塞进每片，代价是每片多几百字节，换来「孤立一片也能播」。
3. **把集边界偏移烘进缓存字节**（remux 后平移 tfdt 再落盘）。
   否决理由：字节一旦带位置，缓存键就必须把起点算进去（D3 的「同一集在任何播放单元共用
   同一文件」不再成立），改 `merge_chunk_size` 会让整块重新 remux、缓存翻倍、旧文件只能等
   LRU。而「出流时平移」把位置留在呈现层，缓存与 D3 都保住。
4. **把时长落成 float（`duration_ms`）避免整数化误差。**
   否决理由：第二份时长真值。合并 10 集最多累计 ~5 s 的显示差（各集 ±0.5 s），
   由 `#EXT-X-DISCONTINUITY` 与播放器自己的时间轴吸收，不值得为它加一列。
5. **给 `segment` 表新增 `channel` 之外的「片数 / 是否已生成」列。**
   否决理由：一集一片之后这些都能由缓存键与 `artifact` 行推出（命中即已生成），
   存起来就是会与真实状态漂移的第二份状态。

## 后果

- **首播成本上移**：以前只看前 30 秒就只切前 30 秒，现在首播必须整集拉完
  （10 分钟集 ≈ 40 MB）。这是「每集一片」的既定代价，不是回归。
- **内存峰值**：整集字节走 `cmd.Output()` → 峰值约 2×整集（40 MB/次），并发闸上限 4
  → 峰值约 320 MB。
- **缓存占用形态变了**：从「一片 650 KB」变成「一集 12–40 MB」，
  `cache_max_mb=8192` 实际可容纳的集数要按集重算（`docs/CACHE-MODEL.md`）。
- **旧缓存不冒充**：键的 preimage 与后缀都变了（`.ts` → `.m4s`），旧片永不命中；
  旧行与旧文件不主动删，交给 `cache_lru_days` 自然淘汰。
- **升级要清 `segment` 表**：迁移 `0006` 直接 `DELETE FROM segment`
  —— 它是派生数据，下次播放重建（一次 `ffprobe`，不再切片）。
- **出流多一次拷贝**：合并块的非首片请求会把整片复制一份再平移 tfdt（12–40 MB/次）——
  这是「缓存按集 + 绝对时间轴」两条同时成立的对价（见「修正：集边界时间戳」）。
- 测试侧新增两个钉子：`assertSelfContainedSegment`（片段必须自包含）与
  `TestMergedPlaylist_DecodesEveryFrame`（整条 playlist 的 `frames == packets`）。
  任何「片段不再自包含」或「时间轴又开始回退」的改动都会在这里红。
