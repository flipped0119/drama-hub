package segment

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// testKey 是通道测试用的内容密钥（16 字节 AES-128，CENC-AES-CTR 的内容密钥）。
var testKey = bytes.Repeat([]byte{0x2a}, 16)

// makeEncryptedVideo 现场造一份 CENC-AES-CTR 密文 MP4，返回它的明文对照源
// （同参数生成的明文，供「解密后是否等价」比较）。加密命令与 MKACG-626 的实测
// 命令一致；容器里的 ffmpeg 就是运行时那个二进制。
func makeEncryptedVideo(t *testing.T, path string, seconds int, size string, key []byte) (plain string) {
	t.Helper()
	plain = filepath.Join(t.TempDir(), "plain.mp4")
	makeVideoSize(t, plain, seconds, size)
	kid := bytes.Repeat([]byte{0x11}, 16)
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-i", plain, "-c", "copy",
		"-encryption_scheme", "cenc-aes-ctr",
		"-encryption_key", hex.EncodeToString(key),
		"-encryption_kid", hex.EncodeToString(kid),
		"-movflags", "+faststart",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成密文样本失败: %v（%s）", err, out)
	}
	return plain
}

// argIndex 返回 argv 里第一个等于 want 的下标；没有返回 -1。
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// frameHash 用 ffmpeg 解出全部视频帧并算 MD5：两条取法（明文 / 密文+key）
// 解出的帧一致，才算「原生解密真的生效」。
func frameHash(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path,
		"-map", "0:v:0", "-f", "hash", "-hash", "md5", "-").Output()
	if err != nil {
		t.Fatalf("算解码帧哈希 %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// writeTemp 把字节写进临时文件（ffprobe / ffmpeg 只认路径）。
func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("写临时文件 %s: %v", path, err)
	}
	return path
}

// TestRemuxWithKey 覆盖 AC「带 key 的流真的能转出分片」：
//   - argv：`-decryption_key <hex>` 出现一次且落在 `-i` 之前；明文流的 argv 里
//     完全没有这个 flag（传空值 ffmpeg 会当非法参数）；
//   - 真转封装：密文源 + 正确 key 转出来的 fMP4 能解码，帧与明文转出来的逐帧一致。
func TestRemuxWithKey(t *testing.T) {
	plain := stream.Direct{URL: "https://cdn.example/ep.mp4"}
	cipher := stream.Direct{URL: "https://cdn.example/ep.mp4", Key: testKey}

	// 1) argv 顺序：ffmpeg 只把 `-i` 之前的选项当输入选项，放后面会被静默忽略。
	args := remuxArgs(cipher, "")
	keyAt, inAt := argIndex(args, "-decryption_key"), argIndex(args, "-i")
	if keyAt < 0 {
		t.Fatalf("带 key 的 remux argv 没有 -decryption_key: %v", args)
	}
	if keyAt > inAt {
		t.Fatalf("-decryption_key 落在 -i 之后（会被当输出选项忽略）: %v", args)
	}
	if got := args[keyAt+1]; got != hex.EncodeToString(testKey) {
		t.Fatalf("-decryption_key 的值 = %q, 期望 %x", got, hex.EncodeToString(testKey))
	}
	for i, a := range args {
		if a == "-decryption_key" && i != keyAt {
			t.Fatalf("-decryption_key 出现多次: %v", args)
		}
	}
	for _, a := range remuxArgs(plain, "") {
		if strings.Contains(a, "decryption") {
			t.Fatalf("明文流的 argv 里不该有解密参数: %v", remuxArgs(plain, ""))
		}
	}

	// 2) 探针同理：解密参数必须在源地址（最后一个参数）之前。
	pargs := probeArgs(cipher, "")
	keyAt = argIndex(pargs, "-decryption_key")
	if keyAt < 0 || keyAt >= len(pargs)-1 {
		t.Fatalf("-decryption_key 不在源地址之前: %v", pargs)
	}
	if last := pargs[len(pargs)-1]; last != cipher.URL {
		t.Fatalf("探针 argv 末位 = %q, 期望源地址 %q（参数顺序错了）", last, cipher.URL)
	}

	// 3) 真转封装：密文源 + 正确 key，解出的帧必须与明文整集一致。
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	plainPath := makeEncryptedVideo(t, encPath, 12, "160x120", testKey)
	encBody, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	cdn := newFakeCDN(t, encBody)

	fromCipher, err := remux(context.Background(), discardLogger, stream.Direct{URL: cdn.url(), Key: testKey}, "")
	if err != nil {
		t.Fatalf("密文源 remux 失败: %v", err)
	}
	assertSelfContainedSegment(t, fromCipher, 0, 12)

	fromPlain, err := remux(context.Background(), discardLogger, stream.Direct{URL: plainPath}, "")
	if err != nil {
		t.Fatalf("明文源 remux 失败: %v", err)
	}
	cipherHash := frameHash(t, writeTemp(t, "decrypted.m4s", fromCipher))
	plainHash := frameHash(t, writeTemp(t, "plain.m4s", fromPlain))
	if cipherHash == "" || cipherHash != plainHash {
		t.Fatalf("解密后帧哈希 %q ≠ 明文 %q（-decryption_key 没生效）", cipherHash, plainHash)
	}
}

