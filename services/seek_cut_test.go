package services

import (
	"context"
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
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6},
		{Index: 4, CodecType: "subtitle", CodecName: "subrip"},
		{Index: 5, CodecType: "subtitle", CodecName: "hdmv_pgs_subtitle"},
		{Index: 6, CodecType: "subtitle", CodecName: "dvd_subtitle"},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", threads: 2})
}

// startedArgs starts a run of h at seek with the given options against the
// fake FFmpeg and returns its arguments (without the program).
func startedArgs(t *testing.T, h *HLS, seek float64, opts ParamOptions) []string {
	t.Helper()
	fakeFFmpeg(t)
	orig := probeRunStart
	probeRunStart = func(_ context.Context, _ string, _ string, s float64) (float64, error) { return s - 2.5, nil }
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

func TestReencodeSeekCuts_CopiedAudioOnly(t *testing.T) {
	if got := seekCutHLS("hevc").reencodeSeekCuts(ParamOptions{}); !reflect.DeepEqual(got, []string{"0:1"}) {
		t.Errorf("cuts %v, want only the copied AAC track 0:1", got)
	}
	// A source that needs its AAC encoded (EncodeAudio after an ADTS
	// failure) has every track through the input trim: nothing to cut.
	if got := seekCutHLS("hevc").reencodeSeekCuts(ParamOptions{EncodeAudio: true}); len(got) != 0 {
		t.Errorf("EncodeAudio: cuts %v, want none", got)
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
// audio track and nowhere else; a run from the start, a copy-route seek and
// a source whose audio is encoded get no cut at all. Negative control: with
// the cut out of startLocked the first case fails (no cut).
func TestReencodeSeekRun_CutsCopiedAudioAtTheSeek(t *testing.T) {
	args := startedArgs(t, seekCutHLS("hevc"), 30, ParamOptions{})
	cut, ss := cutsIn(args)
	for _, m := range []string{"0:1"} {
		if !containsString(cut, m) {
			t.Errorf("re-encode seek: copied audio %s not cut at the seek point: %v", m, strings.Join(args, " "))
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
	copyArgs := startedArgs(t, seekCutHLS("h264"), 30, ParamOptions{})
	if cut, ss := cutsIn(copyArgs); ss != 1 || len(cut) != 0 || !strings.Contains(strings.Join(copyArgs, " "), "-ss 30.000 -noaccurate_seek -i ") {
		t.Errorf("copy route seek: its input seek only; got %d -ss, cuts %v: %v", ss, cut, copyArgs)
	}
	if cut, _ := cutsIn(startedArgs(t, seekCutHLS("hevc"), 30, ParamOptions{EncodeAudio: true})); len(cut) != 0 {
		t.Errorf("EncodeAudio: every track is encoded and trimmed, no cut; got %v", cut)
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
