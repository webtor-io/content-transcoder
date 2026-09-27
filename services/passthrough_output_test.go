package services

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	cp "github.com/webtor-io/content-prober/content-prober"
)

// passthroughSource is an HEVC 2160p source with AAC stereo (copied), E-AC3
// 5.1 (encoded), a text subtitle, a PGS one (dropped by NewHLS) and a DVD
// one (in the master, no output), on the passthrough route.
func passthroughSource(t *testing.T, threads int) *HLS {
	t.Helper()
	h := NewHLS("http://src/movie.mkv?api-key=K", &cp.ProbeReply{
		Streams: []*cp.Stream{
			{Index: 0, CodecType: "video", CodecName: "hevc", Width: 3840, Height: 2160},
			{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
			{Index: 2, CodecType: "audio", CodecName: "eac3", Channels: 6},
			{Index: 3, CodecType: "subtitle", CodecName: "subrip"},
			{Index: 4, CodecType: "subtitle", CodecName: "hdmv_pgs_subtitle"},
			{Index: 5, CodecType: "subtitle", CodecName: "dvd_subtitle"},
		},
		Format: &cp.Format{BitRate: "40000000"},
	}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", threads: threads})
	if !h.usePassthrough() {
		t.Fatal("usePassthrough refused")
	}
	return h
}

const testGen = "0a12b34c56d78e90"

// The whole passthrough command (plan 3.4, 3.8, 3.9): video copied as hvc1
// with the parameter sets out of the samples, every audio track in fMP4 as
// well (copied or encoded as on the old route), both through the hls muxer
// with finished-only segments, an init named after the process, playlists
// straight to their .ffmpeg names; text subtitles exactly as on the old
// route, a track without a decoder left out.
func TestPassthroughParams(t *testing.T) {
	h := passthroughSource(t, 2)
	got, err := h.ffmpegParamsFor("/out", ParamOptions{}, testGen)
	if err != nil {
		t.Fatal(err)
	}
	hls := func(prefix string) string {
		return fmt.Sprintf("-f hls -hls_time 4 -hls_list_size 0 -hls_playlist_type event -hls_segment_type fmp4 -hls_flags temp_file "+
			"-hls_fmp4_init_filename %[1]s-init-%[2]s.mp4 -hls_segment_filename /out/%[1]s-%%d.m4s /out/%[1]s.m3u8.ffmpeg", prefix, testGen)
	}
	want := strings.Join([]string{
		"-filter_threads 2 -threads 2 -reconnect 1 -reconnect_on_network_error 1 -reconnect_delay_max 5 -fix_sub_duration -i http://src/movie.mkv?api-key=K -xerror -seekable 1",
		"-map 0:0 -c:v copy -bsf:v hevc_mp4toannexb -tag:v hvc1 " + hls("v0-2160"),
		"-map 0:1 -c:a copy " + hls("a0"),
		"-map 0:2 -c:a libfdk_aac -ac 2 " + hls("a1"),
		"-map 0:3 -f segment -segment_time 4 -segment_list_type hls -segment_list /out/s0.m3u8 -muxdelay 0 -segment_format webvtt -break_non_keyframes 1 -c:s webvtt /out/s0-%d.vtt",
	}, " ")
	if strings.Join(got, " ") != want {
		t.Errorf("passthrough command\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	// The run's rewrite of -segment_list leaves the hls outputs alone.
	red := strings.Join(redirectSegmentListParams(got), " ")
	if !strings.Contains(red, "-segment_list /out/s0.m3u8.ffmpeg ") || strings.Contains(red, ".m3u8.ffmpeg.ffmpeg") {
		t.Errorf("playlist names after redirect: %s", red)
	}

	// The fallbacks of the old route apply the same way.
	lenient, _ := h.ffmpegParamsFor("/out", ParamOptions{Lenient: true, EncodeAudio: true}, testGen)
	l := strings.Join(lenient, " ")
	if strings.Contains(l, "-xerror") || !strings.Contains(l, "-map 0:1 -c:a libfdk_aac -ac 2 -f hls") {
		t.Errorf("lenient, encoded audio: %s", l)
	}
	// Each process names its own init.
	other, _ := h.ffmpegParamsFor("/out", ParamOptions{}, "ffffffffffffffff")
	if o := strings.Join(other, " "); strings.Contains(o, testGen) || !strings.Contains(o, "v0-2160-init-ffffffffffffffff.mp4") {
		t.Errorf("init not named after the process: %s", o)
	}
}

// The pace loop and the segment handler name the files the command writes.
func TestPassthroughSegmentNamesMatchTheCommand(t *testing.T) {
	h := passthroughSource(t, 0)
	params, err := h.ffmpegParamsFor("/out", ParamOptions{}, testGen)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(params, " ")
	r := newTranscodeRun("k", "/h", 0, "http://src/movie.mkv", h)
	r.outputDir = "/out"
	if got := r.primarySegmentPath(7); got != "/out/v0-2160-7.m4s" || !strings.Contains(cmd, "/out/v0-2160-%d.m4s") {
		t.Errorf("primary segment %s, command %s", got, cmd)
	}
	for _, name := range []string{"v0-2160-12.m4s", "a0-3.m4s", "a1-0.m4s"} {
		if !h.ownsSegment(name) {
			t.Errorf("%s is the session's", name)
		}
	}
	for _, name := range []string{"a2-0.m4s", "v1-2160-0.m4s", "v0-2160-1.ts", "s0-1.m4s", "v0-2160-init-" + testGen + ".mp4", "v0-2160-1.m4s.tmp"} {
		if h.ownsSegment(name) {
			t.Errorf("%s is not the session's", name)
		}
	}
	for _, s := range []*HLSStream{h.primary[0], h.audio[0], h.audio[1]} {
		if !strings.Contains(cmd, "-hls_fmp4_init_filename "+s.initName(testGen)+" ") || h.fmp4Stream(initStreamPrefix(s.initName(testGen))) != s {
			t.Errorf("init of %s: %s", s.streamPrefix(), s.initName(testGen))
		}
	}
	if h.subs[0].GetSegmentExtension() != "vtt" || h.fmp4Stream("s0") != nil {
		t.Error("subtitles are not fMP4")
	}
	// The old route has no fMP4 stream.
	old := hevcHLS(t, 3840, 2160, false, nil)
	if old.ownsSegment("v0-2160-1.m4s") || old.fmp4Stream("v0-2160") != nil || old.primary[0].GetSegmentExtension() != "ts" {
		t.Error("the old route has fMP4 streams")
	}
}

// The seek of a passthrough run: the copy route's input seek at the
// quantized time, every output's zero moved to the keyframe it lands on,
// and the audio outputs cut at that zero.
func TestInjectPassthroughSeekParams(t *testing.T) {
	in := []string{"-reconnect", "1", "-i", "http://src", "-xerror", "-map", "0:0", "-c:v", "copy", "v.m3u8",
		"-map", "0:1", "-c:a", "copy", "a0.m3u8", "-map", "0:2", "-c:a", "libfdk_aac", "a1.m3u8", "-map", "0:3", "-f", "segment", "s0.vtt"}
	got := strings.Join(injectPassthroughSeekParams(in, 30, 20.02, []string{"0:1", "0:2"}), " ")
	want := "-reconnect 1 -ss 30.000 -noaccurate_seek -itsoffset 9.980000 -i http://src -xerror -map 0:0 -c:v copy v.m3u8 " +
		"-ss 0 -map 0:1 -c:a copy a0.m3u8 -ss 0 -map 0:2 -c:a libfdk_aac a1.m3u8 -map 0:3 -f segment s0.vtt"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// No real start (the probe failed): the quantized seek is the zero, and
	// the audio is left whole -- the video starts at the keyframe before it.
	got = strings.Join(injectPassthroughSeekParams(in, 30, 30, nil), " ")
	want = "-reconnect 1 -ss 30.000 -noaccurate_seek -i http://src -xerror -map 0:0 -c:v copy v.m3u8 " +
		"-map 0:1 -c:a copy a0.m3u8 -map 0:2 -c:a libfdk_aac a1.m3u8 -map 0:3 -f segment s0.vtt"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Counted from the file's start time, as every other run: no
	// -seek_timestamp.
	if strings.Contains(strings.Join(passthroughSeekInput(30), " "), "seek_timestamp") {
		t.Error("absolute seek")
	}
	// The audio outputs of a session are its audio streams' maps.
	if got := passthroughSource(t, 0).passthroughAudioMaps(); strings.Join(got, " ") != "0:1 0:2" {
		t.Errorf("audio maps %v", got)
	}
}

// A passthrough seek run started for real: the probe's keyframe goes into
// -itsoffset, -ss stays the quantized seek (at the keyframe itself FFmpeg
// lands a GOP earlier), -xerror is off as on every seek, and the init is
// named after the process that writes it -- a new one on restart.
func TestPassthroughRunStart(t *testing.T) {
	fakeFFmpeg(t)
	orig, origPT := probeRunStart, probePassthroughStart
	// ffprobe's seek is not the run's: a passthrough run never asks it.
	probeRunStart = func(context.Context, string, string, float64) (float64, error) {
		t.Error("a passthrough run's start resolved with ffprobe")
		return 0, os.ErrInvalid
	}
	probePassthroughStart = func(_ context.Context, _ string, stream string, seek float64) (float64, error) {
		if stream != "0" {
			t.Errorf("probe on stream %q", stream)
		}
		return seek - 9.98, nil
	}
	t.Cleanup(func() { probeRunStart, probePassthroughStart = orig, origPT })
	h := hevcHLS(t, 3840, 2160, true, nil)
	m := NewRunManager()
	defer m.CloseAll()
	r, err := m.Acquire(t.TempDir(), 30, "http://src/movie.mkv", h)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r.cmd.Args[1:], " ")
	if !strings.Contains(args, " -ss 30.000 -noaccurate_seek -itsoffset 9.980000 -i http://src/movie.mkv ") || strings.Contains(args, "seek_timestamp") {
		t.Errorf("seek: %s", args)
	}
	if !strings.Contains(args, " -ss 0 -map 0:1 -c:a copy -f hls ") || strings.Count(args, "-ss 0 ") != 1 {
		t.Errorf("the audio output is cut at the real start, and only it: %s", args)
	}
	if strings.Contains(args, "-xerror") {
		t.Errorf("-xerror on a seek: %s", args)
	}
	if rs := r.RealStart(); rs < 20.0199 || rs > 20.0201 {
		t.Errorf("real start %v", rs)
	}
	gen1 := r.Generation()
	if !strings.Contains(args, "v0-2160-init-"+gen1+".mp4") || !strings.Contains(args, "a0-init-"+gen1+".mp4") {
		t.Errorf("init not named after the process %s: %s", gen1, args)
	}
	r.Stop()
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	gen2 := r.Generation()
	args2 := strings.Join(r.cmd.Args[1:], " ")
	if gen2 == gen1 || !strings.Contains(args2, "v0-2160-init-"+gen2+".mp4") || strings.Contains(args2, gen1) {
		t.Errorf("restart: generation %s -> %s, args %s", gen1, gen2, args2)
	}
}

// A passthrough seek run whose real start could not be found runs from the
// quantized seek, with no offset and the audio whole, and keeps nothing:
// the next run of the key probes again and, answered, gets the offset and
// the cut audio -- a probe timing out on a cold source once is not that
// key's offset for the rest of the pod's life. Every probe is counted by
// result. The copy route keeps its fallback, as in 1b25e28.
func TestPassthroughRealStart_FailureIsNotKept(t *testing.T) {
	fakeFFmpeg(t)
	orig, origPT := probeRunStart, probePassthroughStart
	t.Cleanup(func() { probeRunStart, probePassthroughStart = orig, origPT })
	fail := func(context.Context, string, string, float64) (float64, error) { return 0, os.ErrDeadlineExceeded }
	answer := func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 9.98, nil }
	count := func(mode, result string) float64 {
		return testutil.ToFloat64(metricRunRealStartTotal.WithLabelValues(mode, result))
	}
	failed0, ok0 := count(runModePassthrough, realStartFailed), count(runModePassthrough, realStartOK)

	h := hevcHLS(t, 3840, 2160, true, nil)
	dir := t.TempDir()
	key := runKeyFor(dir, h, 30)
	m := NewRunManager()
	defer m.CloseAll()

	probePassthroughStart = fail
	r1, err := m.Acquire(dir, 30, "http://src/movie.mkv", h)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r1.cmd.Args[1:], " ")
	if !strings.Contains(args, " -ss 30.000 -noaccurate_seek -i ") || strings.Contains(args, "-itsoffset") || strings.Contains(args, "-ss 0 ") {
		t.Errorf("unresolved seek: %s", args)
	}
	if r1.RealStart() != 30 {
		t.Errorf("unresolved real start %v", r1.RealStart())
	}
	if _, ok := m.ResolvedStart(key); ok {
		t.Error("a failed resolution was kept for the key")
	}
	if d := count(runModePassthrough, realStartFailed) - failed0; d != 1 {
		t.Errorf("failed counted %v", d)
	}

	// The run object goes (reaped); the next one asks again.
	m.Release(r1)
	r1.Cleanup()
	m.mu.Lock()
	delete(m.runs, key)
	m.mu.Unlock()
	probePassthroughStart = answer
	r2, err := m.Acquire(dir, 30, "http://src/movie.mkv", h)
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Join(r2.cmd.Args[1:], " ")
	if !strings.Contains(args, " -itsoffset 9.980000 -i ") || !strings.Contains(args, " -ss 0 -map 0:1 ") {
		t.Errorf("resolved seek: %s", args)
	}
	if v, ok := m.ResolvedStart(key); !ok || math.Abs(v-20.02) > 1e-9 {
		t.Errorf("the answer is kept: %v %v", v, ok)
	}
	if d := count(runModePassthrough, realStartOK) - ok0; d != 1 {
		t.Errorf("ok counted %v", d)
	}

	// The copy route: a fallback is kept, as it always was.
	copyFailed0 := count(runModeCopy, realStartFailed)
	probeRunStart = fail
	copyHLS := &HLS{primary: []*HLSStream{NewHLSStream(0, Video, &cp.Stream{CodecName: "h264"}, nil, nil, false)}}
	m.mu.Lock()
	rc := m.newRunLocked(runKey(dir, 60), dir, 60, "http://src/movie.mkv", copyHLS)
	m.mu.Unlock()
	rc.resolveRealStartOnce()
	if v, ok := m.ResolvedStart(runKey(dir, 60)); !ok || v != 60 {
		t.Errorf("the copy route's fallback: %v %v", v, ok)
	}
	if d := count(runModeCopy, realStartFailed) - copyFailed0; d != 1 {
		t.Errorf("copy failed counted %v", d)
	}
}

// hvcC bytes of a stream: profile space, tier, profile, compatibility
// flags, the constraint bytes and level (ISO/IEC 14496-15 8.3.3.1).
func hvccWith(space int, tierHigh bool, profile int, compat uint32, constraint [6]byte, level int) hvccHeader {
	return hvccHeader{profileSpace: space, tierHigh: tierHigh, profileIdc: profile, compat: compat, constraint: constraint, levelIdc: level}
}

func TestHEVCCodecString(t *testing.T) {
	c90 := [6]byte{0x90}
	for _, c := range []struct {
		h    hvccHeader
		want string
	}{
		// ISO/IEC 14496-15 E.3 and Apple's authoring spec.
		{hvccWith(0, false, 1, 0x60000000, c90, 93), "hvc1.1.6.L93.90"},
		{hvccWith(0, false, 2, 0x20000000, c90, 150), "hvc1.2.4.L150.90"},
		{hvccWith(0, false, 2, 0x20000000, [6]byte{0xB0}, 123), "hvc1.2.4.L123.B0"},
		// Tier High (UHD BD remux).
		{hvccWith(0, true, 2, 0x20000000, c90, 153), "hvc1.2.4.H153.90"},
		// Profile space, and a constraint byte after zero ones.
		{hvccWith(1, false, 2, 0x20000000, [6]byte{0x90, 0, 0, 0, 0, 0x01}, 120), "hvc1.A2.4.L120.90.00.00.00.00.01"},
		{hvccWith(0, false, 1, 0x60000000, [6]byte{}, 63), "hvc1.1.6.L63"},
	} {
		if got := hevcCodecString("hvc1", c.h); got != c.want {
			t.Errorf("%+v: %s, want %s", c.h, got, c.want)
		}
	}
}

// The inits FFmpeg 8.1.2 wrote for a Main10 and a Main source with exactly
// the passthrough video arguments; FFmpeg's own master (-master_pl_name,
// libavformat/codecstring.c) said hvc1.2.4.L63.90 and hvc1.1.6.L63.90.
func TestInitHEVCConfig_FFmpegInits(t *testing.T) {
	for file, want := range map[string]string{
		"main10-init.mp4": "hvc1.2.4.L63.90",
		"main8-init.mp4":  "hvc1.1.6.L63.90",
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "passthrough", file))
		if err != nil {
			t.Fatal(err)
		}
		fourcc, raw, err := initHEVCConfig(b)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		h, ok := parseHVCC(raw)
		if !ok {
			t.Fatalf("%s: hvcC %x", file, raw[:8])
		}
		if got := hevcCodecString(fourcc, h); got != want {
			t.Errorf("%s: %s, want %s", file, got, want)
		}
	}
}

