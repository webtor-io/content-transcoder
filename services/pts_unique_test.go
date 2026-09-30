package services

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ptsSample is one sample of a test segment: its duration, size and
// composition offset.
type ptsSample struct {
	dur, size uint32
	cto       int32
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// ptsMoof is a moof as FFmpeg's hls muxer writes one for a video segment
// (movenc, 2026-09-30 production segments): tfhd default-base-is-moof with
// defaults, tfdt version 1, trun with durations, sizes and composition
// offsets (flags 0xb05). trunVersion 1 makes the offsets signed.
func ptsMoof(track uint32, base uint64, trunVersion byte, samples []ptsSample) []byte {
	trun := []byte{trunVersion, 0x00, 0x0b, 0x05}
	trun = append(trun, u32(uint32(len(samples)))...)
	trun = append(trun, u32(0)...)          // data_offset
	trun = append(trun, u32(0x02000000)...) // first_sample_flags
	for _, s := range samples {
		trun = append(trun, u32(s.dur)...)
		trun = append(trun, u32(s.size)...)
		trun = append(trun, u32(uint32(s.cto))...)
	}
	tfhd := append([]byte{0, 0x02, 0x00, 0x38}, u32(track)...)
	tfhd = append(tfhd, u32(512)...)        // default_sample_duration
	tfhd = append(tfhd, u32(1000)...)       // default_sample_size
	tfhd = append(tfhd, u32(0x01010000)...) // default_sample_flags
	return box("moof",
		box("mfhd", u32(0), u32(1)),
		box("traf",
			box("tfhd", tfhd),
			box("tfdt", []byte{1, 0, 0, 0}, u64(base)),
			box("trun", trun),
		),
	)
}

// ptsSegment is a segment: styp, sidx, then each moof with an mdat.
func ptsSegment(moofs ...[]byte) []byte {
	b := box("styp", []byte("msdh"), u32(0), []byte("msdhmsix"))
	b = append(b, box("sidx", make([]byte, 44))...)
	for _, m := range moofs {
		b = append(b, m...)
		b = append(b, box("mdat", []byte("frames"))...)
	}
	return b
}

// presentationTimes reads the samples' presentation times back, each moof's
// track in order -- independently of uniquePTSPatches, through findBox.
func presentationTimes(t *testing.T, seg []byte) []int64 {
	t.Helper()
	var out []int64
	for rest := seg; len(rest) >= 8; {
		size, typ, hdr, ok := boxHeader(rest)
		if !ok {
			break
		}
		if typ == "moof" {
			traf, _ := findBox(rest[hdr:size], "traf")
			tfdt, _ := findBox(traf, "tfdt")
			trun, _ := findBox(traf, "trun")
			dts := int64(binary.BigEndian.Uint64(tfdt[4:12]))
			n := int(binary.BigEndian.Uint32(trun[4:8]))
			for i := 0; i < n; i++ {
				s := 16 + i*12
				cto := int64(binary.BigEndian.Uint32(trun[s+8:]))
				if trun[0] == 1 {
					cto = int64(int32(binary.BigEndian.Uint32(trun[s+8:])))
				}
				out = append(out, dts+cto)
				dts += int64(binary.BigEndian.Uint32(trun[s:]))
			}
		}
		rest = rest[size:]
	}
	return out
}

// served is seg as it goes out: with the patches written.
func served(t *testing.T, seg []byte) (out []byte, moved int, ok bool) {
	t.Helper()
	patches, moved, ok := uniquePTSPatches(bytes.NewReader(seg), int64(len(seg)))
	out = make([]byte, len(seg))
	n, err := patchedReaderAt{r: bytes.NewReader(seg), patches: patches}.ReadAt(out, 0)
	if n != len(seg) || err != nil {
		t.Fatalf("ReadAt: %d, %v", n, err)
	}
	return out, moved, ok
}

// The shape found (2026-09-30): a CRA (sample 0) and a RASL_N two samples
// later at the same presentation time. The later one moves one tick; the
// keyframe and everything else stay.
func TestUniquePTS_LeadingPictureAtTheKeyframesTime(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 16000, 0, []ptsSample{
		{512, 900, 1024}, // CRA: 16000 + 1024
		{512, 100, 1536}, // 16512 + 1536
		{512, 80, 0},     // RASL_N: 16000 + 1024 + 0 -- the CRA's time
		{512, 90, 1024},  // 18560
	}))
	out, moved, ok := served(t, seg)
	if !ok || moved != 1 {
		t.Fatalf("ok %v, moved %d; want true, 1", ok, moved)
	}
	if got, want := presentationTimes(t, out), []int64{17024, 18048, 17025, 18560}; !equalInt64(got, want) {
		t.Errorf("times %v, want %v", got, want)
	}
	if len(out) != len(seg) {
		t.Errorf("size %d, want %d", len(out), len(seg))
	}
}

