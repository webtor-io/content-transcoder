package services

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	cp "github.com/webtor-io/content-prober/content-prober"
	"github.com/webtor-io/lazymap"
)

// TestPassthrough_RealFFmpeg plays passthrough sessions through the real
// handlers with the real FFmpeg and ffprobe (content-prober's local probe,
// the source probe, the real-start probe, the run) against the synthetic
// sources in PASSTHROUGH_MEDIA (see its cases), served over HTTP. Run it in
// the production image:
//
//	GOOS=linux GOARCH=<docker's> go test -c -o pt.test ./services
//	docker run --rm -v $PWD:/w -e PASSTHROUGH_MEDIA=/w/media --entrypoint /w/pt.test \
//	  jrottenberg/ffmpeg:8-alpine -test.run TestPassthrough_RealFFmpeg -test.v
func TestPassthrough_RealFFmpeg(t *testing.T) {
	media := os.Getenv("PASSTHROUGH_MEDIA")
	if media == "" {
		t.Skip("PASSTHROUGH_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	for _, c := range []realCase{
		// Main10, 23.976 fps, 10 s GOPs (open), 4 B-frames, AAC, SRT cues at
		// 21, 26 and 41 s; 70 s. The seek to 30 lands on the keyframe at
		// 20.02, whose DTS FFmpeg guesses two frames earlier.
		{file: "a_main10.mkv", decode: "hevc10", codecs: "hvc1.2.4.L63.90", duration: 70, kfPTS: 20.02, realStart: 19.937, subs: true, subsAfterSeek: true},
		// The same in MP4 (90 kHz video): FFmpeg's seek to 30 lands on the
		// keyframe at 30.03, DTS 29.78. mov_text cues after a seek are lost
		// by -fix_sub_duration on every route (not this one's doing).
		{file: "b_main10.mp4", decode: "hevc10", codecs: "hvc1.2.4.L63.90", duration: 70, kfPTS: 30.03, realStart: 29.78, subs: true},
		// Main 8-bit, closed 10 s GOPs at 25 fps, AC3 5.1 (encoded to AAC);
		// 40 s. A keyframe exactly at 30: ffprobe's seek names it, FFmpeg's
		// (3/23 s back first) lands on 20.
		{file: "c_main8.mkv", decode: "hevc8", codecs: "hvc1.1.6.L63.90", duration: 40, kfPTS: 20, realStart: 19.92},
		// Main 8-bit with the parameter sets in-band before every keyframe
		// (x265 repeat-headers), 2 s GOPs; 30 s, the last keyframe at 28.
		{file: "d_repeat.mkv", decode: "hevc8", duration: 30, kfPTS: 28, realStart: 27.92, inBandPS: true},
		// a_main10.mkv remuxed to start at 5 s (-output_ts_offset 5): in
		// movie time everything is where it is in a_main10 -- the seek to 35
		// lands on the keyframe at 20.02, the cues are at 21 and 26.
		{file: "e_start5.mkv", decode: "hevc10", codecs: "hvc1.2.4.L63.90", duration: 70, kfPTS: 20.02, realStart: 19.937, subs: true, subsAfterSeek: true},
	} {
		t.Run(c.file, func(t *testing.T) { realPassthroughSession(t, srv.URL, c) })
	}
	t.Run("restart", func(t *testing.T) { realPassthroughRestart(t, media) })
}

type realCase struct {
	file, decode string
	codecs       string // FFmpeg's own CODECS for the output (-master_pl_name); "" to skip
	duration     float64
	// kfPTS and realStart: the keyframe a seek to 35 (30 quantized) lands
	// on and the real start that seek has (its first DTS).
	kfPTS, realStart              float64
	subs, subsAfterSeek, inBandPS bool
}

func realWeb(t *testing.T) (*Web, *SessionManager) {
	t.Helper()
	runMgr := NewRunManager()
	sm := NewSessionManager(runMgr)
	web := &Web{
		output:         t.TempDir(),
		contentProbe:   &ContentProbe{timeout: 30, LazyMap: lazymap.New[*cp.ProbeReply](&lazymap.Config{})},
		hlsBuilder:     &HLSBuilder{aacCodec: "libfdk_aac", threads: 2, paceLead: 5 * time.Minute, passthrough: capabilityWith("hevc")},
		sessionManager: sm,
		touchMap:       NewTouchMap(),
		sourceProber:   newSourceProber(),
	}
	web.buildHandler()
	t.Cleanup(func() {
		sm.CloseAll()
		runMgr.CloseAll()
	})
	return web, sm
}

func realPost(t *testing.T, web *Web, sm *SessionManager, src, decode string) *Session {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/session?decode="+decode+"&api-key=K&token=T", nil)
	r.Header.Set("X-Source-Url", src)
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, r)
	var resp sessionCreateResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	if resp.VideoRoute != videoRoutePassthrough || resp.RouteReason != reasonOK {
		t.Fatalf("route %s %s", resp.VideoRoute, resp.RouteReason)
	}
	return sm.Get(resp.ID)
}

