package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The source probe fixtures are ffprobe 8.1.2's (the production image's)
// real output for small synthetic files, taken with sourceProbeArgs by
// TestSourceProbe_RealFFprobe run in that image. The Dolby Vision ones are
// MP4s with a dvcC box written in by hand and the RPU ones HEVC streams with
// a NAL of type 62 after every picture: the build has no way to make a real
// Dolby Vision stream.
type sourceProbeFixture struct {
	file   string
	stream int
	check  func(t *testing.T, f sourceHEVCFacts)
	// reason is the route a client declaring everything gets on a 1080p
	// source; noPQ the one of a client without hdr-pq ("" when the same).
	reason, noPQ string
}

var sourceProbeFixtures = []sourceProbeFixture{
	{"sdr8_audiofirst.mkv", 1, func(t *testing.T, f sourceHEVCFacts) {
		h, _ := parseHVCC(f.HVCC)
		if f.PixFmt != "yuv420p" || h.profileIdc != 1 || f.DOVI != nil || f.RPU || f.Packets != 2 {
			t.Errorf("8-bit Main: %+v %+v", f, h)
		}
	}, reasonOK, ""},
	{"plain.mp4", 0, nil, reasonOK, ""},
	{"pq10.mkv", 0, func(t *testing.T, f sourceHEVCFacts) {
		h, _ := parseHVCC(f.HVCC)
		if f.PixFmt != "yuv420p10le" || f.ColorTransfer != transferPQ || h.profileIdc != 2 {
			t.Errorf("PQ Main10 (PQ only in the VUI): %+v %+v", f, h)
		}
	}, reasonOK, reasonNeedsPQ},
	{"pq10.mp4", 0, nil, reasonOK, reasonNeedsPQ},
	{"hlg10.mkv", 0, func(t *testing.T, f sourceHEVCFacts) {
		if f.ColorTransfer != transferHLG {
			t.Errorf("HLG: %+v", f)
		}
	}, reasonHLGLater, ""},
	{"rext422.mkv", 0, func(t *testing.T, f sourceHEVCFacts) {
		h, _ := parseHVCC(f.HVCC)
		if f.PixFmt != "yuv422p10le" || h.profileIdc != 4 {
			t.Errorf("4:2:2: %+v %+v", f, h)
		}
	}, reasonPixFmt, ""},
	{"hightier.mkv", 0, func(t *testing.T, f sourceHEVCFacts) {
		if h, _ := parseHVCC(f.HVCC); !h.tierHigh {
			t.Errorf("x265 high-tier=1: tier flag not read, %+v", h)
		}
	}, reasonOK, ""},
	{"annexb.ts", 0, func(t *testing.T, f sourceHEVCFacts) {
		if _, ok := parseHVCC(f.HVCC); ok {
			t.Errorf("TS extradata read as hvcC: % x", f.HVCC)
		}
	}, reasonNoHVCC, ""},
	{"rpu.mp4", 0, func(t *testing.T, f sourceHEVCFacts) {
		if !f.RPU || f.DOVI != nil {
			t.Errorf("NAL 62 without a record: %+v", f)
		}
	}, reasonDVUnknown, ""},
	{"dv81.mp4", 0, func(t *testing.T, f sourceHEVCFacts) {
		if f.DOVI == nil || f.DOVI.Profile != 8 || f.DOVI.Compatibility != 1 || f.ColorTransfer != transferPQ {
			t.Errorf("DV 8.1: %+v %+v", f, f.DOVI)
		}
	}, reasonOK, reasonNeedsPQ},
	{"dv82_rpu.mp4", 0, func(t *testing.T, f sourceHEVCFacts) {
		if f.DOVI == nil || f.DOVI.Profile != 8 || f.DOVI.Compatibility != 2 || !f.RPU {
			t.Errorf("DV 8.2 with RPUs: %+v %+v", f, f.DOVI)
		}
	}, reasonOK, ""},
	{"dv5.mp4", 0, func(t *testing.T, f sourceHEVCFacts) {
		if f.DOVI == nil || f.DOVI.Profile != 5 {
			t.Errorf("DV 5: %+v", f)
		}
	}, reasonDV5, ""},
	{"dv7.mp4", 0, nil, reasonDV7, ""},
}

