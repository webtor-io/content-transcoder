package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	cp "github.com/webtor-io/content-prober/content-prober"
	"github.com/webtor-io/lazymap"
)

// TestCopyRoute_RealFFmpegStart seeks old-route copy sessions through the
// real handlers with the real FFmpeg (content-prober's local probe, the
// real-start probe, the run) on the sources of e2e/seek/gen.sh in
// COPY_SEEK_MEDIA, served over HTTP, and holds the offset the seek answers
// to the run's first video frame: the offset is that frame's movie time
// (it is found in the source by its decoded MD5; frame n is n/24 s from
// the file's start), which hls.js plays at media time 0. Run it in the
// production image:
//
//	GOOS=linux GOARCH=<docker's> go test -c -o ct.test ./services
//	docker run --rm -v $PWD:/w -e COPY_SEEK_MEDIA=/w/media --entrypoint /w/ct.test \
//	  jrottenberg/ffmpeg:8-alpine -test.run TestCopyRoute_RealFFmpegStart -test.v
//
// Negative control: with ffprobe as the copy route's probe (before this
// test) the offsets were 30.000, 29.917, 59.833, 89.875, 25.000, 30.000 and
// 30.000, five of them 5-30 s off; with the first packet's DTS
// (ffmpegSeekStart) every B-frame case was 83 ms early; with the guard that
// refused an answer after the seek point the MPEG-TS cases reported 30.000
// and 60.000 for runs starting at 35.021 and 65.021.
func TestCopyRoute_RealFFmpegStart(t *testing.T) {
	media := os.Getenv("COPY_SEEK_MEDIA")
	if media == "" {
		t.Skip("COPY_SEEK_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	for _, c := range []struct {
		file string
		seek float64 // quantized; the request asks 5 s later
		want float64 // the offset: the first frame's movie time
	}{
		// Keyframes every 10 s, two frames of B-frame delay: FFmpeg's seek
		// to 30 (3/23 s back first) lands on 20.000.
		{"kf10_bf3.mkv", 30, 20},
		// A keyframe at 29.917, inside those 3/23 s: 20.000 again.
		{"kfwin.mkv", 30, 20},
		// At 59.833, outside them: that keyframe.
		{"kfwin.mkv", 60, 59.833},
		// At 89.875, inside: 80.000.
		{"kfwin.mkv", 90, 80},
		// kf10_bf3 remuxed to start at 5 s: the same movie time.
		{"kf10_bf3_st5.mkv", 30, 20},
		// No B-frames, no heuristic: the keyframe at 30.
		{"kf10_bf0.mkv", 30, 30},
		// The only keyframe before 30 is the first.
		{"kf0_30.mkv", 30, 0},
		// MPEG-TS, keyframes at 5, 15, 25, 35...: no index, FFmpeg's seek
		// lands on the next keyframe after the seek point. The video starts
		// 21.3 ms after the file (the AAC's priming), so frame n is movie
		// n/24 + 0.021.
		{"kf5.ts", 30, 35.021},
		{"kf5.ts", 60, 65.021},
	} {
		t.Run(fmt.Sprintf("%s@%v", c.file, c.seek), func(t *testing.T) {
			off, run := realCopySeek(t, srv.URL+"/"+c.file, c.seek+5)
			seg := filepath.Join(run.dir, run.video+"-0.ts")
			if math.Abs(off-c.want) > 0.001 {
				t.Errorf("offset %.3f, want %.3f", off, c.want)
			}
			movie, pts := firstFrameMovieTime(t, filepath.Join(media, c.file), seg)
			if d := off - movie; math.Abs(d) > 0.001 {
				t.Errorf("offset %.3f, the first frame is movie %.3f (off by %+.3f s)", off, movie, d)
			}
			t.Logf("offset %.3f, first frame movie %.3f at PTS %.3f", off, movie, pts)
		})
	}
}

// copyRunFiles names a finished run's output: its directory and the
// output names (without extension) of the video and of the first subtitle
// track ("" without one).
type copyRunFiles struct {
	dir, video, subs string
}

// realCopySeek opens an old-route session on src, seeks it to at (a
// negative at: no seek, the run from the start) and waits for the run to
// finish; it returns the offset the seek answered (0 without a seek) and
// the run's files.
func realCopySeek(t *testing.T, src string, at float64) (float64, copyRunFiles) {
	t.Helper()
	runMgr := NewRunManager()
	sm := NewSessionManager(runMgr)
	web := &Web{
		output:         t.TempDir(),
		contentProbe:   &ContentProbe{timeout: 30, LazyMap: lazymap.New[*cp.ProbeReply](&lazymap.Config{})},
		hlsBuilder:     &HLSBuilder{aacCodec: "libfdk_aac", threads: 2},
		sessionManager: sm,
		touchMap:       NewTouchMap(),
	}
	web.buildHandler()
	t.Cleanup(func() {
		sm.CloseAll()
		runMgr.CloseAll()
	})
	serve := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("X-Source-Url", src)
		w := httptest.NewRecorder()
		web.handler.ServeHTTP(w, r)
		return w
	}
	w := serve(http.MethodPost, "/session?api-key=K&token=T")
	var created struct {
		ID string `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	offset := 0.0
	if at >= 0 {
		w = serve(http.MethodPost, fmt.Sprintf("/session/%s/seek?t=%v&api-key=K&token=T", created.ID, at))
		var seek struct {
			Offset *float64 `json:"offset"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &seek) != nil || seek.Offset == nil {
			t.Fatalf("seek: %d %s", w.Code, w.Body.String())
		}
		offset = *seek.Offset
	}
	sess := sm.Get(created.ID)
	for i := 0; i < 600 && !sess.runIsCompleted(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !sess.runIsCompleted() {
		t.Fatal("the run did not finish the source")
	}
	if !sess.h.primary[0].IsCopy() {
		t.Fatal("not the copy route")
	}
	files := copyRunFiles{dir: sess.run.OutputDir(), video: strings.TrimSuffix(sess.h.primary[0].GetPlaylistName(), ".m3u8")}
	if len(sess.h.subs) > 0 {
		files.subs = strings.TrimSuffix(sess.h.subs[0].GetPlaylistName(), ".m3u8")
	}
	return offset, files
}

