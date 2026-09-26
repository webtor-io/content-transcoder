package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session URL names "segment N of whatever run the session points at now",
// and a seek swaps the run under it. These tests play the browser against the
// real router: keep what a response said, revalidate with it the way Chrome
// does for a no-cache response, and look at the bytes the player ends up with
// (on a 304 those are the cached ones).

// cachedCopy is what the browser's cache holds of one segment URL.
type cachedCopy struct {
	body    string
	etag    string
	lastMod string
}

// browserGet fetches url the way Chrome does for a no-cache response it has
// cached: with If-None-Match when the copy has an ETag, If-Modified-Since
// when it has a Last-Modified, and on 304 the page gets the cached bytes.
// Returns the status and what the cache holds afterwards (= what the player
// gets).
func browserGet(t *testing.T, url string, cached *cachedCopy, extra ...string) (int, cachedCopy) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cached != nil {
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
		if cached.lastMod != "" {
			req.Header.Set("If-Modified-Since", cached.lastMod)
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusNotModified {
		if cached == nil {
			t.Fatalf("GET %s: 304 to an unconditional request", url)
		}
		return resp.StatusCode, *cached
	}
	return resp.StatusCode, cachedCopy{
		body:    string(body),
		etag:    resp.Header.Get("ETag"),
		lastMod: resp.Header.Get("Last-Modified"),
	}
}

// newSegmentServer serves the real session router over a run manager the
// test fills with finished runs (no FFmpeg).
func newSegmentServer(t *testing.T) (srv *httptest.Server, sm *SessionManager, runMgr *RunManager, hashDir string) {
	t.Helper()
	hashDir = filepath.Join(t.TempDir(), "hash")
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		t.Fatal(err)
	}
	runMgr = NewRunManager()
	sm = NewSessionManager(runMgr)
	web := &Web{sessionManager: sm, touchMap: NewTouchMap()}
	web.buildHandler()
	srv = httptest.NewServer(web.handler)
	t.Cleanup(func() {
		srv.Close()
		sm.CloseAll()
		runMgr.CloseAll()
	})
	return srv, sm, runMgr, hashDir
}

// addFinishedRun registers a run at seek whose FFmpeg already walked the
// source, with files written at mtime, the way a run left by an earlier
// viewer sits in the pod until its grace period ends.
func addFinishedRun(t *testing.T, m *RunManager, hashDir string, seek float64, mtime time.Time, files map[string]string) *TranscodeRun {
	t.Helper()
	key := runKey(hashDir, seek)
	m.mu.Lock()
	run := m.newRunLocked(key, hashDir, seek, "", nil)
	run.completed = true
	m.runs[key] = &managedRun{run: run}
	m.mu.Unlock()
	if err := os.MkdirAll(run.OutputDir(), 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		writeFileAt(t, filepath.Join(run.OutputDir(), name), body, mtime)
	}
	return run
}

// bodyEnd is the end of a body, for messages: it tells the runs' files
// apart (each is filled with its own letter, cues end in their own text).
func bodyEnd(body string) string {
	if len(body) <= 5 {
		return body
	}
	return body[len(body)-5:]
}

