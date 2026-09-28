package services

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cp "github.com/webtor-io/content-prober/content-prober"
)

func audioStream(codec string, channels int32) *cp.Stream {
	return &cp.Stream{Index: 1, CodecType: "audio", CodecName: codec, Channels: channels}
}

var (
	noDecoders = audioDecoders{}
	aac51Only  = audioDecoders{aac51: true}
	ec3Only    = audioDecoders{ec3: true}
	ac3Only    = audioDecoders{ac3: true}
	allAudio   = audioDecoders{aac51: true, ac3: true, ec3: true}
	aac51EC3   = audioDecoders{aac51: true, ec3: true}
)

// Every row of the decision, on both routes (MPEG-TS and fMP4), and the
// codec options each output gets.
func TestAudioOutputFor_Table(t *testing.T) {
	copyAAC := func(ch int) audioOutput { return audioOutput{copy: true, channels: ch, codecs: codecsAAC} }
	stereo := audioOutput{channels: 2, codecs: codecsAAC}
	surround := audioOutput{channels: 6, codecs: codecsAAC}
	cases := []struct {
		name  string
		codec string
		ch    int32
		d     audioDecoders
		ts    audioOutput // old route (MPEG-TS audio)
		fmp4  audioOutput // passthrough (fMP4 audio)
	}{
		// aac <= 2ch: copied, whatever is declared (as always).
		{"aac stereo, nothing declared", "aac", 2, noDecoders, copyAAC(2), copyAAC(2)},
		{"aac stereo, everything declared", "aac", 2, allAudio, copyAAC(2), copyAAC(2)},
		{"aac mono", "aac", 1, allAudio, copyAAC(1), copyAAC(1)},
		{"aac, channels unknown", "aac", 0, noDecoders, copyAAC(0), copyAAC(0)},
		// aac 3-6ch: copied with aac51, stereo without.
		{"aac 5.1, nothing declared", "aac", 6, noDecoders, stereo, stereo},
		{"aac 5.1 with aac51", "aac", 6, aac51Only, copyAAC(6), copyAAC(6)},
		{"aac 3.0 with aac51", "aac", 3, aac51Only, copyAAC(3), copyAAC(3)},
		{"aac 5.1 with ec3 and ac3 only", "aac", 6, audioDecoders{ac3: true, ec3: true}, stereo, stereo},
		// aac 7.1: over what aac51 speaks for, encoded to 5.1.
		{"aac 7.1 with aac51", "aac", 8, aac51Only, surround, surround},
		{"aac 7.1, nothing declared", "aac", 8, noDecoders, stereo, stereo},
		// eac3: copied with ec3 on fMP4 only.
		{"eac3 5.1, nothing declared", "eac3", 6, noDecoders, stereo, stereo},
		{"eac3 5.1 with ec3", "eac3", 6, ec3Only, stereo, audioOutput{copy: true, channels: 6, codecs: codecsEC3}},
		{"eac3 5.1 with ec3 and aac51", "eac3", 6, aac51EC3, surround, audioOutput{copy: true, channels: 6, codecs: codecsEC3}},
		{"eac3 7.1 with ec3", "eac3", 8, ec3Only, stereo, audioOutput{copy: true, channels: 8, codecs: codecsEC3}},
		{"eac3 5.1 with aac51 only", "eac3", 6, aac51Only, surround, surround},
		{"eac3 5.1 with ac3 only", "eac3", 6, ac3Only, stereo, stereo},
		{"eac3 stereo with ec3", "eac3", 2, allAudio, stereo, stereo},
		// ac3: copied with ac3 on fMP4 only; ec3 does not speak for it.
		{"ac3 5.1 with ac3", "ac3", 6, ac3Only, stereo, audioOutput{copy: true, channels: 6, codecs: codecsAC3}},
		{"ac3 5.1 with ec3 only", "ac3", 6, ec3Only, stereo, stereo},
		{"ac3 5.1 with ec3 and aac51", "ac3", 6, aac51EC3, surround, surround},
		{"ac3 stereo with ac3", "ac3", 2, allAudio, stereo, stereo},
		// every other codec over 2 channels: 5.1 with aac51.
		{"dts 5.1 with aac51", "dts", 6, aac51Only, surround, surround},
		{"dts 5.1, everything but aac51", "dts", 6, audioDecoders{ac3: true, ec3: true}, stereo, stereo},
		{"truehd 7.1 with aac51", "truehd", 8, allAudio, surround, surround},
		{"flac 5.1 with aac51", "flac", 6, aac51Only, surround, surround},
		{"dts 5.0 with aac51", "dts", 5, aac51Only, surround, surround},
		// stereo non-AAC: as always.
		{"flac stereo", "flac", 2, allAudio, stereo, stereo},
		{"opus stereo", "opus", 2, allAudio, stereo, stereo},
		{"mp3 stereo", "mp3", 2, allAudio, stereo, stereo},
		{"no channel count", "dts", 0, allAudio, stereo, stereo},
	}
	for _, c := range cases {
		s := audioStream(c.codec, c.ch)
		if got := audioOutputFor(s, c.d, false, ParamOptions{}); got != c.ts {
			t.Errorf("%s, TS: %+v, want %+v", c.name, got, c.ts)
		}
		if got := audioOutputFor(s, c.d, true, ParamOptions{}); got != c.fmp4 {
			t.Errorf("%s, fMP4: %+v, want %+v", c.name, got, c.fmp4)
		}
	}
}

