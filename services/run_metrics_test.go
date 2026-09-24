package services

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFailureCause(t *testing.T) {
	cases := []struct{ tail, exit, want string }{
		{"[sost#3:0/webvtt @ 0x1] Subtitle encoding currently only possible from text to text or bitmap to bitmap\nError opening output files: Invalid argument", ffmpegExitError, failureSubtitleBitmap},
		{"[vist#0:1/none] Decoding requested, but no decoder found for: none\nError opening output files: Invalid argument", ffmpegExitError, failureNoDecoder},
		{"[aost#6:0/libfdk_aac] Non-monotonic DTS; previous: 171675, current: 168030; Error submitting a packet to the muxer: Invalid argument\nConversion failed!", ffmpegExitError, failureTimestamps},
		{"http://src/f.avi: Invalid data found when processing input", ffmpegExitError, failureInvalidData},
		{"[http @ 0x1] HTTP error 404 Not Found\nhttp://src/f.mkv: Server returned 404 Not Found", ffmpegExitError, failureSource},
		{"[in#0/matroska @ 0x1] Error during demuxing: I/O error", ffmpegExitError, failureSource},
		{"frame=  100 fps= 25 q=28.0 size=256KiB time=00:00:04.00 bitrate=524.3kbits/s speed=1.0x", ffmpegExitSignal, failureSignal},
		{"Conversion failed!", ffmpegExitError, failureOther},
	}
	for _, c := range cases {
		if got := failureCause(c.tail, c.exit); got != c.want {
			t.Errorf("failureCause(%q) = %s, want %s", c.tail, got, c.want)
		}
	}
}

func TestLastProgress(t *testing.T) {
	tail := "Input #0\nframe=  10 fps=0.0 q=0.0 size=N/A time=00:00:00.40 bitrate=N/A speed=N/A\nframe= 1500 fps= 17 q=28.0 size=N/A time=00:01:02.50 bitrate=N/A speed=0.69x\n[out] something after"
	media, speed, ok := lastProgress(tail)
	if !ok || media != 62.5 || speed != 0.69 {
		t.Errorf("lastProgress = %v %v %v, want 62.5 0.69 true", media, speed, ok)
	}
	if _, _, ok := lastProgress("frame= 1 fps=0.0 q=0.0 size=N/A time=00:00:00.04 bitrate=N/A speed=N/A"); ok {
		t.Error("speed=N/A must not parse")
	}
	// Audio-only runs print size= lines.
	if media, speed, ok := lastProgress("size=   12288KiB time=01:02:03.00 bitrate= 128.0kbits/s speed= 235x"); !ok || media != 3723 || speed != 235 {
		t.Errorf("audio progress = %v %v %v", media, speed, ok)
	}
}

// The first-segment clock reads the primary playlist's appearance. A restart
// reuses the run dir, so the previous process's playlist is already there:
// it must not count as this process's first segment.
func TestRunFirstSegmentMetric(t *testing.T) {
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "audio", "aac"))
	r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "http://src", h)
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(r.outputDir, "v0-1080.m3u8.ffmpeg")
	if err := os.WriteFile(playlist, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(playlist, old, old); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"mode": runModeCopy, "start": runStartZero}
	before := histogramCount(t, "transcoder_run_first_segment_seconds", labels)

	startFakeProcess(t, r, "sleep", "30")
	defer r.Stop()
	time.Sleep(3 * firstSegmentPoll)
	if got := histogramCount(t, "transcoder_run_first_segment_seconds", labels) - before; got != 0 {
		t.Fatalf("a stale playlist counted as the first segment (%d)", got)
	}
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for histogramCount(t, "transcoder_run_first_segment_seconds", labels)-before != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the fresh playlist was not counted")
		}
		time.Sleep(firstSegmentPoll)
	}
}

// Speed is recorded when a run ends, only once it has 30 s of media behind
// it, under the run's mode.
func TestRunSpeedMetric(t *testing.T) {
	h := testHLS(testStream(0, "video", "hevc"), testStream(1, "audio", "aac"))
	labels := map[string]string{"mode": runModeReencode}
	for _, c := range []struct {
		progress string
		want     uint64
	}{
		{"frame= 1500 fps= 17 q=28.0 size=N/A time=00:01:02.50 bitrate=N/A speed=0.69x\n", 1},
		{"frame=  200 fps= 17 q=28.0 size=N/A time=00:00:08.00 bitrate=N/A speed=0.69x\n", 0},
	} {
		r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "http://src", h)
		if err := os.MkdirAll(r.outputDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte(c.progress), 0644); err != nil {
			t.Fatal(err)
		}
		before := histogramCount(t, "transcoder_run_speed", labels)
		startFakeProcess(t, r, "true")
		<-r.done
		if got := histogramCount(t, "transcoder_run_speed", labels) - before; got != c.want {
			t.Errorf("%q: %d speed samples, want %d", c.progress, got, c.want)
		}
	}
}

// Every failed run is counted by cause (the log line is deduplicated, the
// counter is not); runs we stopped are not failures.
func TestFFmpegFailuresMetric(t *testing.T) {
	cause := func() float64 { return counter(t, metricFFmpegFailuresTotal.WithLabelValues(failureSubtitleBitmap)) }
	for _, c := range []struct {
		name string
		cmd  []string
		stop bool
		want float64
	}{
		{"failed", []string{"false"}, false, 1},
		{"failed again", []string{"false"}, false, 1},
		{"stopped by us", []string{"sleep", "30"}, true, 0},
	} {
		r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "", nil)
		if err := os.MkdirAll(r.outputDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte("Subtitle encoding currently only possible from text to text or bitmap to bitmap\n"), 0644); err != nil {
			t.Fatal(err)
		}
		before := cause()
		startFakeProcess(t, r, c.cmd[0], c.cmd[1:]...)
		if c.stop {
			r.Stop()
		}
		<-r.done
		if got := cause() - before; got != c.want {
			t.Errorf("%s: failures_total{subtitle_bitmap} delta %v, want %v", c.name, got, c.want)
		}
	}
}
