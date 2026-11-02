package segment

import (
	"math"
	"strconv"
	"strings"
)

// playVersion 是自造 playlist 的 HLS 版本。fMP4 片段要求 ≥ 6（EXT-X-MAP 引入），
// 本实现不给 EXT-X-MAP（片段自包含），取 7 与契约示例一致（docs/PLAYBACK-CONTRACT.md）。
const playVersion = 7

// segmentSuffix 是分片 URL 的后缀：fragmented MP4（每集一片）。
const segmentSuffix = ".m4s"

// BuildPlaylist 把时间轴渲染成 VOD playlist 字节（纯函数）。
//
// 每片一行 `#EXTINF:<该集真实时长>,` + 分片 URI `/video/{id}/{seq}.m4s`；集边界
// （EpisodeID 变化）插 `#EXT-X-DISCONTINUITY`。单集与合并集走同一个函数：单集的
// 时间轴只有一集，因此不会出现 DISCONTINUITY（契约：对外不再有「单集是 MP4」）。
//
// TARGETDURATION 取最大片长向上取整（HLS 要求它 ≥ 每个 EXTINF）。
// 字节长度 = len(返回值)，调用方用它做 Content-Length（硬约束 3）。
func BuildPlaylist(id string, slices []Slice) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:" + strconv.Itoa(playVersion) + "\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	maxDur := 0.0
	for _, s := range slices {
		if s.Dur > maxDur {
			maxDur = s.Dur
		}
	}
	b.WriteString("#EXT-X-TARGETDURATION:" + strconv.Itoa(int(math.Ceil(maxDur))) + "\n")

	prevEpisode := int64(-1)
	for _, s := range slices {
		if prevEpisode != -1 && s.EpisodeID != prevEpisode {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString("#EXTINF:" + formatSeconds(s.Dur) + ",\n")
		b.WriteString("/video/" + id + "/" + strconv.Itoa(s.Seq) + segmentSuffix + "\n")
		prevEpisode = s.EpisodeID
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}