// EncodeAudio (the fallback after an ADTS failure) encodes every track: 5.1
// where aac51 would have given 5.1 or a multichannel copy, stereo
// elsewhere.
func TestAudioOutputFor_EncodeAudio(t *testing.T) {
	enc := ParamOptions{EncodeAudio: true}
	for _, c := range []struct {
		codec string
		ch    int32
		d     audioDecoders
		fmp4  bool
		want  int
	}{
		{"aac", 2, noDecoders, false, 2},
		{"aac", 2, allAudio, true, 2},
		{"aac", 6, aac51Only, false, 6},
		{"aac", 6, noDecoders, false, 2},
		{"eac3", 6, aac51EC3, true, 6},
		{"eac3", 6, ec3Only, true, 2},
		{"ac3", 6, allAudio, true, 6},
	} {
		got := audioOutputFor(audioStream(c.codec, c.ch), c.d, c.fmp4, enc)
		if got.copy || got.channels != c.want || got.codecs != codecsAAC {
			t.Errorf("EncodeAudio %s %dch %+v fmp4=%v: %+v, want an AAC encode to %d channels", c.codec, c.ch, c.d, c.fmp4, got, c.want)
		}
	}
}

// The FFmpeg options of each output: copy, today's stereo encode exactly,
// and 5.1 at 384 kb/s.
func TestAudioCodecParams(t *testing.T) {
	h := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "h264", Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
		{Index: 2, CodecType: "audio", CodecName: "eac3", Channels: 6},
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	check := func(label string, want [][]string) {
		t.Helper()
		for i, a := range h.audio {
			if got := a.codecParams(ParamOptions{}); !reflect.DeepEqual(got, want[i]) {
				t.Errorf("%s, a%d: %v, want %v", label, i, got, want[i])
			}
		}
	}
	check("nothing declared", [][]string{{"-c:a", "copy"}, {"-c:a", "libfdk_aac", "-ac", "2"}, {"-c:a", "libfdk_aac", "-ac", "2"}})
	h.useAudioDecoders(aac51Only)
	check("aac51", [][]string{{"-c:a", "copy"}, {"-c:a", "libfdk_aac", "-ac", "6", "-b:a", "384k"}, {"-c:a", "copy"}})
	// An encode gets -break_non_keyframes like every encoded output, a copy
	// does not.
	args := strings.Join(ffmpegParams(t, h), " ")
	for _, want := range []string{
		"-map 0:1 -f segment -segment_time 4 -segment_list_type hls -segment_list /out/a0.m3u8 -muxdelay 0 -segment_format mpegts -c:a copy /out/a0-%d.ts",
		"-map 0:2 -f segment -segment_time 4 -segment_list_type hls -segment_list /out/a1.m3u8 -muxdelay 0 -segment_format mpegts -break_non_keyframes 1 -c:a libfdk_aac -ac 6 -b:a 384k /out/a1-%d.ts",
		"-map 0:3 -f segment -segment_time 4 -segment_list_type hls -segment_list /out/a2.m3u8 -muxdelay 0 -segment_format mpegts -c:a copy /out/a2-%d.ts",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argv lacks %q:\n%s", want, args)
		}
	}
}

