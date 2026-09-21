package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The metrics live on the default registry and the tests in this package
// share it, so every assertion here is on a delta, never on an absolute.

func counter(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}

// histogramCount reads the sample count of one histogram series off the
// default gatherer (testutil has no histogram accessor).
func histogramCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					continue metrics
				}
			}
			return m.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

func TestMetrics_SessionsCreatedAndActive(t *testing.T) {
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	m := NewSessionManager(runMgr)
	defer m.CloseAll()

	total0 := counter(t, metricSessionsTotal)
	active0 := counter(t, metricSessionsActive)

	a := m.Create(SessionConfig{HashDir: t.TempDir()})
	m.Create(SessionConfig{HashDir: t.TempDir()})
	if got := counter(t, metricSessionsTotal) - total0; got != 2 {
		t.Errorf("sessions_total delta = %v, want 2", got)
	}
	if got := counter(t, metricSessionsActive) - active0; got != 2 {
		t.Errorf("sessions_active delta after 2 creates = %v, want 2", got)
	}

	m.Close(a.id)
	m.Close(a.id) // unknown id: nothing to decrement
	if got := counter(t, metricSessionsActive) - active0; got != 1 {
		t.Errorf("sessions_active delta after close = %v, want 1", got)
	}

	m.CloseAll()
	if got := counter(t, metricSessionsActive) - active0; got != 0 {
		t.Errorf("sessions_active delta after CloseAll = %v, want 0", got)
	}
	if got := counter(t, metricSessionsTotal) - total0; got != 2 {
		t.Errorf("sessions_total must only count creates, delta = %v", got)
	}
}

// newLiveRun is a run whose FFmpeg looks alive to IsRunning without a
// process: done stays open until the test closes it.
func newLiveRun(dir string) *TranscodeRun {
	run := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
	run.AddRef()
	run.running = true
	run.done = make(chan struct{})
	return run
}

func TestMetrics_PlaylistWaitOutcomes(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	s := NewSession(SessionConfig{ID: "pl-metrics", HashDir: dir, RunMgr: runMgr})
	runDir := filepath.Join(dir, "runs", "seek-0.000")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	waits := func(outcome, kind string) float64 {
		return counter(t, metricPlaylistWaitsTotal.WithLabelValues(outcome, kind))
	}
	timeout0, notRunning0, ok0, canceled0 := waits("timeout", "variant"), waits("not_running", "variant"), waits("ok", "variant"), waits("canceled", "variant")
	subTimeout0 := waits("timeout", "subtitle")
	okHist0 := histogramCount(t, "transcoder_playlist_wait_seconds", map[string]string{"kind": "variant"})

	// A run that is alive and has written nothing: the wait runs out.
	live := newLiveRun(dir)
	s.run = live
	if _, err := s.WaitForPlaylist(context.Background(), "v0.m3u8", 10*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	if got := waits("timeout", "variant") - timeout0; got != 1 {
		t.Errorf("playlist_waits_total{timeout,variant} delta = %v, want 1", got)
	}

	// Same on a subtitle playlist is counted apart: it is expected there.
	if _, err := s.WaitForPlaylist(context.Background(), "s0.m3u8", 10*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	if got := waits("timeout", "subtitle") - subTimeout0; got != 1 {
		t.Errorf("playlist_waits_total{timeout,subtitle} delta = %v, want 1", got)
	}
	if got := waits("timeout", "variant") - timeout0; got != 1 {
		t.Errorf("subtitle timeout leaked into variant: delta = %v", got)
	}

	// Client gone: the caller's context, not ours.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WaitForPlaylist(ctx, "v0.m3u8", time.Second); err == nil {
		t.Fatal("expected a context error")
	}
	if got := waits("canceled", "variant") - canceled0; got != 1 {
		t.Errorf("playlist_waits_total{canceled,variant} delta = %v, want 1", got)
	}

	// FFmpeg is gone and left no playlist behind.
	close(live.done)
	if _, err := s.WaitForPlaylist(context.Background(), "v0.m3u8", time.Second); err == nil {
		t.Fatal("expected not-running error")
	}
	if got := waits("not_running", "variant") - notRunning0; got != 1 {
		t.Errorf("playlist_waits_total{not_running,variant} delta = %v, want 1", got)
	}
	if got := histogramCount(t, "transcoder_playlist_wait_seconds", map[string]string{"kind": "variant"}) - okHist0; got != 0 {
		t.Errorf("failed waits must not be observed in the histogram, got %d samples", got)
	}

	// The playlist is there: ok, and the wait is observed.
	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.0,\nv0-0.ts\n"
	if err := os.WriteFile(filepath.Join(runDir, "v0.m3u8.ffmpeg"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitForPlaylist(context.Background(), "v0.m3u8", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := waits("ok", "variant") - ok0; got != 1 {
		t.Errorf("playlist_waits_total{ok,variant} delta = %v, want 1", got)
	}
	if got := histogramCount(t, "transcoder_playlist_wait_seconds", map[string]string{"kind": "variant"}) - okHist0; got != 1 {
		t.Errorf("playlist_wait_seconds{variant} sample delta = %d, want 1", got)
	}
}

// An auto-restart is charged where the budget is (EnsureRunning and
// RestartForSegment), whatever the acquire then does. The manager is
// preloaded with a completed run under the session's key so the re-acquire
// reuses it instead of spawning FFmpeg.
func TestMetrics_AutoRestartsCharged(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	runMgr.runs[runKey(dir, 0)] = &managedRun{run: newCompletedRun(t, dir)}

	s := NewSession(SessionConfig{ID: "auto-restart", HashDir: dir, RunMgr: runMgr})
	dead := func() *TranscodeRun {
		r := newTranscodeRun("dead", dir, 0, "", nil)
		r.AddRef()
		return r
	}
	before := counter(t, metricAutoRestartsTotal)

	s.run = dead()
	if err := s.EnsureRunning(); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, metricAutoRestartsTotal) - before; got != 1 {
		t.Errorf("auto_restarts_total after EnsureRunning: delta = %v, want 1", got)
	}

	s.run = dead()
	if err := s.RestartForSegment(3); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, metricAutoRestartsTotal) - before; got != 2 {
		t.Errorf("auto_restarts_total after RestartForSegment: delta = %v, want 2", got)
	}

	// A run that finished is not restarted, and not charged.
	s.run = newCompletedRun(t, dir)
	if err := s.EnsureRunning(); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, metricAutoRestartsTotal) - before; got != 2 {
		t.Errorf("completed run must not count as a restart: delta = %v", got)
	}
}

// The restart cap is counted once per session, on either handler, like the
// log line it mirrors: the player retries the same 503 many times a second.
func TestMetrics_RestartLimitOncePerSession(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	capped := func(id string) *Session {
		s := NewSession(SessionConfig{ID: id, HashDir: dir, RunMgr: runMgr})
		s.restartFails = maxConsecutiveRestarts
		return s
	}
	web := &Web{}
	before := counter(t, metricRestartLimitReachedTotal)

	s1 := capped("capped-playlist")
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		web.sessionPlaylistHandler(w, httptest.NewRequest(http.MethodGet, "/session/x/v0.m3u8", nil), s1, "v0.m3u8")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("playlist attempt %d: status %d, want 503", i, w.Code)
		}
	}
	if got := counter(t, metricRestartLimitReachedTotal) - before; got != 1 {
		t.Errorf("restart_limit_reached_total after 3 playlist 503s on one session: delta = %v, want 1", got)
	}

	s2 := capped("capped-segment")
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		web.sessionSegmentHandler(w, httptest.NewRequest(http.MethodGet, "/session/x/v0-0.ts", nil), s2, "v0-0.ts")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("segment attempt %d: status %d, want 503", i, w.Code)
		}
	}
	if got := counter(t, metricRestartLimitReachedTotal) - before; got != 2 {
		t.Errorf("restart_limit_reached_total after a second capped session: delta = %v, want 2", got)
	}
}

