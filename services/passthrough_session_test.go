package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/testutil"
	cp "github.com/webtor-io/content-prober/content-prober"
)

// hevcHLS is an Online HLS of an HEVC source of the given size with AAC
// audio; passthrough puts it on that route.
func hevcHLS(t *testing.T, w, h int32, passthrough bool, cfg *HLSConfig) *HLS {
	t.Helper()
	if cfg == nil {
		cfg = &HLSConfig{sm: Online, aacCodec: "libfdk_aac"}
	}
	hls := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "hevc", Width: w, Height: h},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
	}}, cfg)
	if passthrough && !hls.usePassthrough() {
		t.Fatal("usePassthrough refused an Online HEVC HLS")
	}
	return hls
}

// standInPassthroughParams replaces the passthrough command for a test: the
// run starts (under fakeFFmpeg) with recognisable arguments.
func standInPassthroughParams(t *testing.T) {
	t.Helper()
	orig := buildPassthroughParams
	buildPassthroughParams = func(h *HLS, in *url.URL, out string, _ ParamOptions, _ string) ([]string, error) {
		return []string{"-i", in.String(), "-passthrough-stand-in", out + "/" + h.primary[0].GetPlaylistName() + ".ffmpeg"}, nil
	}
	t.Cleanup(func() { buildPassthroughParams = orig })
}

// A passthrough session and an old-route session of the same source at the
// same seek never share a run, a directory, a remembered real start or the
// remembered FFmpeg options; the old route's key and directory are the ones
// it always had.
func TestPassthroughRunIdentity(t *testing.T) {
	fakeFFmpeg(t)
	standInPassthroughParams(t)
	orig, origPT := probeRunStart, probePassthroughStart
	probeRunStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 3, nil }
	probePassthroughStart = probeRunStart
	t.Cleanup(func() { probeRunStart, probePassthroughStart = orig, origPT })

	hashDir := t.TempDir()
	old := hevcHLS(t, 1920, 1080, false, nil)
	pass := hevcHLS(t, 1920, 1080, true, nil)
	if k := runKeyFor(hashDir, old, 600); k != runKey(hashDir, 600) || k != hashDir+":seek:600.000" {
		t.Errorf("old route key changed: %s", k)
	}
	if k := runKeyFor(hashDir, pass, 600); k != hashDir+":hevc:seek:600.000" {
		t.Errorf("passthrough key %s", k)
	}

	m := NewRunManager()
	defer m.CloseAll()
	rOld, err := m.Acquire(hashDir, 600, "http://src/movie.mkv", old)
	if err != nil {
		t.Fatal(err)
	}
	rPass, err := m.Acquire(hashDir, 600, "http://src/movie.mkv", pass)
	if err != nil {
		t.Fatal(err)
	}
	if rOld == rPass {
		t.Fatal("both routes got the same run")
	}
	if got, want := rOld.OutputDir(), filepath.Join(hashDir, "runs", "seek-600.000"); got != want {
		t.Errorf("old route dir %s, want %s", got, want)
	}
	if got, want := rPass.OutputDir(), filepath.Join(hashDir, "runs", "hevc-seek-600.000"); got != want {
		t.Errorf("passthrough dir %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(rPass.cmd.Args, " "), "-passthrough-stand-in") || strings.Contains(strings.Join(rOld.cmd.Args, " "), "-passthrough-stand-in") {
		t.Errorf("each run must have its own route's arguments:\nold  %v\npass %v", rOld.cmd.Args, rPass.cmd.Args)
	}

	// The passthrough run is a copy run: its seek resolved a real start, and
	// only its key remembers it -- the old route (a reencode, exact seek)
	// must not answer with the keyframe of the copy.
	if v, ok := m.ResolvedStart(runKeyFor(hashDir, pass, 600)); !ok || v != 597 {
		t.Errorf("passthrough real start %v %v, want 597", v, ok)
	}
	if v, ok := m.ResolvedStart(runKeyFor(hashDir, old, 600)); ok {
		t.Errorf("the old route's key answers the passthrough run's start %v", v)
	}
	sOld := NewSession(SessionConfig{ID: "o", HashDir: hashDir, HLS: old, RunMgr: m})
	sOld.seekTime = 600
	if got := sOld.RunStart(); got != 600 {
		t.Errorf("an idle old-route session reports %v, want its own 600", got)
	}
	sPass := NewSession(SessionConfig{ID: "p", HashDir: hashDir, HLS: pass, RunMgr: m})
	sPass.seekTime = 600
	if got := sPass.RunStart(); got != 597 {
		t.Errorf("an idle passthrough session reports %v, want its run's 597", got)
	}

	// Options a failed passthrough run needed stay with passthrough runs.
	m.rememberFallbacks(fallbackKey(hashDir, pass), ParamOptions{Lenient: true})
	m.mu.Lock()
	nOld := m.newRunLocked(runKeyFor(hashDir, old, 0), hashDir, 0, "", old)
	nPass := m.newRunLocked(runKeyFor(hashDir, pass, 0), hashDir, 0, "", pass)
	m.mu.Unlock()
	if nOld.fallbacks.Lenient || !nPass.fallbacks.Lenient {
		t.Errorf("fallbacks leak across routes: old %+v pass %+v", nOld.fallbacks, nPass.fallbacks)
	}
	m.rememberFallbacks(fallbackKey(hashDir, old), ParamOptions{EncodeAudio: true})
	m.mu.Lock()
	nPass2 := m.newRunLocked(runKeyFor(hashDir, pass, 30), hashDir, 30, "", pass)
	m.mu.Unlock()
	if nPass2.fallbacks.EncodeAudio {
		t.Error("the old route's options reached a passthrough run")
	}
}