func TestParseDecodeDeclaration_AudioTokens(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    string
		pending bool
		dec     audioDecoders
	}{
		{"aac51", "aac51", false, aac51Only},
		{"ec3,ac3,aac51", "aac51,ac3,ec3", false, allAudio},
		{"ec3,hevc10,aac51", "hevc10,aac51,ec3", false, aac51EC3},
		// Exact match: no case folding, no other spellings.
		{"AAC51,aac5.1,eac3,ec-3,ac-3,aac", "", false, noDecoders},
		// unknown with audio tokens only: the video check had not answered.
		{"unknown,aac51", "unknown,aac51", true, aac51Only},
		{"aac51,unknown,ec3", "unknown,aac51,ec3", true, aac51EC3},
		{"unknown,hevc8,aac51", "hevc8,aac51", false, aac51Only},
		{"unknown", "unknown", true, noDecoders},
	} {
		d := decl(c.in)
		if d.String() != c.want || d.pending != c.pending || d.audioDecoders() != c.dec {
			t.Errorf("%q: %q pending=%v %+v, want %q pending=%v %+v", c.in, d.String(), d.pending, d.audioDecoders(), c.want, c.pending, c.dec)
		}
	}
	// Audio tokens cover no HEVC.
	if a := decl("aac51,ac3,ec3"); a.covers(false, false) || a.covers(true, true) || a.declaresVideo() {
		t.Error("audio tokens speak for HEVC")
	}
}

// A declaration of audio tokens only names no video: never
// no_declaration; past the checks every declaration gets (passthrough_off,
// not_hevc, declaration_pending) it is no_hevc_declared, whatever the
// source's size, without the source probe (which could only answer
// needs_main or needs_main10). "unknown" with audio tokens is still pending
// for the video. A video token, hdr-pq alone included, goes on as before.
func TestVideoRouteFor_AudioOnlyDeclaration(t *testing.T) {
	probed := false
	probe := func() (sourceHEVCFacts, error) { probed = true; return sdrMain10(), nil }
	for _, c := range []struct {
		src   sourceVideo
		decl  string
		cap   passthroughCapability
		want  string
		probe bool
	}{
		{hd, "aac51", passthroughCapability{}, reasonPassthroughOff, false},
		{sourceVideo{codec: "h264", width: 1920, height: 1080}, "aac51,ec3", on, reasonNotHEVC, false},
		{sourceVideo{}, "aac51", on, reasonNotHEVC, false},
		{hd, "aac51", on, reasonNoHEVCDeclared, false},
		{hd, "aac51,ac3,ec3", on, reasonNoHEVCDeclared, false},
		{uhd, "aac51,ac3,ec3", on, reasonNoHEVCDeclared, false},
		{sourceVideo{codec: "hevc", width: 7680, height: 4320}, "ec3", on, reasonNoHEVCDeclared, false},
		{hd, "unknown,aac51", on, reasonDeclarationPending, false},
		{uhd, "aac51,unknown", on, reasonDeclarationPending, false},
		// A video token that covers no depth still asks the probe.
		{hd, "hdr-pq,aac51", on, reasonNeedsMain10, true},
	} {
		probed = false
		d := videoRouteFor(c.src, decl(c.decl), c.cap, probe)
		if d.reason != c.want || d.passthrough || probed != c.probe {
			t.Errorf("%+v %q: %s passthrough=%v probed=%v, want %s probed=%v", c.src, c.decl, d.reason, d.passthrough, probed, c.want, c.probe)
		}
	}
}

// Audio tokens next to video tokens change no video decision.
func TestVideoRouteFor_AudioTokensDoNotMoveTheVideo(t *testing.T) {
	sources := []sourceVideo{hd, uhd, {codec: "h264", width: 1920, height: 1080}, {codec: "hevc", width: 2560, height: 1080}}
	decls := []string{allTokens, "hevc10", "hevc8", "hevc8,hevc8-2160", "hevc10-2160,hdr-pq", "hdr-pq,hevc-high", "unknown", "unknown,hevc10"}
	probes := []func() (sourceHEVCFacts, error){
		facts(nil),
		facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p"; f.HVCC = testHVCC(1, false, 120) }),
		facts(func(f *sourceHEVCFacts) { f.ColorTransfer = transferPQ; f.HVCC = testHVCC(2, false, 150) }),
	}
	for _, cap := range []passthroughCapability{on, {}} {
		for _, src := range sources {
			for _, dl := range decls {
				for _, p := range probes {
					for _, extra := range []string{",aac51", ",ac3,ec3", ",aac51,ac3,ec3"} {
						a := videoRouteFor(src, decl(dl), cap, p)
						b := videoRouteFor(src, decl(dl+extra), cap, p)
						if a.reason != b.reason || a.passthrough != b.passthrough {
							t.Errorf("%+v %q vs %q: %s/%v vs %s/%v", src, dl, dl+extra, a.reason, a.passthrough, b.reason, b.passthrough)
						}
					}
				}
			}
		}
	}
}

