package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	cp "github.com/webtor-io/content-prober/content-prober"
)

func TestThreadsFromCPUMax(t *testing.T) {
	for in, want := range map[string]int{
		"100000 100000\n": 1,
		"150000 100000":   2,
		"200000 100000":   2,
		"max 100000":      0,
		"-1 100000":       0, // cgroup v1: no quota
		"garbage":         0,
		"":                0,
	} {
		if got := threadsFromCPUMax(in); got != want {
			t.Errorf("threadsFromCPUMax(%q) = %d, want %d", in, got, want)
		}
	}
}

func hlsWithThreads(threads int, streams ...*cp.Stream) *HLS {
	return NewHLS("http://source/file.mkv", &cp.ProbeReply{Streams: streams}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac", threads: threads})
}

func indexOf(params []string, v string) int {
	for i, p := range params {
		if p == v {
			return i
		}
	}
	return -1
}

// FFmpeg sized its pools from the node's 32 cores under a 1-CPU quota. With
// a quota the decoders (-threads before -i), the filter graphs and the x264
// encoder get that many threads.
func TestGetFFmpegParamsThreads(t *testing.T) {
	h := hlsWithThreads(1, testStream(0, "video", "hevc"), testStream(1, "audio", "aac"))
	p := ffmpegParams(t, h)
	in := indexOf(p, "-i")
	joined := strings.Join(p, " ")
	if ft := indexOf(p, "-filter_threads"); ft < 0 || ft > in || p[ft+1] != "1" {
		t.Errorf("-filter_threads 1 must come before -i: %s", joined)
	}
	if th := indexOf(p, "-threads"); th < 0 || th > in || p[th+1] != "1" {
		t.Errorf("decoder -threads 1 must come before -i: %s", joined)
	}
	if !strings.Contains(joined, "-pix_fmt yuv420p -threads 1") {
		t.Errorf("x264 encoder must get -threads 1: %s", joined)
	}

	// No quota: FFmpeg decides, as before.
	if p := ffmpegParams(t, hlsWithThreads(0, testStream(0, "video", "hevc"))); indexOf(p, "-threads") >= 0 || indexOf(p, "-filter_threads") >= 0 {
		t.Errorf("threads 0 must leave -threads out: %v", p)
	}
	// A copied video has no encoder to size; still a copy.
	h = hlsWithThreads(1, testStream(0, "video", "h264"))
	if !h.primary[0].IsCopy() || strings.Contains(strings.Join(ffmpegParams(t, h), " "), "-c:v copy -threads") {
		t.Error("h264 must stay a copy without encoder threads")
	}
}

func TestGetFFmpegParamsEncodeAudio(t *testing.T) {
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "audio", "aac"))
	if j := strings.Join(ffmpegParams(t, h), " "); !strings.Contains(j, "-c:a copy") {
		t.Fatalf("stereo AAC is copied by default: %s", j)
	}
	p, err := h.GetFFmpegParamsWith("/out", ParamOptions{EncodeAudio: true})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(p, " ")
	if !strings.Contains(j, "-c:a libfdk_aac -ac 2") || !strings.Contains(j, "-break_non_keyframes 1 -c:a") {
		t.Errorf("EncodeAudio must encode the AAC: %s", j)
	}
	if !strings.Contains(j, "-c:v copy") {
		t.Errorf("EncodeAudio must leave the video alone: %s", j)
	}
}

// A copied AAC the mpegts muxer cannot wrap failed every run of the source
// (m4b audiobooks). After the first such failure the run encodes the audio,
// and the manager remembers it for the source's other runs (seeks).
func TestADTSFailureSwitchesToEncodedAudio(t *testing.T) {
	dir := t.TempDir()
	m := NewRunManager()
	defer m.CloseAll()
	h := testHLS(testStream(0, "audio", "aac"))
	m.mu.Lock()
	r := m.newRunLocked(runKey(dir, 0), dir, 0, "http://src/book.m4b", h)
	m.mu.Unlock()
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	stderr := "[adts @ 0x7fe7e07c90c0] Scalable configurations are not allowed in ADTS\n[out#0/segment @ 0x1] Could not write header (incorrect codec parameters ?): Invalid data found when processing input\nConversion failed!\n"
	if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte(stderr), 0644); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, r, "false")
	<-r.done
	r.mu.Lock()
	got := r.fallbacks.EncodeAudio
	r.mu.Unlock()
	if !got {
		t.Fatal("the run must encode the audio after an ADTS failure")
	}
	m.mu.Lock()
	seek := m.newRunLocked(runKey(dir, 600), dir, 600, "http://src/book.m4b", h)
	m.mu.Unlock()
	if !seek.fallbacks.EncodeAudio {
		t.Error("a later run of the same source must start with encoded audio")
	}
	m.mu.Lock()
	other := m.newRunLocked(runKey(t.TempDir(), 0), t.TempDir(), 0, "http://src/other.m4b", h)
	m.mu.Unlock()
	if other.fallbacks.EncodeAudio {
		t.Error("another source must keep copying")
	}
}

