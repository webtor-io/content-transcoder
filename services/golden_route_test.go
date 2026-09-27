package services

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// capabilityWith is a capability source holding codecs whatever this build
// can write (passthroughBuildCodecs): the decisions are tested as they will
// run once the output side exists.
func capabilityWith(codecs ...string) *passthroughCapabilitySource {
	c := passthroughCapability{codecs: map[string]bool{}}
	for _, k := range codecs {
		c.codecs[k] = true
	}
	return &passthroughCapabilitySource{current: c}
}

// noSourceProbe fails the test if the transcoder's own look at a source
// runs: none of the golden replays may need it.
func noSourceProbe(t *testing.T) {
	t.Helper()
	orig := runSourceProbe
	runSourceProbe = func(context.Context, string, int) ([]byte, error) {
		t.Error("the source probe ran for a session that cannot pass through")
		return nil, os.ErrInvalid
	}
	t.Cleanup(func() { runSourceProbe = orig })
}

// The old route is byte for byte what 1b25e28 does -- FFmpeg's arguments at
// start, after a seek and on the legacy route, the master and variant
// playlists, the seek answer, every refusal and its body -- whenever the
// session cannot pass through: passthrough not configured (the default),
// configured but the client declaring nothing, something unparsable, or
// "unknown". The record was taken on 1b25e28 (golden_old_route_test.go),
// with the deliberate departures listed there.
func TestGolden_OldRouteUnchanged(t *testing.T) {
	b, err := os.ReadFile(goldenRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	var want goldenFile
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Sources) != len(goldenSources) {
		t.Fatalf("the record has %d sources, the test %d: regenerate it", len(want.Sources), len(goldenSources))
	}
	withHEVC := func(w *Web) {
		w.hlsBuilder.passthrough = capabilityWith("hevc")
		w.sourceProber = newSourceProber()
	}
	all := "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"
	cases := []struct {
		name      string
		configure func(*Web)
		decode    string
	}{
		{"passthrough not configured, no declaration", nil, ""},
		{"passthrough not configured, full declaration", nil, all},
		{"passthrough configured, no declaration", withHEVC, ""},
		{"passthrough configured, unparsable declaration", withHEVC, "h265,,foo=bar,HEVC10, hevc 10"},
		{"passthrough configured, declaration pending", withHEVC, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noSourceProbe(t)
			got := goldenCapture(t, c.configure, c.decode)
			for _, src := range goldenSources {
				g, w := got[src.name], want.Sources[src.name]
				if !reflect.DeepEqual(g, w) {
					gj, _ := json.MarshalIndent(g, "", "  ")
					wj, _ := json.MarshalIndent(w, "", "  ")
					t.Errorf("%s differs from 1b25e28\n--- got\n%s\n--- want\n%s", src.name, gj, wj)
				}
			}
		})
	}

	// Metrics: every family 1b25e28 exposes is still there with the same
	// type, help and label names; new ones and new label values may be added.
	have := map[string]bool{}
	for _, m := range goldenMetricFamilies(t) {
		have[m] = true
	}
	for _, m := range want.Metrics {
		if !have[m] {
			t.Errorf("metric family changed or gone: %s", m)
		}
	}
}

// Negative control for the replay: a session that does get passthrough is
// not the old route, and the comparison sees it. A stand-in gives the run
// recognisable arguments; the fake FFmpeg writes no init, so the master of
// the passthrough session is not answered (a short wait instead of 5 min).
func TestGolden_ReplayNoticesARouteChange(t *testing.T) {
	b, err := os.ReadFile(goldenRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	var want goldenFile
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	orig := buildPassthroughParams
	buildPassthroughParams = func(h *HLS, in *url.URL, out string, _ ParamOptions, _ string) ([]string, error) {
		return []string{"-i", in.String(), "-passthrough-stand-in", out}, nil
	}
	origWait, origStart := passthroughMasterTimeout, probePassthroughStart
	passthroughMasterTimeout = 100 * time.Millisecond
	// The fake FFmpeg cannot say where a seek lands.
	probePassthroughStart = func(_ context.Context, _ string, _ string, seek float64) (float64, error) { return seek - 2.5, nil }
	t.Cleanup(func() {
		buildPassthroughParams, passthroughMasterTimeout, probePassthroughStart = orig, origWait, origStart
	})
	const passing = "hevc-1080-main10-aac"
	configure := func(w *Web) {
		w.hlsBuilder.passthrough = capabilityWith("hevc")
		w.sourceProber = newSourceProber()
		u, _ := url.Parse(goldenSourceURL(passing))
		sum := sha1.Sum([]byte(u.Path))
		writeSourceFacts(sourceFactsPath(filepath.Join(w.output, hex.EncodeToString(sum[:])), 0), sourceHEVCFacts{
			Version: sourceFactsVersion, PixFmt: "yuv420p10le", HVCC: testHVCC(2, false, 120), Packets: 2,
		})
	}
	// Every other HEVC source needs the look, and it fails: the old route
	// plays them, and where it refuses (the 2160 one) the answer is the
	// retryable 503 instead of the 415.
	probeRuns := 0
	origProbe := runSourceProbe
	runSourceProbe = func(context.Context, string, int) ([]byte, error) {
		probeRuns++
		return nil, os.ErrDeadlineExceeded
	}
	t.Cleanup(func() { runSourceProbe = origProbe })
	const checkFailed = "hevc-2160-hdr"
	got := goldenCapture(t, configure, "hevc10,hevc10-2160,hdr-pq")
	for _, src := range goldenSources {
		g, w := got[src.name], want.Sources[src.name]
		same := reflect.DeepEqual(g, w)
		switch src.name {
		case passing:
			if same {
				t.Errorf("%s went through passthrough and still matched the old route's record", src.name)
			}
			if !strings.Contains(strings.Join(g.StartArgs, " "), "-passthrough-stand-in") {
				t.Errorf("%s: not started through the passthrough builder: %v", passing, g.StartArgs)
			}
		case checkFailed:
			if g.PostStatus != 503 || g.PostBody != errSourceCheckFailed+"\n" {
				t.Errorf("%s: failed check over 1080p answered %d %q, want 503 %q", src.name, g.PostStatus, g.PostBody, errSourceCheckFailed)
			}
			g.PostStatus, g.PostBody = w.PostStatus, w.PostBody
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: only the POST answer may change", src.name)
			}
		default:
			if !same {
				t.Errorf("%s cannot pass through (not hevc, or the look failed) and must match the record", src.name)
			}
		}
	}
	// hevc-800, 2560x1080, cover-art and 2160, each tried twice.
	if probeRuns != 4*sourceProbeAttempts {
		t.Errorf("source probe ran %d times, want %d", probeRuns, 4*sourceProbeAttempts)
	}
}