// box is an ISO BMFF box.
func box(typ string, body ...[]byte) []byte {
	var b []byte
	for _, p := range body {
		b = append(b, p...)
	}
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(8+len(b)))
	copy(h[4:], typ)
	return append(h, b...)
}

// initWith is an init segment whose video sample entry is entry, holding
// boxes after its 78 bytes of fields.
func initWith(entry string, boxes ...[]byte) []byte {
	sample := box(entry, append([][]byte{make([]byte, 78)}, boxes...)...)
	stsd := box("stsd", make([]byte, 8), sample)
	trak := box("trak", box("tkhd", make([]byte, 84)), box("mdia", box("minf", box("stbl", stsd))))
	return append(box("ftyp", []byte("iso5")), box("moov", box("mvhd", make([]byte, 100)), trak)...)
}

func TestInitHEVCConfig_Shapes(t *testing.T) {
	hvcc := testHVCC(2, false, 150)
	if fourcc, raw, err := initHEVCConfig(initWith("hvc1", box("colr", []byte("nclx0000000")), box("hvcC", hvcc))); err != nil || fourcc != "hvc1" || !reflect.DeepEqual(raw, hvcc) {
		t.Errorf("hvc1: %s %x %v", fourcc, raw, err)
	}
	if fourcc, _, err := initWith2("hev1", hvcc); err != nil || fourcc != "hev1" {
		t.Errorf("hev1: %s %v", fourcc, err)
	}
	// A 64-bit box size.
	big := initWith("hvc1", box("hvcC", hvcc))
	i := strings.Index(string(big), "moov") - 4
	moov := big[i:]
	large := append(append([]byte{0, 0, 0, 1}, []byte("moov")...), make([]byte, 8)...)
	binary.BigEndian.PutUint64(large[8:], uint64(len(moov)+8))
	large = append(append(append([]byte{}, big[:i]...), large...), moov[8:]...)
	if _, raw, err := initHEVCConfig(large); err != nil || !reflect.DeepEqual(raw, hvcc) {
		t.Errorf("64-bit size: %v", err)
	}
	for name, b := range map[string][]byte{
		"avc1 only":        initWith("avc1", box("avcC", []byte{1, 2, 3})),
		"no hvcC":          initWith("hvc1", box("colr", []byte("nclx"))),
		"truncated":        initWith("hvc1", box("hvcC", hvcc))[:60],
		"size past end":    append(box("ftyp", []byte("iso5")), 0, 0, 1, 0, 'm', 'o', 'o', 'v'),
		"empty":            nil,
		"short sample box": box("moov", box("trak", box("mdia", box("minf", box("stbl", box("stsd", make([]byte, 8), box("hvc1", make([]byte, 10)))))))),
	} {
		if _, _, err := initHEVCConfig(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func initWith2(entry string, hvcc []byte) (string, []byte, error) {
	return initHEVCConfig(initWith(entry, box("hvcC", hvcc)))
}

func TestPassthroughBandwidth(t *testing.T) {
	for _, c := range []struct {
		source, bytes int64
		secs          float64
		audio         bool
		want          int64
	}{
		{40_000_000, 10_000_000, 10, true, 40_000_000},    // the average is higher
		{5_000_000, 10_000_000, 10, true, 8_192_000},      // the first segment is
		{5_000_000, 10_000_000, 10, false, 8_000_000},     // no audio group
		{0, 10_000_000, 10, true, 8_192_000},              // no average
		{0, 0, 0, false, 1},                               // nothing known
		{3_000_000, 0, 10, true, 3_000_000},               // no segment size
		{3_000_000, 1_000_000, 0, true, 3_000_000},        // no duration
		{20_000_000, 59_000_000, 10.01, true, 47_344_847}, // a 4K GOP
	} {
		if got := passthroughBandwidth(c.source, c.bytes, c.secs, c.audio); got != c.want {
			t.Errorf("%+v: %d, want %d", c, got, c.want)
		}
	}
}

// passthroughRunDir fills dir the way FFmpeg does after the first cut of
// process gen: the video init (an FFmpeg 8.1.2 init) and first segment,
// and the playlist naming both.
func passthroughRunDir(t *testing.T, dir, prefix, gen string, init []byte, seg0 int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, prefix+"-init-"+gen+".mp4"), init, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, prefix+"-0.m4s"), make([]byte, seg0), 0644); err != nil {
		t.Fatal(err)
	}
	pl := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-MAP:URI=\"%s-init-%s.mp4\"\n#EXTINF:10.010000,\n%s-0.m4s\n", prefix, gen, prefix)
	if err := os.WriteFile(filepath.Join(dir, prefix+".m3u8.ffmpeg"), []byte(pl), 0644); err != nil {
		t.Fatal(err)
	}
}