func realGet(t *testing.T, web *Web, sess *Session, name string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/session/"+sess.id+"/"+name, nil))
	return w
}

var (
	codecsAttr = regexp.MustCompile(`CODECS="([^",]+)`)
	mapURI     = regexp.MustCompile(`#EXT-X-MAP:URI="([^"?]+)`)
)

func waitCompleted(t *testing.T, sess *Session) {
	t.Helper()
	for i := 0; i < 600 && !sess.runIsCompleted(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !sess.runIsCompleted() {
		t.Fatal("the run did not finish the source")
	}
}

func realPassthroughSession(t *testing.T, base string, c realCase) {
	web, sm := realWeb(t)
	sess := realPost(t, web, sm, base+"/"+c.file+"?api-key=secret", c.decode)
	const q = "?api-key=K&token=T"

	master := realGet(t, web, sess, "index.m3u8"+q)
	if master.Code != 200 {
		t.Fatalf("master: %d %s", master.Code, master.Body.String())
	}
	m := codecsAttr.FindStringSubmatch(master.Body.String())
	if m == nil || (c.codecs != "" && m[1] != c.codecs) {
		t.Errorf("master CODECS %v, want %s:\n%s", m, c.codecs, master.Body.String())
	}
	if !strings.Contains(master.Body.String(), "RESOLUTION=640x360") || !strings.Contains(master.Body.String(), "VIDEO-RANGE=SDR") || !strings.Contains(master.Body.String(), `mp4a.40.2",VIDEO-RANGE`) {
		t.Errorf("master:\n%s", master.Body.String())
	}
	variant := realGet(t, web, sess, "v0-360.m3u8"+q)
	vb := variant.Body.String()
	gen := sess.currentRun().Generation()
	for _, want := range []string{"#EXT-X-SESSION-OFFSET:0.000\n", `#EXT-X-MAP:URI="v0-360-init-` + gen + `.mp4` + q + `"`, "\nv0-360-0.m4s" + q + "\n"} {
		if !strings.Contains(vb, want) {
			t.Errorf("variant lacks %q:\n%s", want, vb)
		}
	}
	init := realGet(t, web, sess, "v0-360-init-"+gen+".mp4"+q)
	if init.Code != 200 || init.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("init: %d %q", init.Code, init.Header().Get("Content-Type"))
	}
	fourcc, raw, err := initHEVCConfig(init.Body.Bytes())
	h, ok := parseHVCC(raw)
	if err != nil || !ok || m == nil || hevcCodecString(fourcc, h) != m[1] {
		t.Errorf("init %s %v: CODECS %s, master %v", fourcc, err, hevcCodecString(fourcc, h), m)
	}
	seg := realGet(t, web, sess, "v0-360-0.m4s"+q)
	if seg.Code != 200 || seg.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("segment: %d %q", seg.Code, seg.Header().Get("Content-Type"))
	}
	if ps := parameterSetsInSamples(t, seg.Body.Bytes(), h.lengthSize); ps != 0 {
		t.Errorf("%d VPS/SPS/PPS NAL units in the samples of an hvc1 track", ps)
	}
	if c.inBandPS && !parameterSetsInSource(t, filepath.Join(os.Getenv("PASSTHROUGH_MEDIA"), c.file)) {
		t.Error("the in-band source has no in-band parameter sets: the check above proves nothing")
	}
	// Audio: fMP4 as well.
	audio := realGet(t, web, sess, "a0.m3u8"+q)
	am := mapURI.FindStringSubmatch(audio.Body.String())
	if audio.Code != 200 || am == nil || am[1] != "a0-init-"+gen+".mp4" {
		t.Fatalf("audio playlist: %d\n%s", audio.Code, audio.Body.String())
	}
	for _, name := range []string{am[1], "a0-0.m4s"} {
		if w := realGet(t, web, sess, name+q); w.Code != 200 || w.Header().Get("Content-Type") != "video/mp4" || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q %d bytes", name, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	waitCompleted(t, sess)
	if vb := realGet(t, web, sess, "v0-360.m3u8"+q).Body.String(); !strings.Contains(vb, "#EXT-X-ENDLIST") {
		t.Errorf("finished variant without ENDLIST:\n%s", vb)
	}
	if c.subs {
		if cues := subtitleCues(t, web, sess); len(cues) == 0 || math.Abs(cues[0]-21) > 0.001 {
			t.Errorf("cues from the start %v, want 21 first", cues)
		}
	}

	// Seek to 35 (30): the run lands on the keyframe the probe reports, and
	// every output counts from it.
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/session/"+sess.id+"/seek?t=35", nil))
	var seek struct{ Offset float64 }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &seek) != nil || math.Abs(seek.Offset-c.realStart) > 0.001 {
		t.Fatalf("seek: %d %s, want offset %.3f", w.Code, w.Body.String(), c.realStart)
	}
	args := strings.Join(sess.currentRun().cmd.Args, " ")
	if want := fmt.Sprintf(" -ss 30.000 -noaccurate_seek -itsoffset %.6f -i ", 30-c.realStart); !strings.Contains(args, want) {
		t.Errorf("seek args lack %q: %s", want, args)
	}
	if !strings.Contains(args, " -ss 0 -map 0:1 ") {
		t.Errorf("the audio output is not cut at the real start: %s", args)
	}
	realGet(t, web, sess, "v0-360.m3u8"+q)
	waitCompleted(t, sess)
	vb = realGet(t, web, sess, "v0-360.m3u8"+q).Body.String()
	segs, total := parseMediaPlaylist([]byte(vb))
	if want := c.duration - c.kfPTS; math.Abs(total-want) > 0.2 {
		t.Errorf("after the seek the video has %.3f s (%d segments), want %.3f: started at another keyframe", total, len(segs), want)
	}
	if !strings.Contains(vb, fmt.Sprintf("#EXT-X-SESSION-OFFSET:%.3f\n", c.realStart)) {
		t.Errorf("variant offset:\n%s", vb)
	}
	gen = sess.currentRun().Generation()
	vinit := realGet(t, web, sess, "v0-360-init-"+gen+".mp4").Body.Bytes()
	vseg := realGet(t, web, sess, "v0-360-0.m4s").Body.Bytes()
	vts, vpts := firstSampleTimes(t, vinit, vseg)
	ainit := realGet(t, web, sess, "a0-init-"+gen+".mp4").Body.Bytes()
	aseg := realGet(t, web, sess, "a0-0.m4s").Body.Bytes()
	ats, _ := firstSampleTimes(t, ainit, aseg)
	t.Logf("after the seek: video first sample dts %.3f pts %.3f, audio tfdt %.3f (movie time = media time + %.3f)", vts, vpts, ats, c.realStart)
	// The keyframe is shown at its movie time: what the offset says media
	// time 0 is, plus its media time -- the video's first DTS is the zero,
	// so its output shifted nothing.
	if math.Abs(vts) > 0.0005 || math.Abs(c.realStart+vpts-c.kfPTS) > 0.002 {
		t.Errorf("keyframe (dts %.3f) shown at movie %.3f, it is at %.3f", vts, c.realStart+vpts, c.kfPTS)
	}
	if c.subsAfterSeek {
		cues := subtitleCues(t, web, sess)
		// The first cue after the landing keyframe, counted from the zero.
		if len(cues) == 0 || math.Abs(cues[0]-(21-c.realStart)) > 0.001 {
			t.Errorf("cues after the seek %v, want the 21 s cue at %.3f", cues, 21-c.realStart)
		}
	}
}