func (fx sourceProbeFixture) verify(t *testing.T, out []byte) {
	t.Helper()
	f, err := parseSourceProbe(out, fx.stream)
	if err != nil {
		t.Fatalf("%s: %v", fx.file, err)
	}
	if fx.check != nil {
		fx.check(t, f)
	}
	probe := func() (sourceHEVCFacts, error) { return f, nil }
	if got := videoRouteFor(hd, decl(allTokens), on, probe).reason; got != fx.reason {
		t.Errorf("%s: route reason %s, want %s", fx.file, got, fx.reason)
	}
	want := fx.noPQ
	if want == "" {
		want = fx.reason
	}
	if got := videoRouteFor(hd, decl("hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high"), on, probe).reason; got != want {
		t.Errorf("%s without hdr-pq: route reason %s, want %s", fx.file, got, want)
	}
}

func fixturePath(file string) string {
	return filepath.Join("testdata", "source_probe", file+".json")
}

func TestParseSourceProbe_Fixtures(t *testing.T) {
	for _, fx := range sourceProbeFixtures {
		t.Run(fx.file, func(t *testing.T) {
			out, err := os.ReadFile(fixturePath(fx.file))
			if err != nil {
				t.Fatal(err)
			}
			fx.verify(t, out)
		})
	}
}

