package segment

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// 这个文件钉住「出流时平移 tfdt」的两层：
//   - 纯函数层：合成 box 上精确验证 baseMediaDecodeTime 的加数、入参不被改写、
//     offset==0 零拷贝、结构不合规必须报错；
//   - 真链路层：见 video_test.go 的 TestMergedPlaylist_DecodesEveryFrame
//     （整条 playlist 的 frames == packets，MKACG-652 的回归钉子）。

// mp4Box 拼一个 box：四字符类型 + 子 box。
func mp4Box(typ string, children ...[]byte) []byte {
	payload := make([]byte, 0, 64)
	for _, c := range children {
		payload = append(payload, c...)
	}
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

// fullBox 拼一个带 version/flags + 原始字段的 box。
func fullBox(typ string, version byte, fields []byte) []byte {
	body := make([]byte, 0, 4+len(fields))
	body = append(body, version, 0, 0, 0)
	body = append(body, fields...)
	return mp4Box(typ, body)
}

// u32 / u64 是大端字段。
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// fmp4Fixture 造一个最小的自包含 fMP4：ftyp + moov（视频 timescale=12800、
// 音频 timescale=44100）+ 两个 moof（各含 traf/tfhd/tfdt）+ mdat。
func fmp4Fixture(t *testing.T, tfdtVersion byte) []byte {
	t.Helper()
	tfdt := func(trackID uint32, decodeTime uint64) []byte {
		fields := u32(trackID) // tfhd 的 payload 只有 track_ID（flags 全 0）
		tfhd := fullBox("tfhd", 0, fields)
		var dt []byte
		if tfdtVersion == 1 {
			dt = fullBox("tfdt", 1, u64(decodeTime))
		} else {
			dt = fullBox("tfdt", 0, u32(uint32(decodeTime)))
		}
		return mp4Box("moof", mp4Box("traf", tfhd, dt))
	}
	trak := func(trackID, timescale uint32) []byte {
		// tkhd v0：creation(4) + modification(4) + track_ID(4)
		tkhd := fullBox("tkhd", 0, append(append(u32(0), u32(0)...), u32(trackID)...))
		// mdhd v0：creation(4) + modification(4) + timescale(4)
		mdhd := fullBox("mdhd", 0, append(append(u32(0), u32(0)...), u32(timescale)...))
		return mp4Box("trak", tkhd, mp4Box("mdia", mdhd))
	}
	// 顶层是平铺的兄弟 box（不是嵌在 ftyp 里）：ftyp / moov / moof / moof / moof / mdat。
	out := mp4Box("ftyp")
	out = append(out, mp4Box("moov", trak(1, 12800), trak(2, 44100), trak(3, 90000))...)
	out = append(out, tfdt(1, 1024)...)
	out = append(out, tfdt(2, 4096)...)
	out = append(out, tfdt(3, 800)...)
	out = append(out, mp4Box("mdat", []byte("payload"))...)
	return out
}

// tfdtValues 读回所有 tfdt 的 baseMediaDecodeTime（按出现顺序）。
func tfdtValues(t *testing.T, body []byte) []uint64 {
	t.Helper()
	var out []uint64
	for _, b := range mustBoxes(body, 0, len(body)) {
		if b.typ != "moof" {
			continue
		}
		for _, traf := range mustBoxes(body, b.payload, b.end) {
			for _, c := range mustBoxes(body, traf.payload, traf.end) {
				if c.typ != "tfdt" {
					continue
				}
				if body[c.payload] == 1 {
					out = append(out, binary.BigEndian.Uint64(body[c.payload+4:]))
				} else {
					out = append(out, uint64(binary.BigEndian.Uint32(body[c.payload+4:])))
				}
			}
		}
	}
	return out
}

// AC：每个轨道的 tfdt 都按「该轨道自己的 timescale」加数，不是一刀切。
func TestShiftTimeline_PerTrackTimescale(t *testing.T) {
	for _, version := range []byte{0, 1} {
		body := fmp4Fixture(t, version)
		before := tfdtValues(t, body)
		out, err := shiftTimeline(body, 4*time.Second)
		if err != nil {
			t.Fatalf("v%d shiftTimeline: %v", version, err)
		}
		got := tfdtValues(t, out)
		want := []uint64{
			before[0] + 4*12800, // 视频：4 s × 12800
			before[1] + 4*44100, // 音频：4 s × 44100
			before[2] + 4*90000, // 另一条轨道：4 s × 90000
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("v%d tfdt[%d] = %d, 期望 %d", version, i, got[i], want[i])
			}
		}
		if !equalBytes(out[:8], body[:8]) || len(out) != len(body) {
			t.Error("平移改了片头或长度（自包含结构必须原样保留）")
		}
	}
}

// AC：入参不被改写（命中缓存时这段字节可能是多个请求共享的只读缓冲），且 offset==0 零拷贝。
func TestShiftTimeline_DoesNotMutateInput(t *testing.T) {
	body := fmp4Fixture(t, 0)
	snapshot := append([]byte(nil), body...)
	if _, err := shiftTimeline(body, 9*time.Second); err != nil {
		t.Fatalf("shiftTimeline: %v", err)
	}
	if !equalBytes(body, snapshot) {
		t.Fatal("入参被就地改写（会让 single-flight 的其它等待者拿到被平移的字节）")
	}

	same, err := shiftTimeline(body, 0)
	if err != nil {
		t.Fatalf("offset=0: %v", err)
	}
	if &same[0] != &body[0] {
		t.Error("offset=0 应原样返回同一段字节（首片/单集零拷贝）")
	}
}

// AC：结构不合规必须报错，不能静默「平移成功」继续出流。
func TestShiftTimeline_RejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"不是 fMP4（没有 moov）": mp4Box("ftyp", mp4Box("mdat", []byte("x"))),
		"moov 里没有轨道":       mp4Box("ftyp", mp4Box("moov"), mp4Box("moof")),
		"moof 里没有 tfdt":    mp4Box("ftyp", fmp4Fixture(t, 0)[8:20], mp4Box("moof", mp4Box("traf", fullBox("tfhd", 0, u32(1))))),
		"空字节":              nil,
	}
	for name, body := range cases {
		if _, err := shiftTimeline(body, time.Second); err == nil {
			t.Errorf("%s：没有报错", name)
		} else if strings.Contains(err.Error(), "http") {
			t.Errorf("%s：错误文本不该带 URL：%v", name, err)
		}
	}
}

// AC：轨道在 tfhd 里出现但 moov 里没有对应 timescale → 报错（否则时间轴会错得无声无息）。
func TestShiftTimeline_UnknownTrack(t *testing.T) {
	tfhd := fullBox("tfhd", 0, u32(42))
	tfdt := fullBox("tfdt", 0, u32(0))
	body := mp4Box("ftyp", mp4Box("moov", mp4Box("trak",
		fullBox("tkhd", 0, append(append(u32(0), u32(0)...), u32(1)...)),
		mp4Box("mdia", fullBox("mdhd", 0, append(append(u32(0), u32(0)...), u32(12800)...))),
	)), mp4Box("moof", mp4Box("traf", tfhd, tfdt)))
	if _, err := shiftTimeline(body, time.Second); err == nil {
		t.Fatal("未知 track_id 没有报错")
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