func fixtureInit(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "passthrough", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The master of a passthrough session describes the output: CODECS from the
// init's hvcC (not the source's), the source's size, PQ or SDR, a
// bandwidth that is not the 8 Mbit/s cap; the renditions as on the old
// route. A source hvcC the output does not match is counted by field.
func TestPassthroughMaster_FromTheOutputInit(t *testing.T) {
	h := passthroughSource(t, 0)
	pq := sdrMain10()
	pq.ColorTransfer = transferPQ
	pq.HVCC = testHVCC(2, false, 150) // the output's is level 63 (the fixture)
	h.passFacts = &pq
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sess := NewSession(SessionConfig{ID: "m", HashDir: dir, HLS: h, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := newCompletedRun(t, dir) // finished: its init is final
	run.h = h
	sess.run = run
	passthroughRunDir(t, run.OutputDir(), "v0-2160", run.Generation(), fixtureInit(t, "main10-init.mp4"), 12_500_000)

	level := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchLevel))
	profile := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchProfile))
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(sess.outputDir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	want := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,URI="a0.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",URI="a1.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="Subtitle #1",URI="s0.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="Subtitle #2",URI="s1.m3u8"` + "\n" +
		// 12.5 MB over 10.01 s = 9,990,009 + the audio allowance; the
		// source's 40 Mbit/s average is higher.
		`#EXT-X-STREAM-INF:BANDWIDTH=40000000,RESOLUTION=3840x2160,CODECS="hvc1.2.4.L63.90,mp4a.40.2",VIDEO-RANGE=PQ,AUDIO="audio",SUBTITLES="subtitles"` + "\n" +
		"v0-2160.m3u8\n"
	if string(got) != want {
		t.Errorf("master\n got %q\nwant %q", got, want)
	}
	if d := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchLevel)) - level; d != 1 {
		t.Errorf("level mismatch counted %v times", d)
	}
	if d := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchProfile)) - profile; d != 0 {
		t.Errorf("profile mismatch counted %v times, both are Main10", d)
	}
	// Written once: a later process's init does not rewrite it.
	if err := os.WriteFile(filepath.Join(run.OutputDir(), "v0-2160-init-"+run.Generation()+".mp4"), []byte("garbage"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(sess.outputDir, "index.m3u8")); string(again) != want {
		t.Error("master rewritten")
	}

	// SDR, no audio, no subtitles, and a source without an average rate.
	sdr := hevcHLS(t, 1920, 1080, true, nil)
	sdr.audio, sdr.subs = nil, nil
	f := sdrMain10()
	f.HVCC = testHVCC(1, false, 63)
	sdr.passFacts = &f
	if got := sdr.passthroughMasterPlaylist("hvc1.1.6.L63.90", passthroughBandwidth(0, 1_000_000, 10, false)); got != "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1920x1080,CODECS=\"hvc1.1.6.L63.90\",VIDEO-RANGE=SDR\nv0-1080.m3u8\n" {
		t.Errorf("SDR master %q", got)
	}
}

