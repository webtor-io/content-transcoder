package services

// The golden record of the old route: what POST /session, its playlists, a
// seek and the legacy GET /index.m3u8 produce -- FFmpeg's arguments, the
// playlists, the refusals -- for a set of representative sources, captured
// on 1b25e28 (the image in production when passthrough was added) by
// running this very file there with GOLDEN_WRITE set. golden_route_test.go
// replays it on this code with passthrough configured in the ways that must
// change nothing.
//
// This file uses only what 1b25e28 has, so it can be copied onto it to
// regenerate the record.
//
// Deliberate departures from 1b25e28, spliced into the record by hand
// (only these seek_args entries; every other byte is still 1b25e28's).
// A record regenerated on 1b25e28 has to get them again:
//   - a seek run of a re-encoded video cuts each copied audio track at the
//     seek point: "-ss", "0" before its "-map" (hevc-1080-main10-aac,
//     hevc-2560x1080, hevc-cover-art-first: 0:1 or 0:2, their only track;
//     hevc-800-ac3-dvdsub: 0:2, the AAC one, not the encoded AC3);
//   - and each subtitle output there: "-ss", "0" before its "-map"
//     (hevc-800-ac3-dvdsub: 0:4, the subrip track; the dvd_subtitle one has
//     no output);
//   - a copy-route seek run counts its outputs from where FFmpeg's seek
//     lands: "-itsoffset", "2.500000" after "-noaccurate_seek" (the record's
//     probe answers 2.5 s before the seek; h264-1080-aac and
//     h264-2160-eac3-subs), and "-ss", "0" before the "-map" of each
//     subtitle output (h264-2160-eac3-subs: 0:2 subrip and 0:4 ass; the PGS
//     track has no output).
//
// Declared audio (decode=aac51, ac3, ec3) is not in this record: the
// record is the old route without a declaration, and it stays so for every
// declaration that changes no audio output. The declared cases have their
// own record, testdata/golden_audio.json, taken on this code
// (golden_audio_test.go, GOLDEN_AUDIO_WRITE):
//   - ts_aac51: passthrough not configured, decode=aac51, every source here
//     and two with every kind of multichannel track (h264-1080-surround,
//     hevc-1080-surround). A source without a track over 2 channels in a
//     session that opens answers byte for byte as recorded here; the
//     others (h264-2160-eac3-subs, hevc-800-ac3-dvdsub, audio-only-flac-51,
//     the surround ones) do not;
//   - passthrough_aac51_ec3: the capability hevc, decode=hevc10,aac51,ec3;
//     hevc-1080-main10-aac, hevc-800-ac3-dvdsub and hevc-1080-surround pass
//     through (E-AC-3 copied, CODECS "...,mp4a.40.2,ec-3"), the rest takes
//     the old route with the declared audio.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	cp "github.com/webtor-io/content-prober/content-prober"
	"github.com/webtor-io/lazymap"
)

const goldenRecordPath = "testdata/golden_old_route.json"

type goldenSource struct {
	name    string
	streams []*cp.Stream
}

func goldenStream(i int32, typ, codec string, w, h, ch int32, tags map[string]string) *cp.Stream {
	return &cp.Stream{Index: i, CodecType: typ, CodecName: codec, Width: w, Height: h, Channels: ch, Tags: tags}
}