// startFakeProcess does what startLocked does once FFmpeg is spawned, with
// an arbitrary command in its place: the reaping and the bookkeeping under
// test are shared, the ffmpeg lookup and argument building are not.
func startFakeProcess(t *testing.T, r *TranscodeRun, name string, args ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx, r.cancel = context.WithCancel(r.runCtx)
	r.done = make(chan struct{})
	r.cmd = exec.CommandContext(r.ctx, name, args...)
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.watchProcessLocked()
}

func TestMetrics_RunOutcomes(t *testing.T) {
	runs := func(outcome string) float64 { return counter(t, metricRunsTotal.WithLabelValues(outcome)) }
	exits := func(reason string) float64 { return counter(t, metricFFmpegExitsTotal.WithLabelValues(reason)) }
	active0 := counter(t, metricRunsActive)

	cases := []struct {
		name      string
		cmd       []string
		stop      func(r *TranscodeRun)
		outcome   string
		reason    string
		completed bool
	}{
		{"clean exit is finished", []string{"true"}, nil, runOutcomeFinished, ffmpegExitOK, true},
		{"error exit nobody asked for is failed", []string{"false"}, nil, runOutcomeFailed, ffmpegExitError, false},
		{"signal nobody sent is failed", []string{"sh", "-c", "kill -9 $$"}, nil, runOutcomeFailed, ffmpegExitSignal, false},
		{"idle reaper records released_idle", []string{"sleep", "30"}, func(r *TranscodeRun) {
			// The run manager's reaper, on a run whose grace period is over.
			rm := NewRunManager()
			defer rm.CloseAll()
			rm.runs[r.key] = &managedRun{run: r, idleSince: time.Now().Add(-2 * runGracePeriod)}
			rm.cleanupIdleRuns()
		}, runOutcomeReleasedIdle, ffmpegExitSignal, false},
		{"Cleanup records killed", []string{"sleep", "30"}, func(r *TranscodeRun) { r.Cleanup() }, runOutcomeKilled, ffmpegExitSignal, false},
		{"Stop records killed", []string{"sleep", "30"}, func(r *TranscodeRun) { r.Stop() }, runOutcomeKilled, ffmpegExitSignal, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "", nil)
			runs0, exits0 := runs(c.outcome), exits(c.reason)
			startFakeProcess(t, r, c.cmd[0], c.cmd[1:]...)
			if got := counter(t, metricRunsActive) - active0; got != 1 {
				t.Errorf("runs_active while running: delta = %v, want 1", got)
			}
			if c.stop != nil {
				c.stop(r)
			}
			select {
			case <-r.done:
			case <-time.After(5 * time.Second):
				t.Fatal("process not reaped")
			}
			if got := runs(c.outcome) - runs0; got != 1 {
				t.Errorf("runs_total{%s} delta = %v, want 1", c.outcome, got)
			}
			if got := exits(c.reason) - exits0; got != 1 {
				t.Errorf("ffmpeg_exits_total{%s} delta = %v, want 1", c.reason, got)
			}
			if got := counter(t, metricRunsActive) - active0; got != 0 {
				t.Errorf("runs_active after exit: delta = %v, want 0", got)
			}
			if r.IsCompleted() != c.completed {
				t.Errorf("completed = %v, want %v", r.IsCompleted(), c.completed)
			}
			if r.IsRunning() {
				t.Error("still running after exit")
			}
		})
	}
}