// subtitleCues are the start times of the cues of s0, from its playlist.
func subtitleCues(t *testing.T, web *Web, sess *Session) []float64 {
	t.Helper()
	pl := realGet(t, web, sess, "s0.m3u8").Body.String()
	var cues []float64
	cue := regexp.MustCompile(`(?m)^(?:(\d+):)?(\d+):(\d+\.\d+) -->`)
	for _, line := range strings.Split(pl, "\n") {
		if !strings.HasSuffix(line, ".vtt") {
			continue
		}
		body := realGet(t, web, sess, line).Body.String()
		for _, m := range cue.FindAllStringSubmatch(body, -1) {
			h, _ := strconv.Atoi(m[1])
			mi, _ := strconv.Atoi(m[2])
			s, _ := strconv.ParseFloat(m[3], 64)
			cues = append(cues, float64(h*3600+mi*60)+s)
		}
	}
	return cues
}

// parameterSetsInSamples counts VPS, SPS and PPS NAL units in the samples of
// an fMP4 segment (its mdat, NAL units prefixed with lengthSize bytes).
func parameterSetsInSamples(t *testing.T, seg []byte, lengthSize int) int {
	t.Helper()
	mdat, ok := findBox(seg, "mdat")
	if !ok || lengthSize == 0 {
		t.Fatal("no mdat")
	}
	n := 0
	for i := 0; i+lengthSize <= len(mdat); {
		l := 0
		for k := 0; k < lengthSize; k++ {
			l = l<<8 | int(mdat[i+k])
		}
		i += lengthSize
		if l <= 0 || i+l > len(mdat) {
			t.Fatalf("bad NAL length %d at %d", l, i)
		}
		if typ := (mdat[i] >> 1) & 0x3f; typ >= 32 && typ <= 34 {
			n++
		}
		i += l
	}
	return n
}