// goldenSources covers every branch of the old route: copy, reencode at and
// under 1080, the 415 over 1080 (height) and the reencode of a source wider
// than 1920 but not taller than 1080, audio-only, cover art before the
// video, subtitle tracks with and without output, audio that is copied and
// audio that is encoded, and a source with nothing playable.
var goldenSources = []goldenSource{
	{"h264-1080-aac", []*cp.Stream{
		goldenStream(0, "video", "h264", 1920, 1080, 0, nil),
		goldenStream(1, "audio", "aac", 0, 0, 2, map[string]string{"language": "eng"}),
	}},
	{"h264-2160-eac3-subs", []*cp.Stream{
		goldenStream(0, "video", "h264", 3840, 2160, 0, nil),
		goldenStream(1, "audio", "eac3", 0, 0, 6, map[string]string{"language": "eng", "title": "Surround"}),
		goldenStream(2, "subtitle", "subrip", 0, 0, 0, map[string]string{"language": "rus"}),
		goldenStream(3, "subtitle", "hdmv_pgs_subtitle", 0, 0, 0, nil),
		goldenStream(4, "subtitle", "ass", 0, 0, 0, nil),
	}},
	{"hevc-1080-main10-aac", []*cp.Stream{
		goldenStream(0, "video", "hevc", 1920, 1080, 0, nil),
		goldenStream(1, "audio", "aac", 0, 0, 2, nil),
	}},
	{"hevc-800-ac3-dvdsub", []*cp.Stream{
		goldenStream(0, "video", "hevc", 1920, 800, 0, nil),
		goldenStream(1, "audio", "ac3", 0, 0, 6, nil),
		goldenStream(2, "audio", "aac", 0, 0, 2, map[string]string{"language": "jpn"}),
		goldenStream(3, "subtitle", "dvd_subtitle", 0, 0, 0, nil),
		goldenStream(4, "subtitle", "subrip", 0, 0, 0, map[string]string{"language": "eng"}),
	}},
	{"hevc-2160-hdr", []*cp.Stream{
		goldenStream(0, "video", "hevc", 3840, 2160, 0, nil),
		goldenStream(1, "audio", "truehd", 0, 0, 8, nil),
	}},
	{"hevc-2560x1080", []*cp.Stream{
		goldenStream(0, "video", "hevc", 2560, 1080, 0, nil),
		goldenStream(1, "audio", "aac", 0, 0, 2, nil),
	}},
	{"hevc-cover-art-first", []*cp.Stream{
		goldenStream(0, "video", "mjpeg", 600, 600, 0, nil),
		goldenStream(1, "video", "hevc", 1280, 720, 0, nil),
		goldenStream(2, "audio", "aac", 0, 0, 2, nil),
	}},
	{"av1-1080-opus", []*cp.Stream{
		goldenStream(0, "video", "av1", 1920, 1080, 0, nil),
		goldenStream(1, "audio", "opus", 0, 0, 2, nil),
	}},
	{"av1-2160", []*cp.Stream{
		goldenStream(0, "video", "av1", 3840, 2160, 0, nil),
		goldenStream(1, "audio", "opus", 0, 0, 2, nil),
	}},
	{"mpeg4-576-mp3", []*cp.Stream{
		goldenStream(0, "video", "mpeg4", 720, 576, 0, nil),
		goldenStream(1, "audio", "mp3", 0, 0, 2, nil),
	}},
	{"vp9-720", []*cp.Stream{
		goldenStream(0, "video", "vp9", 1280, 720, 0, nil),
	}},
	{"audio-only-flac-51", []*cp.Stream{
		goldenStream(0, "audio", "flac", 0, 0, 6, nil),
		goldenStream(1, "subtitle", "subrip", 0, 0, 0, nil),
	}},
	{"audio-only-aac", []*cp.Stream{
		goldenStream(0, "audio", "aac", 0, 0, 2, nil),
	}},
	{"nothing-playable", []*cp.Stream{
		goldenStream(0, "attachment", "ttf", 0, 0, 0, nil),
	}},
}

// goldenRecord is everything the old route answered for one source.
type goldenRecord struct {
	PostStatus int      `json:"post_status"`
	PostBody   string   `json:"post_body,omitempty"`
	PostJSON   string   `json:"post_json,omitempty"` // id and duration of a 200
	StartArgs  []string `json:"start_args,omitempty"`
	Master     string   `json:"master,omitempty"`
	Variant    string   `json:"variant,omitempty"`
	// Media are the audio and subtitle playlists as served (status, then
	// body), Segments the answer to the first primary and audio segment
	// (status, headers with the run's generation normalized, body hash),
	// by name.
	Media       map[string]string `json:"media,omitempty"`
	Segments    map[string]string `json:"segments,omitempty"`
	SeekBody    string            `json:"seek_body,omitempty"`
	SeekArgs    []string          `json:"seek_args,omitempty"`
	LegacyCode  int               `json:"legacy_status"`
	LegacyBody  string            `json:"legacy_body"`
	LegacyArgs  []string          `json:"legacy_args,omitempty"`
	LegacyError string            `json:"legacy_error,omitempty"`
}

type goldenFile struct {
	Sources map[string]goldenRecord `json:"sources"`
	// Metrics are the metric families of the default registry: name, type,
	// help and label names, one line each.
	Metrics []string `json:"metrics"`
}

