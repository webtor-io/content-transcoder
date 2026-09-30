package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
)

// unknownSessionWeb is a Web behind the real router with the tarpit set to
// delay, and one live session (known) whose master is on disk.
func unknownSessionWeb(t *testing.T, delay time.Duration) (*Web, *Session) {
	t.Helper()
	runMgr := NewRunManager()
	sm := NewSessionManager(runMgr)
	t.Cleanup(func() {
		sm.CloseAll()
		runMgr.CloseAll()
	})
	web := &Web{sessionManager: sm, touchMap: NewTouchMap(), unknownSessionDelay: delay}
	web.buildHandler()
	known := sm.Create(SessionConfig{HashDir: t.TempDir()})
	if err := os.MkdirAll(known.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv0-720.m3u8\n"
	if err := os.WriteFile(filepath.Join(known.outputDir, "index.m3u8"), []byte(master), 0644); err != nil {
		t.Fatal(err)
	}
	return web, known
}

// serve runs one request through the router and returns the answer and how
// long the handler took.
func serve(web *Web, r *http.Request) (*httptest.ResponseRecorder, time.Duration) {
	w := httptest.NewRecorder()
	start := time.Now()
	web.handler.ServeHTTP(w, r)
	return w, time.Since(start)
}

const deadSession = "0123456789abcdef0123456789abcdef"

// The tarpit itself, at the delay production runs with: a playlist or
// segment of a session this pod does not hold is the 404 it always was, but
// not before the delay -- the one thing that caps a player asking again the
// moment its 404 arrives. The status must stay in class 400: the transcoder's
// TTFB alert and panels leave that class out to keep the held answers out of
// its latency (unknown_session.go), and a held answer of another class would
// put about 2 s back into every p95.
func TestUnknownSession_HLSRequestAnswered404AfterTheDelay(t *testing.T) {
	web, _ := unknownSessionWeb(t, defaultUnknownSessionDelay)
	delayed0 := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionDelayed))

	w, took := serve(web, httptest.NewRequest(http.MethodGet, "/session/"+deadSession+"/v0-1080-148.ts?token=t", nil))
	if w.Code != http.StatusNotFound || w.Body.String() != "session not found\n" {
		t.Fatalf("answer = %d %q, want 404 %q", w.Code, w.Body.String(), "session not found\n")
	}
	if took < defaultUnknownSessionDelay {
		t.Errorf("404 after %v, want not before %v", took, defaultUnknownSessionDelay)
	}
	if took > defaultUnknownSessionDelay+time.Second {
		t.Errorf("404 after %v, want about %v", took, defaultUnknownSessionDelay)
	}
	if got := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionDelayed)) - delayed0; got != 1 {
		t.Errorf("unknown_session_requests_total{answer=delayed} delta = %v, want 1", got)
	}
}

// Every file an hls.js loader asks for is held, playlists, segments and
// passthrough inits alike, by GET and by HEAD.
func TestUnknownSession_EveryLoaderFileIsHeld(t *testing.T) {
	const delay = 150 * time.Millisecond
	web, _ := unknownSessionWeb(t, delay)
	for _, c := range []struct{ method, file string }{
		{http.MethodGet, "index.m3u8"},
		{http.MethodGet, "v0-1080.m3u8"},
		{http.MethodGet, "a0.m3u8"},
		{http.MethodGet, "s0.m3u8"},
		{http.MethodGet, "a0-12.ts"},
		{http.MethodGet, "s0-3.vtt"},
		{http.MethodGet, "v0-2076-4.m4s"},
		{http.MethodGet, "a1-init-cfa1588fdcfecf34.mp4"},
		{http.MethodHead, "v0-1080-7.ts"},
	} {
		w, took := serve(web, httptest.NewRequest(c.method, "/session/"+deadSession+"/"+c.file, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", c.method, c.file, w.Code)
		}
		if took < delay {
			t.Errorf("%s %s: 404 after %v, want not before %v", c.method, c.file, took, delay)
		}
	}
}

// A client that leaves during the wait ends it: the handler returns at once
// and writes nothing, so a tab that reloads, or a proxy that gives up, does
// not keep a goroutine here for the rest of the delay.
func TestUnknownSession_CanceledRequestReturnsAtOnce(t *testing.T) {
	web, _ := unknownSessionWeb(t, 10*time.Second)
	canceled0 := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionCanceled))
	delayed0 := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionDelayed))

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	r := httptest.NewRequest(http.MethodGet, "/session/"+deadSession+"/v0-1080.m3u8", nil).WithContext(ctx)
	w, took := serve(web, r)
	if took > time.Second {
		t.Fatalf("canceled request returned after %v, want at once (the delay is 10s)", took)
	}
	if w.Body.Len() != 0 {
		t.Errorf("canceled request got a body %q, want nothing written", w.Body.String())
	}
	if got := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionCanceled)) - canceled0; got != 1 {
		t.Errorf("unknown_session_requests_total{answer=canceled} delta = %v, want 1", got)
	}
	if got := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionDelayed)) - delayed0; got != 0 {
		t.Errorf("unknown_session_requests_total{answer=delayed} delta = %v, want 0", got)
	}
}

