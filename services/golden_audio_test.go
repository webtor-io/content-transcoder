package services

// The golden record of the declared audio cases (listed in the header of
// golden_old_route_test.go): what POST /session, its playlists and a seek
// produce for clients that declare audio tokens, recorded on this code
// (GOLDEN_AUDIO_WRITE) in testdata/golden_audio.json.
//
//   - ts_aac51: passthrough not configured, decode=aac51, every golden
//     source and the two surround sources below, through goldenRun (the old
//     record's shape). A source whose audio the declaration leaves as it
//     is must match 1b25e28's record byte for byte; the others must not.
//   - passthrough_aac51_ec3: the capability hevc, decode=hevc10,aac51,ec3;
//     the HEVC sources with source facts pass through (their masters built
//     from a real FFmpeg init), the others take the old route.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	cp "github.com/webtor-io/content-prober/content-prober"
)

const goldenAudioPath = "testdata/golden_audio.json"

var goldenInitGen = regexp.MustCompile(`-init-[0-9a-f]{16}\.mp4`)

// surroundStreams is every kind of audio track after a video of codec:
// AAC stereo, E-AC-3 5.1 at 640 kb/s, AAC 5.1 with a BPS tag, AC-3 5.1,
// AAC 7.1, DTS 5.1 and TrueHD 7.1.
func surroundStreams(codec string) []*cp.Stream {
	return []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: codec, Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2, Tags: map[string]string{"language": "eng"}},
		{Index: 2, CodecType: "audio", CodecName: "eac3", Channels: 6, BitRate: "640000", Tags: map[string]string{"language": "eng", "title": "Atmos"}},
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6, Tags: map[string]string{"language": "rus", "BPS": "384123"}},
		{Index: 4, CodecType: "audio", CodecName: "ac3", Channels: 6, BitRate: "448000"},
		{Index: 5, CodecType: "audio", CodecName: "aac", Channels: 8},
		{Index: 6, CodecType: "audio", CodecName: "dts", Channels: 6, BitRate: "1509000"},
		{Index: 7, CodecType: "audio", CodecName: "truehd", Channels: 8},
	}
}

var audioGoldenSources = []goldenSource{
	{"h264-1080-surround", surroundStreams("h264")},
	{"hevc-1080-surround", surroundStreams("hevc")},
}

// goldenAudioChanged are the golden sources whose record the declaration
// aac51 changes: a track over 2 channels in a session that opens (the
// TrueHD of hevc-2160-hdr is in a session refused with 415).
var goldenAudioChanged = map[string]bool{
	"h264-2160-eac3-subs": true, "hevc-800-ac3-dvdsub": true, "audio-only-flac-51": true,
	"h264-1080-surround": true, "hevc-1080-surround": true,
}

// audioGoldenRecord is what a passthrough-case session answered.
type audioGoldenRecord struct {
	PostStatus int      `json:"post_status"`
	PostBody   string   `json:"post_body,omitempty"`
	Route      string   `json:"route,omitempty"`
	StartArgs  []string `json:"start_args,omitempty"`
	Master     string   `json:"master,omitempty"`
	SeekArgs   []string `json:"seek_args,omitempty"`
}

type goldenAudioFile struct {
	TS          map[string]goldenRecord      `json:"ts_aac51"`
	Passthrough map[string]audioGoldenRecord `json:"passthrough_aac51_ec3"`
}

// goldenHashDir is the output dir of a golden source under out.
func goldenHashDir(out, name string) string {
	u, _ := url.Parse(goldenSourceURL(name))
	sum := sha1.Sum([]byte(u.Path))
	return filepath.Join(out, hex.EncodeToString(sum[:]))
}

