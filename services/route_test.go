package services

import (
	"errors"
	"strings"
	"testing"
)

// testHVCC is a minimal hvcC record (ISO/IEC 14496-15): the fixed 23-byte
// head plus one empty parameter-set array, profile_space 0, compatibility
// flag of the profile set, 4-byte NAL lengths.
func testHVCC(profileIdc int, tierHigh bool, level int) []byte {
	b := make([]byte, 23, 28)
	b[0] = 1
	b[1] = byte(profileIdc & 0x1f)
	if tierHigh {
		b[1] |= 0x20
	}
	b[2] = 0x80 >> uint(profileIdc) // general_profile_compatibility_flag[profileIdc]
	b[6] = 0x90                     // progressive_source, frame_only
	b[12] = byte(level)
	b[13], b[14] = 0xf0, 0x00
	b[15] = 0xfc
	b[16] = 0xfd // chroma 4:2:0
	b[17] = 0xf8
	b[18] = 0xf8
	b[21] = 0x0f                       // lengthSizeMinusOne 3
	b[22] = 1                          // numOfArrays
	return append(b, 0xa0, 0x00, 0x00) // VPS array, no NAL units
}

// sdrMain10 is the facts of an ordinary 1080p Main10 SDR MKV.
func sdrMain10() sourceHEVCFacts {
	return sourceHEVCFacts{Version: sourceFactsVersion, PixFmt: "yuv420p10le", FieldOrder: "progressive", HVCC: testHVCC(2, false, 120), Packets: 2}
}

func decl(tokens string) viewerDeclaration {
	return parseDecodeDeclaration([]string{tokens})
}

var (
	hd  = sourceVideo{codec: "hevc", width: 1920, height: 1080}
	uhd = sourceVideo{codec: "hevc", width: 3840, height: 2160}
	// everything a capable browser declares
	allTokens = "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"
	on        = passthroughCapability{codecs: map[string]bool{"hevc": true}}
)

func facts(mut func(*sourceHEVCFacts)) func() (sourceHEVCFacts, error) {
	return func() (sourceHEVCFacts, error) {
		f := sdrMain10()
		if mut != nil {
			mut(&f)
		}
		return f, nil
	}
}

