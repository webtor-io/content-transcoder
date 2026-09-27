package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The transfer the route reads: whichever of the container's and the
// bitstream's would be shown wrong if the other were believed.
func TestSourceTransfer(t *testing.T) {
	for _, c := range []struct{ stream, frame, want string }{
		{"", transferPQ, transferPQ},           // Colour element without a transfer (pqvui_colourunspec)
		{"unknown", transferPQ, transferPQ},    // the same, printed
		{"bt709", transferPQ, transferPQ},      // the container says SDR, the VUI PQ
		{transferPQ, "bt709", transferPQ},      // and the other way round
		{transferPQ, "", transferPQ},           // no frame transfer
		{transferPQ, transferHLG, transferHLG}, // HLG never passes through
		{transferHLG, transferPQ, transferHLG},
		{"bt2020-10", transferHLG, transferHLG}, // alternative transfer SEI
		{"bt709", "", "bt709"},
		{"", "bt709", "bt709"},
		{"", "", ""},
	} {
		f := sourceHEVCFacts{ColorTransfer: c.stream, FrameColorTransfer: c.frame}
		if got := f.transfer(); got != c.want {
			t.Errorf("stream %q, frames %q: %q, want %q", c.stream, c.frame, got, c.want)
		}
	}
	// And what the route makes of it: PQ in the bitstream alone needs
	// hdr-pq and is labelled PQ.
	f := sdrMain10()
	f.ColorTransfer, f.FrameColorTransfer = "", transferPQ
	probe := func() (sourceHEVCFacts, error) { return f, nil }
	if got := videoRouteFor(hd, decl("hevc10"), on, probe).reason; got != reasonNeedsPQ {
		t.Errorf("PQ in the VUI only, no hdr-pq: %s", got)
	}
	h := hevcHLS(t, 1920, 1080, true, nil)
	h.passFacts = &f
	if m := h.passthroughMasterPlaylist("hvc1.2.4.L120.90", 1); !strings.Contains(m, "VIDEO-RANGE=PQ") {
		t.Errorf("PQ in the VUI only labelled %q", m)
	}
}

// On every real record at hand -- x265's in the probe fixtures and FFmpeg's
// own in the passthrough inits -- the parameter sets say what the head does,
// except in the two whose head was patched.
func TestOutputHVCC_RealRecords(t *testing.T) {
	for _, fx := range sourceProbeFixtures {
		out, err := os.ReadFile(fixturePath(fx.file))
		if err != nil {
			t.Fatal(err)
		}
		f, err := parseSourceProbe(out, fx.stream)
		if err != nil {
			t.Fatalf("%s: %v", fx.file, err)
		}
		head, isHVCC := parseHVCC(f.HVCC)
		got, ok := outputHVCC(f.HVCC)
		switch {
		case !isHVCC:
			if ok {
				t.Errorf("%s: no hvcC, yet an output one", fx.file)
			}
		case !ok:
			t.Errorf("%s: a real hvcC refused", fx.file)
		case fx.file == "lvl_head_high.mp4" || fx.file == "lvl_head_low.mp4":
			if got.levelIdc == head.levelIdc {
				t.Errorf("%s: the patched head's level read as the output's", fx.file)
			}
		case got != head:
			t.Errorf("%s: output %+v, head %+v", fx.file, got, head)
		}
	}
	for _, file := range []string{"main10-init.mp4", "main8-init.mp4", "lvl-head-high-init.mp4", "lvl-head-low-init.mp4"} {
		_, raw, err := initHEVCConfig(fixtureInit(t, file))
		if err != nil {
			t.Fatal(err)
		}
		head, _ := parseHVCC(raw)
		if got, ok := outputHVCC(raw); !ok || got != head {
			t.Errorf("%s: FFmpeg's own record: output %+v (%v), head %+v", file, got, ok, head)
		}
	}
}

// The prediction against FFmpeg 8.1.2: the CODECS of the init it wrote with
// the passthrough arguments for a source whose hvcC head disagrees with its
// SPS is the one outputHVCC gives for that source -- not the head's.
func TestOutputHVCC_PredictsFFmpeg(t *testing.T) {
	for src, init := range map[string]string{
		"lvl_head_high.mp4": "lvl-head-high-init.mp4", // head L153 over an SPS at L30
		"lvl_head_low.mp4":  "lvl-head-low-init.mp4",  // head L30 over an SPS at H153
	} {
		out, err := os.ReadFile(fixturePath(src))
		if err != nil {
			t.Fatal(err)
		}
		f, err := parseSourceProbe(out, 0)
		if err != nil {
			t.Fatal(err)
		}
		fourcc, raw, err := initHEVCConfig(fixtureInit(t, init))
		if err != nil {
			t.Fatal(err)
		}
		ffmpeg, _ := parseHVCC(raw)
		predicted, ok := outputHVCC(f.HVCC)
		head, _ := parseHVCC(f.HVCC)
		if !ok || hevcCodecString(fourcc, predicted) != hevcCodecString(fourcc, ffmpeg) {
			t.Errorf("%s: predicted %s, FFmpeg wrote %s", src, hevcCodecString(fourcc, predicted), hevcCodecString(fourcc, ffmpeg))
		}
		if hevcCodecString(fourcc, head) == hevcCodecString(fourcc, ffmpeg) {
			t.Errorf("%s: the head alone gives FFmpeg's CODECS too: the case proves nothing", src)
		}
		if d := codecsMismatches(predicted, ffmpeg); len(d) != 0 {
			t.Errorf("%s: mismatch %v counted against the prediction", src, d)
		}
	}
}

