package cache

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/justforlxz/drama-hub/internal/store"
)

// 缓存键的两种规则（真值见 docs/CACHE-MODEL.md「落盘布局与键」）。
//
// 键只用不含 secret 的稳定标识：分片用 `e<episode.id>` + 通道，海报用
// `<provider>/<provider_id>`。所以换 stream_secret 只会让已发出的 URL 失效，
// 不会让缓存失效（ADR-0007「后果」）。

// keySeparator 是分片键的分段符（\x1f 不会出现在 uid / 通道名里）。
const keySeparator = "\x1f"

// SegmentKey 是分片的 artifact 键，同时也是落盘文件名：
// sha256("e<episode.id>" \x1f channel) 的十六进制（ADR-0011 第 3 条）。
//
// 片段只由「哪一集 + 哪个通道」决定：合并集的第 k 片、单集播放、重新分组后的同一集，
// 都命中同一个文件（seq 不进键）。通道进键是因为同一集的网页源与 App 源字节流不同
// （App 源是 CENC 密文，ffmpeg 用 -decryption_key 解出来），只按集算键会串味。
//
// 本函数不认识通道的语义，只消费调用方给的字符串 —— 判据只有一处
// （segment.channelOf），索引行与这里拿的是同一个值。
func SegmentKey(episodeUID, channel string) string {
	preimage := episodeUID + keySeparator + channel
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// CoverKey 是海报的 artifact 键：`<provider>/<provider_id>`。
//
// S3（internal/cache/cover.go）已有实现，规则收在这里，避免两处各拼一遍。
func CoverKey(provider, providerID string) string { return provider + "/" + providerID }

// cacheHash 把 artifact 键映射成落盘用的十六进制哈希。
//
// 契约里 `<hash> = sha256(<键>)`：分片那条的 `<键>` 本身已经是 sha256(uid|channel)，
// 直接用（再哈希一次就与文档的路径不符）；海报的键是语义键，取 sha256(键)。
func cacheHash(kind, key string) string {
	if kind == store.ArtifactKindSegment {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