// The over-1080p refusal and the transcoding-disabled refusal stand for "the
// video would have to be encoded": the old route keeps them exactly, the
// passthrough route is not subject to them.
func TestPassthroughGate(t *testing.T) {
	disabled := &HLSConfig{sm: Online, aacCodec: "libfdk_aac", disableVideoTranscoding: true}
	for _, c := range []struct {
		name string
		h    *HLS
		want error
	}{
		{"2160 old route", hevcHLS(t, 3840, 2160, false, nil), ErrResolutionNotSupported},
		{"2160 passthrough", hevcHLS(t, 3840, 2160, true, nil), nil},
		{"transcoding disabled, old route", hevcHLS(t, 1920, 1080, false, disabled), ErrTranscodingDisabled},
		{"transcoding disabled, passthrough", hevcHLS(t, 1920, 1080, true, disabled), nil},
	} {
		_, err := c.h.ffmpegParamsFor("/out", ParamOptions{}, "0123456789abcdef")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A passthrough command names its files after its process: none
	// without one.
	if _, err := hevcHLS(t, 1920, 1080, true, nil).GetFFmpegParams("/out"); !errors.Is(err, errNoGeneration) {
		t.Errorf("passthrough without a generation: %v", err)
	}
	// h264 over 1080p is copied, as it always was.
	h := NewHLS("http://src/x.mkv", &cp.ProbeReply{Streams: []*cp.Stream{{Index: 0, CodecType: "video", CodecName: "h264", Width: 3840, Height: 2160}}}, &HLSConfig{sm: Online})
	if _, err := h.GetFFmpegParams("/out"); err != nil {
		t.Errorf("h264 2160: %v", err)
	}
}

// A passthrough video is a copy to everything that asks (the seek with
// -noaccurate_seek, the real-start probe, the playlist offset), its mode is
// its own, and it goes out as hvc1 with the parameter sets out of the samples.
func TestPassthroughStream(t *testing.T) {
	h := hevcHLS(t, 3840, 2160, true, nil)
	v := h.primaryVideo()
	if !v.IsCopy() {
		t.Error("passthrough video is not a copy")
	}
	if got := strings.Join(v.GetCodecParams(), " "); got != "-c:v copy -bsf:v hevc_mp4toannexb -tag:v hvc1" {
		t.Errorf("codec params %q", got)
	}
	r := newTranscodeRun("k", t.TempDir(), 600, "http://src", h)
	if !r.isVideoCopy() || r.runMode() != runModePassthrough || h.videoRoute() != videoRoutePassthrough {
		t.Errorf("copy=%v mode=%s route=%s", r.isVideoCopy(), r.runMode(), h.videoRoute())
	}
	// The audio of the session is untouched by the route.
	if got := strings.Join(h.audio[0].GetCodecParams(), " "); got != "-c:a copy" {
		t.Errorf("audio %q", got)
	}
	// Only an Online HLS with a video can take the route.
	audioOnly := NewHLS("http://src/a.mka", &cp.ProbeReply{Streams: []*cp.Stream{{Index: 0, CodecType: "audio", CodecName: "aac", Channels: 2}}}, &HLSConfig{sm: Online})
	if audioOnly.usePassthrough() || audioOnly.passthrough {
		t.Error("audio-only source on passthrough")
	}
	multi := hevcHLS(t, 1920, 1080, false, &HLSConfig{sm: MultiBitrate, aacCodec: "libfdk_aac"})
	if multi.usePassthrough() {
		t.Error("multi-bitrate HLS on passthrough")
	}
	old := hevcHLS(t, 1920, 1080, false, nil)
	if old.primaryVideo().IsCopy() || old.videoRoute() != videoRouteReencode {
		t.Error("the old route of HEVC is an encode")
	}
}

// passthroughWeb is a Web whose source "hevc" is HEVC of the given size
// with AAC audio, passthrough configured, and facts for its video on disk
// (the probe does not run).
func passthroughWeb(t *testing.T, w, h int32, f *sourceHEVCFacts) (*Web, *SessionManager) {
	t.Helper()
	fakeFFmpeg(t)
	web, sm, _ := goldenWeb(t, func(web *Web) {
		web.hlsBuilder.passthrough = capabilityWith("hevc")
		web.sourceProber = newSourceProber()
	})
	hashDir, _, _ := web.sourceHashDir("http://source/hevc/file.mkv")
	probe, _ := json.Marshal(&cp.ProbeReply{Streams: []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "hevc", Width: w, Height: h},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2},
	}})
	if err := os.WriteFile(filepath.Join(hashDir, "index.json"), probe, 0644); err != nil {
		t.Fatal(err)
	}
	if f != nil {
		writeSourceFacts(sourceFactsPath(hashDir, 0), *f)
	}
	return web, sm
}

