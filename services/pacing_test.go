package services

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	cp "github.com/webtor-io/content-prober/content-prober"
)

// fastPacing runs the pace loop at test speed for the duration of a test.
func fastPacing(t *testing.T) {
	t.Helper()
	poll, gap := pacePoll, paceResumeGap
	pacePoll, paceResumeGap = 20*time.Millisecond, 8*time.Second // resume 2 segments closer than the lead
	t.Cleanup(func() { pacePoll, paceResumeGap = poll, gap })
}

// pacedRun is a copy-mode run (h264 1080p video, AAC audio) with the given
// pace lead, its output dir created.
func pacedRun(t *testing.T, lead time.Duration) *TranscodeRun {
	t.Helper()
	h := NewHLS("http://src/f.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "h264", Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
	}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", paceLead: lead})
	r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "http://src/f.mkv", h)
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	return r
}

// writeSegments creates primary segments 0..n-1, as FFmpeg would.
func writeSegments(t *testing.T, r *TranscodeRun, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(r.primarySegmentPath(i), []byte("ts"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// stopped reports whether the process is in the stopped state (SIGSTOP),
// read off ps, not off the run's own bookkeeping.
func stopped(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "T")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// never checks cond stays false for a while (several pace polls).
func never(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("unexpected: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPaceSegments(t *testing.T) {
	for d, want := range map[time.Duration]int{
		5 * time.Minute:  75,
		4 * time.Minute:  60,
		20 * time.Second: 5,
		3 * time.Second:  0,
	} {
		if got := paceSegments(d); got != want {
			t.Errorf("paceSegments(%v) = %d, want %d", d, got, want)
		}
	}
}

// The pace loop watches the files FFmpeg actually writes: the primary
// stream's segment names must match GetFFmpegParams' output pattern.
func TestPrimarySegmentPathMatchesFFmpegOutput(t *testing.T) {
	for _, c := range []struct {
		streams []*cp.Stream
		want    string
	}{
		{[]*cp.Stream{{Index: 0, CodecType: "video", CodecName: "hevc", Height: 720}}, "v0-720-%d.ts"},
		{[]*cp.Stream{{Index: 0, CodecType: "audio", CodecName: "aac", Channels: 2}}, "a0-%d.ts"},
	} {
		h := NewHLS("http://src/f", &cp.ProbeReply{Streams: c.streams}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
		params, err := h.GetFFmpegParams("/out")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(params, " "), "/out/"+c.want) {
			t.Fatalf("FFmpeg output pattern changed: want /out/%s in %v", c.want, params)
		}
		r := newTranscodeRun("k", "/h", 0, "http://src/f", h)
		r.outputDir = "/out"
		if got, want := r.primarySegmentPath(7), "/out/"+strings.Replace(c.want, "%d", "7", 1); got != want {
			t.Errorf("primarySegmentPath(7) = %s, want %s", got, want)
		}
	}
}

func TestNoteDemandKeepsTheFurthest(t *testing.T) {
	r := newTranscodeRun("k", t.TempDir(), 0, "", nil)
	if r.demand != -1 {
		t.Fatalf("a fresh run has no demand, got %d", r.demand)
	}
	for _, n := range []int{3, 10, 7} {
		r.noteDemand(n)
	}
	if r.demand != 10 {
		t.Errorf("demand = %d, want the furthest (10); a viewer lagging behind another must not pull the run back", r.demand)
	}
}

// The whole cycle on a real process: frozen once it is the lead ahead of
// the viewers (counted from 0 before any request), continued when a viewer
// comes within the resume distance, frozen again when it runs ahead again.
func TestPaceFreezesAndReleasesTheProcess(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 20*time.Second) // lead 5 segments, resume at 3
	pausedBefore := counter(t, metricRunsPaused)
	pauseSecBefore := counter(t, metricRunPauseSeconds.WithLabelValues(runModeCopy))

	startFakeProcess(t, r, "sleep", "30")
	defer r.Stop()
	pid := r.cmd.Process.Pid

	writeSegments(t, r, 5) // 0..4: segment 5 (0 + lead) not there yet
	never(t, "frozen before it is the lead ahead", func() bool { return stopped(t, pid) })

	writeSegments(t, r, 6) // segment 5 exists: the lead ahead of demand 0
	eventually(t, "frozen at the lead", func() bool { return stopped(t, pid) })
	if got := counter(t, metricRunsPaused) - pausedBefore; got != 1 {
		t.Errorf("runs_paused = +%v while frozen, want +1", got)
	}

	r.noteDemand(1) // 1+3 = segment 4 exists: not close enough yet
	never(t, "continued before a viewer is within the resume distance", func() bool { return !stopped(t, pid) })

	r.noteDemand(3) // 3+3 = segment 6 missing: continue
	eventually(t, "continued", func() bool { return !stopped(t, pid) })
	if got := counter(t, metricRunsPaused) - pausedBefore; got != 0 {
		t.Errorf("runs_paused = +%v after continuing, want 0", got)
	}
	if got := counter(t, metricRunPauseSeconds.WithLabelValues(runModeCopy)) - pauseSecBefore; got <= 0 {
		t.Errorf("run_pause_seconds_total did not grow (%v)", got)
	}
	r.mu.Lock()
	pausedFor := r.pausedFor
	r.mu.Unlock()
	if pausedFor <= 0 {
		t.Error("the run must account the time it was frozen")
	}

	writeSegments(t, r, 9) // 3+5 = segment 8 exists: the lead ahead again
	eventually(t, "frozen again", func() bool { return stopped(t, pid) })
}

// Negative control: with pacing off the process is never frozen however far
// ahead it gets.
func TestPaceDisabled(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 0)
	startFakeProcess(t, r, "sleep", "30")
	defer r.Stop()
	writeSegments(t, r, 200)
	never(t, "frozen with pacing disabled", func() bool { return stopped(t, r.cmd.Process.Pid) })
}

// A frozen run must still stop: the idle reaper, a seek and shutdown all
// stop runs pace may be holding. (No SIGCONT is needed: stopping cancels the
// run's context, and exec.CommandContext kills the process with SIGKILL,
// which a stopped process does not hold back.)
func TestStopWhilePaused(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 20*time.Second)
	pausedBefore := counter(t, metricRunsPaused)
	startFakeProcess(t, r, "sleep", "30")
	pid := r.cmd.Process.Pid
	writeSegments(t, r, 6)
	eventually(t, "frozen", func() bool { return stopped(t, pid) })

	began := time.Now()
	r.Stop()
	if d := time.Since(began); d > runGracefulStopTimeout+time.Second {
		t.Errorf("Stop on a frozen run took %v", d)
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("frozen process was not reaped")
	}
	eventually(t, "runs_paused back to its level", func() bool { return counter(t, metricRunsPaused) == pausedBefore })
}

// A frozen process that dies under us (OOM, node) leaves no paused gauge
// behind and no signals to a pid that may be reused.
func TestPausedProcessDies(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 20*time.Second)
	pausedBefore := counter(t, metricRunsPaused)
	startFakeProcess(t, r, "sleep", "30")
	pid := r.cmd.Process.Pid
	writeSegments(t, r, 6)
	eventually(t, "frozen", func() bool { return stopped(t, pid) })
	_ = syscall.Kill(pid, syscall.SIGKILL)
	<-r.done
	eventually(t, "runs_paused back to its level", func() bool { return counter(t, metricRunsPaused) == pausedBefore })
	r.mu.Lock()
	paused := r.paused
	r.mu.Unlock()
	if paused {
		t.Error("run still marked paused after its process died")
	}
}