// parameterSetsInSource reports whether the first video packets of the
// source carry VPS/SPS/PPS in-band.
func parameterSetsInSource(t *testing.T, file string) bool {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// The MKV's CodecPrivate holds one set; an in-band copy means the
	// VPS NAL header (0x40 0x01) appears more than once.
	return strings.Count(string(b), "\x40\x01\x0c") > 1
}

// firstSampleTimes are the decode and presentation time of the first
// sample of an fMP4 segment, in seconds, with the timescale of its init
// (mdhd), the base decode time of its tfdt and its trun.
func firstSampleTimes(t *testing.T, init, seg []byte) (dts, pts float64) {
	t.Helper()
	b := init
	for _, want := range []string{"moov", "trak", "mdia", "mdhd"} {
		var ok bool
		if b, ok = findBox(b, want); !ok {
			t.Fatalf("init without %s", want)
		}
	}
	var timescale uint32
	if b[0] == 1 {
		timescale = binary.BigEndian.Uint32(b[20:24])
	} else {
		timescale = binary.BigEndian.Uint32(b[12:16])
	}
	traf := seg
	for _, want := range []string{"moof", "traf"} {
		var ok bool
		if traf, ok = findBox(traf, want); !ok {
			t.Fatalf("segment without %s", want)
		}
	}
	tfdt, ok := findBox(traf, "tfdt")
	if !ok {
		t.Fatal("no tfdt")
	}
	var base uint64
	if tfdt[0] == 1 {
		base = binary.BigEndian.Uint64(tfdt[4:12])
	} else {
		base = uint64(binary.BigEndian.Uint32(tfdt[4:8]))
	}
	trun, ok := findBox(traf, "trun")
	if !ok {
		t.Fatal("no trun")
	}
	flags := binary.BigEndian.Uint32(trun[0:4]) & 0xffffff
	p := 8
	if flags&0x1 != 0 {
		p += 4
	}
	if flags&0x4 != 0 {
		p += 4
	}
	for _, f := range []uint32{0x100, 0x200, 0x400} {
		if flags&f != 0 {
			p += 4
		}
	}
	var cto int64
	if flags&0x800 != 0 {
		cto = int64(int32(binary.BigEndian.Uint32(trun[p : p+4])))
	}
	dts = float64(base) / float64(timescale)
	return dts, dts + float64(cto)/float64(timescale)
}