// The next free tick: the one after the duplicate taken too.
func TestUniquePTS_NextFreeTick(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 0, 0, []ptsSample{
		{100, 1, 200}, // 200
		{100, 1, 101}, // 201
		{100, 1, 0},   // 200 -> 201 taken -> 202
	}))
	out, moved, ok := served(t, seg)
	if !ok || moved != 1 {
		t.Fatalf("ok %v, moved %d", ok, moved)
	}
	if got, want := presentationTimes(t, out), []int64{200, 201, 202}; !equalInt64(got, want) {
		t.Errorf("times %v, want %v", got, want)
	}
}

// Signed offsets (trun version 1), a duplicate below the decode time.
func TestUniquePTS_SignedOffsets(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 1000, 1, []ptsSample{
		{100, 1, 0},    // 1000
		{100, 1, -100}, // 1000
	}))
	out, moved, ok := served(t, seg)
	if !ok || moved != 1 {
		t.Fatalf("ok %v, moved %d", ok, moved)
	}
	if got, want := presentationTimes(t, out), []int64{1000, 1001}; !equalInt64(got, want) {
		t.Errorf("times %v, want %v", got, want)
	}
}

// Two moofs of one segment are one timeline (their tfdt): a duplicate
// across them moves too.
func TestUniquePTS_AcrossMoofsOfOneSegment(t *testing.T) {
	seg := ptsSegment(
		ptsMoof(1, 0, 0, []ptsSample{{100, 1, 100}, {100, 1, 100}}), // 100, 200
		ptsMoof(1, 200, 0, []ptsSample{{100, 1, 0}}),                // 200
	)
	out, moved, ok := served(t, seg)
	if !ok || moved != 1 {
		t.Fatalf("ok %v, moved %d", ok, moved)
	}
	if got, want := presentationTimes(t, out), []int64{100, 200, 201}; !equalInt64(got, want) {
		t.Errorf("times %v, want %v", got, want)
	}
}

// Nothing to move: no patches, and the bytes as written.
func TestUniquePTS_Clean(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 0, 0, []ptsSample{{100, 1, 200}, {100, 1, 0}, {100, 1, 300}}))
	patches, moved, ok := uniquePTSPatches(bytes.NewReader(seg), int64(len(seg)))
	if !ok || moved != 0 || len(patches) != 0 {
		t.Errorf("ok %v, moved %d, %d patches; want true, 0, 0", ok, moved, len(patches))
	}
}