// The same over a real connection: a client that hangs up (hls.js aborting
// on startLoad, a closed tab) cancels the request's context, and the wait
// ends then, not at the delay.
func TestUnknownSession_ClientHangUpEndsTheWait(t *testing.T) {
	web, _ := unknownSessionWeb(t, 10*time.Second)
	srv := httptest.NewServer(web.handler)
	t.Cleanup(srv.Close)
	canceled := metricUnknownSessionTotal.WithLabelValues(unknownSessionCanceled)
	canceled0 := counter(t, canceled)

	cl := &http.Client{Timeout: 100 * time.Millisecond}
	start := time.Now()
	if res, err := cl.Get(srv.URL + "/session/" + deadSession + "/a0-12.ts"); err == nil {
		res.Body.Close()
		t.Fatalf("got %d before the client's timeout, want the request held", res.StatusCode)
	}
	for counter(t, canceled)-canceled0 < 1 {
		if time.Since(start) > 2*time.Second {
			t.Fatal("the handler still waits 2s after the client hung up (the delay is 10s)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Only the session lookup failing leads to the wait: a session the pod
// holds is served as before, and so is a 404 inside it (a file the session
// does not have).
func TestUnknownSession_KnownSessionIsNotHeld(t *testing.T) {
	web, known := unknownSessionWeb(t, 10*time.Second)

	w, took := serve(web, httptest.NewRequest(http.MethodGet, "/session/"+known.id+"/index.m3u8", nil))
	if w.Code != http.StatusOK {
		t.Errorf("known session master: status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if took > time.Second {
		t.Errorf("known session master took %v, want at once", took)
	}

	w, took = serve(web, httptest.NewRequest(http.MethodGet, "/session/"+known.id+"/v9-init-0000000000000000.mp4", nil))
	if w.Code != http.StatusNotFound || w.Body.String() != "not found\n" {
		t.Errorf("known session, unknown file: answer = %d %q, want 404 %q", w.Code, w.Body.String(), "not found\n")
	}
	if took > time.Second {
		t.Errorf("known session, unknown file: 404 after %v, want at once", took)
	}
}

// What the tarpit leaves alone is answered at once: the seek (POST, and the
// GET of its offset the player makes once), DELETE, the session's bare URL,
// files no loader asks for, CORS preflights -- and rest-api's CacheMap probe,
// which the legacy route answers and must keep answering 404 at once.
func TestUnknownSession_OtherRequestsAnsweredAtOnce(t *testing.T) {
	web, _ := unknownSessionWeb(t, 10*time.Second)
	immediate0 := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionImmediate))

	for _, c := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/session/" + deadSession + "/seek?t=300", http.StatusNotFound},
		{http.MethodGet, "/session/" + deadSession + "/seek", http.StatusNotFound},
		{http.MethodDelete, "/session/" + deadSession, http.StatusNotFound},
		{http.MethodGet, "/session/" + deadSession, http.StatusNotFound},
		{http.MethodGet, "/session/" + deadSession + "/index.json", http.StatusNotFound},
		{http.MethodPost, "/session/" + deadSession + "/v0-1080-3.ts", http.StatusNotFound},
		{http.MethodOptions, "/session/" + deadSession + "/v0-1080-3.ts", http.StatusOK},
	} {
		w, took := serve(web, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.status {
			t.Errorf("%s %s: status = %d, want %d", c.method, c.path, w.Code, c.status)
		}
		if took > time.Second {
			t.Errorf("%s %s: answered after %v, want at once", c.method, c.path, took)
		}
	}
	// The six 404s above; the preflight is answered before the lookup.
	if got := counter(t, metricUnknownSessionTotal.WithLabelValues(unknownSessionImmediate)) - immediate0; got != 6 {
		t.Errorf("unknown_session_requests_total{answer=immediate} delta = %v, want 6", got)
	}

	r := httptest.NewRequest(http.MethodGet, "/index.m3u8?done=true&api-key=k", nil)
	r.Header.Set("X-Source-Url", "http://example/src.mkv")
	w, took := serve(web, r)
	if w.Code != http.StatusNotFound || took > time.Second {
		t.Errorf("CacheMap probe: %d after %v, want 404 at once", w.Code, took)
	}
}

// UNKNOWN_SESSION_DELAY=0 turns the tarpit off: the 404 at once, as before,
// counted as immediate and not logged as held.
func TestUnknownSession_ZeroDelayAnswersAtOnce(t *testing.T) {
	hook := test.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(make(log.LevelHooks)) })
	web, _ := unknownSessionWeb(t, 0)
	immediate := metricUnknownSessionTotal.WithLabelValues(unknownSessionImmediate)
	immediate0 := counter(t, immediate)
	w, took := serve(web, httptest.NewRequest(http.MethodGet, "/session/"+deadSession+"/v0-1080-148.ts", nil))
	if w.Code != http.StatusNotFound || took > 100*time.Millisecond {
		t.Errorf("delay 0: %d after %v, want 404 at once", w.Code, took)
	}
	if got := counter(t, immediate) - immediate0; got != 1 {
		t.Errorf("delay 0: unknown_session_requests_total{answer=immediate} delta = %v, want 1", got)
	}
	for _, e := range hook.AllEntries() {
		if e.Message == "session: unknown session, 404 held (tarpit)" {
			t.Errorf("delay 0: logged %q, nothing was held", e.Message)
		}
	}
}

// The flag is on by default, at 2 s, and 0 switches it off.
func TestUnknownSession_DelayFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want time.Duration
	}{
		{nil, 2 * time.Second},
		{[]string{"--unknown-session-delay=0"}, 0},
		{[]string{"--unknown-session-delay=1500ms"}, 1500 * time.Millisecond},
	} {
		var got time.Duration
		app := cli.NewApp()
		app.Flags = RegisterWebFlags(nil)
		app.Action = func(c *cli.Context) error {
			got = NewWeb(c, nil, nil, nil, nil).unknownSessionDelay
			return nil
		}
		if err := app.Run(append([]string{"server"}, c.args...)); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("args %v: unknownSessionDelay = %v, want %v", c.args, got, c.want)
		}
	}
}