// No CODECS out of the init: no master (a guessed CODECS fails in the
// player at once), and it is counted.
func TestPassthroughMaster_Unbuildable(t *testing.T) {
	h := passthroughSource(t, 0)
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sess := NewSession(SessionConfig{ID: "u", HashDir: dir, HLS: h, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := newCompletedRun(t, dir)
	run.h = h
	sess.run = run
	passthroughRunDir(t, run.OutputDir(), "v0-2160", run.Generation(), initWith("avc1", box("avcC", []byte{1})), 10)
	before := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchUnbuildable))
	if err := sess.writePassthroughMaster(context.Background(), 0); err != errCodecsUnbuildable {
		t.Fatalf("err %v", err)
	}
	if fileExists(filepath.Join(sess.outputDir, "index.m3u8")) {
		t.Error("a master was written")
	}
	if d := testutil.ToFloat64(metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchUnbuildable)) - before; d != 1 {
		t.Errorf("unbuildable counted %v times", d)
	}
}

func TestCodecsMismatches(t *testing.T) {
	src := hvccWith(0, false, 2, 0x20000000, [6]byte{0x90}, 150)
	for _, c := range []struct {
		out  hvccHeader
		want []string
	}{
		{src, nil},
		// compat and constraint bits are ANDed by the rebuild: not a mismatch.
		{hvccWith(0, false, 2, 0x60000000, [6]byte{0x80}, 150), nil},
		{hvccWith(0, false, 1, 0x20000000, [6]byte{0x90}, 150), []string{codecsMismatchProfile}},
		{hvccWith(0, true, 2, 0x20000000, [6]byte{0x90}, 153), []string{codecsMismatchTier, codecsMismatchLevel}},
	} {
		if got := codecsMismatches(src, c.out); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: %v, want %v", c.out, got, c.want)
		}
	}
}