// slowFiles serves files at about 320 KB/s: a run over it lasts long
// enough to be killed in the middle.
type slowFiles struct{ dir string }

func (s slowFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.dir, filepath.Base(r.URL.Path)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), slowReader{f})
}

type slowReader struct{ f *os.File }

func (s slowReader) Read(p []byte) (int, error) {
	if len(p) > 16<<10 {
		p = p[:16<<10]
	}
	time.Sleep(50 * time.Millisecond)
	return s.f.Read(p)
}

func (s slowReader) Seek(off int64, whence int) (int64, error) { return s.f.Seek(off, whence) }

// FFmpeg killed (-9) in the middle of a run: the next request restarts it
// in the same directory, the new process's init has a name of its own,
// the old one stays whole, and no init request is ever answered with an
// empty body.
func realPassthroughRestart(t *testing.T, media string) {
	srv := httptest.NewServer(slowFiles{media})
	t.Cleanup(srv.Close) // after the sessions' cleanup, which ends FFmpeg's connection
	web, sm := realWeb(t)
	sess := realPost(t, web, sm, srv.URL+"/d_repeat.mkv", "hevc8")
	if w := realGet(t, web, sess, "index.m3u8"); w.Code != 200 {
		t.Fatalf("master: %d", w.Code)
	}
	run := sess.currentRun()
	gen1, running := run.processState()
	if !running {
		t.Fatal("the run ended before the kill: the source is served too fast")
	}
	init1 := "v0-360-init-" + gen1 + ".mp4"

	var mu sync.Mutex
	names := []string{init1}
	stop := make(chan struct{})
	polled := make(chan int)
	go func() {
		n := 0
		defer func() { polled <- n }()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			mu.Lock()
			ns := append([]string{}, names...)
			mu.Unlock()
			for _, name := range ns {
				w := realGet(t, web, sess, name)
				n++
				if w.Code == 200 && w.Body.Len() == 0 {
					t.Errorf("%s answered 200 with no bytes", name)
				}
			}
		}
	}()

	if err := syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && sess.IsRunning(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	var gen2 string
	for i := 0; i < 200; i++ {
		// Until the new process's first cut the old playlist is served, and
		// it names the old, whole init.
		w := realGet(t, web, sess, "v0-360.m3u8")
		vb := w.Body.String()
		m := mapURI.FindStringSubmatch(vb)
		if w.Code != 200 || m == nil {
			t.Fatalf("variant during the restart: %d\n%s", w.Code, vb)
		}
		if m[1] != init1 {
			gen2 = strings.TrimSuffix(strings.TrimPrefix(m[1], "v0-360-init-"), ".mp4")
			break
		}
		if gen2 == "" {
			if g, _ := sess.currentRun().processState(); g != gen1 {
				mu.Lock()
				if len(names) == 1 {
					names = append(names, "v0-360-init-"+g+".mp4")
				}
				mu.Unlock()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	n := <-polled
	if gen2 == "" || gen2 == gen1 {
		t.Fatalf("no new process named its init (%s -> %s)", gen1, gen2)
	}
	for _, name := range []string{init1, "v0-360-init-" + gen2 + ".mp4"} {
		if w := realGet(t, web, sess, name); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s after the restart: %d, %d bytes", name, w.Code, w.Body.Len())
		}
	}
	t.Logf("restart %s -> %s, %d init requests while it happened", gen1, gen2, n)
}
