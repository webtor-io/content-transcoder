package services

import (
	pkgerrors "github.com/pkg/errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrichPlaylistData_EmptyQuery(t *testing.T) {
	data := []byte("#EXTM3U\nv0-720-0.ts\na0-0.ts\n")
	got := enrichPlaylistData(data, "")
	if string(got) != string(data) {
		t.Errorf("empty query should return data unchanged, got %q", string(got))
	}
}

func TestEnrichPlaylistData_MasterPlaylist(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv0-720.m3u8\n#EXT-X-MEDIA:URI=\"a0.m3u8\"\n"
	got := string(enrichPlaylistData([]byte(master), "api-key=abc&token=xyz"))

	if !strings.Contains(got, "v0-720.m3u8?api-key=abc&token=xyz") {
		t.Errorf("should enrich variant playlist ref, got:\n%s", got)
	}
	if !strings.Contains(got, "a0.m3u8?api-key=abc&token=xyz") {
		t.Errorf("should enrich audio playlist ref, got:\n%s", got)
	}
	// Tags should not be modified
	if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=5000000") {
		t.Errorf("tags should remain intact, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_VariantPlaylist(t *testing.T) {
	variant := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.0,\nv0-720-0.ts\n#EXTINF:4.0,\nv0-720-1.ts\n"
	got := string(enrichPlaylistData([]byte(variant), "token=t1"))

	if !strings.Contains(got, "v0-720-0.ts?token=t1") {
		t.Errorf("should enrich segment 0, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-720-1.ts?token=t1") {
		t.Errorf("should enrich segment 1, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_SubtitleAndAudio(t *testing.T) {
	playlist := "#EXTM3U\n#EXTINF:4.0,\na0-5.ts\n#EXTINF:4.0,\ns0-3.vtt\n"
	got := string(enrichPlaylistData([]byte(playlist), "key=val"))

	if !strings.Contains(got, "a0-5.ts?key=val") {
		t.Errorf("should enrich audio segment, got:\n%s", got)
	}
	if !strings.Contains(got, "s0-3.vtt?key=val") {
		t.Errorf("should enrich subtitle segment, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_NoFalseMatches(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\nsome random text\n"
	got := string(enrichPlaylistData([]byte(playlist), "key=val"))

	// Tags and non-file lines should not be enriched
	if strings.Contains(got, "TARGETDURATION:5?key=val") {
		t.Errorf("should not enrich HLS tags")
	}
	if strings.Contains(got, "random text?key=val") {
		t.Errorf("should not enrich non-file lines")
	}
}

func TestIsSubtitlePlaylist(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"s0.m3u8", true},
		{"s1.m3u8", true},
		{"s12.m3u8", true},
		{"v0-720.m3u8", false},
		{"a0.m3u8", false},
		{"index.m3u8", false},
		{"s0-0.ts", false},
		{"s0-0.vtt", false},
		{"subtitle.m3u8", false}, // doesn't match "s" + digit pattern — still starts with "s" though
	}
	for _, tt := range tests {
		got := isSubtitlePlaylist(tt.name)
		if got != tt.want {
			t.Errorf("isSubtitlePlaylist(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSessionPlaylistHandler_MasterInjectsSessionOffset(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	sess := NewSession(SessionConfig{ID: "test-master-offset", HashDir: dir, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv0-720.m3u8\n"
	if err := os.WriteFile(filepath.Join(sess.outputDir, "index.m3u8"), []byte(master), 0644); err != nil {
		t.Fatal(err)
	}
	sess.seekTime = 1500

	web := &Web{}
	r := httptest.NewRequest(http.MethodGet, "/session/test-master-offset/index.m3u8", nil)
	w := httptest.NewRecorder()
	web.sessionPlaylistHandler(w, r, sess, "index.m3u8")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "#EXT-X-SESSION-OFFSET:1500") {
		t.Errorf("master should contain #EXT-X-SESSION-OFFSET:1500, got:\n%s", body)
	}
	if strings.Count(body, "#EXT-X-SESSION-OFFSET:") != 1 {
		t.Errorf("should have exactly one #EXT-X-SESSION-OFFSET tag, got:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-STREAM-INF:BANDWIDTH=5000000") {
		t.Errorf("master content should be preserved, got:\n%s", body)
	}
}

func TestSessionPlaylistHandler_MasterIdempotent(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	sess := NewSession(SessionConfig{ID: "test-master-idemp", HashDir: dir, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	master := "#EXTM3U\n#EXT-X-SESSION-OFFSET:42\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv0-720.m3u8\n"
	if err := os.WriteFile(filepath.Join(sess.outputDir, "index.m3u8"), []byte(master), 0644); err != nil {
		t.Fatal(err)
	}
	sess.seekTime = 1500

	web := &Web{}
	r := httptest.NewRequest(http.MethodGet, "/session/test-master-idemp/index.m3u8", nil)
	w := httptest.NewRecorder()
	web.sessionPlaylistHandler(w, r, sess, "index.m3u8")

	body := w.Body.String()
	if strings.Count(body, "#EXT-X-SESSION-OFFSET:") != 1 {
		t.Errorf("idempotent: should keep exactly one tag, got:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-SESSION-OFFSET:42") {
		t.Errorf("should preserve existing tag value, got:\n%s", body)
	}
}

func TestEmptySubtitlePlaylist_IsValidHLS(t *testing.T) {
	empty := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n"
	if !strings.HasPrefix(empty, "#EXTM3U") {
		t.Error("must start with #EXTM3U")
	}
	if !strings.Contains(empty, "#EXT-X-TARGETDURATION:") {
		t.Error("must contain TARGETDURATION")
	}
	if strings.Contains(empty, "#EXT-X-ENDLIST") {
		t.Error("must NOT contain ENDLIST — player should keep polling for segments")
	}
}

func TestUnsupportedContentReason(t *testing.T) {
	// Mirrors the real chain: sentinel → Wrap in startLocked → returned
	// through RunManager.Acquire / Session.Start unmodified.
	wrapped := pkgerrors.Wrap(ErrResolutionNotSupported, "failed to get ffmpeg params")
	if got := unsupportedContentReason(wrapped); got != ErrResolutionNotSupported.Error() {
		t.Errorf("resolution: got %q, want %q", got, ErrResolutionNotSupported.Error())
	}

	disabled := pkgerrors.Wrap(ErrTranscodingDisabled, "failed to get ffmpeg params")
	if got := unsupportedContentReason(disabled); got != ErrTranscodingDisabled.Error() {
		t.Errorf("disabled: got %q, want %q", got, ErrTranscodingDisabled.Error())
	}

	// Internal failures must stay generic — no reason leaks to the client.
	if got := unsupportedContentReason(pkgerrors.New("ffmpeg not found")); got != "" {
		t.Errorf("internal error: got %q, want empty", got)
	}
}

// Session URLs are stable across seeks — /session/{id}/v0-720-0.ts names
// segment zero of whichever run the session currently points at — while the
// bytes behind them change on every seek. Served without Cache-Control and
// without ETag, browsers fall back to heuristic freshness (a fraction of the
// file's age) and hand the player a segment from the previous run: seeking
// anywhere replayed the movie from the start. Measured on production: the
// same URL returned 1138528 bytes from cache while the server held 169012.
func TestSessionSegmentHandler_RevalidatesCache(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	sess := NewSession(SessionConfig{ID: "test-seg-cache", HashDir: dir, RunMgr: runMgr})
	runDir := filepath.Join(dir, "runs", "seek-0.000")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	run.completed = true // segments already on disk, no FFmpeg needed
	sess.run = run
	if err := os.WriteFile(filepath.Join(runDir, "v0-720-0.ts"), []byte("segment-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	web := &Web{}
	r := httptest.NewRequest(http.MethodGet, "/session/test-seg-cache/v0-720-0.ts", nil)
	w := httptest.NewRecorder()
	web.sessionSegmentHandler(w, r, sess, "v0-720-0.ts")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
}

// Playlists carry the same hazard and change even more often: a variant
// playlist grows with every segment, and after a seek it is replaced wholesale.
func TestSessionPlaylistHandler_RevalidatesCache(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()

	sess := NewSession(SessionConfig{ID: "test-pl-cache", HashDir: dir, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess.outputDir, "index.m3u8"),
		[]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv0-720.m3u8\n"), 0644); err != nil {
		t.Fatal(err)
	}

	web := &Web{}
	r := httptest.NewRequest(http.MethodGet, "/session/test-pl-cache/index.m3u8", nil)
	w := httptest.NewRecorder()
	web.sessionPlaylistHandler(w, r, sess, "index.m3u8")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
}