func postSession(web *Web, decode string) *httptest.ResponseRecorder {
	q := ""
	if decode != "" {
		q = "?decode=" + url.QueryEscape(decode)
	}
	r := httptest.NewRequest(http.MethodPost, "/session"+q, nil)
	r.Header.Set("X-Source-Url", "http://source/hevc/file.mkv")
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, r)
	return w
}

// POST /session says which route the session got and why, counts it, and
// a refusal carries its reason in a header with the body it always had.
func TestSessionCreate_RouteInTheAnswer(t *testing.T) {
	standInPassthroughParams(t)
	failProbe := func(t *testing.T) *int32 {
		return stubSourceProbe(t, func(int) ([]byte, error) { return nil, errors.New("timeout") })
	}
	sdr := sdrMain10()
	cases := []struct {
		name         string
		w, h         int32
		facts        *sourceHEVCFacts
		decode       string
		failingProbe bool
		code         int
		route        string
		reason       string
		body         string
	}{
		{"old client", 1920, 1080, nil, "", false, 200, videoRouteReencode, reasonNoDeclaration, ""},
		{"passthrough", 1920, 1080, &sdr, "hevc10", false, 200, videoRoutePassthrough, reasonOK, ""},
		{"1080 needs Main10", 1920, 1080, &sdr, "hevc8", false, 200, videoRouteReencode, reasonNeedsMain10, ""},
		{"1080, check failed", 1920, 1080, nil, "hevc10", true, 200, videoRouteReencode, reasonProbeFailed, ""},
		{"2160 old client: the 415", 3840, 2160, nil, "", false, 415, videoRouteRefused, reasonNoDeclaration, "resolution over 1080p is not supported\n"},
		{"2160 pending: the same 415", 3840, 2160, nil, "unknown", false, 415, videoRouteRefused, reasonDeclarationPending, "resolution over 1080p is not supported\n"},
		{"2160 without a 2160 token", 3840, 2160, nil, "hevc10", false, 415, videoRouteRefused, reasonNeeds2160, "resolution over 1080p is not supported\n"},
		{"2160 check failed: retry", 3840, 2160, nil, "hevc10-2160", true, 503, videoRouteRefused, reasonProbeFailed, "source check failed\n"},
		{"2160 passthrough", 3840, 2160, &sdr, "hevc10-2160", false, 200, videoRoutePassthrough, reasonOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.failingProbe {
				failProbe(t)
			} else {
				noSourceProbe(t)
			}
			web, sm := passthroughWeb(t, c.w, c.h, c.facts)
			counter := metricVideoRouteTotal.WithLabelValues(c.route, c.reason)
			before := testutil.ToFloat64(counter)
			w := postSession(web, c.decode)
			if w.Code != c.code {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.code, w.Body.String())
			}
			if got := testutil.ToFloat64(counter) - before; got != 1 {
				t.Errorf("video_route_total{%s,%s} +%v, want +1", c.route, c.reason, got)
			}
			if c.code != 200 {
				if w.Body.String() != c.body {
					t.Errorf("body %q, want %q", w.Body.String(), c.body)
				}
				if got := w.Header().Get(routeReasonHeader); got != c.reason {
					t.Errorf("%s = %q, want %q", routeReasonHeader, got, c.reason)
				}
				if got := w.Header().Get("Retry-After"); (c.code == 503) != (got == "5") {
					t.Errorf("Retry-After %q on a %d", got, c.code)
				}
				return
			}
			var resp sessionCreateResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.VideoRoute != c.route || resp.RouteReason != c.reason || resp.ID == "" {
				t.Errorf("answer %+v, want route %s reason %s", resp, c.route, c.reason)
			}
			sess := sm.Get(resp.ID)
			wantDir := "seek-0.000"
			if c.route == videoRoutePassthrough {
				wantDir = "hevc-seek-0.000"
			}
			if filepath.Base(sess.run.OutputDir()) != wantDir || sess.run.runMode() != c.route {
				t.Errorf("run %s mode %s", sess.run.OutputDir(), sess.run.runMode())
			}
		})
	}
}