// TestSourceProbe_RealFFprobe runs the real ffprobe with sourceProbeArgs
// over HTTP (the whitelist allows nothing else) against the synthetic files
// in SOURCE_PROBE_MEDIA, and with SOURCE_PROBE_FIXTURES set writes what it
// got there. Run in the production image:
//
//	GOOS=linux go test -c -o probe.test ./services
//	docker run --rm -v $PWD:/w -e SOURCE_PROBE_MEDIA=/w/media \
//	  -e SOURCE_PROBE_FIXTURES=/w/fixtures --entrypoint /w/probe.test \
//	  jrottenberg/ffmpeg:8-alpine -test.run TestSourceProbe_RealFFprobe -test.v
func TestSourceProbe_RealFFprobe(t *testing.T) {
	media := os.Getenv("SOURCE_PROBE_MEDIA")
	if media == "" {
		t.Skip("SOURCE_PROBE_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	fixtures := os.Getenv("SOURCE_PROBE_FIXTURES")
	for _, fx := range sourceProbeFixtures {
		t.Run(fx.file, func(t *testing.T) {
			started := time.Now()
			out, err := ffprobeSource(context.Background(), srv.URL+"/"+fx.file+"?api-key=secret", fx.stream)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d bytes in %v", fx.file, len(out), time.Since(started).Round(time.Millisecond))
			fx.verify(t, out)
			if fixtures != "" {
				if err := os.WriteFile(filepath.Join(fixtures, fx.file+".json"), out, 0644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	// A source that is not there is an error, not an empty answer.
	if _, err := ffprobeSource(context.Background(), srv.URL+"/missing.mkv", 0); err == nil {
		t.Error("probe of a missing file succeeded")
	}
}

func TestParseSourceProbe_Failures(t *testing.T) {
	good, err := os.ReadFile(fixturePath("plain.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]interface{}
	if err := json.Unmarshal(good, &o); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(o map[string]interface{})) []byte {
		var c map[string]interface{}
		_ = json.Unmarshal(good, &c)
		f(c)
		b, _ := json.Marshal(c)
		return b
	}
	stream := func(o map[string]interface{}) map[string]interface{} {
		return o["streams"].([]interface{})[0].(map[string]interface{})
	}
	for name, out := range map[string][]byte{
		"not json":      []byte("ffprobe: error"),
		"empty":         []byte("{}"),
		"other stream":  mutate(func(o map[string]interface{}) { stream(o)["index"] = 3 }),
		"not hevc":      mutate(func(o map[string]interface{}) { stream(o)["codec_name"] = "h264" }),
		"no pix_fmt":    mutate(func(o map[string]interface{}) { delete(stream(o), "pix_fmt") }),
		"no packets":    mutate(func(o map[string]interface{}) { delete(o, "packets") }),
		"bad extradata": mutate(func(o map[string]interface{}) { stream(o)["extradata"] = "\n!!!\n" }),
		"bad dv record": mutate(func(o map[string]interface{}) {
			stream(o)["side_data_list"] = []interface{}{map[string]interface{}{"side_data_type": doviSideDataType}}
		}),
		"packet garbled": mutate(func(o map[string]interface{}) {
			o["packets"].([]interface{})[0].(map[string]interface{})["data"] = "\n%%%\n"
		}),
	} {
		if _, err := parseSourceProbe(out, 0); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := parseSourceProbe(good, 0); err != nil {
		t.Fatalf("the unmutated fixture must parse: %v", err)
	}
}

func TestDecodeDataDump(t *testing.T) {
	data := make([]byte, 150)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// ffprobe's layout: a newline, then 60-byte lines.
	dump := "\n"
	for i := 0; i < len(data); i += 60 {
		end := i + 60
		if end > len(data) {
			end = len(data)
		}
		dump += base64.StdEncoding.EncodeToString(data[i:end]) + "\n"
	}
	got, err := decodeDataDump(dump)
	if err != nil || string(got) != string(data) {
		t.Fatalf("round trip: %v, %d bytes", err, len(got))
	}
}

func TestHasDolbyVisionNAL(t *testing.T) {
	lp := func(nals ...[]byte) []byte {
		var b []byte
		for _, n := range nals {
			b = append(b, byte(len(n)>>24), byte(len(n)>>16), byte(len(n)>>8), byte(len(n)))
			b = append(b, n...)
		}
		return b
	}
	idr := []byte{19 << 1, 1, 0xaf, 0x00, 0x01, 0x7c} // IDR; its payload holds 00 01 7c, not a NAL
	rpu := []byte{62 << 1, 1, 8, 9}
	el := []byte{63 << 1, 1, 8, 9}
	cases := []struct {
		name string
		pkt  []byte
		ls   int
		want bool
	}{
		{"length-prefixed IDR", lp(idr), 4, false},
		{"length-prefixed IDR + RPU", lp(idr, rpu), 4, true},
		{"length-prefixed EL", lp(idr, el), 4, true},
		{"annex B IDR + RPU", append(append([]byte{0, 0, 0, 1}, idr...), append([]byte{0, 0, 1}, rpu...)...), 0, true},
		{"annex B IDR", append([]byte{0, 0, 0, 1}, idr...), 0, false},
		// 380-byte NAL: its length field is 00 00 01 7c -- a start code and
		// an RPU header to a blind scan.
		{"length field that looks like a start code", lp(append([]byte{1 << 1, 1}, make([]byte, 378)...)), 4, false},
		{"2-byte lengths", []byte{0, 4, 62 << 1, 1, 8, 9}, 2, true},
	}
	for _, c := range cases {
		if got := hasDolbyVisionNAL(c.pkt, c.ls); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// stubSourceProbe answers every probe with out after an optional wait,
// counting the runs.
func stubSourceProbe(t *testing.T, out func(attempt int) ([]byte, error)) *int32 {
	t.Helper()
	var n int32
	orig := runSourceProbe
	runSourceProbe = func(context.Context, string, int) ([]byte, error) {
		return out(int(atomic.AddInt32(&n, 1)))
	}
	t.Cleanup(func() { runSourceProbe = orig })
	return &n
}

// One retry, the failure not kept (the next session asks again), a success
// kept on disk for every later session and pod of the node.
func TestSourceProber_RetryAndCache(t *testing.T) {
	good, err := os.ReadFile(fixturePath("pq10.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	hashDir := t.TempDir()
	ok0 := histogramCount(t, "transcoder_source_probe_seconds", map[string]string{"result": sourceProbeOK})
	failed0 := histogramCount(t, "transcoder_source_probe_seconds", map[string]string{"result": sourceProbeFailed})

	runs := stubSourceProbe(t, func(int) ([]byte, error) { return nil, errors.New("timeout") })
	p := newSourceProber()
	if _, err := p.Facts("http://src/x.mkv", hashDir, 0); err == nil {
		t.Fatal("failing probe answered")
	}
	if *runs != sourceProbeAttempts {
		t.Errorf("%d attempts, want %d", *runs, sourceProbeAttempts)
	}
	if _, err := os.Stat(sourceFactsPath(hashDir, 0)); err == nil {
		t.Error("a failure was written to the cache")
	}

	// First attempt fails, the retry answers.
	runs = stubSourceProbe(t, func(n int) ([]byte, error) {
		if n == 1 {
			return nil, errors.New("reset")
		}
		return good, nil
	})
	f, err := p.Facts("http://src/x.mkv", hashDir, 0)
	if err != nil || f.ColorTransfer != transferPQ {
		t.Fatalf("retry: %+v %v", f, err)
	}
	if *runs != 2 {
		t.Errorf("%d runs, want 2", *runs)
	}

	// Cached: another prober (another pod of the node) does not run it.
	runs = stubSourceProbe(t, func(int) ([]byte, error) { return nil, errors.New("must not run") })
	f2, err := newSourceProber().Facts("http://src/x.mkv", hashDir, 0)
	if err != nil || *runs != 0 || fmt.Sprint(f2) != fmt.Sprint(f) {
		t.Errorf("cache: %+v %v after %d runs", f2, err, *runs)
	}
	// Another stream of the same source is another probe.
	if _, err := p.Facts("http://src/x.mkv", hashDir, 1); err == nil || *runs == 0 {
		t.Errorf("stream 1 answered from stream 0's cache")
	}

	if got := histogramCount(t, "transcoder_source_probe_seconds", map[string]string{"result": sourceProbeOK}) - ok0; got != 1 {
		t.Errorf("source_probe_seconds{ok} +%d, want +1 (the cached answer is not a probe)", got)
	}
	if got := histogramCount(t, "transcoder_source_probe_seconds", map[string]string{"result": sourceProbeFailed}) - failed0; got != 2 {
		t.Errorf("source_probe_seconds{failed} +%d, want +2", got)
	}
}

// A stale cache layout is probed again, not trusted.
func TestSourceProber_StaleCacheVersion(t *testing.T) {
	hashDir := t.TempDir()
	if err := os.WriteFile(sourceFactsPath(hashDir, 0), []byte(`{"v":0,"pix_fmt":"yuv420p","rpu":false}`), 0644); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(fixturePath("plain.mp4"))
	runs := stubSourceProbe(t, func(int) ([]byte, error) { return good, nil })
	if _, err := newSourceProber().Facts("http://src/x.mkv", hashDir, 0); err != nil || *runs != 1 {
		t.Errorf("stale cache: err=%v runs=%d", err, *runs)
	}
}

// Sessions arriving together for one source run one probe.
func TestSourceProber_Singleflight(t *testing.T) {
	good, _ := os.ReadFile(fixturePath("plain.mp4"))
	release := make(chan struct{})
	runs := stubSourceProbe(t, func(int) ([]byte, error) { <-release; return good, nil })
	p := newSourceProber()
	hashDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Facts("http://src/x.mkv", hashDir, 0)
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if *runs != 1 {
		t.Errorf("%d probes for 8 concurrent sessions, want 1", *runs)
	}
}

func TestSourceProbeArgs(t *testing.T) {
	args := strings.Join(sourceProbeArgs("http://src/x.mkv?api-key=k", 3), " ")
	for _, want := range []string{
		"-protocol_whitelist http,https,tcp,tls",
		"-probesize 5000000",
		"-select_streams 3",
		"-read_intervals %+#2",
		"-show_data -data_dump_format base64",
		"-of json",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if !strings.HasSuffix(args, " http://src/x.mkv?api-key=k") {
		t.Errorf("the source must be the last argument: %q", args)
	}
	if _, err := ffprobeSource(context.Background(), "-i/etc/passwd", 0); err == nil {
		t.Error("a source that parses as an option ran")
	}
}