// writeGoldenProbes puts content-prober's answer for srcs under out.
func writeGoldenProbes(t *testing.T, out string, srcs []goldenSource) {
	t.Helper()
	for _, src := range srcs {
		hashDir := goldenHashDir(out, src.name)
		if err := os.MkdirAll(hashDir, 0755); err != nil {
			t.Fatal(err)
		}
		probe, _ := json.Marshal(&cp.ProbeReply{Streams: src.streams, Format: &cp.Format{Duration: "5400.000000"}})
		if err := os.WriteFile(filepath.Join(hashDir, "index.json"), probe, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// stubGoldenProbes answers the real-start probes 2.5 s before the seek, as
// the old record's capture does, and fails the source probe of any HEVC
// source without facts.
func stubGoldenProbes(t *testing.T) {
	t.Helper()
	origRun, origPass, origSrc := probeRunStart, probePassthroughStart, runSourceProbe
	probeRunStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 2.5, nil }
	probePassthroughStart = probeRunStart
	runSourceProbe = func(context.Context, string, int) ([]byte, error) { return nil, os.ErrDeadlineExceeded }
	t.Cleanup(func() { probeRunStart, probePassthroughStart, runSourceProbe = origRun, origPass, origSrc })
}

func goldenAudioCaptureTS(t *testing.T) map[string]goldenRecord {
	t.Helper()
	fakeFFmpeg(t)
	stubGoldenProbes(t)
	web, sm, out := goldenWeb(t, nil)
	writeGoldenProbes(t, out, audioGoldenSources)
	res := map[string]goldenRecord{}
	for _, src := range append(append([]goldenSource{}, goldenSources...), audioGoldenSources...) {
		res[src.name] = goldenRun(t, web, sm, out, src, "aac51")
	}
	return res
}

// passthroughGoldenFacts are the sources that pass through: their source
// facts are on disk, as a successful source probe leaves them.
var passthroughGoldenFacts = []string{"hevc-1080-main10-aac", "hevc-800-ac3-dvdsub", "hevc-1080-surround"}

func goldenAudioCapturePassthrough(t *testing.T) map[string]audioGoldenRecord {
	t.Helper()
	fakeFFmpeg(t)
	stubGoldenProbes(t)
	origWait := passthroughMasterTimeout
	passthroughMasterTimeout = 2 * time.Second
	t.Cleanup(func() { passthroughMasterTimeout = origWait })
	web, sm, out := goldenWeb(t, func(w *Web) {
		w.hlsBuilder.passthrough = capabilityWith("hevc")
		w.sourceProber = newSourceProber()
	})
	writeGoldenProbes(t, out, audioGoldenSources)
	for _, name := range passthroughGoldenFacts {
		writeSourceFacts(sourceFactsPath(goldenHashDir(out, name), 0), sdrMain10())
	}
	// Session ids and the process generations in init names are random.
	norm := func(s string) string {
		s = goldenSessionID.ReplaceAllString(strings.ReplaceAll(s, out, "<OUT>"), "<SID>")
		return goldenInitGen.ReplaceAllString(s, "-init-<GEN>.mp4")
	}
	normArgs := func(args []string) []string {
		res := make([]string, 0, len(args))
		for _, a := range args[1:] {
			res = append(res, norm(a))
		}
		return res
	}
	res := map[string]audioGoldenRecord{}
	for _, src := range append(append([]goldenSource{}, goldenSources...), audioGoldenSources...) {
		serve := func(method, target string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, target, nil)
			r.Header.Set("X-Source-Url", goldenSourceURL(src.name))
			w := httptest.NewRecorder()
			web.handler.ServeHTTP(w, r)
			return w
		}
		var rec audioGoldenRecord
		w := serve(http.MethodPost, "/session?decode="+url.QueryEscape("hevc10,aac51,ec3"))
		rec.PostStatus = w.Code
		if w.Code != http.StatusOK {
			rec.PostBody = w.Body.String()
			res[src.name] = rec
			continue
		}
		var resp sessionCreateResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		rec.Route = resp.VideoRoute + "/" + resp.RouteReason
		sess := sm.Get(resp.ID)
		rec.StartArgs = normArgs(sess.run.cmd.Args)
		if sess.h.passthrough {
			// What FFmpeg leaves after its first cut: the video's init (a
			// real FFmpeg 8.1.2 one) and first segment.
			v := sess.h.primaryVideo()
			passthroughRunDir(t, sess.run.OutputDir(), v.streamPrefix(), sess.run.Generation(), fixtureInit(t, "main10-init.mp4"), 1_000_000)
		}
		rec.Master = norm(serve(http.MethodGet, "/session/"+resp.ID+"/index.m3u8").Body.String())
		serve(http.MethodPost, "/session/"+resp.ID+"/seek?t=615")
		if s := sm.Get(resp.ID); s != nil && s.run != nil && s.run.cmd != nil {
			rec.SeekArgs = normArgs(s.run.cmd.Args)
		}
		sm.Close(resp.ID)
		res[src.name] = rec
	}
	return res
}

// TestGoldenAudioWrite writes the declared record; it runs only with
// GOLDEN_AUDIO_WRITE set (to the file to write).
func TestGoldenAudioWrite(t *testing.T) {
	path := os.Getenv("GOLDEN_AUDIO_WRITE")
	if path == "" {
		t.Skip("GOLDEN_AUDIO_WRITE not set")
	}
	g := goldenAudioFile{TS: goldenAudioCaptureTS(t), Passthrough: goldenAudioCapturePassthrough(t)}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

// The declared cases answer what the record says, and a declaration that
// leaves a source's audio as it is leaves its whole record as 1b25e28's.
func TestGolden_DeclaredAudio(t *testing.T) {
	b, err := os.ReadFile(goldenAudioPath)
	if err != nil {
		t.Fatal(err)
	}
	var want goldenAudioFile
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	ob, err := os.ReadFile(goldenRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	var old goldenFile
	if err := json.Unmarshal(ob, &old); err != nil {
		t.Fatal(err)
	}
	t.Run("ts_aac51", func(t *testing.T) {
		got := goldenAudioCaptureTS(t)
		if len(got) != len(want.TS) {
			t.Fatalf("%d sources, the record %d: regenerate it", len(got), len(want.TS))
		}
		for name, g := range got {
			if !reflect.DeepEqual(g, want.TS[name]) {
				gj, _ := json.MarshalIndent(g, "", "  ")
				wj, _ := json.MarshalIndent(want.TS[name], "", "  ")
				t.Errorf("%s differs from the declared record\n--- got\n%s\n--- want\n%s", name, gj, wj)
			}
			o, inOld := old.Sources[name]
			if !inOld {
				continue
			}
			if same := reflect.DeepEqual(g, o); same == goldenAudioChanged[name] {
				t.Errorf("%s: same as 1b25e28 %v, want %v (its audio is changed by aac51: %v)", name, same, !goldenAudioChanged[name], goldenAudioChanged[name])
			}
		}
	})
	t.Run("passthrough_aac51_ec3", func(t *testing.T) {
		got := goldenAudioCapturePassthrough(t)
		if len(got) != len(want.Passthrough) {
			t.Fatalf("%d sources, the record %d: regenerate it", len(got), len(want.Passthrough))
		}
		for name, g := range got {
			if !reflect.DeepEqual(g, want.Passthrough[name]) {
				gj, _ := json.MarshalIndent(g, "", "  ")
				wj, _ := json.MarshalIndent(want.Passthrough[name], "", "  ")
				t.Errorf("%s differs from the declared record\n--- got\n%s\n--- want\n%s", name, gj, wj)
			}
		}
	})
}
