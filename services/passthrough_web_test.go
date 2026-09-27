package services

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passthroughWebSession is a passthrough session behind the real router
// whose run has a live process (sleep): generation gen is running, its
// files are what the test writes into dir.
func passthroughWebSession(t *testing.T) (web *Web, sess *Session, run *TranscodeRun) {
	t.Helper()
	runMgr := NewRunManager()
	sm := NewSessionManager(runMgr)
	web = &Web{sessionManager: sm, touchMap: NewTouchMap()}
	web.buildHandler()
	hashDir := t.TempDir()
	h := hevcHLS(t, 1920, 1080, true, nil)
	f := sdrMain10()
	h.passFacts = &f
	sess = sm.Create(SessionConfig{HashDir: hashDir, HLS: h})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	run = newTranscodeRun(runKeyFor(hashDir, h, 0), hashDir, 0, "http://src/movie.mkv", h)
	if err := os.MkdirAll(run.OutputDir(), 0755); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, run, "sleep", "60")
	run.AddRef()
	sess.run, sess.started = run, true
	t.Cleanup(func() {
		run.Stop()
		sm.CloseAll()
		runMgr.CloseAll()
	})
	return web, sess, run
}

func get(web *Web, sess *Session, name string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/session/"+sess.id+"/"+name, nil))
	return w
}

func writeRunFile(t *testing.T, run *TranscodeRun, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(run.OutputDir(), name), body, 0644); err != nil {
		t.Fatal(err)
	}
}

// A passthrough session serves its fMP4 files as video/mp4 -- the old route
// has no .m4s or init, and whatever else ends in .mp4 is not found.
func TestPassthroughWeb_Routing(t *testing.T) {
	// Whatever the machine's mime tables say about .mp4 (the production
	// image has none, a Mac has Apache's): the type is set, not looked up.
	if err := mime.AddExtensionType(".mp4", "application/x-not-video"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mime.AddExtensionType(".mp4", "video/mp4") })
	web, sess, run := passthroughWebSession(t)
	gen := run.Generation()
	passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, fixtureInit(t, "main10-init.mp4"), 1000)
	writeRunFile(t, run, "a0-0.m4s", []byte("audio"))

	for _, c := range []struct {
		name string
		code int
		etag string
	}{
		{"v0-1080-0.m4s", 200, segmentETag(gen, 1000)},
		{"a0-0.m4s", 200, segmentETag(gen, 5)},
		{"v0-1080-init-" + gen + ".mp4", 200, segmentETag(gen, int64(len(fixtureInit(t, "main10-init.mp4"))))},
	} {
		w := get(web, sess, c.name)
		if w.Code != c.code || w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("ETag") != c.etag || w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q %q %q", c.name, w.Code, w.Header().Get("Content-Type"), w.Header().Get("ETag"), w.Header().Get("Cache-Control"))
		}
	}
	started := time.Now()
	for _, name := range []string{"a1-0.m4s", "x0-0.m4s", "v0-720-0.m4s", "movie.mp4", "v0-1080-init-zzzz.mp4", "a1-init-" + gen + ".mp4", "s0-init-" + gen + ".mp4"} {
		if w := get(web, sess, name); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, w.Code)
		}
	}
	if time.Since(started) > time.Second {
		t.Errorf("not-found names took %v: they must not wait", time.Since(started))
	}

	// The same names on an old-route session: not found, as before.
	old := hevcHLS(t, 1920, 1080, false, nil)
	sess.h = old
	for _, name := range []string{"v0-1080-0.m4s", "v0-1080-init-" + gen + ".mp4"} {
		if w := get(web, sess, name); w.Code != http.StatusNotFound {
			t.Errorf("old route %s: %d, want 404", name, w.Code)
		}
	}
}

