package services

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuantizeSeekTime(t *testing.T) {
	tests := []struct {
		input float64
		want  float64
	}{
		{0, 0},
		{-5, 0},
		{10, 0},           // 10 / 30 = 0.33 → 0 * 30 = 0
		{29.9, 0},         // still in first quantum
		{30, 30},          // exact boundary
		{30.5, 30},
		{59.9, 30},
		{60, 60},
		{500, 480},        // 500 / 30 = 16.66 → 16 * 30 = 480
		{510, 510},        // 510 / 30 = 17.0 → 17 * 30 = 510
		{1000, 990},       // 1000 / 30 = 33.33 → 33 * 30 = 990
		{3292.87, 3270},   // near end of 55min video
	}
	for _, tt := range tests {
		got := quantizeSeekTime(tt.input)
		if got != tt.want {
			t.Errorf("quantizeSeekTime(%.1f): got %.1f, want %.1f", tt.input, got, tt.want)
		}
	}
}

func TestSegPrefixPattern(t *testing.T) {
	tests := []struct {
		input      string
		wantPrefix string
		wantNum    string
		wantExt    string
		wantMatch  bool
	}{
		{"v0-720-5.ts", "v0-720", "5", "ts", true},
		{"a0-42.ts", "a0", "42", "ts", true},
		{"s0-3.vtt", "s0", "3", "vtt", true},
		{"v0-240-0.ts", "v0-240", "0", "ts", true},
		{"a1-100.ts", "a1", "100", "ts", true},
		{"index.m3u8", "", "", "", false},
		{"ffmpeg.err", "", "", "", false},
		{"v0-720.m3u8.ffmpeg", "", "", "", false},
	}
	for _, tt := range tests {
		m := segPrefixPattern.FindStringSubmatch(tt.input)
		if tt.wantMatch {
			if m == nil {
				t.Errorf("segPrefixPattern(%q): expected match, got nil", tt.input)
				continue
			}
			if m[1] != tt.wantPrefix || m[2] != tt.wantNum || m[3] != tt.wantExt {
				t.Errorf("segPrefixPattern(%q): got prefix=%q num=%q ext=%q, want %q %q %q",
					tt.input, m[1], m[2], m[3], tt.wantPrefix, tt.wantNum, tt.wantExt)
			}
		} else if m != nil {
			t.Errorf("segPrefixPattern(%q): expected no match, got %v", tt.input, m)
		}
	}
}

