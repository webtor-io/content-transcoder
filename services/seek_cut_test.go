package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	cp "github.com/webtor-io/content-prober/content-prober"
)

// seekCutHLS is an old-route session of a source whose video is re-encoded
// (hevc) or copied (h264), with a copied AAC track (input 1), an AC3 track
// (2, encoded) and a 5.1 AAC track (3, encoded), a subrip subtitle (4), a
// PGS one (5, not in the session) and a dvd_subtitle one (6, no output).
func seekCutHLS(videoCodec string) *HLS {
	return NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: videoCodec, Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
		{Index: 2, CodecType: "audio", CodecName: "ac3", Channels: 6},
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6, ChannelLayout: "5.1"},
		{Index: 4, CodecType: "subtitle", CodecName: "subrip"},
		{Index: 5, CodecType: "subtitle", CodecName: "hdmv_pgs_subtitle"},
		{Index: 6, CodecType: "subtitle", CodecName: "dvd_subtitle"},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", threads: 2})
}

// startedArgs starts a run of h at seek with the given options against the
// fake FFmpeg, the copy route's probe answering 2.5 s before the seek, and
// returns its arguments (without the program).
func startedArgs(t *testing.T, h *HLS, seek float64, opts ParamOptions) []string {
	t.Helper()
	return startedArgsProbed(t, h, seek, opts, func(_ context.Context, _ string, _ string, s float64) (float64, error) { return s - 2.5, nil })
}

// startedArgsProbed is startedArgs with the copy route's probe given.
func startedArgsProbed(t *testing.T, h *HLS, seek float64, opts ParamOptions, probe func(context.Context, string, string, float64) (float64, error)) []string {
	t.Helper()
	fakeFFmpeg(t)
	orig := probeRunStart
	probeRunStart = probe
	t.Cleanup(func() { probeRunStart = orig })
	r := newTranscodeRun("k", t.TempDir(), seek, "http://src/movie.mkv", h)
	r.fallbacks = opts
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cleanup)
	return append([]string(nil), r.cmd.Args[1:]...)
}

// cutsIn returns the -map value of every output that has -ss 0 right
// before its -map, and the number of -ss options in args.
func cutsIn(args []string) (cut []string, ss int) {
	for i, a := range args {
		if a == "-ss" {
			ss++
		}
		if a == "-map" && i >= 2 && args[i-2] == "-ss" && args[i-1] == "0" && i+1 < len(args) {
			cut = append(cut, args[i+1])
		}
	}
	return cut, ss
}

func TestReencodeSeekCuts_CopiedAudioAndSubtitles(t *testing.T) {
	if got := seekCutHLS("hevc").reencodeSeekCuts(ParamOptions{}); !reflect.DeepEqual(got, []string{"0:1", "0:4"}) {
		t.Errorf("cuts %v, want the copied AAC track 0:1 and the subrip output 0:4 (not the dvd_subtitle one, which has no output)", got)
	}
	// A source that needs its AAC encoded (EncodeAudio after an ADTS
	// failure) has every audio track through the input trim; the
	// subtitles still need the cut.
	if got := seekCutHLS("hevc").reencodeSeekCuts(ParamOptions{EncodeAudio: true}); !reflect.DeepEqual(got, []string{"0:4"}) {
		t.Errorf("EncodeAudio: cuts %v, want the subtitle output only", got)
	}
	// A webvtt track is copied, not encoded: cut all the same.
	v := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "vp9", Height: 720},
		{Index: 1, CodecType: "subtitle", CodecName: "webvtt"},
		{Index: 2, CodecType: "subtitle", CodecName: "ass"},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	if got := v.reencodeSeekCuts(ParamOptions{}); !reflect.DeepEqual(got, []string{"0:1", "0:2"}) {
		t.Errorf("webvtt and ass subtitles: cuts %v, want both", got)
	}
	// Audio-only: the track is the primary, not in h.audio.
	a := NewHLS("http://src/book.m4a", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "audio", CodecName: "aac", Channels: 2},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	if got := a.reencodeSeekCuts(ParamOptions{}); len(got) != 0 {
		t.Errorf("audio-only: cuts %v, want none", got)
	}
}

func TestCutAtOutputStart(t *testing.T) {
	in := []string{"-ss", "30.000", "-i", "U", "-map", "0:0", "-c:v", "h264", "v.ts", "-map", "0:1", "-c:a", "copy", "a0.ts", "-map", "0:2", "a1.ts"}
	got := cutAtOutputStart(in, []string{"0:1"})
	want := []string{"-ss", "30.000", "-i", "U", "-map", "0:0", "-c:v", "h264", "v.ts", "-ss", "0", "-map", "0:1", "-c:a", "copy", "a0.ts", "-map", "0:2", "a1.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := cutAtOutputStart(in, nil); !reflect.DeepEqual(got, in) {
		t.Errorf("no maps must change nothing: %v", got)
	}
}