// fakeFFmpeg puts an "ffmpeg" that only sleeps first in PATH: the runs start
// for real (argument building, seek injection, the process bookkeeping) and
// write nothing.
func fakeFFmpeg(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\nexec sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

var goldenSessionID = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

// goldenWeb is a Web over a temporary output dir with content-prober's
// answers preset for every golden source; configure adjusts it (the
// passthrough configuration of the replays).
func goldenWeb(t *testing.T, configure func(*Web)) (*Web, *SessionManager, string) {
	t.Helper()
	out := t.TempDir()
	for _, src := range goldenSources {
		u, _ := url.Parse(goldenSourceURL(src.name))
		sum := sha1.Sum([]byte(u.Path))
		hashDir := filepath.Join(out, hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(hashDir, 0755); err != nil {
			t.Fatal(err)
		}
		probe, _ := json.Marshal(&cp.ProbeReply{
			Streams: src.streams,
			Format:  &cp.Format{Duration: "5400.000000"},
		})
		if err := os.WriteFile(filepath.Join(hashDir, "index.json"), probe, 0644); err != nil {
			t.Fatal(err)
		}
	}
	runMgr := NewRunManager()
	sm := NewSessionManager(runMgr)
	web := &Web{
		output:         out,
		contentProbe:   &ContentProbe{LazyMap: lazymap.New[*cp.ProbeReply](&lazymap.Config{})},
		hlsBuilder:     &HLSBuilder{aacCodec: "libfdk_aac", threads: 2, paceLead: 5 * time.Minute},
		sessionManager: sm,
		touchMap:       NewTouchMap(),
	}
	if configure != nil {
		configure(web)
	}
	web.buildHandler()
	t.Cleanup(func() {
		sm.CloseAll()
		runMgr.CloseAll()
	})
	return web, sm, out
}

func goldenSourceURL(name string) string {
	return "http://source/" + name + "/file.mkv"
}

// goldenRun records one source through the old route. decode is the value
// of the client's decode parameter ("" sends none).
func goldenRun(t *testing.T, web *Web, sm *SessionManager, out string, src goldenSource, decode string) goldenRecord {
	t.Helper()
	norm := func(s string) string {
		s = strings.ReplaceAll(s, out, "<OUT>")
		return goldenSessionID.ReplaceAllString(s, "<SID>")
	}
	normArgs := func(args []string) []string {
		res := make([]string, 0, len(args))
		for _, a := range args[1:] { // [0] is the fake's path
			res = append(res, norm(a))
		}
		return res
	}
	serve := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("X-Source-Url", goldenSourceURL(src.name))
		w := httptest.NewRecorder()
		web.handler.ServeHTTP(w, r)
		return w
	}
	auth := "api-key=K&token=T"
	var rec goldenRecord

	q := auth
	if decode != "" {
		q = "decode=" + url.QueryEscape(decode) + "&" + auth
	}
	w := serve(http.MethodPost, "/session?"+q)
	rec.PostStatus = w.Code
	if w.Code != http.StatusOK {
		rec.PostBody = w.Body.String()
	} else {
		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		rec.PostJSON = fmt.Sprintf("duration=%v", resp["duration"])
		id, _ := resp["id"].(string)
		sess := sm.Get(id)
		if sess == nil || sess.run == nil || sess.run.cmd == nil {
			t.Fatalf("%s: no run behind the session", src.name)
		}
		rec.StartArgs = normArgs(sess.run.cmd.Args)

		w = serve(http.MethodGet, "/session/"+id+"/index.m3u8?"+auth)
		rec.Master = norm(w.Body.String())

		primary := sess.h.primary[0].GetPlaylistName()
		ext := sess.h.primary[0].GetSegmentExtension()
		base := strings.TrimSuffix(primary, ".m3u8")
		variant := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ALLOW-CACHE:YES\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.004000,\n%[1]s-0.%[2]s\n#EXTINF:4.004000,\n%[1]s-1.%[2]s\n", base, ext)
		if err := os.WriteFile(filepath.Join(sess.run.OutputDir(), primary+".ffmpeg"), []byte(variant), 0644); err != nil {
			t.Fatal(err)
		}
		w = serve(http.MethodGet, "/session/"+id+"/"+primary+"?"+auth)
		rec.Variant = norm(w.Body.String())

		// The audio and subtitle playlists, from lists as FFmpeg's segment
		// muxer writes them (a subtitle without output never has one).
		for _, m := range append(append([]*HLSStream{}, sess.h.audio...), sess.h.subs...) {
			if rec.Media == nil { // none for a source without: as the record reads back
				rec.Media = map[string]string{}
			}
			name := m.GetPlaylistName()
			pl := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ALLOW-CACHE:YES\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.000000,\n%s-0.%s\n",
				strings.TrimSuffix(name, ".m3u8"), m.GetSegmentExtension())
			if err := os.WriteFile(filepath.Join(sess.run.OutputDir(), name+".ffmpeg"), []byte(pl), 0644); err != nil {
				t.Fatal(err)
			}
			w = serve(http.MethodGet, "/session/"+id+"/"+name+"?"+auth)
			rec.Media[name] = fmt.Sprintf("%d\n%s", w.Code, norm(w.Body.String()))
		}
		// A segment of the primary stream and of the first audio track.
		rec.Segments = map[string]string{}
		segs := []*HLSStream{sess.h.primary[0]}
		if len(sess.h.audio) > 0 && sess.h.audio[0] != sess.h.primary[0] {
			segs = append(segs, sess.h.audio[0])
		}
		for _, m := range segs {
			name := strings.TrimSuffix(m.GetPlaylistName(), ".m3u8") + "-0." + m.GetSegmentExtension()
			body := make([]byte, 376)
			for i := range body {
				body[i] = byte(0x47 + i%7)
			}
			if err := os.WriteFile(filepath.Join(sess.run.OutputDir(), name), body, 0644); err != nil {
				t.Fatal(err)
			}
			w = serve(http.MethodGet, "/session/"+id+"/"+name+"?"+auth)
			rec.Segments[name] = goldenResponse(w, sess.run.Generation())
		}

		w = serve(http.MethodPost, "/session/"+id+"/seek?t=615&"+auth)
		rec.SeekBody = w.Body.String()
		sess = sm.Get(id)
		if sess != nil && sess.run != nil && sess.run.cmd != nil {
			rec.SeekArgs = normArgs(sess.run.cmd.Args)
		}
		sm.Close(id)
	}

	w = serve(http.MethodGet, "/index.m3u8?"+auth)
	rec.LegacyCode = w.Code
	rec.LegacyBody = norm(w.Body.String())
	if w.Code == http.StatusOK {
		m := goldenSessionID.FindString(w.Body.String())
		if sess := sm.Get(m); sess != nil {
			if err := sess.EnsureRunning(); err != nil {
				rec.LegacyError = norm(err.Error())
			} else if sess.run != nil && sess.run.cmd != nil {
				rec.LegacyArgs = normArgs(sess.run.cmd.Args)
			}
			sm.Close(m)
		}
	}
	return rec
}