// An init request is not a viewer at a segment: it moves no demand and
// restarts nothing -- not even when the generation in its name is all
// digits, which parseSegmentNumber would read as segment 123456789012345.
func TestPassthroughWeb_InitIsNotDemand(t *testing.T) {
	web, sess, run := passthroughWebSession(t)
	const digits = "0123456789012345"
	writeRunFile(t, run, "v0-1080-init-"+digits+".mp4", []byte("an older process's init"))
	if w := get(web, sess, "v0-1080-init-"+digits+".mp4"); w.Code != 200 {
		t.Fatalf("init: %d", w.Code)
	}
	run.mu.Lock()
	demand, media := run.demand, run.mediaDemand
	run.mu.Unlock()
	if demand != -1 || media != nil {
		t.Errorf("an init moved the demand: %d %v", demand, media)
	}
	// A segment does.
	writeRunFile(t, run, "v0-1080-3.m4s", []byte("seg"))
	get(web, sess, "v0-1080-3.m4s")
	run.mu.Lock()
	demand, media = run.demand, run.mediaDemand
	run.mu.Unlock()
	if demand != 3 || media["v0-1080.m3u8"] != 3 {
		t.Errorf("a segment request: demand %d %v", demand, media)
	}
}

// The running process's init is served only once it is complete -- the
// playlist naming it is written after it is -- never the empty file FFmpeg
// holds from its start; a request waits for it a while. Another process's
// init is final: served if it has bytes, not found at once if not.
func TestPassthroughWeb_InitOnlyWhenComplete(t *testing.T) {
	orig := passthroughInitWait
	passthroughInitWait = 400 * time.Millisecond
	t.Cleanup(func() { passthroughInitWait = orig })
	web, sess, run := passthroughWebSession(t)
	gen := run.Generation()
	name := "v0-1080-init-" + gen + ".mp4"
	init := fixtureInit(t, "main10-init.mp4")

	// Opened by FFmpeg, empty until the first cut.
	writeRunFile(t, run, name, nil)
	started := time.Now()
	if w := get(web, sess, name); w.Code != http.StatusNotFound || w.Body.Len() > 100 {
		t.Errorf("empty init: %d, %d bytes", w.Code, w.Body.Len())
	}
	if d := time.Since(started); d < passthroughInitWait {
		t.Errorf("gave up after %v, before the wait", d)
	}
	// Bytes in it, and the playlist on disk is the previous process's (a
	// restart reuses the directory): it names that process's init, not
	// this one, which is not known to be whole.
	passthroughRunDir(t, run.OutputDir(), "v0-1080", "1111111111111111", init, 10)
	writeRunFile(t, run, name, init[:100])
	if w := get(web, sess, name); w.Code != http.StatusNotFound {
		t.Errorf("init no playlist names: %d", w.Code)
	}
	// Named while the request waits: served, whole.
	writeRunFile(t, run, name, nil)
	go func() {
		time.Sleep(150 * time.Millisecond)
		passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, init, 10)
	}()
	if w := get(web, sess, name); w.Code != 200 || w.Body.Len() != len(init) {
		t.Errorf("init once named: %d, %d bytes of %d", w.Code, w.Body.Len(), len(init))
	}

	// Earlier processes of the run: final.
	writeRunFile(t, run, "v0-1080-init-1111111111111111.mp4", init)
	writeRunFile(t, run, "v0-1080-init-2222222222222222.mp4", nil)
	started = time.Now()
	if w := get(web, sess, "v0-1080-init-1111111111111111.mp4"); w.Code != 200 || w.Body.Len() != len(init) {
		t.Errorf("earlier init: %d %d", w.Code, w.Body.Len())
	}
	if w := get(web, sess, "v0-1080-init-2222222222222222.mp4"); w.Code != http.StatusNotFound || w.Body.Len() > 100 {
		t.Errorf("empty earlier init: %d %d", w.Code, w.Body.Len())
	}
	if w := get(web, sess, "v0-1080-init-3333333333333333.mp4"); w.Code != http.StatusNotFound {
		t.Errorf("init of no process: %d", w.Code)
	}
	if d := time.Since(started); d > passthroughInitWait {
		t.Errorf("final inits waited %v", d)
	}
}

