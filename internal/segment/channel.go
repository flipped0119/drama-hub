package segment

import (
	"github.com/justforlxz/drama-hub/internal/stream"
)

// 通道取值（MKACG-626 裁决 3）。
//
// 同一集的网页源与 App 源字节流不同（App 源是 CENC 密文，ffmpeg 要用 -decryption_key
// 解），通道因此同时决定两件事：分片缓存键（cache.SegmentKey 的第二段）与 ffmpeg 的
// 出站身份（fetchHeaders）。判据只有一处：channelOf。
const (
	channelWeb = "web" // 网页通道：明文源
	channelApp = "app" // App 通道：CENC 密文源，ffmpeg 要 -decryption_key
)

// channelOf 是通道的唯一判据：带内容密钥的流是 App 通道，其余是网页通道。
//
// 判据只能是「有没有 key」：插件契约里没有通道名字段，新增一个就为了拼缓存键是
// 多余的概念；而字节流的分水岭恰好就是这个 —— 网页通道交付明文（受保护剧只给
// 10 秒预览，按 626 D1 直接 ErrNoStream，不当作直链交付），密文只可能来自 App
// 通道（docs/PLUGIN-CONTRACT.md「加密切片与 Key」）。
func channelOf(d stream.Direct) string {
	if len(d.Key) > 0 {
		return channelApp
	}
	return channelWeb
}