// session_segments_served counts the primary segments a session was given,
// by route, when it goes; a legacy session nobody played is not a session.
func TestSessionSegmentsServedMetric(t *testing.T) {
	dir := t.TempDir()
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	h := testHLS(testStream(0, "video", "h264"), testStream(1, "audio", "aac"))
	sess := NewSession(SessionConfig{ID: "served", HashDir: dir, HLS: h, RunMgr: runMgr})
	run := newCompletedRun(t, dir)
	sess.run, sess.started = run, true
	if err := os.MkdirAll(run.OutputDir(), 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"v0-1080-0.ts", "v0-1080-1.ts", "a0-0.ts"} {
		if err := os.WriteFile(filepath.Join(run.OutputDir(), f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{"route": videoRouteCopy}
	before := histogramCount(t, "transcoder_session_segments_served", labels)
	for _, f := range []string{"v0-1080-0.ts", "v0-1080-1.ts", "a0-0.ts", "v0-1080-1.ts", "v0-1080-9.ts"} {
		(&Web{}).sessionSegmentHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/session/served/"+f, nil), sess, f)
	}
	if sess.primaryServed != 3 {
		t.Errorf("primary served %d, want 3 (two, one again; audio and the missing one are not)", sess.primaryServed)
	}
	sess.Close()
	if got := histogramCount(t, "transcoder_session_segments_served", labels) - before; got != 1 {
		t.Errorf("observed %d times, want once", got)
	}

	idle := NewSession(SessionConfig{ID: "idle", HashDir: dir, HLS: h, RunMgr: runMgr})
	before = histogramCount(t, "transcoder_session_segments_served", labels)
	idle.Close()
	if got := histogramCount(t, "transcoder_session_segments_served", labels) - before; got != 0 {
		t.Errorf("a session that never ran was observed")
	}
}

// X-Video-Route-Reason names the reason only on the refusals the route
// causes: the old route will not encode the video (over 1080p, or encoding
// disabled), or the check that could have passed it did not answer. A
// source with nothing playable is refused whatever the route: no header,
// and the route metric counts it as an error, not a refusal.
func TestSessionCreate_ReasonHeaderOnlyOnRouteRefusals(t *testing.T) {
	noSourceProbe(t)
	fakeFFmpeg(t)
	all := "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"
	post := func(web *Web, source, decode string) *httptest.ResponseRecorder {
		q := ""
		if decode != "" {
			q = "?decode=" + url.QueryEscape(decode)
		}
		r := httptest.NewRequest(http.MethodPost, "/session"+q, nil)
		r.Header.Set("X-Source-Url", goldenSourceURL(source))
		w := httptest.NewRecorder()
		web.handler.ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct {
		name, source, decode string
		disabled             bool
		code                 int
		body, header, label  string
		reason               string
	}{
		{"nothing playable, capability off", "nothing-playable", all, false, 415, ErrNoPlayableStreams.Error(), "", videoRouteError, reasonPassthroughOff},
		{"nothing playable, no declaration", "nothing-playable", "", false, 415, ErrNoPlayableStreams.Error(), "", videoRouteError, reasonNoDeclaration},
		{"over 1080p", "hevc-2160-hdr", "", false, 415, ErrResolutionNotSupported.Error(), reasonNoDeclaration, videoRouteRefused, reasonNoDeclaration},
		{"encoding disabled", "hevc-1080-main10-aac", "", true, 415, ErrTranscodingDisabled.Error(), reasonNoDeclaration, videoRouteRefused, reasonNoDeclaration},
	} {
		t.Run(c.name, func(t *testing.T) {
			web, _, _ := goldenWeb(t, func(w *Web) { w.hlsBuilder.disableVideoTranscoding = c.disabled })
			counter := metricVideoRouteTotal.WithLabelValues(c.label, c.reason)
			before := testutil.ToFloat64(counter)
			w := post(web, c.source, c.decode)
			if w.Code != c.code || w.Body.String() != c.body+"\n" {
				t.Fatalf("%d %q, want %d %q", w.Code, w.Body.String(), c.code, c.body)
			}
			if got := w.Header().Get(routeReasonHeader); got != c.header {
				t.Errorf("%s = %q, want %q", routeReasonHeader, got, c.header)
			}
			if d := testutil.ToFloat64(counter) - before; d != 1 {
				t.Errorf("video_route_total{%s,%s} +%v, want +1", c.label, c.reason, d)
			}
		})
	}
}