// TestCopyRoute_RealFFmpegCues seeks copy-route sessions with embedded SRT
// (e2e/seek/gen.sh's edge.srt: cues at 1, 21, 26, 28, 33, 41, 61 and 68 s)
// through the real handlers and FFmpeg, and holds every cue the seek run
// serves to its movie time by the offset -- cue start + offset, what
// subtitle-translate stores and the player shows -- with none from before
// the offset. A run that lands on the file's first frame (offset 0.000)
// must serve the run from the start's subtitle playlist and segments byte
// for byte: subtitle-translate takes offset 0 for that run. Negative
// control: without -itsoffset and the cut (before this test) every cue came
// 1 s early on both -- the subtitle output shifted its first cue to 0, 21 s
// over the offset 20.000 and 1 s over 0.000 -- and the playlists differed.
func TestCopyRoute_RealFFmpegCues(t *testing.T) {
	media := os.Getenv("COPY_SEEK_MEDIA")
	if media == "" {
		t.Skip("COPY_SEEK_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	for _, c := range []struct {
		file        string
		offset      float64
		want        []int // movie seconds of the cues the run serves
		sameAsFrom0 bool
	}{
		// Keyframes every 10 s: the seek to 35 (30) lands on 20.
		{"kf10_bf3.mkv", 20, []int{21, 26, 28, 33, 41, 61}, false},
		// Keyframes at 0 and 30 only: on 0.
		{"kf0_30_subs.mkv", 0, []int{1, 21, 26, 28, 33, 41, 61}, true},
	} {
		t.Run(c.file, func(t *testing.T) {
			off, run := realCopySeek(t, srv.URL+"/"+c.file, 35)
			if math.Abs(off-c.offset) > 0.001 {
				t.Fatalf("offset %.3f, want %.3f", off, c.offset)
			}
			cues := servedCues(t, run)
			var got []int
			for _, q := range cues {
				got = append(got, q.movie)
				if d := q.start + off - float64(q.movie); math.Abs(d) > 0.001 {
					t.Errorf("cue at movie %d served at %.3f: %+.3f s off by the offset %.3f", q.movie, q.start, d, off)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("cues served (movie s) %v, want %v", got, c.want)
			}
			if !c.sameAsFrom0 {
				return
			}
			_, from0 := realCopySeek(t, srv.URL+"/"+c.file, -1)
			a, b := subtitleFiles(t, from0), subtitleFiles(t, run)
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("the seek run's subtitle playlist and segments differ from the run from the start's:\nfrom 0: %q\nseek:   %q", a, b)
			}
		})
	}
}

type servedCue struct {
	start float64
	movie int
}

var vttCue = regexp.MustCompile(`(?m)^(?:(\d+):)?(\d\d):(\d\d)\.(\d\d\d) --> .*\n\D*(\d+)`)

// servedCues reads the run's subtitle segments in playlist order: each
// cue's start and the movie second its text names.
func servedCues(t *testing.T, run copyRunFiles) []servedCue {
	t.Helper()
	var cues []servedCue
	for _, seg := range subtitleFiles(t, run)[1:] {
		for _, m := range vttCue.FindAllStringSubmatch(seg, -1) {
			h, _ := strconv.Atoi(m[1])
			mi, _ := strconv.Atoi(m[2])
			sec, _ := strconv.Atoi(m[3])
			ms, _ := strconv.Atoi(m[4])
			movie, _ := strconv.Atoi(m[5])
			cues = append(cues, servedCue{float64(h*3600+mi*60+sec) + float64(ms)/1000, movie})
		}
	}
	return cues
}

// subtitleFiles returns the run's subtitle playlist, then its segments in
// its order.
func subtitleFiles(t *testing.T, run copyRunFiles) []string {
	t.Helper()
	if run.subs == "" {
		t.Fatal("no subtitle track")
	}
	pl, err := os.ReadFile(filepath.Join(run.dir, run.subs+".m3u8.ffmpeg"))
	if err != nil {
		t.Fatal(err)
	}
	res := []string{string(pl)}
	for _, l := range strings.Split(string(pl), "\n") {
		if l == "" || l[0] == '#' {
			continue
		}
		b, err := os.ReadFile(filepath.Join(run.dir, strings.TrimSpace(l)))
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, string(b))
	}
	return res
}