// A seek run of a re-encoded video puts -ss 0 before the -map of the copied
// audio track and of the subtitle output and nowhere else; a run from the
// start gets no cut at all, a source whose audio is encoded only the
// subtitles' (the copy route: TestCopySeekRun_CountsFromTheRealStart).
// Negative control: with the cut out of startLocked the first case fails
// (no cut), and with the subtitles out of reencodeSeekCuts the subtitle
// case does.
func TestReencodeSeekRun_CutsCopiedAudioAndSubtitlesAtTheSeek(t *testing.T) {
	args := startedArgs(t, seekCutHLS("hevc"), 30, ParamOptions{})
	cut, ss := cutsIn(args)
	for _, m := range []string{"0:1", "0:4"} {
		if !containsString(cut, m) {
			t.Errorf("re-encode seek: output %s not cut at the seek point: %v", m, strings.Join(args, " "))
		}
	}
	for _, m := range []string{"0:0", "0:2", "0:3"} {
		if containsString(cut, m) {
			t.Errorf("re-encode seek: %s must not be cut (the video and encoded audio are trimmed by the input seek)", m)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "-ss 30.000 -i ") {
		t.Errorf("the input seek must stay: %v", args)
	}
	if ss != 1+len(cut) {
		t.Errorf("%d -ss options for %d cuts: %v", ss, len(cut), args)
	}

	if cut, ss := cutsIn(startedArgs(t, seekCutHLS("hevc"), 0, ParamOptions{})); ss != 0 || len(cut) != 0 {
		t.Errorf("from the start: no seek, no cut; got %d -ss, cuts %v", ss, cut)
	}
	if cut, _ := cutsIn(startedArgs(t, seekCutHLS("hevc"), 30, ParamOptions{EncodeAudio: true})); !reflect.DeepEqual(cut, []string{"0:4"}) {
		t.Errorf("EncodeAudio: every audio track is encoded and trimmed, only the subtitles are cut; got %v", cut)
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// A copy-route seek run counts every output from where FFmpeg's seek lands
// (realStart, 2.5 s before the seek here): -itsoffset <seek - realStart>
// among the input options, and -ss 0 before the -map of each subtitle
// output, so the cues start at the offset and none from before the keyframe
// moves the rest; the video and the audio outputs are not cut. A probe that
// failed (realStart the quantized seek) or a run that starts after the seek
// point (an MPEG-TS) gets no offset, only the cut; a run from the start
// neither. Negative control: without -itsoffset (injectCopySeekParams) or
// without the subtitle maps passed to it (startLocked) the first case fails.
func TestCopySeekRun_CountsFromTheRealStart(t *testing.T) {
	args := startedArgs(t, seekCutHLS("h264"), 30, ParamOptions{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, " -fix_sub_duration -ss 30.000 -noaccurate_seek -itsoffset 2.500000 -i http://src/movie.mkv ") {
		t.Errorf("copy seek: want the input seek and -itsoffset 2.500000 before -i: %v", joined)
	}
	cut, ss := cutsIn(args)
	if !reflect.DeepEqual(cut, []string{"0:4"}) || ss != 2 {
		t.Errorf("copy seek: want -ss 0 before the subrip output (0:4) only, the input seek the other -ss; got cuts %v, %d -ss: %v", cut, ss, joined)
	}
	if strings.Count(joined, "-itsoffset") != 1 {
		t.Errorf("one -itsoffset: %v", joined)
	}

	// The probe found a start after the seek point (MPEG-TS): no
	// -itsoffset, the cut all the same.
	after := func(_ context.Context, _ string, _ string, s float64) (float64, error) { return s + 5.021, nil }
	args = startedArgsProbed(t, seekCutHLS("h264"), 30, ParamOptions{}, after)
	joined = strings.Join(args, " ")
	if cut, _ := cutsIn(args); strings.Contains(joined, "-itsoffset") || !reflect.DeepEqual(cut, []string{"0:4"}) ||
		!strings.Contains(joined, " -ss 30.000 -noaccurate_seek -i ") {
		t.Errorf("run starts after the seek: want the input seek, no -itsoffset, the subtitle cut; got cuts %v: %v", cut, joined)
	}

	// No answer: the run's zero is not known, and the argv is the one
	// before the cut existed -- the input seek alone.
	failed := func(context.Context, string, string, float64) (float64, error) { return 0, errors.New("cold source") }
	implausible := func(_ context.Context, _ string, _ string, s float64) (float64, error) { return s + 90, nil }
	for name, probe := range map[string]func(context.Context, string, string, float64) (float64, error){"probe failed": failed, "implausible answer": implausible} {
		args := startedArgsProbed(t, seekCutHLS("h264"), 30, ParamOptions{}, probe)
		joined := strings.Join(args, " ")
		if cut, ss := cutsIn(args); strings.Contains(joined, "-itsoffset") || len(cut) != 0 || ss != 1 ||
			!strings.Contains(joined, " -ss 30.000 -noaccurate_seek -i ") {
			t.Errorf("%s: want the input seek alone, no -itsoffset, no cut; got cuts %v: %v", name, cut, joined)
		}
	}

	from0 := strings.Join(startedArgs(t, seekCutHLS("h264"), 0, ParamOptions{}), " ")
	if strings.Contains(from0, "-itsoffset") || strings.Contains(from0, "-ss ") {
		t.Errorf("from the start: no seek, no offset, no cut: %v", from0)
	}
}

// A run re-created from the manager's memory (the reaper deletes idle runs
// under live sessions) cuts the subtitles only where the first run's probe
// answered: the memory keeps a fallback's offset too, and must keep that it
// was one, or the second run of the key cuts where the first did not.
func TestCopySeekRun_MemoryKeepsWhetherProbed(t *testing.T) {
	fakeFFmpeg(t)
	orig := probeRunStart
	t.Cleanup(func() { probeRunStart = orig })
	for _, c := range []struct {
		name   string
		probe  func(context.Context, string, string, float64) (float64, error)
		cut    []string
		offset bool
	}{
		{"probed", func(_ context.Context, _ string, _ string, s float64) (float64, error) { return s - 2.5, nil }, []string{"0:4"}, true},
		{"fallback", func(context.Context, string, string, float64) (float64, error) { return 0, errors.New("cold source") }, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			key := runKey(dir, 30)
			m := NewRunManager()
			defer m.CloseAll()
			probeRunStart = c.probe
			for i := 0; i < 2; i++ {
				r, err := m.Acquire(dir, 30, "http://src/movie.mkv", seekCutHLS("h264"))
				if err != nil {
					t.Fatal(err)
				}
				args := r.cmd.Args[1:]
				joined := strings.Join(args, " ")
				if cut, _ := cutsIn(args); !reflect.DeepEqual(cut, c.cut) || strings.Contains(joined, "-itsoffset") != c.offset {
					t.Errorf("run %d: cuts %v (want %v), -itsoffset %v (want %v): %v", i+1, cut, c.cut, !c.offset, c.offset, joined)
				}
				// Reaped; the next run of the key comes from the memory
				// and must not probe.
				m.Release(r)
				r.Cleanup()
				m.mu.Lock()
				delete(m.runs, key)
				m.mu.Unlock()
				probeRunStart = func(context.Context, string, string, float64) (float64, error) {
					t.Error("a run re-created from the memory probed again")
					return 0, errors.New("probed")
				}
			}
		})
		probeRunStart = orig
	}
}

func TestInjectCopySeekParams(t *testing.T) {
	in := []string{"-fix_sub_duration", "-i", "U", "-map", "0:0", "v.ts", "-map", "0:1", "a.ts", "-map", "0:2", "s.vtt"}
	for _, c := range []struct {
		realStart float64
		want      []string
	}{
		{20, []string{"-fix_sub_duration", "-ss", "30.000", "-noaccurate_seek", "-itsoffset", "10.000000", "-i", "U", "-map", "0:0", "v.ts", "-map", "0:1", "a.ts", "-ss", "0", "-map", "0:2", "s.vtt"}},
		{0, []string{"-fix_sub_duration", "-ss", "30.000", "-noaccurate_seek", "-itsoffset", "30.000000", "-i", "U", "-map", "0:0", "v.ts", "-map", "0:1", "a.ts", "-ss", "0", "-map", "0:2", "s.vtt"}},
		{30, []string{"-fix_sub_duration", "-ss", "30.000", "-noaccurate_seek", "-i", "U", "-map", "0:0", "v.ts", "-map", "0:1", "a.ts", "-ss", "0", "-map", "0:2", "s.vtt"}},
		{35.021, []string{"-fix_sub_duration", "-ss", "30.000", "-noaccurate_seek", "-i", "U", "-map", "0:0", "v.ts", "-map", "0:1", "a.ts", "-ss", "0", "-map", "0:2", "s.vtt"}},
	} {
		if got := injectCopySeekParams(in, 30, c.realStart, []string{"0:2"}); !reflect.DeepEqual(got, c.want) {
			t.Errorf("realStart %v:\n got  %v\n want %v", c.realStart, got, c.want)
		}
	}
}