// TestProbeRejectsBadKey 覆盖 AC「坏 key 时 ffprobe 失败被识别为错误，而不是产出空时长」。
//
// 为什么必须有这条：key 错时 ffprobe 常常**以 0 退出**、只往 stderr 吐
// `non-existing PPS …`，拿不到时长也不算失败 —— 那种「成功」会把 0 写进
// episode.duration_sec（`.nfo` 的 <runtime> 恒 0），还会让垃圾分片进缓存并被永久命中
// （MKACG-626 实测）。
func TestProbeRejectsBadKey(t *testing.T) {
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	makeEncryptedVideo(t, encPath, 12, "160x120", testKey)
	body, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	cdn := newFakeCDN(t, body)

	// 正确 key：探测成功，时长拿得到。
	got, err := probeDuration(context.Background(), discardLogger, stream.Direct{URL: cdn.url(), Key: testKey}, "")
	if err != nil {
		t.Fatalf("正确 key 的探测失败: %v", err)
	}
	if got < 11 || got > 13 {
		t.Fatalf("正确 key 的时长 = %v, 期望约 12 秒", got)
	}

	// 错 key：必须报错（包 httpx.ErrUpstream → 502），不能返回一份「成功但 0」的结果。
	wrong := bytes.Repeat([]byte{0x5a}, 16)
	if _, err := probeDuration(context.Background(), discardLogger, stream.Direct{URL: cdn.url(), Key: wrong}, ""); err == nil {
		t.Fatal("错 key 的探测返回成功：0 时长会被当成合法值写进库")
	} else if !errors.Is(err, httpx.ErrUpstream) {
		t.Fatalf("err = %v, 期望包 httpx.ErrUpstream（边界映射 502）", err)
	}

	// 首播整条链路：错 key → 502，且 segment 表与分片缓存都不留任何东西。
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	enc2Path := filepath.Join(t.TempDir(), "enc2.mp4")
	makeEncryptedVideo(t, enc2Path, 12, "160x120", testKey)
	enc2, err := os.ReadFile(enc2Path)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	r.cdn[ep.ID].setBody(enc2)
	r.keys[ep.ID] = wrong
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusBadGateway {
		t.Fatalf("错 key 首播状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	if _, err := r.st.GetSegment(ctx, ep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("错 key 后 segment 行错误 = %v, 期望 ErrNotFound", err)
	}
	if arts, err := r.st.ListArtifactsOldest(ctx, store.ArtifactKindSegment, 10); err != nil || len(arts) != 0 {
		t.Fatalf("错 key 后分片缓存行 = (%v, %v), 期望空", arts, err)
	}
}

// TestChannelSwitchesCacheKey 覆盖 AC「同一集在 web 与 app 两个通道下不串味」：
// 分片键只由「集 + 通道」决定，通道一翻键就变，旧片不会冒充新片；segment 行的
// 通道跟着 playlist 的解析结果更新。
func TestChannelSwitchesCacheKey(t *testing.T) {
	const uid = "e1"
	if cache.SegmentKey(uid, channelWeb) == cache.SegmentKey(uid, channelApp) {
		t.Fatal("两个通道算出同一个分片键（会命中另一通道的片）")
	}

	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	ctx := context.Background()
	id := r.id("e" + strconv.FormatInt(ep.ID, 10))

	// 1) web 通道首播 + 播一片：通道行与分片都按 web 落。
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("web 首播状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seg, err := r.st.GetSegment(ctx, ep.ID)
	if err != nil {
		t.Fatalf("GetSegment: %v", err)
	}
	if seg.Channel != channelWeb {
		t.Fatalf("web 首播后通道 = %q, 期望 %q", seg.Channel, channelWeb)
	}
	webRec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s")
	if webRec.Code != http.StatusOK {
		t.Fatalf("web 分片状态码 = %d, 期望 200（body=%s）", webRec.Code, webRec.Body.String())
	}

	// 2) 同一集换成 App 通道：密文源、分辨率也不同（字节流不同）。
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	makeEncryptedVideo(t, encPath, 12, "176x144", testKey)
	encBody, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	r.cdn[ep.ID].setBody(encBody)
	r.keys[ep.ID] = testKey

	// 3) playlist 现解析直链 → 判出 app 通道 → 通道行被覆盖。
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("app 首播状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seg, err = r.st.GetSegment(ctx, ep.ID)
	if err != nil {
		t.Fatalf("GetSegment(翻通道后): %v", err)
	}
	if seg.Channel != channelApp {
		t.Fatalf("翻通道后通道 = %q, 期望 %q", seg.Channel, channelApp)
	}

	// 4) 同一 seq 取到的是 app 通道的内容，不是缓存里 web 通道的那份。
	appRec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s")
	if appRec.Code != http.StatusOK {
		t.Fatalf("app 分片状态码 = %d, 期望 200（body=%s）", appRec.Code, appRec.Body.String())
	}
	if bytes.Equal(appRec.Body.Bytes(), webRec.Body.Bytes()) {
		t.Fatal("换通道后分片返回了 web 通道的缓存内容（串味）")
	}
	assertSelfContainedSegment(t, appRec.Body.Bytes(), 0, 12)

	// 两个通道各一份，键不同：同一集在缓存里最多两条（不是按 seq 膨胀）。
	arts, err := r.st.ListArtifactsOldest(ctx, store.ArtifactKindSegment, 10)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("分片缓存行数 = %d, 期望 2（web / app 各一）", len(arts))
	}
	wantKeys := map[string]bool{
		cache.SegmentKey("e"+strconv.FormatInt(ep.ID, 10), channelWeb): true,
		cache.SegmentKey("e"+strconv.FormatInt(ep.ID, 10), channelApp): true,
	}
	for _, a := range arts {
		if !wantKeys[a.Key] {
			t.Errorf("缓存键 %q 不在 (集, 通道) 的两种可能里", a.Key)
		}
	}
}