// framecrc as FFmpeg 8.1.2 wrote it for the run's seek to 30 (-copyts):
// an MKV (matroska has no DTS; FFmpeg's guess is two frames before the
// keyframe's PTS) and an MP4 (90 kHz, DTS from the file).
func TestParseFrameCRCStart(t *testing.T) {
	head := "#extradata 0:     2443, 0xcea748ac\n#software: Lavf62.12.102\n#tb 0: %s\n#media_type 0: video\n#codec_id 0: hevc\n#dimensions 0: 640x360\n#sar 0: 1/1\n"
	for _, c := range []struct {
		out  string
		want float64
	}{
		{fmt.Sprintf(head, "1/1000") + "0,      19937,      20020,       41,     8075, 0x71caa088\n", 19.937},
		{fmt.Sprintf(head, "1/90000") + "0,    2680200,    2702700,     3690,     8803, 0x6dbe02ab\n", 29.78},
		// No DTS at all: the PTS.
		{fmt.Sprintf(head, "1/1000") + "0, -9223372036854775808,      20020,       41,     8075, 0x71caa088\n", 20.02},
		// A PTS before the DTS (not seen, but the earlier one is the zero).
		{fmt.Sprintf(head, "1/1000") + "0,      20020,      19937,       41,     8075, 0x71caa088\n", 19.937},
	} {
		got, err := parseFrameCRCStart([]byte(c.out))
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%q: %v %v, want %v", c.out, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"",
		fmt.Sprintf(head, "1/1000"),
		"0,      19937,      20020,       41,     8075, 0x71caa088\n",
		fmt.Sprintf(head, "1/1000") + "0, -9223372036854775808, -9223372036854775808, 41, 8075, 0x0\n",
		fmt.Sprintf(head, "1/1000") + "garbage\n",
	} {
		if v, err := parseFrameCRCStart([]byte(bad)); err == nil {
			t.Errorf("%q: %v, no error", bad, v)
		}
	}
}

