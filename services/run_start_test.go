package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/errors"
	cp "github.com/webtor-io/content-prober/content-prober"
)

// fakeSeekTools puts an ffmpeg that records its arguments and writes the
// framecrc line given, and an ffprobe that answers 30.000, first in PATH;
// it returns the file the arguments go to.
func fakeSeekTools(t *testing.T, framecrc string) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	ffmpeg := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nprintf '#tb 0: 1/1000\\n" + framecrc + "\\n'\n"
	ffprobe := "#!/bin/sh\nprintf '30.000000,N/A\\n'\n"
	for name, script := range map[string]string{"ffmpeg": ffmpeg, "ffprobe": ffprobe} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// The copy route asks FFmpeg where its seek lands, with the run's own seek
// options, not ffprobe: for a seek to 30 on an MKV with B-frames whose
// keyframe at 30.000 is inside FFmpeg's 3/23 s heuristic, ffprobe names that
// keyframe (the answer the fake ffprobe gives), while FFmpeg lands on the
// one at 20.000 (the fake ffmpeg's framecrc: DTS -10.083 and PTS -10.000
// from the seek). The run reports the keyframe's PTS: 20.000. Negative
// control: probeRunStart back on ffprobe reports 30, on ffmpegSeekStart
// (the DTS) 19.917.
func TestCopyRouteRealStartIsFFmpegsFirstFrame(t *testing.T) {
	argsFile := fakeSeekTools(t, "0,     -10083,    -10000,       41,     8075, 0x71caa088")
	h := NewHLS("http://src/x.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "h264", Height: 360},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	run := newTranscodeRun("k", t.TempDir(), 30, "http://src/x.mkv", h)
	run.resolveRealStartOnce()
	if got := run.RealStart(); math.Abs(got-20) > 1e-9 {
		t.Fatalf("copy route real start %v, want the keyframe's PTS by FFmpeg's seek, 20.000", got)
	}
	// The probe seeks exactly as the run will (copySeekInput in both).
	b, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(b), strings.Join(copySeekInput(30), " ")+" -i http://src/x.mkv -map 0:0 ") {
		t.Errorf("probe args %s", b)
	}
	runArgs := strings.Join(injectSeekParams([]string{"-fix_sub_duration", "-i", "http://src/x.mkv"}, 30, true), " ")
	if runArgs != "-fix_sub_duration "+strings.Join(copySeekInput(30), " ")+" -i http://src/x.mkv" {
		t.Errorf("run args %s", runArgs)
	}
}

// The first frame is the packet's PTS, its DTS when it has none; the
// passthrough answer on the same packet stays the earlier one (the DTS).
func TestFFmpegSeekFirstFrame(t *testing.T) {
	for _, c := range []struct {
		line           string
		frame, ptStart float64
	}{
		{"0,     -10083,    -10000,       41,     8075, 0x71caa088", 20, 19.917},
		{"0, -9223372036854775808,    -10000,       41,     8075, 0x71caa088", 20, 20},
		{"0,     -10083, -9223372036854775808,       41,     8075, 0x71caa088", 19.917, 19.917},
		// The first keyframe of the file, two frames of B-frame delay.
		{"0,     -30083,    -30000,       41,     8075, 0x71caa088", 0, -0.083},
	} {
		fakeSeekTools(t, c.line)
		got, err := ffmpegSeekFirstFrame(context.Background(), "http://src/x.mkv", "0", 30)
		if err != nil || math.Abs(got-c.frame) > 1e-9 {
			t.Errorf("%q: first frame %v %v, want %v", c.line, got, err, c.frame)
		}
		got, err = ffmpegSeekStart(context.Background(), "http://src/x.mkv", "0", 30)
		if err != nil || math.Abs(got-c.ptStart) > 1e-9 {
			t.Errorf("%q: passthrough start %v %v, want %v", c.line, got, err, c.ptStart)
		}
	}
	if _, err := ffmpegSeekFirstFrame(context.Background(), "-i", "0", 30); err == nil {
		t.Error("a source that parses as an option was run")
	}
}

// TestResolveRealStart pins the fallbacks: anything that cannot be a
// keyframe for this seek — an error, a time after the seek point, one
// implausibly far before it — reports the quantized seek, never breaks the
// run.
func TestResolveRealStart(t *testing.T) {
	orig := probeRunStart
	t.Cleanup(func() { probeRunStart = orig })
	run := newTranscodeRun("k", t.TempDir(), 600, "http://src", nil)

	for _, c := range []struct {
		name   string
		k      float64
		err    error
		want   float64
		result string
	}{
		{"keyframe", 598.343, nil, 598.343, realStartOK},
		{"error falls back", 0, errors.New("boom"), 600, realStartFailed},
		{"after the seek point falls back", 601, nil, 600, realStartImplausible},
		{"implausibly early falls back", 500, nil, 600, realStartImplausible},
	} {
		probeRunStart = func(context.Context, string, string, float64) (float64, error) { return c.k, c.err }
		if got, result := run.resolveRealStart(); got != c.want || result != c.result {
			t.Errorf("%s: %v %s, want %v %s", c.name, got, result, c.want, c.result)
		}
	}
}