// What cannot be read is served as written: no patches, ok false.
func TestUniquePTS_Unreadable(t *testing.T) {
	good := ptsMoof(1, 0, 0, []ptsSample{{100, 1, 0}, {0, 1, 0}})
	// A duplicate of a decode time (a zero duration) and no composition
	// offsets to move it by: flags 0x305.
	noCTO := func() []byte {
		trun := []byte{0, 0x00, 0x03, 0x05}
		trun = append(trun, u32(2)...)
		trun = append(trun, u32(0)...)
		trun = append(trun, u32(0)...)
		trun = append(trun, u32(0)...) // dur 0
		trun = append(trun, u32(1)...)
		trun = append(trun, u32(0)...)
		trun = append(trun, u32(1)...)
		return box("moof", box("traf", box("tfhd", []byte{0, 0x02, 0, 0}, u32(1)), box("tfdt", []byte{1, 0, 0, 0}, u64(0)), box("trun", trun)))
	}()
	// No duration in the trun and none in the tfhd: flags 0xa05, tfhd 0.
	noDur := func() []byte {
		trun := []byte{0, 0x00, 0x0a, 0x05}
		trun = append(trun, u32(1)...)
		trun = append(trun, u32(0)...)
		trun = append(trun, u32(0)...)
		trun = append(trun, u32(1)...)
		trun = append(trun, u32(0)...)
		return box("moof", box("traf", box("tfhd", []byte{0, 0x02, 0, 0}, u32(1)), box("tfdt", []byte{1, 0, 0, 0}, u64(0)), box("trun", trun)))
	}()
	noTfdt := box("moof", box("traf", box("tfhd", []byte{0, 0x02, 0, 0x08}, u32(1), u32(100)), box("trun", []byte{0, 0, 0x08, 0x01}, u32(1), u32(0), u32(0))))
	cutMoof := ptsSegment(good)
	cutMoof = cutMoof[:len(ptsSegment())+len(good)-4]
	for name, seg := range map[string][]byte{
		"a duplicate without an offset": ptsSegment(noCTO),
		"no duration":                   ptsSegment(noDur),
		"no tfdt":                       ptsSegment(noTfdt),
		"a moof cut short":              cutMoof,
	} {
		patches, _, ok := uniquePTSPatches(bytes.NewReader(seg), int64(len(seg)))
		if ok || len(patches) != 0 {
			t.Errorf("%s: ok %v, %d patches; want false, none", name, ok, len(patches))
		}
	}
}

// What is not one of ours is not read into memory or walked sample by
// sample: a moof over maxMoofSize, a trun counting more than maxTrunSamples.
func TestUniquePTS_Bounds(t *testing.T) {
	big := box("moof", make([]byte, maxMoofSize))
	if _, _, ok := uniquePTSPatches(bytes.NewReader(big), int64(len(big))); ok {
		t.Error("a moof over maxMoofSize: read")
	}
	// No per-sample fields: the count costs no bytes (flags 0x001).
	many := box("moof", box("traf",
		box("tfhd", []byte{0, 0x02, 0, 0x08}, u32(1), u32(100)),
		box("tfdt", []byte{1, 0, 0, 0}, u64(0)),
		box("trun", []byte{0, 0, 0, 0x01}, u32(maxTrunSamples+1), u32(0))))
	if _, _, ok := uniquePTSPatches(bytes.NewReader(many), int64(len(many))); ok {
		t.Error("a trun over maxTrunSamples: walked")
	}
}

// The mdat's header is what is read of it: a head of a segment -- the
// fixtures below -- is read whole.
func TestUniquePTS_MdatPastTheEnd(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 0, 0, []ptsSample{{100, 1, 100}, {100, 1, 0}}))
	seg = seg[:len(seg)-3]
	if _, moved, ok := uniquePTSPatches(bytes.NewReader(seg), int64(len(seg))); !ok || moved != 1 {
		t.Errorf("ok %v, moved %d; want true, 1", ok, moved)
	}
}

// The production fragments of 2026-09-30, their boxes up to the mdat's
// header (testdata/passthrough/pts): dup-dropped is the one Chrome 154 buffers
// nothing of, dup-taken one with the same pattern it takes, *.fixed the same
// with the duplicate moved one tick -- the bytes the MSE bench played whole.
// one-tick is a CRA with its leading picture a tick off already.
func TestUniquePTS_ProductionFragments(t *testing.T) {
	dir := filepath.Join("testdata", "passthrough", "pts")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, name := range []string{"dup-dropped", "dup-taken"} {
		out, moved, ok := served(t, read(name+".head"))
		if !ok || moved != 1 {
			t.Errorf("%s: ok %v, moved %d; want true, 1", name, ok, moved)
		}
		if !bytes.Equal(out, read(name+".fixed.head")) {
			t.Errorf("%s: served bytes differ from the bench's fixed fragment", name)
		}
	}
	patches, moved, ok := uniquePTSPatches(bytes.NewReader(read("one-tick.head")), int64(len(read("one-tick.head"))))
	if !ok || moved != 0 || len(patches) != 0 {
		t.Errorf("one-tick: ok %v, moved %d; want true, 0", ok, moved)
	}
}