// Every row of the decision table, in order, and the reason it gives.
func TestVideoRouteFor_Table(t *testing.T) {
	failing := func() (sourceHEVCFacts, error) { return sourceHEVCFacts{}, errors.New("timeout") }
	cases := []struct {
		name  string
		src   sourceVideo
		decl  string
		cap   passthroughCapability
		probe func() (sourceHEVCFacts, error)
		want  string
	}{
		{"no declaration", hd, "", on, facts(nil), reasonNoDeclaration},
		{"only unknown tokens", hd, "h265,avc,HEVC10", on, facts(nil), reasonNoDeclaration},
		{"capability off", hd, allTokens, passthroughCapability{}, facts(nil), reasonPassthroughOff},
		{"h264", sourceVideo{codec: "h264", width: 1920, height: 1080}, allTokens, on, facts(nil), reasonNotHEVC},
		{"av1 1080", sourceVideo{codec: "av1", width: 1920, height: 1080}, allTokens, on, facts(nil), reasonNotHEVC},
		{"av1 2160", sourceVideo{codec: "av1", width: 3840, height: 2160}, allTokens, on, facts(nil), reasonNotHEVC},
		{"no video", sourceVideo{}, allTokens, on, facts(nil), reasonNotHEVC},
		{"declaration pending 1080", hd, "unknown", on, facts(nil), reasonDeclarationPending},
		{"declaration pending 2160", uhd, "unknown", on, facts(nil), reasonDeclarationPending},
		{"wider than 3840", sourceVideo{codec: "hevc", width: 4096, height: 2160}, allTokens, on, facts(nil), reasonTooLarge},
		{"taller than 2160", sourceVideo{codec: "hevc", width: 3840, height: 2400}, allTokens, on, facts(nil), reasonTooLarge},
		{"2160 without a 2160 token", uhd, "hevc8,hevc10,hdr-pq", on, facts(nil), reasonNeeds2160},
		{"2560x1080 is over 1080 too", sourceVideo{codec: "hevc", width: 2560, height: 1080}, "hevc10", on, facts(nil), reasonNeeds2160},
		{"probe failed 1080", hd, allTokens, on, failing, reasonProbeFailed},
		{"probe failed 2160", uhd, allTokens, on, failing, reasonProbeFailed},
		{"dolby vision 5", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.DOVI = &doviRecord{Profile: 5}; f.RPU = true }), reasonDV5},
		{"dolby vision 7", uhd, allTokens, on, facts(func(f *sourceHEVCFacts) {
			f.DOVI = &doviRecord{Profile: 7, Compatibility: 6}
			f.ColorTransfer = transferPQ
		}), reasonDV7},
		{"dolby vision 8 with compatibility 0", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.DOVI = &doviRecord{Profile: 8} }), reasonDVBase},
		{"dolby vision 8.1 on an SDR transfer", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.DOVI = &doviRecord{Profile: 8, Compatibility: 1} }), reasonDVBase},
		{"dolby vision 8.2 on PQ", hd, allTokens, on, facts(func(f *sourceHEVCFacts) {
			f.DOVI = &doviRecord{Profile: 8, Compatibility: 2}
			f.ColorTransfer = transferPQ
		}), reasonDVBase},
		{"dolby vision 4", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.DOVI = &doviRecord{Profile: 4, Compatibility: 2} }), reasonDVBase},
		{"RPU without a record", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.RPU = true }), reasonDVUnknown},
		{"4:2:2 10-bit", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv422p10le"; f.HVCC = testHVCC(4, false, 120) }), reasonPixFmt},
		{"12-bit", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p12le" }), reasonPixFmt},
		{"interlaced", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.FieldOrder = "tt" }), reasonInterlaced},
		{"field order not reported", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.FieldOrder = "" }), reasonOK},
		{"annex B extradata", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = []byte{0, 0, 0, 1, 0x40, 1, 0x0c} }), reasonNoHVCC},
		{"hvcC without arrays", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = f.HVCC[:23] }), reasonNoHVCC},
		{"no extradata", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = nil }), reasonNoHVCC},
		{"Rext profile on 4:2:0", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(4, false, 120) }), reasonProfile},
		{"level 6.1", uhd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 183) }), reasonTooLarge},
		{"level 5.2", uhd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 156) }), reasonTooLarge},
		{"10-bit 1080 with 8-bit tokens only", hd, "hevc8,hevc8-2160", on, facts(nil), reasonNeedsMain10},
		{"10-bit 2160 with hevc10 only (1080)", uhd, "hevc10,hevc8-2160", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 150) }), reasonNeeds2160},
		{"10-bit 2160 with 8-bit 2160 only", uhd, "hevc8-2160,hdr-pq", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 150) }), reasonNeedsMain10},
		{"1080 at level 5.0 needs a 2160 token", hd, "hevc10", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 150) }), reasonNeeds2160},
		{"1080 at level 4.1 does not", hd, "hevc10", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, false, 123) }), reasonOK},
		{"8-bit Main with hevc8", hd, "hevc8", on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p"; f.HVCC = testHVCC(1, false, 120) }), reasonOK},
		{"8-bit Main with hevc10 (covers Main)", hd, "hevc10", on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p"; f.HVCC = testHVCC(1, false, 120) }), reasonOK},
		{"8-bit Main10 profile needs Main10", hd, "hevc8", on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p" }), reasonNeedsMain10},
		{"8-bit with no depth token", hd, "hdr-pq,hevc-high", on, facts(func(f *sourceHEVCFacts) { f.PixFmt = "yuv420p"; f.HVCC = testHVCC(1, false, 120) }), reasonNeedsMain},
		{"tier High without hevc-high", uhd, "hevc10-2160,hdr-pq", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, true, 153) }), reasonNeedsHighTier},
		{"tier High with hevc-high", uhd, "hevc10-2160,hevc-high", on, facts(func(f *sourceHEVCFacts) { f.HVCC = testHVCC(2, true, 153) }), reasonOK},
		{"PQ without hdr-pq", uhd, "hevc10-2160", on, facts(func(f *sourceHEVCFacts) { f.ColorTransfer = transferPQ; f.HVCC = testHVCC(2, false, 150) }), reasonNeedsPQ},
		{"dolby vision 8.1 without hdr-pq", uhd, "hevc10-2160", on, facts(func(f *sourceHEVCFacts) {
			f.ColorTransfer = transferPQ
			f.DOVI = &doviRecord{Profile: 8, Compatibility: 1}
			f.RPU = true
			f.HVCC = testHVCC(2, false, 150)
		}), reasonNeedsPQ},
		{"HLG", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.ColorTransfer = transferHLG }), reasonHLGLater},
		{"dolby vision 8.4 (HLG)", hd, allTokens, on, facts(func(f *sourceHEVCFacts) {
			f.ColorTransfer = transferHLG
			f.DOVI = &doviRecord{Profile: 8, Compatibility: 4}
		}), reasonHLGLater},
		{"SDR Main10 1080", hd, allTokens, on, facts(nil), reasonOK},
		{"HDR10 2160", uhd, "hevc10-2160,hdr-pq", on, facts(func(f *sourceHEVCFacts) { f.ColorTransfer = transferPQ; f.HVCC = testHVCC(2, false, 153) }), reasonOK},
		{"dolby vision 8.1 with hdr-pq", uhd, allTokens, on, facts(func(f *sourceHEVCFacts) {
			f.ColorTransfer = transferPQ
			f.DOVI = &doviRecord{Profile: 8, Compatibility: 1}
			f.RPU = true
			f.HVCC = testHVCC(2, false, 153)
		}), reasonOK},
		{"dolby vision 8.2 SDR", hd, allTokens, on, facts(func(f *sourceHEVCFacts) { f.DOVI = &doviRecord{Profile: 8, Compatibility: 2}; f.RPU = true }), reasonOK},
		{"hevc10-2160 alone covers 1080 Main10", hd, "hevc10-2160", on, facts(nil), reasonOK},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := videoRouteFor(c.src, decl(c.decl), c.cap, c.probe)
			if got.reason != c.want {
				t.Errorf("reason = %s, want %s", got.reason, c.want)
			}
			if got.passthrough != (c.want == reasonOK) {
				t.Errorf("passthrough = %v with reason %s", got.passthrough, got.reason)
			}
		})
		seen[c.want] = true
	}
	for _, r := range routeReasons {
		if !seen[r] {
			t.Errorf("no case for reason %s", r)
		}
	}
}

