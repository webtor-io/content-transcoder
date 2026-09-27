package services

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mediaPlaylist is FFmpeg's playlist for a stream: n segments of dur
// seconds each, named <stream>-<i>.m4s.
type mediaPlaylist struct {
	stream string // "v0-2160", "a0"
	dur    float64
	n      int
}

func (p mediaPlaylist) write(t *testing.T, dir string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	b.WriteString(`#EXT-X-MAP:URI="` + p.stream + `-init-0a12b34c56d78e90.mp4"` + "\n")
	for i := 0; i < p.n; i++ {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s-%d.m4s\n", p.dur, p.stream, i)
	}
	// Written whole, like hlsenc with temp_file.
	tmp := filepath.Join(dir, p.stream+".m3u8.ffmpeg.tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, p.stream+".m3u8.ffmpeg")); err != nil {
		t.Fatal(err)
	}
}

// passthroughRun is a passthrough run of a 4K HEVC source with AAC audio,
// its output dir created.
func passthroughRun(t *testing.T, lead time.Duration) *TranscodeRun {
	t.Helper()
	h := hevcHLS(t, 3840, 2160, true, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", paceLead: lead})
	dir := t.TempDir()
	r := newTranscodeRun(runKeyFor(dir, h, 0), dir, 0, "http://src/movie.mkv", h)
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSegmentStreamPlaylist(t *testing.T) {
	for in, want := range map[string]string{
		"v0-2160-12.m4s": "v0-2160.m3u8 12",
		"a1-3.m4s":       "a1.m3u8 3",
		"s0-7.vtt":       "s0.m3u8 7",
		"v0-720-0.ts":    "v0-720.m3u8 0",
	} {
		s, n, ok := segmentStreamPlaylist(in)
		if got := fmt.Sprintf("%s %d", s, n); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"v0-2160-init-0a12b34c56d78e90.mp4", "index.m3u8", "../../etc/passwd-1.ts", "x0-1.ts", "v0.m3u8", "v0-1080-12345678901234567890.ts"} {
		if _, _, ok := segmentStreamPlaylist(in); ok {
			t.Errorf("%s read as a segment", in)
		}
	}
}

func TestParseMediaPlaylist(t *testing.T) {
	data := "#EXTM3U\n#EXT-X-MAP:URI=\"v0-2160-init-00.mp4\"\n#EXTINF:10.010000,\nv0-2160-0.m4s\n#EXTINF:8.5,\nv0-2160-1.m4s?token=x\n#EXT-X-DISCONTINUITY\n#EXTINF:4,title\n/abs/v0-2160-2.m4s\n#EXTINF:bad,\nv0-2160-3.m4s\n#EXTINF:2.0,\n"
	segs, total := parseMediaPlaylist([]byte(data))
	want := []mediaSegment{{0, 0, 10.01}, {1, 10.01, 8.5}, {2, 18.51, 4}, {3, 22.51, 0}}
	if len(segs) != len(want) {
		t.Fatalf("segments %+v", segs)
	}
	for i := range want {
		if segs[i].n != want[i].n || math.Abs(segs[i].start-want[i].start) > 1e-9 || segs[i].dur != want[i].dur {
			t.Errorf("segment %d: %+v, want %+v", i, segs[i], want[i])
		}
	}
	// A trailing EXTINF without its URI is a segment not written yet.
	if math.Abs(total-22.51) > 1e-9 {
		t.Errorf("total %v", total)
	}
}

// The blocker the review found, as a simulation: the video of a passthrough
// run is copied and cut at its 10 s keyframes, the audio every 4 s, and the
// player asks for both about 30 s ahead of the playhead. FFmpeg copies at
// 20x while it is let go. Wherever the viewer is -- the first minutes, or
// half an hour in -- FFmpeg is frozen within a GOP of 5 minutes ahead of
// what was asked for. Counting segment numbers instead (the pacing of the
// old route) freezes it at 2.5(t+30)+750 s of media: 14 min ahead at the
// start, an hour at t = 30 min.
func TestMediaPace_LeadStaysInMediaTime(t *testing.T) {
	const (
		gop      = 10.0
		audioSeg = 4.0
		buffer   = 30.0
		speed    = 20.0 // media seconds FFmpeg copies per wall second
		lead     = 300.0
	)
	resumeLead := lead - paceResumeGap.Seconds()
	r := passthroughRun(t, 5*time.Minute)
	video := mediaPlaylist{stream: "v0-2160", dur: gop}
	audio := mediaPlaylist{stream: "a0", dur: audioSeg}
	produced := 0.0 // media FFmpeg has copied, both streams
	asked := 0.0    // the start of the furthest segment requested, any stream
	frozen := false
	worst := map[string]float64{}
	freezes := map[string]int{}
	for playhead := 0.0; playhead <= 1860; playhead++ {
		if !frozen {
			produced += speed
		}
		// Closed segments: a GOP closes when the next keyframe arrives.
		if n := int(produced / gop); n != video.n {
			video.n = n
			video.write(t, r.outputDir)
		}
		if n := int(produced / audioSeg); n != audio.n {
			audio.n = n
			audio.write(t, r.outputDir)
		}
		// The player's requests: the segment holding playhead+buffer of
		// each stream, once it is listed.
		want := playhead + buffer
		if n := int(want / gop); n < video.n {
			r.noteMediaDemand(fmt.Sprintf("v0-2160-%d.m4s", n))
			asked = math.Max(asked, float64(n)*gop)
		}
		if n := int(want / audioSeg); n < audio.n {
			r.noteMediaDemand(fmt.Sprintf("a0-%d.m4s", n))
			asked = math.Max(asked, float64(n)*audioSeg)
		}
		freeze := mediaPaceWantsFrozen(frozen, r.readMediaPace(), lead, resumeLead)
		if freeze && !frozen {
			phase := "start"
			if playhead >= 1800 {
				phase = "30min"
			}
			freezes[phase]++
			// Measured on the simulation's own clock, not on what the
			// pace loop read.
			if ahead := float64(video.n)*gop - asked; ahead > worst[phase] {
				worst[phase] = ahead
			}
		}
		frozen = freeze
	}
	t.Logf("frozen ahead of the furthest request: start %.0f s, at 30 min %.0f s (%d, %d freezes)", worst["start"], worst["30min"], freezes["start"], freezes["30min"])
	for _, phase := range []string{"start", "30min"} {
		if freezes[phase] == 0 {
			t.Fatalf("%s: never frozen", phase)
		}
		// Frozen as soon as production is lead past demand: at most one GOP
		// of copying (speed per poll here) beyond it.
		if w := worst[phase]; w < lead || w > lead+speed {
			t.Errorf("%s: frozen %.0f s ahead of the furthest request, want %.0f..%.0f", phase, w, lead, lead+speed)
		}
	}
}

// The same rule on a real process: frozen when production is the lead past
// the furthest request of any stream in media time, let go when it falls
// within the resume lead, with the passthrough mode on the metrics.
func TestMediaPace_FreezesAndReleasesTheProcess(t *testing.T) {
	fastPacing(t) // resume 8 s closer than the lead
	r := passthroughRun(t, 40*time.Second)
	pausedBefore := counter(t, metricRunsPaused)
	pauseSec0 := counter(t, metricRunPauseSeconds.WithLabelValues(runModePassthrough))

	startFakeProcess(t, r, "sleep", "30")
	defer r.Stop()
	pid := r.cmd.Process.Pid

	video := mediaPlaylist{stream: "v0-2160", dur: 10}
	audio := mediaPlaylist{stream: "a0", dur: 4}
	video.n, audio.n = 3, 8 // 30 s and 32 s: under the lead of 40
	video.write(t, r.outputDir)
	audio.write(t, r.outputDir)
	never(t, "frozen under the lead", func() bool { return stopped(t, pid) })

	video.n = 4 // 40 s: the lead past demand 0
	video.write(t, r.outputDir)
	eventually(t, "frozen at the lead", func() bool { return stopped(t, pid) })

	// Audio segment 5 starts at 20 s: 40 < 20+32, let go. Audio numbers
	// are 2.5x video numbers here; by number, 5 would have been video
	// segment 5 (50 s) and nothing moves.
	r.noteMediaDemand("a0-2.m4s") // 8 s: 40 >= 8+32, stays
	never(t, "let go before a viewer is within the resume lead", func() bool { return !stopped(t, pid) })
	r.noteMediaDemand("a0-5.m4s")
	eventually(t, "let go", func() bool { return !stopped(t, pid) })
	if got := counter(t, metricRunsPaused) - pausedBefore; got != 0 {
		t.Errorf("runs_paused +%v after letting go", got)
	}
	if got := counter(t, metricRunPauseSeconds.WithLabelValues(runModePassthrough)) - pauseSec0; got <= 0 {
		t.Error("run_pause_seconds_total{passthrough} did not grow")
	}

	// A request past what is listed is a viewer at the stream's edge.
	r.noteMediaDemand("v0-2160-9.m4s")
	video.n = 6 // 60 s: segment 9 not listed, demand is the edge, 60
	video.write(t, r.outputDir)
	never(t, "frozen while the viewer waits at the edge", func() bool { return stopped(t, pid) })
	// Listed now, segment 9 of the 10 s video is a viewer at 90 s, not at
	// segment 9 of 4 s (36 s).
	video.n = 12 // 120 s < 90 + 40
	video.write(t, r.outputDir)
	never(t, "frozen short of the lead past the video request", func() bool { return stopped(t, pid) })
	video.n = 13 // 130 s = 90 + 40
	video.write(t, r.outputDir)
	eventually(t, "frozen again", func() bool { return stopped(t, pid) })
}

// Resumes are timed and stalls counted under mode passthrough, by finished
// primary segments.
func TestMediaPace_ResumeMetrics(t *testing.T) {
	fastPacing(t)
	orig := paceResumeStallMedia
	paceResumeStallMedia = 200 * time.Millisecond
	t.Cleanup(func() { paceResumeStallMedia = orig })
	labels := map[string]string{"mode": runModePassthrough}
	seg0 := histogramCount(t, "transcoder_run_resume_segment_seconds", labels)
	stalls0 := counter(t, metricRunResumeStalls.WithLabelValues(runModePassthrough))
	copyStalls0 := counter(t, metricRunResumeStalls.WithLabelValues(runModeCopy))

	r := passthroughRun(t, 40*time.Second)
	startFakeProcess(t, r, "sleep", "30")
	defer r.Stop()
	pid := r.cmd.Process.Pid
	video := mediaPlaylist{stream: "v0-2160", dur: 10, n: 4}
	video.write(t, r.outputDir)
	eventually(t, "frozen", func() bool { return stopped(t, pid) })
	r.noteMediaDemand("v0-2160-1.m4s") // 10 s: 40 < 10+32
	eventually(t, "let go", func() bool { return !stopped(t, pid) })
	eventually(t, "the stall counted", func() bool {
		return counter(t, metricRunResumeStalls.WithLabelValues(runModePassthrough))-stalls0 == 1
	})
	video.n = 5
	video.write(t, r.outputDir)
	eventually(t, "the late segment timed", func() bool {
		return histogramCount(t, "transcoder_run_resume_segment_seconds", labels) == seg0+1
	})
	if got := counter(t, metricRunResumeStalls.WithLabelValues(runModeCopy)) - copyStalls0; got != 0 {
		t.Errorf("a passthrough stall was counted for copy (+%v)", got)
	}
}

// Only passthrough runs keep media demand; the old route's runs and their
// segment pacing do not see it. And the segment handler feeds it.
func TestMediaDemand_OnlyPassthrough(t *testing.T) {
	old := pacedRun(t, 20*time.Second)
	old.noteMediaDemand("v0-1080-3.ts")
	if old.mediaDemand != nil {
		t.Errorf("old-route run keeps media demand %v", old.mediaDemand)
	}

	r := passthroughRun(t, 5*time.Minute)
	for _, f := range []string{"a0-7.m4s", "a0-3.m4s", "v0-2160-2.m4s", "a1-9.m4s", "s0-4.vtt", "v0-2160-init-0a12b34c56d78e90.mp4"} {
		r.noteMediaDemand(f)
	}
	// a1 and s0 are not streams of this run; the init is not a segment.
	if fmt.Sprint(r.mediaDemand) != "map[a0.m3u8:7 v0-2160.m3u8:2]" {
		t.Errorf("media demand %v", r.mediaDemand)
	}

	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sess := NewSession(SessionConfig{ID: "pt", HashDir: dir, HLS: r.h, RunMgr: runMgr})
	r.completed = true // answers at once
	sess.run = r
	if err := os.WriteFile(filepath.Join(r.outputDir, "a0-40.m4s"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	(&Web{}).sessionSegmentHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/session/pt/a0-40.m4s", nil), sess, "a0-40.m4s")
	if r.mediaDemand["a0.m3u8"] != 40 {
		t.Errorf("the segment handler did not feed media demand: %v", r.mediaDemand)
	}
}

// A new process of the run starts with no media demand.
func TestMediaDemand_ResetOnRestart(t *testing.T) {
	fastPacing(t)
	r := passthroughRun(t, 5*time.Minute)
	r.noteMediaDemand("a0-7.m4s")
	startFakeProcess(t, r, "true")
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mediaDemand != nil {
		t.Errorf("media demand survived the restart: %v", r.mediaDemand)
	}
}