// The probe runs the run's own seek, and its timestamps come relative to
// the seek point, as the run's do: the real start is the seek plus the
// first packet's.
func TestFFmpegSeekStartArgs(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	// FFmpeg 8.1.2's answer for a seek to 30 on an MKV whose keyframe is
	// at 20.020 (DTS guessed 19.937).
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nprintf '#tb 0: 1/1000\\n0,     -10063,     -9980,       41,     8075, 0x71caa088\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := ffmpegSeekStart(context.Background(), "http://src/movie.mkv?api-key=K", "3", 30)
	if err != nil || math.Abs(got-19.937) > 1e-9 {
		t.Fatalf("%v %v", got, err)
	}
	b, _ := os.ReadFile(argsFile)
	want := "-nostdin -v error -protocol_whitelist http,https,tcp,tls " + strings.Join(passthroughSeekInput(30), " ") +
		" -i http://src/movie.mkv?api-key=K -map 0:3 -c copy -frames:v 1 -f framecrc -"
	if strings.TrimSpace(string(b)) != want {
		t.Errorf("args\n got %s\nwant %s", b, want)
	}
	if _, err := ffmpegSeekStart(context.Background(), "-i", "0", 30); err == nil {
		t.Error("a source that parses as an option was run")
	}
	// The run seeks with the same options.
	run := strings.Join(injectPassthroughSeekParams([]string{"-i", "u"}, 30, 19.937, nil), " ")
	if !strings.HasPrefix(run, strings.Join(passthroughSeekInput(30), " ")+" -itsoffset 10.063000 -i") {
		t.Errorf("run seek %s", run)
	}
}
