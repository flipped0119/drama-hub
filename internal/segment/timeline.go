package segment

// TimelineEpisode 是时间轴上的一个成员集：它自己的真实时长（episode.duration_sec）。
//
// 「每集一片」之后一集恰好贡献一个分片，所以时间轴上没有「集内切片」这一层：
// 第 seq 片就是第 seq 集。
type TimelineEpisode struct {
	EpisodeID int64
	Dur       float64
}

// Slice 是 seq 解析出的一个分片：它就是某一整集。
//
// Seq 是播放单元里的全局序号（0 起、连续）；Dur 是该集的真实时长，播放列表按它写
// EXTINF 并在集边界插 EXT-X-DISCONTINUITY。
type Slice struct {
	Seq       int
	EpisodeID int64
	Dur       float64
}

// Timeline 是把若干集按给定顺序拼成的播放时间轴。seq 从 0 起连续、跨集不断号。
// 纯内存结构（不落库、不缓存）；At 是下标取值，O(1)。
type Timeline struct {
	slices []Slice
}

// NewTimeline 按 eps 的顺序排分片，返回 seq 从 0 起的映射。
func NewTimeline(eps []TimelineEpisode) *Timeline {
	t := &Timeline{slices: make([]Slice, 0, len(eps))}
	for _, ep := range eps {
		t.slices = append(t.slices, Slice{Seq: len(t.slices), EpisodeID: ep.EpisodeID, Dur: ep.Dur})
	}
	return t
}

// Len 返回时间轴上的分片总数（= 成员集数）。
func (t *Timeline) Len() int { return len(t.slices) }

// At 返回 seq 对应的分片；seq 越界（< 0 或 ≥ Len）返回 ok=false。
func (t *Timeline) At(seq int) (Slice, bool) {
	if seq < 0 || seq >= len(t.slices) {
		return Slice{}, false
	}
	return t.slices[seq], true
}

// Total 返回时间轴总时长：各集时长之和。
func (t *Timeline) Total() float64 {
	total := 0.0
	for _, s := range t.slices {
		total += s.Dur
	}
	return total
}