// goldenResponse is a response as the record keeps it: the status, every
// header but the date (the run's generation, random, as <GEN>), and the
// body's hash.
func goldenResponse(w *httptest.ResponseRecorder, gen string) string {
	var keys []string
	for k := range w.Header() {
		if k != "Date" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := []string{fmt.Sprint(w.Code)}
	for _, k := range keys {
		lines = append(lines, k+": "+strings.ReplaceAll(strings.Join(w.Header()[k], ","), gen, "<GEN>"))
	}
	sum := sha1.Sum(w.Body.Bytes())
	return strings.Join(append(lines, "body sha1 "+hex.EncodeToString(sum[:])), "\n")
}

// goldenMetricFamilies lists the metric families of the default registry.
func goldenMetricFamilies(t *testing.T) []string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, mf := range families {
		if !strings.HasPrefix(mf.GetName(), metricsNamespace+"_") {
			continue
		}
		labels := map[string]bool{}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = true
			}
		}
		var names []string
		for l := range labels {
			names = append(names, l)
		}
		sort.Strings(names)
		out = append(out, fmt.Sprintf("%s %s {%s} %s", mf.GetName(), mf.GetType(), strings.Join(names, ","), mf.GetHelp()))
	}
	sort.Strings(out)
	return out
}

// goldenCapture runs every golden source with the given setup.
func goldenCapture(t *testing.T, configure func(*Web), decode string) map[string]goldenRecord {
	t.Helper()
	fakeFFmpeg(t)
	orig := probeRunStart
	probeRunStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) {
		return seek - 2.5, nil
	}
	t.Cleanup(func() { probeRunStart = orig })
	web, sm, out := goldenWeb(t, configure)
	res := map[string]goldenRecord{}
	for _, src := range goldenSources {
		res[src.name] = goldenRun(t, web, sm, out, src, decode)
	}
	return res
}

// TestGoldenWrite writes the record; it runs only with GOLDEN_WRITE set
// (to the file to write), on the commit the record is taken from.
func TestGoldenWrite(t *testing.T) {
	path := os.Getenv("GOLDEN_WRITE")
	if path == "" {
		t.Skip("GOLDEN_WRITE not set")
	}
	g := goldenFile{Sources: goldenCapture(t, nil, ""), Metrics: goldenMetricFamilies(t)}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