// The segment muxer prints two lines per output on its way out; a run with
// a dozen outputs pushed the error out of the 15-line tail.
func TestStderrTailDropsMuxerNoise(t *testing.T) {
	var b strings.Builder
	b.WriteString("[in#0/matroska @ 0x1] Error during demuxing: I/O error\n")
	for i := 0; i < 20; i++ {
		b.WriteString("[segment @ 0x1] Opening '/webtor/data1/x/runs/seek-0.000/s1.m3u8.ffmpeg.tmp' for writing\n")
		b.WriteString("[out#4/segment @ 0x1] video:0KiB audio:0KiB subtitle:5KiB other streams:0KiB global headers:0KiB muxing overhead: unknown\n")
	}
	b.WriteString("Conversion failed!\n")
	path := filepath.Join(t.TempDir(), "ffmpeg.err")
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if got := stderrTail(path); !strings.Contains(got, "Error during demuxing") {
		t.Errorf("the error was pushed out by muxer noise:\n%s", got)
	}
}

// Every process end logs one line with its numbers, for joining a run with
// how its source was served.
func TestRunEndLogLine(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	h := testHLS(testStream(0, "video", "hevc"), testStream(1, "audio", "aac"))
	r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "http://src/abc/f.mkv?api-key=secret&token=t", h)
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte("frame= 1500 fps= 17 q=28.0 size=N/A time=00:01:02.50 bitrate=N/A speed=0.69x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, r, "true")
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("not reaped")
	}
	for _, e := range hook.AllEntries() {
		if e.Message != "run: ffmpeg ended" {
			continue
		}
		if e.Level != log.InfoLevel || e.Data["mode"] != runModeReencode || e.Data["outcome"] != runOutcomeFinished || e.Data["speed"] != 0.69 || e.Data["media"] != "62.5" {
			t.Errorf("unexpected fields: %v", e.Data)
		}
		if s, _ := e.Data["source"].(string); strings.Contains(s, "secret") {
			t.Errorf("source not redacted: %s", s)
		}
		return
	}
	t.Error("no run: ffmpeg ended line")
}

// -xerror made FFmpeg's timestamp repairs fatal on every start of some
// sources (seek runs, without -xerror, played them). After such a failure
// the source runs without -xerror; everything else keeps it.
func TestTimestampsFailureDropsXerrorForTheSource(t *testing.T) {
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "audio", "aac"))
	if indexOf(ffmpegParams(t, h), "-xerror") < 0 {
		t.Fatal("-xerror is the default")
	}
	p, err := h.GetFFmpegParamsWith("/out", ParamOptions{Lenient: true})
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(p, "-xerror") >= 0 {
		t.Fatal("Lenient must drop -xerror")
	}

	dir := t.TempDir()
	m := NewRunManager()
	defer m.CloseAll()
	m.mu.Lock()
	r := m.newRunLocked(runKey(dir, 0), dir, 0, "http://src/f.avi", h)
	m.mu.Unlock()
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	stderr := "[vost#0:0/copy @ 0x1] Non-monotonic DTS; previous: 171675, current: 168030; Error submitting a packet to the muxer: Invalid argument\nConversion failed!\n"
	if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte(stderr), 0644); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, r, "false")
	<-r.done
	r.mu.Lock()
	got := r.fallbacks
	r.mu.Unlock()
	if !got.Lenient || got.EncodeAudio {
		t.Fatalf("fallbacks = %+v, want Lenient only", got)
	}
	m.mu.Lock()
	next := m.newRunLocked(runKey(dir, 0), dir, 0, "http://src/f.avi", h)
	m.mu.Unlock()
	if !next.fallbacks.Lenient {
		t.Error("the source's next run must start without -xerror")
	}

	// Another failure (not timestamps) changes nothing.
	dir2 := t.TempDir()
	m.mu.Lock()
	r2 := m.newRunLocked(runKey(dir2, 0), dir2, 0, "http://src/g.mkv", h)
	m.mu.Unlock()
	if err := os.MkdirAll(r2.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r2.outputDir, "ffmpeg.err"), []byte("Error during demuxing: I/O error\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startFakeProcess(t, r2, "false")
	<-r2.done
	if r2.fallbacks != (ParamOptions{}) {
		t.Errorf("an I/O failure must not relax -xerror: %+v", r2.fallbacks)
	}
}