// FFmpeg's merge (hvcc_update_ptl), set by set in the record's order.
func TestOutputHVCC_Merge(t *testing.T) {
	main10 := func(level int) hevcPTL { return testPTL(2, false, level) }
	for _, c := range []struct {
		name     string
		vps, sps hevcPTL
		want     hevcPTL
	}{
		{"agree", main10(120), main10(120), main10(120)},
		{"the higher level", main10(150), main10(120), main10(150)},
		{"a higher tier takes its own level", main10(150), testPTL(2, true, 90), testPTL(2, true, 90)},
		{"the higher profile, the flags both have", testPTL(1, false, 93), hevcPTL{profile: 2, compat: 0x60000000, constraint: 0x80 << 40, level: 93},
			hevcPTL{profile: 2, compat: 0x40000000, constraint: 0x80 << 40, level: 93}},
		{"the last profile space", hevcPTL{space: 1, profile: 2, compat: 0x20000000, level: 120}, main10(120),
			hevcPTL{space: 0, profile: 2, compat: 0x20000000, level: 120}},
	} {
		head := testPTL(1, false, 30) // says something else entirely
		got, ok := outputHVCC(testHVCCWith(head, testVPS(c.vps), testSPS(c.sps), testPPS()))
		want := hvccHeader{profileSpace: c.want.space, tierHigh: c.want.tier, profileIdc: c.want.profile, compat: c.want.compat, levelIdc: c.want.level, lengthSize: 4}
		for i := range want.constraint {
			want.constraint[i] = byte(c.want.constraint >> (40 - 8*i))
		}
		if !ok || got != want {
			t.Errorf("%s: %+v (%v), want %+v", c.name, got, ok, want)
		}
	}
}

// Records a passthrough output cannot be made right from.
func TestOutputHVCC_Refusals(t *testing.T) {
	ptl := testPTL(2, false, 120)
	vps, sps, pps := testVPS(ptl), testSPS(ptl), testPPS()
	layer1 := func(nal []byte) []byte { // the same set, nuh_layer_id 1
		n := append([]byte{}, nal...)
		n[1] |= 1 << 3
		return n
	}
	idr := []byte{19 << 1, 1, 0xaf, 0x10}
	if _, ok := outputHVCC(testHVCCWith(ptl, vps, sps, pps)); !ok {
		t.Fatal("the complete record is refused")
	}
	for name, b := range map[string][]byte{
		"no PPS":                  testHVCCWith(ptl, vps, sps),
		"no SPS":                  testHVCCWith(ptl, vps, pps),
		"no VPS":                  testHVCCWith(ptl, sps, pps),
		"only an enhancement SPS": testHVCCWith(ptl, vps, layer1(sps), pps),
		"a slice in the arrays":   testHVCCWith(ptl, vps, sps, pps, idr),
		"a NAL cut short":         testHVCCWith(ptl, vps, testSPS(ptl)[:6], pps),
		"arrays cut short":        testHVCCWith(ptl, vps, sps, pps)[:40],
		"an empty NAL":            append(testHVCCWith(ptl, vps, sps, pps)[:22], 1, 0xa0, 0, 1, 0, 0),
		"annex B":                 {0, 0, 0, 1, nalVPS << 1, 1},
	} {
		if got, ok := outputHVCC(b); ok {
			t.Errorf("%s: read as %+v", name, got)
		}
	}
	// SEI arrays are fine (x265 writes one), and an enhancement-layer set
	// next to the base one is skipped, as FFmpeg's writer skips it.
	sei := []byte{nalSEIPrefix << 1, 1, 5, 2, 0xaa, 0xbb, 0x80}
	high := layer1(testSPS(testPTL(2, true, 183)))
	if got, ok := outputHVCC(testHVCCWith(ptl, vps, sps, pps, sei, high)); !ok || got.levelIdc != 120 || got.tierHigh {
		t.Errorf("SEI and a layer-1 SPS: %+v %v", got, ok)
	}
}

// Parameter sets whose profile_tier_level runs into 00 00 carry emulation
// prevention bytes; the fields are read with them taken out.
func TestOutputHVCC_EmulationPrevention(t *testing.T) {
	// No compatibility flags, no constraint flags, level 0x03: 00 00 00 00
	// 00 ... 03 in the RBSP.
	ptl := hevcPTL{profile: 2, level: 3}
	sps := testSPS(ptl)
	if n := len(sps) - len(append(append([]byte{0x01}, ptlBits(ptl)...), 0xa0, 0x80)) - 2; n < 2 {
		t.Fatalf("the test SPS has %d emulation prevention bytes, want several", n)
	}
	got, ok := outputHVCC(testHVCCWith(ptl, testVPS(ptl), sps, testPPS()))
	if !ok || got.levelIdc != 3 || got.compat != 0 || got.profileIdc != 2 {
		t.Errorf("%+v %v", got, ok)
	}
}

// A real source whose SPS is at level 5.1 under a head saying 1: a client
// declaring 1080 only is not sent a 5.1 stream.
func TestOutputHVCC_RouteGoesByTheOutput(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "source_probe", "lvl_head_low.mp4.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseSourceProbe(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	probe := func() (sourceHEVCFacts, error) { return f, nil }
	if got := videoRouteFor(hd, decl("hevc8,hevc10,hevc-high"), on, probe).reason; got != reasonNeeds2160 {
		t.Errorf("reason %s, want %s", got, reasonNeeds2160)
	}
	if got := videoRouteFor(hd, decl("hevc8-2160,hevc-high"), on, probe); !got.passthrough {
		t.Errorf("with a 2160 token: %s", got.reason)
	}
}
