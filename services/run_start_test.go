package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
	cp "github.com/webtor-io/content-prober/content-prober"
)

func TestParseFirstPacketTime(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want float64
		err  bool
	}{
		{"pts and dts, dts earlier", "595.567000,595.467000\n", 595.467, false},
		{"pts only (dts N/A)", "595.567000,N/A\n", 595.567, false},
		{"side data lines are skipped", "side_data,\n1495.500000,1495.500000\n", 1495.5, false},
		{"empty output", "\n", 0, true},
		{"garbage", "N/A,N/A\n", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFirstPacketTime([]byte(c.out))
			if c.err != (err != nil) || (!c.err && got != c.want) {
				t.Fatalf("got %v, %v; want %v, err=%v", got, err, c.want, c.err)
			}
		})
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

	probeRunStart = func(context.Context, string, string, float64) (float64, error) { return 598.343, nil }
	if got := run.resolveRealStart(); got != 598.343 {
		t.Fatalf("keyframe: %v", got)
	}
	probeRunStart = func(context.Context, string, string, float64) (float64, error) { return 0, errors.New("boom") }
	if got := run.resolveRealStart(); got != 600 {
		t.Fatalf("error falls back: %v", got)
	}
	probeRunStart = func(context.Context, string, string, float64) (float64, error) { return 601, nil }
	if got := run.resolveRealStart(); got != 600 {
		t.Fatalf("after the seek point falls back: %v", got)
	}
	probeRunStart = func(context.Context, string, string, float64) (float64, error) { return 500, nil }
	if got := run.resolveRealStart(); got != 600 {
		t.Fatalf("implausibly early falls back: %v", got)
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
	run.resolveRealStart()
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
	if v, ok := m.ResolvedStart(dir, 600); !ok || v != 598.343 {
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
