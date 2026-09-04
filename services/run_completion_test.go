package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCompletedRun builds a run in the state FFmpeg leaves behind after it has
// walked the source to its end and exited cleanly.
func newCompletedRun(t *testing.T, dir string) *TranscodeRun {
	t.Helper()
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	run.completed = true
	return run
}

// A run that reached the end of the source has already written every segment
// it will ever write. Stripping ENDLIST there leaves the playlist looking live
// forever: hls.js keeps chasing a live edge that never settles and never
// starts playback. This is what a 22h audiobook hit — FFmpeg copied it in 5.5
// minutes, then the playlist was restarted from zero over and over.
func TestPlaylistForStreamKeepsEndlistOnCompletedRun(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "completed", HashDir: dir, RunMgr: runMgr})
	runDir := filepath.Join(dir, "runs", "seek-0.000")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	s.run = newCompletedRun(t, dir)

	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\na0-0.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(runDir, "a0.m3u8.ffmpeg"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := s.PlaylistForStream("a0.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	res := string(got)

	if !strings.Contains(res, "#EXT-X-ENDLIST") {
		t.Errorf("completed run must keep #EXT-X-ENDLIST, got:\n%s", res)
	}
	if !strings.Contains(res, "a0-0.ts") {
		t.Error("segment reference should remain")
	}
}

// Negative control for the above: while FFmpeg is still producing, ENDLIST
// must still be stripped so the player keeps polling for new segments.
func TestPlaylistForStreamStripsEndlistOnRunningRun(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "running", HashDir: dir, RunMgr: runMgr})
	runDir := filepath.Join(dir, "runs", "seek-0.000")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	s.run = run

	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\na0-0.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(runDir, "a0.m3u8.ffmpeg"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := s.PlaylistForStream("a0.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "#EXT-X-ENDLIST") {
		t.Errorf("unfinished run must not expose ENDLIST, got:\n%s", got)
	}
}

// EnsureRunning cannot read "FFmpeg is not running" as "FFmpeg died": a run
// that completed is done, and restarting it truncates the playlist back to
// zero. Restarting also burns the restart budget, ending in 503
// "transcoder restart limit reached" after maxConsecutiveRestarts.
func TestEnsureRunningSkipsCompletedRun(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "no-restart", HashDir: dir, RunMgr: runMgr})
	s.run = newCompletedRun(t, dir)

	for i := 0; i < maxConsecutiveRestarts+2; i++ {
		if err := s.EnsureRunning(); err != nil {
			t.Fatalf("attempt %d: unexpected error %v", i, err)
		}
	}
	if s.restartFails != 0 {
		t.Errorf("completed run must not consume the restart budget, got %d", s.restartFails)
	}
	if s.run == nil || !s.run.IsCompleted() {
		t.Error("completed run should be kept as-is")
	}
}

// Same rule for the segment path: a missing segment behind a completed run is
// genuinely missing, not a symptom of a dead FFmpeg.
func TestRestartForSegmentSkipsCompletedRun(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "no-restart-seg", HashDir: dir, RunMgr: runMgr})
	s.run = newCompletedRun(t, dir)

	if err := s.RestartForSegment(3); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if s.restartFails != 0 {
		t.Errorf("completed run must not consume the restart budget, got %d", s.restartFails)
	}
}

// A second viewer arriving after the run finished must reuse the segments on
// disk. Restarting for them wipes the playlist under the first viewer.
func TestAcquireReusesCompletedRunWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewRunManager()
	defer m.CloseAll()

	run := newCompletedRun(t, dir)
	m.runs[runKey(dir, 0)] = &managedRun{run: run}

	got, err := m.Acquire(dir, 0, "", nil)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got != run {
		t.Fatal("should reuse the existing run")
	}
	if got.IsRunning() {
		t.Error("completed run must not be restarted on acquire")
	}
}