func TestClassifyExit(t *testing.T) {
	if o, r := classifyExit(nil, runOutcomeKilled); o != runOutcomeFinished || r != ffmpegExitOK {
		t.Errorf("clean exit under a stop: %s/%s, want finished/ok", o, r)
	}
	if o, r := classifyExit(exec.Command("false").Run(), ""); o != runOutcomeFailed || r != ffmpegExitError {
		t.Errorf("error exit: %s/%s, want failed/error", o, r)
	}
	if o, r := classifyExit(exec.Command("sh", "-c", "kill -TERM $$").Run(), runOutcomeReleasedIdle); o != runOutcomeReleasedIdle || r != ffmpegExitSignal {
		t.Errorf("signal under the idle reaper: %s/%s, want released_idle/signal", o, r)
	}
}

// The probe is timed only when it actually probes: a cached index.json is a
// disk read, and counting it would make every replay look like a fast source.
func TestMetrics_SourceOpenObservedOnlyWhenProbing(t *testing.T) {
	probe := &ContentProbe{timeout: 5}
	ok0 := histogramCount(t, "transcoder_source_open_seconds", map[string]string{"outcome": "ok"})
	err0 := histogramCount(t, "transcoder_source_open_seconds", map[string]string{"outcome": "error"})

	cached := t.TempDir()
	if err := os.WriteFile(filepath.Join(cached, "index.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.get("http://127.0.0.1:1/x.mkv", cached); err != nil {
		t.Fatal(err)
	}
	if got := histogramCount(t, "transcoder_source_open_seconds", map[string]string{"outcome": "ok"}) - ok0; got != 0 {
		t.Errorf("cached probe observed %d times, want 0", got)
	}

	// Nothing listens on port 1 (and ffprobe may be absent): either way the
	// probe fails, and the failure is what gets timed.
	if _, err := probe.get("http://127.0.0.1:1/x.mkv", t.TempDir()); err == nil {
		t.Fatal("expected the probe to fail")
	}
	if got := histogramCount(t, "transcoder_source_open_seconds", map[string]string{"outcome": "error"}) - err0; got != 1 {
		t.Errorf("source_open_seconds{error} sample delta = %d, want 1", got)
	}
	if got := histogramCount(t, "transcoder_source_open_seconds", map[string]string{"outcome": "ok"}) - ok0; got != 0 {
		t.Errorf("failed probe must not count as ok, delta = %d", got)
	}
}

// A stop reason is for the process being stopped, not for the run: a run
// stopped and later restarted (Acquire after the 60 s release) must report
// its next death on its own terms.
func TestMetrics_RunOutcomeResetOnRestart(t *testing.T) {
	r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "", nil)
	killed0 := counter(t, metricRunsTotal.WithLabelValues(runOutcomeKilled))
	failed0 := counter(t, metricRunsTotal.WithLabelValues(runOutcomeFailed))

	startFakeProcess(t, r, "sleep", "30")
	r.Stop()
	if got := counter(t, metricRunsTotal.WithLabelValues(runOutcomeKilled)) - killed0; got != 1 {
		t.Fatalf("runs_total{killed} delta = %v, want 1", got)
	}

	startFakeProcess(t, r, "false")
	<-r.done
	if got := counter(t, metricRunsTotal.WithLabelValues(runOutcomeFailed)) - failed0; got != 1 {
		t.Errorf("runs_total{failed} delta = %v, want 1", got)
	}
	if got := counter(t, metricRunsTotal.WithLabelValues(runOutcomeKilled)) - killed0; got != 1 {
		t.Errorf("stale stop reason charged to the restarted run: killed delta = %v", got)
	}
}