// TestCopyRoute_RealFFmpegTickRounding asks the real FFmpeg where a copy
// seek lands on an AVI whose time base is 1001/24000 and whose keyframes
// are at 0 and 70.9 s: the seek to 60 reaches the packets rounded to the
// nearest tick, the first frame reads -0.018, and the answer is the first
// frame, 0.000; from a seek to 30 it reads +0.010. Negative control: without
// the clamp (ffmpegSeekFirstFrame) the seek to 60 answers -0.018, which
// resolveRealStart throws away for 60.000.
func TestCopyRoute_RealFFmpegTickRounding(t *testing.T) {
	media := os.Getenv("COPY_SEEK_MEDIA")
	if media == "" {
		t.Skip("COPY_SEEK_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	for _, c := range []struct {
		seek, want float64
	}{
		{60, 0},
		{30, 30 - 719*1001.0/24000},
	} {
		got, err := ffmpegSeekFirstFrame(context.Background(), srv.URL+"/avi_tick.avi", "0", c.seek)
		if err != nil || math.Abs(got-c.want) > 1e-6 {
			t.Errorf("seek %v: %.6f %v, want %.6f", c.seek, got, err, c.want)
		}
	}
}

// firstFrameMovieTime finds the first video frame of seg in source by its
// decoded MD5 and returns its movie time -- frame n at 24 fps, from the
// video's start, which is that far into the file (format start_time
// against the video stream's: 21.3 ms in a TS whose AAC starts with
// priming) -- and its PTS in seg.
func firstFrameMovieTime(t *testing.T, source, seg string) (movie, pts float64) {
	t.Helper()
	md5s := func(args ...string) []string {
		out, err := exec.Command("ffmpeg", append([]string{"-nostdin", "-v", "error"}, append(args, "-f", "framemd5", "-")...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		var res []string
		for _, l := range strings.Split(string(out), "\n") {
			if l == "" || l[0] == '#' {
				continue
			}
			f := strings.Split(l, ",")
			res = append(res, strings.TrimSpace(f[len(f)-1]))
		}
		return res
	}
	first := md5s("-i", seg, "-map", "0:v:0", "-frames:v", "1")
	if len(first) != 1 {
		t.Fatalf("no frame in %s", seg)
	}
	n := -1
	for i, m := range md5s("-i", source, "-map", "0:v:0") {
		if m == first[0] {
			n = i
			break
		}
	}
	if n < 0 {
		t.Fatalf("the first frame of %s is not in %s", seg, source)
	}
	starts, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
		"format=start_time:stream=start_time", "-of", "json", source).Output()
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Streams []struct {
			StartTime string `json:"start_time"`
		} `json:"streams"`
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
	}
	if json.Unmarshal(starts, &st) != nil || len(st.Streams) != 1 {
		t.Fatalf("start times of %s: %s", source, starts)
	}
	vs, _ := strconv.ParseFloat(st.Streams[0].StartTime, 64)
	fs, _ := strconv.ParseFloat(st.Format.StartTime, 64)
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time",
		"-read_intervals", "%+#1", "-of", "csv=p=0", seg).Output()
	if err != nil {
		t.Fatal(err)
	}
	pts, err = strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(out)), ","), 64)
	if err != nil {
		t.Fatalf("first packet of %s: %q", seg, out)
	}
	return float64(n)/24 + vs - fs, pts
}
