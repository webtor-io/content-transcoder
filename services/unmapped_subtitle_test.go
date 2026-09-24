package services

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cp "github.com/webtor-io/content-prober/content-prober"
	"github.com/webtor-io/lazymap"
)

// A subtitle track with no FFmpeg output (see textSubtitleCodecs) is still
// in the master playlist, so a player may ask for it -- hls.js re-polls a
// segment-less live playlist every few seconds. Each poll used to go
// through WaitForPlaylist: a 5 s wait while the run is going and a
// playlist_waits_total{kind=subtitle} tick every time, and one viewer with
// such a track selected is ~900 ticks per 30 min, over the
// TranscoderSessionsStuck threshold of 60. It gets the empty playlist at
// once and is not counted.
func TestSessionPlaylistHandler_UnmappedSubtitleAnswersAtOnce(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "subtitle", "dvd_subtitle"))
	sess := NewSession(SessionConfig{ID: "unmapped", HashDir: dir, HLS: h, RunMgr: runMgr})
	// A finished copy run: EnsureRunning leaves it alone, so the only way
	// to the stub is the wait path this test guards against.
	sess.run = newCompletedRun(t, dir)

	waits := func() float64 {
		sum := 0.0
		for _, o := range []string{playlistWaitOK, playlistWaitTimeout, playlistWaitNotRunning, playlistWaitCanceled} {
			sum += counter(t, metricPlaylistWaitsTotal.WithLabelValues(o, playlistKindSubtitle))
		}
		return sum
	}
	before := waits()
	started := time.Now()
	w := httptest.NewRecorder()
	(&Web{}).sessionPlaylistHandler(w, httptest.NewRequest(http.MethodGet, "/session/unmapped/s0.m3u8", nil), sess, "s0.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "#EXT-X-SESSION-OFFSET:") || strings.Contains(w.Body.String(), "#EXTINF") {
		t.Errorf("want the empty subtitle playlist, got:\n%s", w.Body.String())
	}
	if d := time.Since(started); d > time.Second {
		t.Errorf("answered in %v, want at once", d)
	}
	if got := waits() - before; got != 0 {
		t.Errorf("playlist_waits_total{kind=subtitle} moved by %v for a track with no output", got)
	}
}

// Negative control: a mapped text track still goes through the wait (and
// is counted), so the early answer above is specific to unmapped tracks.
func TestSessionPlaylistHandler_MappedSubtitleStillWaits(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "subtitle", "subrip"))
	sess := NewSession(SessionConfig{ID: "mapped", HashDir: dir, HLS: h, RunMgr: runMgr})
	sess.run = newCompletedRun(t, dir)
	before := counter(t, metricPlaylistWaitsTotal.WithLabelValues(playlistWaitNotRunning, playlistKindSubtitle))
	w := httptest.NewRecorder()
	(&Web{}).sessionPlaylistHandler(w, httptest.NewRequest(http.MethodGet, "/session/mapped/s0.m3u8", nil), sess, "s0.m3u8")
	if got := counter(t, metricPlaylistWaitsTotal.WithLabelValues(playlistWaitNotRunning, playlistKindSubtitle)) - before; got != 1 {
		t.Errorf("mapped track wait not counted: delta %v", got)
	}
}

// A source with neither video nor audio is refused before a session exists,
// on both routes: the legacy GET /index.m3u8 opens a session without
// starting FFmpeg, and used to answer 200 with a master that had no
// variant.
func TestOpenSession_NoPlayableStreams(t *testing.T) {
	out := t.TempDir()
	src := "http://source/abc/book.pdf"
	u, _ := url.Parse(src)
	sum := sha1.Sum([]byte(u.Path))
	hashDir := filepath.Join(out, hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		t.Fatal(err)
	}
	probe, _ := json.Marshal(&cp.ProbeReply{Streams: []*cp.Stream{{Index: 0, CodecType: "attachment"}}})
	if err := os.WriteFile(filepath.Join(hashDir, "index.json"), probe, 0644); err != nil {
		t.Fatal(err)
	}
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sm := NewSessionManager(runMgr)
	web := &Web{
		output:         out,
		contentProbe:   &ContentProbe{LazyMap: lazymap.New[*cp.ProbeReply](&lazymap.Config{})},
		hlsBuilder:     &HLSBuilder{},
		sessionManager: sm,
		touchMap:       NewTouchMap(),
	}
	for _, start := range []bool{false, true} {
		sess, code, msg := web.openSession(src, start)
		if sess != nil || code != http.StatusUnsupportedMediaType || msg != ErrNoPlayableStreams.Error() {
			t.Errorf("start=%v: got session=%v code=%d msg=%q, want 415 %q", start, sess != nil, code, msg, ErrNoPlayableStreams.Error())
		}
	}
	sm.mu.Lock()
	n := len(sm.sessions)
	sm.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions created for a refused source", n)
	}
}