// Every read of the served bytes -- a Range, http.ServeContent's pieces --
// sees the patches, wherever it starts and ends.
func TestPatchedReaderAt_AnyWindow(t *testing.T) {
	seg := ptsSegment(ptsMoof(1, 0, 0, []ptsSample{{100, 1, 100}, {100, 1, 0}, {100, 1, 300}}))
	want, _, _ := served(t, seg)
	patches, _, _ := uniquePTSPatches(bytes.NewReader(seg), int64(len(seg)))
	p := patchedReaderAt{r: bytes.NewReader(seg), patches: patches}
	for off := 0; off < len(seg); off++ {
		for n := 1; off+n <= len(seg) && n <= 9; n++ {
			got := make([]byte, n)
			if _, err := p.ReadAt(got, int64(off)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want[off:off+n]) {
				t.Fatalf("ReadAt(%d, %d) = %x, want %x", off, n, got, want[off:off+n])
			}
		}
	}
}

// Through the server: a passthrough video segment goes out moved, under an
// ETag the unmoved copy a browser already holds does not match, and a Range
// of it is a range of the moved bytes; an audio segment goes out as written.
func TestSessionSegment_PassthroughVideoGoesOutWithUniqueTimes(t *testing.T) {
	web, sess, run := passthroughWebSession(t)
	gen := run.Generation()
	video := ptsSegment(ptsMoof(1, 0, 0, []ptsSample{{100, 1, 100}, {100, 1, 0}}))
	// The same duplicate in an audio segment: not ours to move.
	audio := ptsSegment(ptsMoof(2, 0, 0, []ptsSample{{100, 1, 100}, {100, 1, 0}}))
	writeRunFile(t, run, "v0-1080-0.m4s", video)
	writeRunFile(t, run, "a0-0.m4s", audio)
	want, _, _ := served(t, video)
	before := testutil.ToFloat64(metricPassthroughSegmentPTS.WithLabelValues(ptsResultFixed))

	req := func(name string, header ...string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/session/"+sess.id+"/"+name, nil)
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		web.handler.ServeHTTP(w, r)
		return w
	}
	w := req("v0-1080-0.m4s")
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("video: %d, moved %v; want 200 with the moved bytes", w.Code, bytes.Equal(w.Body.Bytes(), want))
	}
	if etag != segmentETagUniquePTS(gen, int64(len(video))) || etag == segmentETag(gen, int64(len(video))) {
		t.Errorf("video ETag %s: want the moved bytes' own", etag)
	}
	if w := req("v0-1080-0.m4s", "If-None-Match", segmentETag(gen, int64(len(video)))); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Errorf("a copy taken before the fix: %d; want 200 with the moved bytes", w.Code)
	}
	if w := req("v0-1080-0.m4s", "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Errorf("the moved copy revalidated: %d, want 304", w.Code)
	}
	if w := req("v0-1080-0.m4s", "Range", "bytes=100-199"); w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), want[100:200]) {
		t.Errorf("Range: %d, %d bytes, moved %v", w.Code, w.Body.Len(), bytes.Equal(w.Body.Bytes(), want[100:200]))
	}
	if d := testutil.ToFloat64(metricPassthroughSegmentPTS.WithLabelValues(ptsResultFixed)) - before; d != 1 {
		t.Errorf("fixed counted %v times over four responses, want once", d)
	}

	w = req("a0-0.m4s")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), audio) || w.Header().Get("ETag") != segmentETag(gen, int64(len(audio))) {
		t.Errorf("audio: %d, as written %v, ETag %s", w.Code, bytes.Equal(w.Body.Bytes(), audio), w.Header().Get("ETag"))
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