// The playlists of a passthrough session carry the client's query on every
// reference, the init in #EXT-X-MAP included, whatever hex the generation
// holds; the session prefix of the legacy route lands in front of whole
// names. (The old route's playlists keep the old pattern, byte for byte:
// the golden test.)
func TestPassthroughWeb_RefsCarryTheQuery(t *testing.T) {
	h := hevcHLS(t, 1920, 1080, true, nil)
	for _, gen := range []string{"0a12b34c56d78e90", "0123456789abca12", "0123456789012345", "a1a1a1a1a1a1a1a1"} {
		variant := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-MAP:URI=\"v0-1080-init-" + gen + ".mp4\"\n#EXTINF:10.010000,\nv0-1080-0.m4s\n#EXTINF:10.010000,\nv0-1080-1.m4s\n"
		got := string(enrichPlaylistDataFor(h, []byte(variant), "api-key=K&token=T$1"))
		want := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-MAP:URI=\"v0-1080-init-" + gen + ".mp4?api-key=K&token=T$1\"\n#EXTINF:10.010000,\nv0-1080-0.m4s?api-key=K&token=T$1\n#EXTINF:10.010000,\nv0-1080-1.m4s?api-key=K&token=T$1\n"
		if got != want {
			t.Errorf("gen %s:\n got %q\nwant %q", gen, got, want)
		}
		pre := string(prefixPlaylistRefsFor(h, []byte(variant), "session/abc/"))
		if !strings.Contains(pre, "URI=\"session/abc/v0-1080-init-"+gen+".mp4\"") || !strings.Contains(pre, "\nsession/abc/v0-1080-1.m4s\n") {
			t.Errorf("gen %s prefixed:\n%s", gen, pre)
		}
	}
	// A track title that looks like a file name is not a reference, and a
	// reference that already has a query is left as it is.
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Track a1.vtt\",URI=\"a0.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"x\",URI=\"a1.m3u8?k=v\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subtitles\",URI=\"s0.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"hvc1.2.4.L150.90,mp4a.40.2\",VIDEO-RANGE=PQ\nv0-2160.m3u8\n"
	want := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Track a1.vtt\",URI=\"a0.m3u8?q=1\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"x\",URI=\"a1.m3u8?k=v\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subtitles\",URI=\"s0.m3u8?q=1\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"hvc1.2.4.L150.90,mp4a.40.2\",VIDEO-RANGE=PQ\nv0-2160.m3u8?q=1\n"
	if got := string(enrichPlaylistDataFor(h, []byte(master), "q=1")); got != want {
		t.Errorf("master:\n got %q\nwant %q", got, want)
	}
	if got := string(enrichPlaylistDataFor(h, []byte(master), "")); got != master {
		t.Error("an empty query changed the playlist")
	}
	// The old route's playlists take the old path.
	old := hevcHLS(t, 1920, 1080, false, nil)
	tsVariant := "#EXTM3U\n#EXTINF:4.0,\nv0-1080-0.ts\n"
	if string(enrichPlaylistDataFor(old, []byte(tsVariant), "q=1")) != string(enrichPlaylistData([]byte(tsVariant), "q=1")) {
		t.Error("old route rewritten differently")
	}
}

// The master of a passthrough session is written when it is first asked
// for, from the init of the run's current process; until then the request
// waits, and a run that is not running is started first.
func TestPassthroughWeb_MasterWaitsForTheInit(t *testing.T) {
	web, sess, run := passthroughWebSession(t)
	gen := run.Generation()
	init := fixtureInit(t, "main10-init.mp4")
	go func() {
		time.Sleep(300 * time.Millisecond)
		passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, init, 1_000_000)
	}()
	started := time.Now()
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/session/"+sess.id+"/index.m3u8?api-key=K", nil))
	body := w.Body.String()
	if w.Code != 200 || time.Since(started) < 300*time.Millisecond {
		t.Fatalf("master: %d after %v: %s", w.Code, time.Since(started), body)
	}
	for _, want := range []string{
		"#EXT-X-SESSION-OFFSET:0.000\n",
		`CODECS="hvc1.2.4.L63.90,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="audio"`,
		"RESOLUTION=1920x1080",
		"BANDWIDTH=991200,", // 1 MB over 10.01 s + the audio allowance
		`URI="a0.m3u8?api-key=K"`,
		"\nv0-1080.m3u8?api-key=K\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("master lacks %q:\n%s", want, body)
		}
	}
	if w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Errorf("content type %q", w.Header().Get("Content-Type"))
	}
}

