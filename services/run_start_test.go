package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
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

	probeRunStart = func(context.Context, string, float64) (float64, error) { return 598.343, nil }
	if got := run.resolveRealStart(); got != 598.343 {
		t.Fatalf("keyframe: %v", got)
	}
	probeRunStart = func(context.Context, string, float64) (float64, error) { return 0, errors.New("boom") }
	if got := run.resolveRealStart(); got != 600 {
		t.Fatalf("error falls back: %v", got)
	}
	probeRunStart = func(context.Context, string, float64) (float64, error) { return 601, nil }
	if got := run.resolveRealStart(); got != 600 {
		t.Fatalf("after the seek point falls back: %v", got)
	}
	probeRunStart = func(context.Context, string, float64) (float64, error) { return 500, nil }
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
	if got := run.RealStart(); got != 598.343 {
		t.Fatalf("resolved: %v", got)
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