// The checks up to the size one need no probe; the probe runs only past
// them. A probe is a subprocess reading the source over the network, and a
// session that cannot pass through anyway must not pay for it.
func TestVideoRouteFor_CheapChecksDoNotProbe(t *testing.T) {
	probed := false
	probe := func() (sourceHEVCFacts, error) { probed = true; return sdrMain10(), nil }
	for _, c := range []struct {
		src  sourceVideo
		decl string
		cap  passthroughCapability
	}{
		{hd, "", on},
		{hd, allTokens, passthroughCapability{}},
		{sourceVideo{codec: "h264", width: 1920, height: 1080}, allTokens, on},
		{uhd, "unknown", on},
		{sourceVideo{codec: "hevc", width: 7680, height: 4320}, allTokens, on},
		{uhd, "hevc10", on},
	} {
		probed = false
		d := videoRouteFor(c.src, decl(c.decl), c.cap, probe)
		if probed {
			t.Errorf("%+v %q: probed for a %s decision", c.src, c.decl, d.reason)
		}
	}
	probed = false
	if d := videoRouteFor(uhd, decl(allTokens), on, probe); !probed || !d.passthrough {
		t.Errorf("past the cheap checks the probe must run: probed=%v %+v", probed, d)
	}
	if d := videoRouteFor(hd, decl(allTokens), on, nil); d.reason != reasonProbeFailed {
		t.Errorf("no prober reads as a failed probe, got %s", d.reason)
	}
}

func TestParseDecodeDeclaration(t *testing.T) {
	cases := []struct {
		in      []string
		want    string // String()
		pending bool
		none    bool
	}{
		{nil, "", false, true},
		{[]string{""}, "", false, true},
		{[]string{"garbage"}, "", false, true},
		{[]string{",,,"}, "", false, true},
		{[]string{"HEVC10"}, "", false, true}, // exact match: case is not folded
		{[]string{"hevc10;hevc8"}, "", false, true},
		{[]string{"hevc10,hevc10,hevc10"}, "hevc10", false, false},
		{[]string{" hevc8 , hdr-pq "}, "hevc8,hdr-pq", false, false},
		{[]string{"hdr-pq,hevc10-2160,x-new-token"}, "hevc10-2160,hdr-pq", false, false},
		{[]string{"hevc8", "hevc10-2160"}, "hevc8,hevc10-2160", false, false}, // repeated parameter
		{[]string{"unknown"}, "unknown", true, false},
		{[]string{"unknown,unknown"}, "unknown", true, false},
		{[]string{"unknown,hevc8"}, "hevc8", false, false}, // what did answer is an answer
		{[]string{"unknown,garbage"}, "unknown", true, false},
		{[]string{strings.Repeat("hevc8,", 100)}, "", false, true}, // over the size limit
		{[]string{"hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"}, "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq", false, false},
	}
	for _, c := range cases {
		d := parseDecodeDeclaration(c.in)
		if d.String() != c.want || d.pending != c.pending || (len(d.tokens) == 0 && !d.pending) != c.none {
			t.Errorf("%q: got %q pending=%v tokens=%v, want %q pending=%v none=%v", c.in, d.String(), d.pending, d.tokens, c.want, c.pending, c.none)
		}
	}
}

func TestParseHVCC(t *testing.T) {
	h, ok := parseHVCC(testHVCC(2, true, 153))
	if !ok || h.profileIdc != 2 || !h.tierHigh || h.levelIdc != 153 || h.profileSpace != 0 || h.lengthSize != 4 || h.compat != 0x20000000 {
		t.Errorf("Main10 High 5.1: %+v ok=%v", h, ok)
	}
	h, ok = parseHVCC(testHVCC(1, false, 93))
	if !ok || h.profileIdc != 1 || h.tierHigh || h.levelIdc != 93 || h.compat != 0x40000000 {
		t.Errorf("Main Main 3.1: %+v ok=%v", h, ok)
	}
	for name, b := range map[string][]byte{
		"nil":            nil,
		"23 bytes":       testHVCC(2, false, 120)[:23],
		"version 0":      append([]byte{0}, testHVCC(2, false, 120)[1:]...),
		"annex B":        {0, 0, 0, 1, 0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x01, 0x60, 0, 0, 3, 0, 0x90, 0, 0, 3, 0, 0, 3, 0, 0x5d},
		"start code (3)": {0, 0, 1, 0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x01, 0x60, 0, 0, 3, 0, 0x90, 0, 0, 3, 0, 0, 3, 0, 0x5d},
	} {
		if _, ok := parseHVCC(b); ok {
			t.Errorf("%s read as an hvcC", name)
		}
	}
}