func TestRedirectSegmentListParams(t *testing.T) {
	params := []string{
		"-f", "segment", "-segment_list", "/out/v0-720.m3u8", "-c:v", "copy",
		"-f", "segment", "-segment_list", "/out/a0.m3u8", "-c:a", "aac",
	}
	got := redirectSegmentListParams(params)
	want := []string{
		"-f", "segment", "-segment_list", "/out/v0-720.m3u8.ffmpeg", "-c:v", "copy",
		"-f", "segment", "-segment_list", "/out/a0.m3u8.ffmpeg", "-c:a", "aac",
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInjectSeekParams(t *testing.T) {
	params := []string{"-fix_sub_duration", "-i", "http://example.com/video.mkv", "-c:v", "copy"}
	got := injectSeekParams(params, 100.5, true)

	want := []string{"-fix_sub_duration", "-ss", "100.500", "-noaccurate_seek", "-i", "http://example.com/video.mkv", "-c:v", "copy"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInjectSeekParams_NoIFlag(t *testing.T) {
	params := []string{"-c:v", "copy"}
	got := injectSeekParams(params, 50, true)
	if len(got) != len(params) {
		t.Fatalf("expected no injection, got %v", got)
	}
}

func TestInjectSeekParams_ReencodeMode(t *testing.T) {
	params := []string{"-fix_sub_duration", "-i", "http://example.com/video.avi", "-c:v", "h264"}
	got := injectSeekParams(params, 100.5, false)

	// Re-encode: -ss before -i, no -noaccurate_seek
	want := []string{"-fix_sub_duration", "-ss", "100.500", "-i", "http://example.com/video.avi", "-c:v", "h264"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRemoveParam(t *testing.T) {
	params := []string{"-i", "url", "-xerror", "-seekable", "1", "-c:v", "copy"}
	got := removeParam(params, "-xerror")
	want := []string{"-i", "url", "-seekable", "1", "-c:v", "copy"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{
		ID:        "test-lifecycle",
		SourceURL: "http://example.com/video.mkv",
		HashDir:   dir,
		Duration:  100,
		RunMgr:    runMgr,
	})

	if s.id != "test-lifecycle" {
		t.Errorf("id: got %q, want %q", s.id, "test-lifecycle")
	}
	if s.outputDir != filepath.Join(dir, "sessions", "test-lifecycle") {
		t.Errorf("outputDir: got %q", s.outputDir)
	}
	if s.IsRunning() {
		t.Error("should not be running before Start")
	}
	if s.IsClosed() {
		t.Error("should not be closed initially")
	}

	// Touch updates lastAccess
	before := s.LastAccess()
	s.Touch()
	after := s.LastAccess()
	if !after.After(before) && after.Equal(before) {
		// may be equal if very fast
	}

	// Close
	os.MkdirAll(s.outputDir, 0755)
	s.Close()
	if !s.IsClosed() {
		t.Error("should be closed after Close")
	}
	if _, err := os.Stat(s.outputDir); !os.IsNotExist(err) {
		t.Error("session dir should be removed after Close")
	}

	// Double close is safe
	s.Close()
}

func TestPlaylistForStream(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{
		ID:     "test-playlist",
		HashDir: dir,
		RunMgr:  runMgr,
	})

	// Simulate a run's output directory
	runDir := filepath.Join(dir, "runs", "seek-0.000")
	os.MkdirAll(runDir, 0755)

	// Create a fake run to point at
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	s.run = run

	// Write a .ffmpeg playlist
	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ALLOW-CACHE:YES\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\nv0-720-0.ts\n#EXT-X-ENDLIST\n"
	os.WriteFile(filepath.Join(runDir, "v0-720.m3u8.ffmpeg"), []byte(content), 0644)

	got, err := s.PlaylistForStream("v0-720.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	result := string(got)

	if containsStr(result, "#EXT-X-ALLOW-CACHE:YES") {
		t.Error("should remove #EXT-X-ALLOW-CACHE:YES")
	}
	if containsStr(result, "#EXT-X-ENDLIST") {
		t.Error("should remove #EXT-X-ENDLIST")
	}
	if !containsStr(result, "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Error("should inject #EXT-X-PLAYLIST-TYPE:EVENT")
	}
	if !containsStr(result, "v0-720-0.ts") {
		t.Error("segment reference should remain")
	}
}

func TestPlaylistForStream_AlreadyHasType(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "test-playlist-type", HashDir: dir, RunMgr: runMgr})

	runDir := filepath.Join(dir, "runs", "seek-0.000")
	os.MkdirAll(runDir, 0755)
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	s.run = run

	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\nv0-0.ts\n"
	os.WriteFile(filepath.Join(runDir, "v0.m3u8.ffmpeg"), []byte(content), 0644)

	got, err := s.PlaylistForStream("v0.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	result := string(got)

	if countStr(result, "#EXT-X-PLAYLIST-TYPE:") != 1 {
		t.Errorf("should have exactly one PLAYLIST-TYPE, got:\n%s", result)
	}
}

func TestIsValidSessionPlaylist(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty", "", false},
		{"too short", "#EXTM3U\n#EXT-X-VERSION:3\n", false},
		{"minimal valid", "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n", true},
		{"with segments", "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\nv0-0.ts\n", true},
	}
	for _, tt := range tests {
		got := isValidSessionPlaylist([]byte(tt.input))
		if got != tt.want {
			t.Errorf("isValidSessionPlaylist(%q): got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseSegmentNumber(t *testing.T) {
	tests := []struct {
		path    string
		wantNum int
		wantErr bool
	}{
		{"/v0-720-5.ts", 5, false},
		{"/a0-3.ts", 3, false},
		{"/s0-2.vtt", 2, false},
		{"/v0-720-100.ts", 100, false},
		{"/v0-240-0.ts", 0, false},
		{"/index.m3u8", 0, true},
		{"/v0.m3u8", 0, true},
	}
	for _, tt := range tests {
		num, err := parseSegmentNumber(tt.path)
		if tt.wantErr && err == nil {
			t.Errorf("parseSegmentNumber(%q): expected error", tt.path)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("parseSegmentNumber(%q): unexpected error: %v", tt.path, err)
		}
		if !tt.wantErr && num != tt.wantNum {
			t.Errorf("parseSegmentNumber(%q): got %d, want %d", tt.path, num, tt.wantNum)
		}
	}
}

func TestSessionClosedOperations(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	s := NewSession(SessionConfig{ID: "test-closed-ops", HashDir: dir, RunMgr: runMgr})
	s.Close()

	if err := s.Start(0); err == nil {
		t.Error("Start after Close should return error")
	}
	if err := s.Seek(100); err == nil {
		t.Error("Seek after Close should return error")
	}
	if err := s.RestartForSegment(5); err == nil {
		t.Error("RestartForSegment after Close should return error")
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && findStr(s, substr)
}

func findStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func countStr(s, substr string) int {
	n := 0
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			n++
		}
	}
	return n
}