// surroundHLS is a source with a video of the given codec and every kind of
// audio track: AAC stereo (1), E-AC-3 5.1 at 640 kb/s (2), AAC 5.1 with
// mkvmerge's BPS tag (3), AC-3 5.1 (4), AAC 7.1 (5), DTS 5.1 (6), and a
// subrip subtitle (7).
func surroundHLS(t *testing.T, videoCodec string, passthrough bool) *HLS {
	t.Helper()
	h := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: videoCodec, Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
		{Index: 2, CodecType: "audio", CodecName: "eac3", Channels: 6, BitRate: "640000"},
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6, Tags: map[string]string{"BPS": "384123"}},
		{Index: 4, CodecType: "audio", CodecName: "ac3", Channels: 6, BitRate: "448000"},
		{Index: 5, CodecType: "audio", CodecName: "aac", Channels: 8},
		{Index: 6, CodecType: "audio", CodecName: "dts", Channels: 6, BitRate: "1509000"},
		{Index: 7, CodecType: "subtitle", CodecName: "subrip"},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	if passthrough && !h.usePassthrough() {
		t.Fatal("usePassthrough refused")
	}
	return h
}

// The run variant: empty (the old key and directory) when no audio output
// differs from what it is without a declaration, one code per output when
// one does; sessions whose arguments are the same share it.
func TestAudioVariant(t *testing.T) {
	stereoOnly := func() *HLS {
		return NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
			{Index: 0, CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080},
			{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
			{Index: 2, CodecType: "audio", CodecName: "ac3", Channels: 2},
		}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	}
	for _, c := range []struct {
		name string
		h    func() *HLS
		d    audioDecoders
		want string
	}{
		{"old route, nothing declared", func() *HLS { return surroundHLS(t, "hevc", false) }, noDecoders, ""},
		{"passthrough, nothing declared", func() *HLS { return surroundHLS(t, "hevc", true) }, noDecoders, "hevc"},
		{"stereo tracks, everything declared", stereoOnly, allAudio, ""},
		{"old route, ec3 only (TS: nothing copied)", func() *HLS { return surroundHLS(t, "hevc", false) }, ec3Only, ""},
		{"old route, aac51", func() *HLS { return surroundHLS(t, "hevc", false) }, aac51Only, "ac6c666"},
		{"old route, everything", func() *HLS { return surroundHLS(t, "hevc", false) }, allAudio, "ac6c666"},
		{"passthrough, ec3", func() *HLS { return surroundHLS(t, "hevc", true) }, ec3Only, "hevc-acc2222"},
		{"passthrough, aac51 and ec3", func() *HLS { return surroundHLS(t, "hevc", true) }, aac51EC3, "hevc-accc666"},
		{"passthrough, everything", func() *HLS { return surroundHLS(t, "hevc", true) }, allAudio, "hevc-acccc66"},
		{"audio-only source, aac51", func() *HLS {
			return NewHLS("http://src/a.flac", &cp.ProbeReply{Streams: []*cp.Stream{{Index: 0, CodecType: "audio", CodecName: "flac", Channels: 6}}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
		}, aac51Only, "a6"},
	} {
		h := c.h()
		h.useAudioDecoders(c.d)
		if got := h.runVariant(); got != c.want {
			t.Errorf("%s: variant %q, want %q", c.name, got, c.want)
		}
		hashDir := "/o/h"
		key := runKeyFor(hashDir, h, 30)
		if c.want == "" && key != runKey(hashDir, 30) {
			t.Errorf("%s: key %s, want the old one", c.name, key)
		}
		if c.want != "" && key != hashDir+":"+c.want+":seek:30.000" {
			t.Errorf("%s: key %s", c.name, key)
		}
		dir := newTranscodeRun(key, hashDir, 30, "", h).OutputDir()
		wantDir := filepath.Join(hashDir, "runs", "seek-30.000")
		if c.want != "" {
			wantDir = filepath.Join(hashDir, "runs", c.want+"-seek-30.000")
		}
		if dir != wantDir {
			t.Errorf("%s: dir %s, want %s", c.name, dir, wantDir)
		}
		if fk := fallbackKey(hashDir, h); (c.want == "" && fk != hashDir) || (c.want != "" && fk != hashDir+":"+c.want) {
			t.Errorf("%s: fallback key %s", c.name, fk)
		}
	}
	// Declarations that give the same arguments give the same key: ac3 on a
	// source whose AC-3 is not copied (the old route) changes nothing.
	a, b := surroundHLS(t, "hevc", false), surroundHLS(t, "hevc", false)
	a.useAudioDecoders(aac51Only)
	b.useAudioDecoders(allAudio)
	pa, _ := a.GetFFmpegParams("/out")
	pb, _ := b.GetFFmpegParams("/out")
	if runKeyFor("/h", a, 0) != runKeyFor("/h", b, 0) || !reflect.DeepEqual(pa, pb) {
		t.Error("the same arguments under different keys, or different arguments under one")
	}
}

// Runs of sessions whose audio differs are different runs, in different
// directories, with their own remembered real start; sessions that declare
// nothing the source's audio needs share the old route's run.
func TestAudioRunIdentity(t *testing.T) {
	fakeFFmpeg(t)
	orig := probeRunStart
	probeRunStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 3, nil }
	t.Cleanup(func() { probeRunStart = orig })
	hashDir := t.TempDir()
	m := NewRunManager()
	defer m.CloseAll()
	plain := surroundHLS(t, "h264", false)
	declared := surroundHLS(t, "h264", false)
	declared.useAudioDecoders(aac51Only)
	ec3OnTS := surroundHLS(t, "h264", false)
	ec3OnTS.useAudioDecoders(ec3Only)
	rPlain, err := m.Acquire(hashDir, 600, "http://src/movie.mkv", plain)
	if err != nil {
		t.Fatal(err)
	}
	rDecl, err := m.Acquire(hashDir, 600, "http://src/movie.mkv", declared)
	if err != nil {
		t.Fatal(err)
	}
	rEC3, err := m.Acquire(hashDir, 600, "http://src/movie.mkv", ec3OnTS)
	if err != nil {
		t.Fatal(err)
	}
	if rPlain == rDecl || rEC3 != rPlain {
		t.Fatalf("runs: plain %p declared %p ec3-on-TS %p", rPlain, rDecl, rEC3)
	}
	if rPlain.OutputDir() == rDecl.OutputDir() {
		t.Error("one directory for two argument lists")
	}
	if strings.Contains(strings.Join(rPlain.cmd.Args, " "), "-ac 6") || !strings.Contains(strings.Join(rDecl.cmd.Args, " "), "-ac 6 -b:a 384k") {
		t.Errorf("each run its own audio:\nplain    %v\ndeclared %v", rPlain.cmd.Args, rDecl.cmd.Args)
	}
	if v, ok := m.ResolvedStart(runKeyFor(hashDir, declared, 600)); !ok || v != 597 {
		t.Errorf("the declared run's real start %v %v", v, ok)
	}
	if _, ok := m.ResolvedStart(runKey(hashDir, 600)); !ok {
		t.Error("the old route's run remembers its own")
	}
	// Options learned by the declared variant stay with it.
	m.rememberFallbacks(fallbackKey(hashDir, declared), ParamOptions{EncodeAudio: true})
	m.mu.Lock()
	nPlain := m.newRunLocked(runKeyFor(hashDir, plain, 0), hashDir, 0, "", plain)
	nDecl := m.newRunLocked(runKeyFor(hashDir, declared, 0), hashDir, 0, "", declared)
	m.mu.Unlock()
	if nPlain.fallbacks.EncodeAudio || !nDecl.fallbacks.EncodeAudio {
		t.Errorf("fallbacks leak across audio variants: plain %+v declared %+v", nPlain.fallbacks, nDecl.fallbacks)
	}
}

// Without a declaration the old route's master is byte for byte what it
// has always been, for a source full of multichannel audio too.
func TestOldRouteMaster_NoDeclarationUnchanged(t *testing.T) {
	h := surroundHLS(t, "hevc", false)
	dir := t.TempDir()
	if err := h.MakeMasterPlaylist(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	want := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,URI="a0.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",URI="a1.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #3",URI="a2.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #4",URI="a3.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #5",URI="a4.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #6",URI="a5.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="Subtitle #1",URI="s0.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=8000000,CODECS="avc1.42e00a,mp4a.40.2",AUDIO="audio",SUBTITLES="subtitles"` + "\n" +
		"v0-1080.m3u8\n"
	if string(got) != want {
		t.Errorf("master\n got %q\nwant %q", got, want)
	}
}

// With aac51 the old route's master says each rendition's channels and
// counts the largest audio in BANDWIDTH; CODECS stays mp4a.40.2 (every
// output is AAC).
func TestOldRouteMaster_Declared(t *testing.T) {
	h := surroundHLS(t, "hevc", false)
	h.useAudioDecoders(allAudio)
	dir := t.TempDir()
	if err := h.MakeMasterPlaylist(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	want := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="a0.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",CHANNELS="6",URI="a1.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #3",CHANNELS="6",URI="a2.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #4",CHANNELS="6",URI="a3.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #5",CHANNELS="6",URI="a4.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #6",CHANNELS="6",URI="a5.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="Subtitle #1",URI="s0.m3u8"` + "\n" +
		// 8000 kb/s of video and the copied AAC 5.1 at its BPS, 384123.
		`#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=8384123,CODECS="avc1.42e00a,mp4a.40.2",AUDIO="audio",SUBTITLES="subtitles"` + "\n" +
		"v0-1080.m3u8\n"
	if string(got) != want {
		t.Errorf("master\n got %q\nwant %q", got, want)
	}
	// An audio-only source: BANDWIDTH was 1, now its audio.
	a := NewHLS("http://src/a.flac", &cp.ProbeReply{Streams: []*cp.Stream{{Index: 0, CodecType: "audio", CodecName: "flac", Channels: 6}}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	a.useAudioDecoders(aac51Only)
	if err := a.MakeMasterPlaylist(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "index.m3u8")); string(got) != "#EXTM3U\n#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=384000,CODECS=\"avc1.42e00a,mp4a.40.2\"\na0.m3u8\n" {
		t.Errorf("audio-only master %q", got)
	}
}

// The passthrough master lists every audio codec its renditions have, says
// their channels, and counts the largest audio in BANDWIDTH.
func TestPassthroughMaster_DeclaredAudio(t *testing.T) {
	h := surroundHLS(t, "hevc", true)
	f := sdrMain10()
	h.passFacts = &f
	if got := h.passthroughMasterPlaylist("hvc1.2.4.L120.90", passthroughBandwidth(0, 10_000_000, 10, h.passthroughAudioAllowance())); !strings.Contains(got, `BANDWIDTH=8192000,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L120.90,mp4a.40.2",`) || strings.Contains(got, "CHANNELS") {
		t.Errorf("nothing declared:\n%s", got)
	}
	h.useAudioDecoders(allAudio)
	got := h.passthroughMasterPlaylist("hvc1.2.4.L120.90", passthroughBandwidth(0, 10_000_000, 10, h.passthroughAudioAllowance()))
	want := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="a0.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",CHANNELS="6",URI="a1.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #3",CHANNELS="6",URI="a2.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #4",CHANNELS="6",URI="a3.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #5",CHANNELS="6",URI="a4.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #6",CHANNELS="6",URI="a5.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="Subtitle #1",URI="s0.m3u8"` + "\n" +
		// 8 Mbit/s of video, the copied E-AC-3's 640 kb/s the largest audio.
		`#EXT-X-STREAM-INF:BANDWIDTH=8640000,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L120.90,mp4a.40.2,ec-3,ac-3",VIDEO-RANGE=SDR,AUDIO="audio",SUBTITLES="subtitles"` + "\n" +
		"v0-1080.m3u8\n"
	if got != want {
		t.Errorf("master\n got %q\nwant %q", got, want)
	}
}

func TestAudioOutputBitRate(t *testing.T) {
	for _, c := range []struct {
		s    *cp.Stream
		o    audioOutput
		want int64
	}{
		{&cp.Stream{BitRate: "640000"}, audioOutput{copy: true, channels: 6}, 640_000},
		{&cp.Stream{Tags: map[string]string{"BPS": "448000"}}, audioOutput{copy: true, channels: 6}, 448_000},
		{&cp.Stream{Tags: map[string]string{"BPS-eng": "255999"}}, audioOutput{copy: true, channels: 6}, 255_999},
		{&cp.Stream{BitRate: "N/A", Tags: map[string]string{"BPS": "x"}}, audioOutput{copy: true, channels: 6}, copiedMultichannelBitRate},
		{&cp.Stream{}, audioOutput{copy: true, channels: 2}, copiedStereoBitRate},
		{&cp.Stream{BitRate: "1509000"}, audioOutput{channels: 6}, aac51BitRate},
		{&cp.Stream{BitRate: "1509000"}, audioOutput{channels: 2}, encodedStereoBitRate},
	} {
		if got := c.o.bitRate(c.s); got != c.want {
			t.Errorf("%+v %+v: %d, want %d", c.s, c.o, got, c.want)
		}
	}
}

// A seek run of a re-encoded video cuts every copied audio output: with
// aac51 the AAC 5.1 (0:3 of seekCutHLS) is copied and cut too, the AC-3
// (0:2) is encoded to 5.1 and trimmed by the input seek, not cut.
func TestReencodeSeekCuts_CopiedAAC51(t *testing.T) {
	h := seekCutHLS("hevc")
	h.useAudioDecoders(aac51Only)
	if got := h.reencodeSeekCuts(ParamOptions{}); !reflect.DeepEqual(got, []string{"0:1", "0:3", "0:4"}) {
		t.Errorf("cuts %v, want the copied AAC tracks 0:1 and 0:3 and the subtitle 0:4", got)
	}
	cut, _ := cutsIn(startedArgs(t, h, 30, ParamOptions{}))
	if !reflect.DeepEqual(cut, []string{"0:1", "0:3", "0:4"}) {
		t.Errorf("seek run cuts %v", cut)
	}
	// EncodeAudio: every track encoded and trimmed, only the subtitle cut.
	if cut, _ := cutsIn(startedArgs(t, h, 30, ParamOptions{EncodeAudio: true})); !reflect.DeepEqual(cut, []string{"0:4"}) {
		t.Errorf("EncodeAudio cuts %v", cut)
	}
}

// A passthrough seek run cuts every audio output at the real start, the
// copied E-AC-3 and AC-3 among them.
func TestPassthroughSeek_CutsCopiedDolby(t *testing.T) {
	h := surroundHLS(t, "hevc", true)
	h.useAudioDecoders(allAudio)
	fakeFFmpeg(t)
	orig := probePassthroughStart
	probePassthroughStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 2.5, nil }
	t.Cleanup(func() { probePassthroughStart = orig })
	r := newTranscodeRun("k", t.TempDir(), 30, "http://src/movie.mkv", h)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cleanup)
	args := r.cmd.Args[1:]
	cut, _ := cutsIn(args)
	if !reflect.DeepEqual(cut, []string{"0:1", "0:2", "0:3", "0:4", "0:5", "0:6"}) {
		t.Errorf("cuts %v, want every audio output", cut)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-ss 0 -map 0:2 -c:a copy -f hls", "-ss 0 -map 0:4 -c:a copy -f hls", "-ss 0 -map 0:3 -c:a copy -f hls", "-ss 0 -map 0:5 -c:a libfdk_aac -ac 6 -b:a 384k -f hls"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
}

// A passthrough session whose declaration leaves the audio as it was
// (hevc8 alone) counts its audio as 079acfd did: the 192 kb/s allowance,
// not the copied AAC's own 128 kb/s -- the master must stay byte for byte
// the old one, and audioBandwidth would say 128000 here (it says 192000
// for a copy without a rate, which is why the other cases cannot tell).
func TestPassthroughBandwidth_UndeclaredAudioKeepsAllowance(t *testing.T) {
	h := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2, BitRate: "128000"},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	if !h.usePassthrough() {
		t.Fatal("usePassthrough refused")
	}
	f := sdrMain10()
	h.passFacts = &f
	h.useAudioDecoders(decl("hevc8").audioDecoders())
	if got := h.passthroughAudioAllowance(); got != passthroughAudioBandwidth {
		t.Errorf("allowance %d, want %d", got, passthroughAudioBandwidth)
	}
	// 079acfd: passthroughBandwidth(0, 10e6, 10, true) = 8 Mbit/s + 192000.
	got := h.passthroughMasterPlaylist("hvc1.2.4.L120.90", passthroughBandwidth(0, 10_000_000, 10, h.passthroughAudioAllowance()))
	want := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,URI="a0.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=8192000,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L120.90,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="audio"` + "\n" +
		"v0-1080.m3u8\n"
	if got != want {
		t.Errorf("master\n got %q\nwant %q", got, want)
	}
}