func TestRealStartFallsBackToSeekTime(t *testing.T) {
	run := newTranscodeRun("k", t.TempDir(), 600, "http://src", nil)
	if got := run.RealStart(); got != 600 {
		t.Fatalf("unresolved: %v", got)
	}
	run.realStart = 598.343
	run.realStartResolved = true
	if got := run.RealStart(); got != 598.343 {
		t.Fatalf("resolved: %v", got)
	}
	// 0.000 is a real answer (the only keyframe before a short seek is the
	// first frame), not the "unresolved" sentinel.
	run2 := newTranscodeRun("k2", t.TempDir(), 30, "http://src", nil)
	run2.realStart = 0
	run2.realStartResolved = true
	if got := run2.RealStart(); got != 0 {
		t.Fatalf("a resolved keyframe at zero must be reported as zero, got %v", got)
	}
}

// TestPlaylistCarriesTheRealStart: the SESSION-OFFSET every downstream
// consumer reads (torrent-http-proxy's grace rewrite, subtitle-translate's
// run detection, the player's cue shift) is the run's real start, fractional
// — not the quantized seek that used to run ahead of the sound by a GOP.
func TestPlaylistCarriesTheRealStart(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	s := NewSession(SessionConfig{ID: "test-real-offset", HashDir: dir, RunMgr: runMgr})
	s.seekTime = 1500

	runDir := t.TempDir()
	run := newTranscodeRun("k", dir, 1500, "", nil)
	run.outputDir = runDir
	run.realStart = 1495.5
	run.realStartResolved = true
	run.AddRef()
	s.run = run

	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\nv0-0.ts\n"
	if err := os.WriteFile(filepath.Join(runDir, "v0.m3u8.ffmpeg"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := s.PlaylistForStream("v0.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(string(got), "#EXT-X-SESSION-OFFSET:1495.500\n") {
		t.Fatalf("want the run's real start in the tag, got:\n%s", got)
	}
}

// The keyframe probe must look at the stream the run maps, not v:0: with
// cover art first, v:0 is the picture.
func TestResolveRealStartProbesTheMappedVideo(t *testing.T) {
	orig := probeRunStart
	t.Cleanup(func() { probeRunStart = orig })
	var got string
	probeRunStart = func(_ context.Context, _ string, stream string, _ float64) (float64, error) {
		got = stream
		return 598, nil
	}
	h := NewHLS("http://src/x.mp4", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "mjpeg"},
		{Index: 1, CodecType: "video", CodecName: "h264", Height: 720},
	}}, &HLSConfig{sm: Online})
	run := newTranscodeRun("k", t.TempDir(), 600, "http://src/x.mp4", h)
	_, _ = run.resolveRealStart()
	if got != "1" {
		t.Fatalf("probed stream %q, want \"1\"", got)
	}
}

// TestResolvedStartSurvivesTheRunObject: the reaper deletes idle runs out
// from under 10-minute sessions; a re-created run for the same key must
// report the offset the first one resolved, not re-probe (a cold source
// fails the probe and the fallback would move the playlist tag mid-session,
// which every consumer reads as a new run). The session-level fallback
// (Session.RunStart with no run at all) reads the same memory.
func TestResolvedStartSurvivesTheRunObject(t *testing.T) {
	orig := probeRunStart
	t.Cleanup(func() { probeRunStart = orig })
	probes := 0
	probeRunStart = func(context.Context, string, string, float64) (float64, error) {
		probes++
		return 598.343, nil
	}
	dir := t.TempDir()
	m := NewRunManager()
	defer m.CloseAll()
	// A copy-mode video: h264 in, so GetCodecParams answers "copy" and the
	// probe gate opens. proto getters are nil-safe for the rest.
	h := &HLS{primary: []*HLSStream{NewHLSStream(0, Video, &cp.Stream{CodecName: "h264"}, nil, nil, false)}}
	key := runKey(dir, 600)

	m.mu.Lock()
	r1 := m.newRunLocked(key, dir, 600, "http://src/x.mkv", h)
	m.mu.Unlock()
	r1.resolveRealStartOnce()
	if got := r1.RealStart(); got != 598.343 || probes != 1 {
		t.Fatalf("first run resolves once: got %v after %d probes", got, probes)
	}
	if v, ok := m.ResolvedStart(runKey(dir, 600)); !ok || v != 598.343 {
		t.Fatalf("the manager must remember what the run reported: %v %v", v, ok)
	}

	// The run object is gone (reaped); the next one is preset and must not
	// probe — the stub would now fail and fall back to 600.
	probeRunStart = func(context.Context, string, string, float64) (float64, error) {
		probes++
		return 0, errors.New("source went cold")
	}
	m.mu.Lock()
	r2 := m.newRunLocked(key, dir, 600, "http://src/x.mkv", h)
	m.mu.Unlock()
	r2.resolveRealStartOnce()
	if got := r2.RealStart(); got != 598.343 {
		t.Fatalf("the re-created run must report the remembered start, got %v", got)
	}
	if probes != 1 {
		t.Fatalf("a preset run must not probe, got %d probes", probes)
	}

	// And a session whose run is momentarily absent answers from the same
	// memory rather than the quantized seek.
	s := NewSession(SessionConfig{ID: "s", HashDir: dir, RunMgr: m})
	s.seekTime = 600
	if got := s.RunStart(); got != 598.343 {
		t.Fatalf("session fallback must use the remembered start, got %v", got)
	}
}