// One log line per unknown session id per unknownSessionLogEvery, carrying
// the requests held since the last one: a looping tab must not turn into a
// log line per request.
func TestUnknownSessionLog_OncePerIDPerPeriod(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	l := &unknownSessionLog{now: func() time.Time { return now }}

	if held, ok := l.take("a"); !ok || held != 1 {
		t.Fatalf("first request of a: take = %d, %v; want 1, true", held, ok)
	}
	for i := 0; i < 99; i++ {
		now = now.Add(time.Second)
		if _, ok := l.take("a"); ok {
			t.Fatalf("request %d of a within the period was logged", i+2)
		}
	}
	if held, ok := l.take("b"); !ok || held != 1 {
		t.Errorf("first request of b: take = %d, %v; want 1, true (ids are logged apart)", held, ok)
	}
	now = now.Add(unknownSessionLogEvery)
	if held, ok := l.take("a"); !ok || held != 100 {
		t.Errorf("a after the period: take = %d, %v; want 100 (99 held since the line, and this one), true", held, ok)
	}
	if _, ok := l.take("a"); ok {
		t.Error("a right after its second line was logged again")
	}
}

// Through the handler: five held requests for one dead session, one line.
func TestUnknownSession_LogsOnceForALoopingTab(t *testing.T) {
	hook := test.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(make(log.LevelHooks)) })
	web, _ := unknownSessionWeb(t, time.Millisecond)
	const sid = "fedcba9876543210fedcba9876543210"
	for i := 0; i < 5; i++ {
		serve(web, httptest.NewRequest(http.MethodGet, "/session/"+sid+"/v0-1080-"+strconv.Itoa(i)+".ts", nil))
	}
	var lines []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Message == "session: unknown session, 404 held (tarpit)" && e.Data["sessionID"] == sid {
			lines = append(lines, e)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("log lines for the looping session = %d, want 1", len(lines))
	}
	if lines[0].Level != log.InfoLevel || lines[0].Data["file"] != "v0-1080-0.ts" || lines[0].Data["held"] != 1 {
		t.Errorf("line = %v %v, want info with file v0-1080-0.ts, held 1", lines[0].Level, lines[0].Data)
	}
}

// The ids remembered for the log are bounded -- an id is whatever a client
// puts in the URL. At the bound a new id is not logged until the old ones
// are a period old, and then they are forgotten to make room.
func TestUnknownSessionLog_BoundedIDs(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	l := &unknownSessionLog{now: func() time.Time { return now }}
	for i := 0; i < unknownSessionLogMax; i++ {
		l.take("id-" + strconv.Itoa(i))
	}
	now = now.Add(time.Minute)
	if _, ok := l.take("one-more"); ok {
		t.Error("an id past the bound was logged while every remembered id is recent")
	}
	if len(l.entries) != unknownSessionLogMax {
		t.Errorf("remembered ids = %d, want the bound %d", len(l.entries), unknownSessionLogMax)
	}
	now = now.Add(unknownSessionLogEvery)
	if held, ok := l.take("one-more"); !ok || held != 1 {
		t.Errorf("an id past the bound after the period: take = %d, %v; want 1, true", held, ok)
	}
	if len(l.entries) != 1 {
		t.Errorf("remembered ids after forgetting = %d, want 1", len(l.entries))
	}
}

// An id is remembered and logged at most unknownSessionIDMax bytes long:
// the bound on how many ids are remembered is a bound on memory only if
// each one is.
func TestUnknownSessionLog_LongIDIsCut(t *testing.T) {
	l := &unknownSessionLog{}
	long := strings.Repeat("f", 10000)
	l.note(long, http.MethodGet, "v0-1080-1.ts", time.Second)
	if len(l.entries) != 1 {
		t.Fatalf("remembered ids = %d, want 1", len(l.entries))
	}
	for id := range l.entries {
		if len(id) != unknownSessionIDMax {
			t.Errorf("remembered id is %d bytes, want %d", len(id), unknownSessionIDMax)
		}
	}
}