func writeFileAt(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// The 2026-09-26 reproduction (Chrome 154, 3 of 3): a seek to 20:00 landed on
// a run of 20:00 left by a viewer 1-4 minutes earlier, so its files were
// OLDER than the ones the browser held from the run at 0:00. Revalidated by
// date, every segment came back 304 and the player played 0:00-0:16 as
// 19:30-19:46 (v0-1040-0: 60,912 cached bytes instead of 5,065,848).
func TestSessionSegment_SeekIntoOlderRun_PlayerGetsThatRunsBytes(t *testing.T) {
	srv, sm, runMgr, hashDir := newSegmentServer(t)
	now := time.Now().Truncate(time.Second)

	// Sizes as they come: video differs, audio and subtitles can match to the
	// byte (188-byte TS packets of steady AAC; cue files of equal length),
	// and a validator made of the size alone would pass them off as equal.
	atZero := map[string]string{
		"v0-1040-0.ts": strings.Repeat("A", 609),
		"a0-0.ts":      strings.Repeat("a", 1128),
		"s0-0.vtt":     "WEBVTT\n\n00:00.000 --> 00:01.000\nzero\n",
	}
	atTwenty := map[string]string{
		"v0-1040-0.ts": strings.Repeat("B", 5065),
		"a0-0.ts":      strings.Repeat("b", 1128),
		"s0-0.vtt":     "WEBVTT\n\n00:00.000 --> 00:01.000\n20m!\n",
	}
	addFinishedRun(t, runMgr, hashDir, 1200, now.Add(-3*time.Minute), atTwenty)
	addFinishedRun(t, runMgr, hashDir, 0, now, atZero)

	sess := sm.Create(SessionConfig{SourceURL: "http://source/film.mkv", HashDir: hashDir})
	if err := sess.Start(0); err != nil {
		t.Fatal(err)
	}
	url := func(name string) string { return srv.URL + "/session/" + sess.id + "/" + name + "?token=t" }

	cache := map[string]cachedCopy{}
	for name, want := range atZero {
		status, got := browserGet(t, url(name), nil)
		if status != http.StatusOK || got.body != want {
			t.Fatalf("%s at 0:00: status %d, %d bytes; want 200 with run 0:00's %d", name, status, len(got.body), len(want))
		}
		if got.etag == "" && got.lastMod == "" {
			t.Fatalf("%s: no validator at all, every revisit is a full download", name)
		}
		// Same run, nothing changed: the cheap answer stays.
		if status, _ := browserGet(t, url(name), &got); status != http.StatusNotModified {
			t.Errorf("%s: revalidation within the same run: status %d, want 304", name, status)
		}
		cache[name] = got
	}

	if err := sess.Seek(1200); err != nil {
		t.Fatal(err)
	}
	for name, want := range atTwenty {
		held := cache[name]
		status, got := browserGet(t, url(name), &held)
		if got.body != want {
			t.Errorf("%s after the seek to 20:00: status %d, player got %d bytes ending %q; want run 20:00's %d bytes",
				name, status, len(got.body), bodyEnd(got.body), len(want))
			continue
		}
		cache[name] = got

		// A copy that carries only a date (cached before this validator
		// existed, or by any client that speaks only dates) must not be
		// confirmed by one either.
		dateOnly := cachedCopy{body: atZero[name], lastMod: now.UTC().Format(http.TimeFormat)}
		if _, got := browserGet(t, url(name), &dateOnly); got.body != want {
			t.Errorf("%s after the seek, revalidated by date alone: player got %d bytes; want run 20:00's %d",
				name, len(got.body), len(want))
		}

		// Within run 20:00 the cheap answer is back.
		if status, _ := browserGet(t, url(name), &got); status != http.StatusNotModified {
			t.Errorf("%s: revalidation within run 20:00: status %d, want 304", name, status)
		}
	}

	// And back to 0:00, whose run is still there: the browser holds 20:00.
	if err := sess.Seek(0); err != nil {
		t.Fatal(err)
	}
	for name, want := range atZero {
		held := cache[name]
		if _, got := browserGet(t, url(name), &held); got.body != want {
			t.Errorf("%s after seeking back to 0:00: player got %d bytes; want run 0:00's %d", name, len(got.body), len(want))
		}
	}
}

// hls.js only asks for segments the playlist lists, which are closed, but a
// segment request is answered as soon as the file is non-empty. A copy taken
// while FFmpeg was still writing, revalidated within the same second, was
// confirmed by date and the player kept the truncated segment.
func TestSessionSegment_GrowingSegmentIsNot304(t *testing.T) {
	srv, sm, runMgr, hashDir := newSegmentServer(t)
	sec := time.Now().Truncate(time.Second)
	run := addFinishedRun(t, runMgr, hashDir, 0, sec.Add(100*time.Millisecond), map[string]string{
		"v0-720-3.ts": strings.Repeat("x", 376),
	})
	sess := sm.Create(SessionConfig{SourceURL: "http://source/film.mkv", HashDir: hashDir})
	if err := sess.Start(0); err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/session/" + sess.id + "/v0-720-3.ts"

	_, partial := browserGet(t, url, nil)
	if len(partial.body) != 376 {
		t.Fatalf("first read: %d bytes, want 376", len(partial.body))
	}
	whole := strings.Repeat("x", 376) + strings.Repeat("y", 376)
	writeFileAt(t, filepath.Join(run.OutputDir(), "v0-720-3.ts"), whole, sec.Add(600*time.Millisecond))

	status, got := browserGet(t, url, &partial)
	if got.body != whole {
		t.Fatalf("revalidating a partial copy: status %d, player got %d bytes; want the whole %d", status, len(got.body), len(whole))
	}
	if status, _ := browserGet(t, url, &got); status != http.StatusNotModified {
		t.Errorf("revalidating the whole segment: status %d, want 304", status)
	}
}

// An auto-restart of a run starts a new FFmpeg process in the same dir, and
// it writes the segments again under the same names: bytes that need not be
// the old ones even where the size (and the second) is the same.
func TestSessionSegment_NewProcessOfTheSameRunIsNot304(t *testing.T) {
	srv, sm, runMgr, hashDir := newSegmentServer(t)
	mtime := time.Now().Truncate(time.Second)
	run := addFinishedRun(t, runMgr, hashDir, 0, mtime, map[string]string{
		"a0-7.ts": strings.Repeat("o", 940),
	})
	sess := sm.Create(SessionConfig{SourceURL: "http://source/film.mkv", HashDir: hashDir})
	if err := sess.Start(0); err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/session/" + sess.id + "/a0-7.ts"
	_, old := browserGet(t, url, nil)

	startFakeProcess(t, run, "true")
	<-run.done
	rewritten := strings.Repeat("n", 940)
	writeFileAt(t, filepath.Join(run.OutputDir(), "a0-7.ts"), rewritten, mtime)

	status, got := browserGet(t, url, &old)
	if got.body != rewritten {
		t.Fatalf("after a new process of the run: status %d, player got bytes ending %q; want the new process's bytes", status, bodyEnd(got.body))
	}
}

// A resumed download (Range + If-Range) splices the cached head with the
// range it asks for. The splice is allowed within one run and must not reach
// across a seek: with a validator that does not name the run, two segments
// of equal size would splice run 0:00's head onto run 20:00's tail. If-Range
// is compared strongly, so the ETag has to be strong for a resume to work at
// all.
func TestSessionSegment_ResumeIsSplicedOnlyWithinTheRun(t *testing.T) {
	srv, sm, runMgr, hashDir := newSegmentServer(t)
	now := time.Now().Truncate(time.Second)
	addFinishedRun(t, runMgr, hashDir, 1200, now.Add(-3*time.Minute), map[string]string{"v0-1040-0.ts": strings.Repeat("B", 2000)})
	addFinishedRun(t, runMgr, hashDir, 0, now, map[string]string{"v0-1040-0.ts": strings.Repeat("A", 2000)})
	sess := sm.Create(SessionConfig{SourceURL: "http://source/film.mkv", HashDir: hashDir})
	if err := sess.Start(0); err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/session/" + sess.id + "/v0-1040-0.ts"
	_, initial := browserGet(t, url, nil)
	validator := initial.etag
	if validator == "" {
		validator = initial.lastMod
	}

	status, got := browserGet(t, url, nil, "Range", "bytes=1000-", "If-Range", validator)
	if status != http.StatusPartialContent || got.body != strings.Repeat("A", 1000) {
		t.Errorf("resume within the run: status %d, %d bytes; want 206 with the last 1000", status, len(got.body))
	}

	if err := sess.Seek(1200); err != nil {
		t.Fatal(err)
	}
	status, got = browserGet(t, url, nil, "Range", "bytes=1000-", "If-Range", validator)
	if status != http.StatusOK || got.body != strings.Repeat("B", 2000) {
		t.Errorf("resume across the seek: status %d, %d bytes ending %q; want 200 with run 20:00's whole segment", status, len(got.body), bodyEnd(got.body))
	}
}

// Playlists are built per request and go out with no validator, so no
// conditional request can come back 304, across a seek or otherwise. A guard:
// adding Last-Modified or an ETag here would reopen the hole segments had.
func TestSessionPlaylists_CarryNoValidator(t *testing.T) {
	srv, sm, runMgr, hashDir := newSegmentServer(t)
	now := time.Now().Truncate(time.Second)
	variant := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.0,\n%s-0.ts\n"
	addFinishedRun(t, runMgr, hashDir, 1200, now.Add(-3*time.Minute), map[string]string{
		"v0-1040.m3u8.ffmpeg": strings.ReplaceAll(variant, "%s", "v0-1040"),
		"a0.m3u8.ffmpeg":      strings.ReplaceAll(variant, "%s", "a0"),
		"s0.m3u8.ffmpeg":      strings.ReplaceAll(strings.ReplaceAll(variant, "%s-0.ts", "s0-0.vtt"), "%s", "s0"),
	})
	addFinishedRun(t, runMgr, hashDir, 0, now, map[string]string{
		"v0-1040.m3u8.ffmpeg": strings.ReplaceAll(variant, "%s", "v0-1040"),
		"a0.m3u8.ffmpeg":      strings.ReplaceAll(variant, "%s", "a0"),
		"s0.m3u8.ffmpeg":      strings.ReplaceAll(strings.ReplaceAll(variant, "%s-0.ts", "s0-0.vtt"), "%s", "s0"),
	})
	sess := sm.Create(SessionConfig{SourceURL: "http://source/film.mkv", HashDir: hashDir})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(sess.outputDir, "index.m3u8"), "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv0-1040.m3u8\n", now)
	if err := sess.Start(0); err != nil {
		t.Fatal(err)
	}
	if err := sess.Seek(1200); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.m3u8", "v0-1040.m3u8", "a0.m3u8", "s0.m3u8"} {
		// A date later than anything on disk: a handler that validated by
		// date at all would confirm the copy.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/session/"+sess.id+"/"+name, nil)
		req.Header.Set("If-Modified-Since", now.Add(time.Hour).UTC().Format(http.TimeFormat))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, resp.StatusCode)
		}
		if !strings.Contains(string(body), "#EXT-X-SESSION-OFFSET:1200.000") {
			t.Errorf("%s: not run 20:00's playlist:\n%s", name, body)
		}
		for _, h := range []string{"ETag", "Last-Modified"} {
			if v := resp.Header.Get(h); v != "" {
				t.Errorf("%s: %s %q on a playlist", name, h, v)
			}
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", name, cc)
		}
	}
}