// A master asked for while the run is not running starts it (a first
// process that died before its init is restarted, as for a variant), and a
// master that cannot be built is an error, not a guess.
func TestPassthroughWeb_MasterStartsTheRun(t *testing.T) {
	fakeFFmpeg(t)
	standInPassthroughParams(t)
	orig := passthroughMasterTimeout
	passthroughMasterTimeout = 2 * time.Second
	t.Cleanup(func() { passthroughMasterTimeout = orig })
	web, sm := passthroughWeb(t, 1920, 1080, nil)
	hashDir, _, _ := web.sourceHashDir("http://source/hevc/file.mkv")
	f := sdrMain10()
	f.ColorTransfer = transferPQ // the session keeps the facts it was decided on
	writeSourceFacts(sourceFactsPath(hashDir, 0), f)
	w := postSession(web, "hevc10,hdr-pq")
	if w.Code != 200 {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	var id string
	sm.mu.Lock()
	for _, s := range sm.sessions {
		id = s.id
	}
	sm.mu.Unlock()
	sess := sm.Get(id)
	if fileExists(filepath.Join(sess.outputDir, "index.m3u8")) {
		t.Fatal("a passthrough master written before any init")
	}
	// The run was released: the master request starts a new process.
	sess.Stop()
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(50 * time.Millisecond)
			if run := sess.currentRun(); run != nil {
				gen, running := run.processState()
				if running {
					passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, fixtureInit(t, "main10-init.mp4"), 1000)
					return
				}
			}
		}
	}()
	if w := get(web, sess, "index.m3u8"); w.Code != 200 || !strings.Contains(w.Body.String(), `CODECS="hvc1.2.4.L63.90,mp4a.40.2",VIDEO-RANGE=PQ`) {
		t.Fatalf("master after restart: %d %s", w.Code, w.Body.String())
	}

	// Unbuildable: 500, no master.
	w = postSession(web, "hevc10,hdr-pq")
	var sess2 *Session
	sm.mu.Lock()
	for _, s := range sm.sessions {
		if s.id != id {
			sess2 = s
		}
	}
	sm.mu.Unlock()
	if w.Code != 200 || sess2 == nil {
		t.Fatalf("second POST: %d", w.Code)
	}
	run := sess2.currentRun()
	gen, _ := run.processState()
	passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, []byte("not an init"), 10)
	if w := get(web, sess2, "index.m3u8"); w.Code != http.StatusInternalServerError {
		t.Errorf("unbuildable master: %d %s", w.Code, w.Body.String())
	}
}

// A process that dies before its init while the master waits for it is
// restarted once more and waited for, instead of failing the master (a
// first run of a source that dies on its timestamps at 0 comes back
// lenient): the master comes from the next process's init.
func TestPassthroughWeb_MasterOutlivesADyingProcess(t *testing.T) {
	// The first FFmpeg the session starts lives half a second and fails;
	// the ones after it run.
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\nn=$(cat " + count + " 2>/dev/null || echo 0)\necho $((n+1)) > " + count +
		"\nif [ \"$n\" = 0 ]; then sleep 0.5; exit 1; fi\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	standInPassthroughParams(t)
	orig := passthroughMasterTimeout
	passthroughMasterTimeout = 5 * time.Second
	t.Cleanup(func() { passthroughMasterTimeout = orig })
	f := sdrMain10()
	web, sm := passthroughWeb(t, 1920, 1080, &f)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")) // over passthroughWeb's fake
	if w := postSession(web, "hevc10"); w.Code != 200 {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	var sess *Session
	sm.mu.Lock()
	for _, s := range sm.sessions {
		sess = s
	}
	sm.mu.Unlock()
	first := sess.currentRun().Generation()
	// The init appears for a process after the first.
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(50 * time.Millisecond)
			if run := sess.currentRun(); run != nil {
				if gen, running := run.processState(); running && gen != first {
					passthroughRunDir(t, run.OutputDir(), "v0-1080", gen, fixtureInit(t, "main10-init.mp4"), 1000)
					return
				}
			}
		}
	}()
	w := get(web, sess, "index.m3u8")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `CODECS="hvc1.2.4.L63.90,mp4a.40.2"`) {
		t.Fatalf("master: %d %s", w.Code, w.Body.String())
	}
	if b, _ := os.ReadFile(count); strings.TrimSpace(string(b)) != "2" {
		t.Errorf("FFmpeg started %s times, want 2", b)
	}
}
