package services

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
// (ffmpegSeekStart) every B-frame case was 83 ms early.
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
	} {
		t.Run(fmt.Sprintf("%s@%v", c.file, c.seek), func(t *testing.T) {
			off, seg := realCopySeek(t, srv.URL+"/"+c.file, c.seek+5)
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

// realCopySeek opens an old-route session on src, seeks it to t and waits
// for the run to finish; it returns the offset the seek answered and the
// first video segment of the run.
func realCopySeek(t *testing.T, src string, at float64) (float64, string) {
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
	w = serve(http.MethodPost, fmt.Sprintf("/session/%s/seek?t=%v&api-key=K&token=T", created.ID, at))
	var seek struct {
		Offset *float64 `json:"offset"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &seek) != nil || seek.Offset == nil {
		t.Fatalf("seek: %d %s", w.Code, w.Body.String())
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
	name := strings.TrimSuffix(sess.h.primary[0].GetPlaylistName(), ".m3u8") + "-0.ts"
	return *seek.Offset, filepath.Join(sess.run.OutputDir(), name)
}

// firstFrameMovieTime finds the first video frame of seg in source by its
// decoded MD5 and returns its movie time (frame n at 24 fps from the file's
// start) and its PTS in seg.
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
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time",
		"-read_intervals", "%+#1", "-of", "csv=p=0", seg).Output()
	if err != nil {
		t.Fatal(err)
	}
	pts, err = strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(out)), ","), 64)
	if err != nil {
		t.Fatalf("first packet of %s: %q", seg, out)
	}
	return float64(n) / 24, pts
}