// A new process of the same run (a restart) starts with no demand and no
// paused time: the old viewer positions describe the old process's output.
func TestRestartResetsDemand(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 20*time.Second)
	r.noteDemand(40)
	r.pausedFor = time.Minute
	startFakeProcess(t, r, "true")
	<-r.done
	r.mu.Lock()
	demand, pausedFor := r.demand, r.pausedFor
	r.mu.Unlock()
	if demand != -1 || pausedFor != 0 {
		t.Errorf("after a new process: demand %d pausedFor %v, want -1 and 0", demand, pausedFor)
	}
}

func TestActiveSpeed(t *testing.T) {
	if s, ok := activeSpeed(600, 10*time.Minute, 8*time.Minute); !ok || s != 5 {
		t.Errorf("600 s of media in 2 active minutes = %v %v, want 5x", s, ok)
	}
	if _, ok := activeSpeed(600, 10*time.Minute, 0); ok {
		t.Error("a run never frozen keeps FFmpeg's own speed")
	}
	if _, ok := activeSpeed(600, time.Minute, 2*time.Minute); ok {
		t.Error("paused longer than wall time is nonsense, keep FFmpeg's speed")
	}
}

// The end-of-run line and the speed metric use the active speed.
func TestRunEndSpeedExcludesPausedTime(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	r := pacedRun(t, 0)
	if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte("frame= 1 fps=1 q=0 size=N/A time=00:10:00.00 bitrate=N/A speed=1.0x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, r, "sleep", "0.3")
	r.mu.Lock()
	r.pausedFor = 200 * time.Millisecond // as if pace had held it
	r.mu.Unlock()
	<-r.done
	for _, e := range hook.AllEntries() {
		if e.Message != "run: ffmpeg ended" {
			continue
		}
		speed, _ := e.Data["speed"].(float64)
		// 600 s of media over ~0.1-0.2 s active: thousands, not FFmpeg's 1.0x.
		if speed < 1000 {
			t.Errorf("speed %v: paused time was not excluded", speed)
		}
		if _, ok := e.Data["paused"]; !ok {
			t.Error("the end line must carry the paused time")
		}
		return
	}
	t.Error("no end line")
}

// Every segment request tells the session's run how far the viewer is.
func TestSegmentRequestFeedsDemand(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sess := NewSession(SessionConfig{ID: "demand", HashDir: dir, RunMgr: runMgr})
	r := newCompletedRun(t, dir) // not running: the handler answers at once
	sess.run = r
	w := httptest.NewRecorder()
	(&Web{}).sessionSegmentHandler(w, httptest.NewRequest(http.MethodGet, "/session/demand/v0-1080-17.ts", nil), sess, "v0-1080-17.ts")
	if r.demand != 17 {
		t.Errorf("demand = %d after a request for segment 17", r.demand)
	}
	(&Web{}).sessionSegmentHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/session/demand/a0-12.ts", nil), sess, "a0-12.ts")
	if r.demand != 17 {
		t.Errorf("an audio request behind the video must not pull demand back: %d", r.demand)
	}
}

// -reconnect goes on the input: it is an input (http protocol) option.
func TestReconnectIsAnInputOption(t *testing.T) {
	p := ffmpegParams(t, testHLS(testStream(0, "video", "h264")))
	in := indexOf(p, "-i")
	for _, opt := range []string{"-reconnect", "-reconnect_on_network_error", "-reconnect_delay_max"} {
		if i := indexOf(p, opt); i < 0 || i > in {
			t.Errorf("%s must come before -i: %v", opt, p)
		}
	}
}
