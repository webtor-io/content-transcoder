package services

import (
	"strings"
)

// Video routes: what a session does with the source's video. copy, reencode
// and audio are the routes the transcoder had before passthrough (together,
// "the old route") and carry the names of the run modes; passthrough hands
// the source's HEVC to the player as it is (fMP4). refused and error only
// label the route metric: the session was not opened.
const (
	videoRoutePassthrough = "passthrough"
	videoRouteCopy        = runModeCopy
	videoRouteReencode    = runModeReencode
	videoRouteAudio       = runModeAudio
	videoRouteRefused     = "refused" // 415, or 503 when the source check failed
	videoRouteError       = "error"   // any other failure after the route was decided
)

// Route reasons: why the session got its route, a closed set. Every
// decision carries exactly one; the order of videoRouteFor is the order of
// the checks, the first that applies wins.
const (
	reasonNoDeclaration      = "no_declaration"      // the client said nothing we know about what it decodes
	reasonPassthroughOff     = "passthrough_off"     // this transcoder passes no HEVC through
	reasonNotHEVC            = "not_hevc"            // the source video is not HEVC (or there is none)
	reasonDeclarationPending = "declaration_pending" // the client's own check had not answered yet
	reasonTooLarge           = "too_large"           // over 3840x2160, or level above 5.1
	reasonNeeds2160          = "needs_2160"          // over 1080 (or its level) without a 2160 token
	reasonProbeFailed        = "probe_failed"        // the local look at the source did not answer
	reasonDV5                = "dv5"                 // Dolby Vision 5: no base layer a browser shows right
	reasonDV7                = "dv7"                 // Dolby Vision 7: enhancement layer
	reasonDVBase             = "dv_base"             // Dolby Vision whose base layer is not proven PQ/SDR/HLG
	reasonDVUnknown          = "dv_unknown"          // RPU in the stream, no configuration record
	reasonPixFmt             = "pix_fmt"             // not 4:2:0 8/10-bit
	reasonProfile            = "profile"             // not Main or Main10 (the only profiles tokens speak for)
	reasonInterlaced         = "interlaced"          // fields, not frames
	reasonNoHVCC             = "no_hvcc"             // no hvcC with parameter sets (Annex B source)
	reasonNeedsMain          = "needs_main"          // 8-bit, and the client declared no HEVC depth at all
	reasonNeedsMain10        = "needs_main10"        // 10-bit, the client declared 8-bit only
	reasonNeedsHighTier      = "needs_high_tier"     // tier High without hevc-high
	reasonNeedsPQ            = "needs_pq"            // PQ without hdr-pq
	reasonHLGLater           = "hlg_later"           // HLG: not passed through in v1
	reasonOK                 = "ok"                  // passthrough
)

// routeReasons lists every reason, for metric registration and tests.
var routeReasons = []string{
	reasonNoDeclaration, reasonPassthroughOff, reasonNotHEVC, reasonDeclarationPending,
	reasonTooLarge, reasonNeeds2160, reasonProbeFailed, reasonDV5, reasonDV7, reasonDVBase,
	reasonDVUnknown, reasonPixFmt, reasonProfile, reasonInterlaced, reasonNoHVCC, reasonNeedsMain,
	reasonNeedsMain10, reasonNeedsHighTier, reasonNeedsPQ, reasonHLGLater, reasonOK,
}

// Tokens of the client's "decode" declaration: what the browser says it
// decodes (MediaSource.isTypeSupported or canPlayType answering yes for the
// token's codec string -- any support counts, hardware or not). The
// transcoder only reads them; which route a session gets it decides itself.
const (
	tokenHEVC8      = "hevc8"       // Main, up to 1920x1080, level 4.1
	tokenHEVC10     = "hevc10"      // Main10 (and Main), up to 1920x1080, level 4.1
	tokenHEVC8UHD   = "hevc8-2160"  // Main, up to 3840x2160, level 5.1
	tokenHEVC10UHD  = "hevc10-2160" // Main10 (and Main), up to 3840x2160, level 5.1
	tokenHEVCHigh   = "hevc-high"   // tier High
	tokenHDRPQ      = "hdr-pq"      // PQ transfer
	tokenUnknownYet = "unknown"     // the check had not answered when the form was sent
)

var knownDecodeTokens = map[string]bool{
	tokenHEVC8: true, tokenHEVC10: true, tokenHEVC8UHD: true, tokenHEVC10UHD: true,
	tokenHEVCHigh: true, tokenHDRPQ: true,
}

// maxDecodeDeclaration bounds the value the parser looks at: a real one is
// under 80 bytes, anything much longer is not from our page.
const maxDecodeDeclaration = 512

// viewerDeclaration is the client's "decode" query parameter, parsed.
type viewerDeclaration struct {
	// tokens are the known capability tokens it carries.
	tokens map[string]bool
	// pending: it carries "unknown" and no capability token -- the check
	// had not answered. A declaration with both takes the tokens: whatever
	// did answer is an answer.
	pending bool
}

// parseDecodeDeclaration reads the values of the "decode" query parameter:
// comma-separated tokens, matched exactly against the allowlist. Unknown
// tokens are ignored; an empty, over-long or unparsable value is no
// declaration at all -- the old route, as for a client that never sends one.
func parseDecodeDeclaration(values []string) viewerDeclaration {
	d := viewerDeclaration{}
	raw := strings.Join(values, ",")
	if len(raw) > maxDecodeDeclaration {
		return d
	}
	unknown := false
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if knownDecodeTokens[t] {
			if d.tokens == nil {
				d.tokens = map[string]bool{}
			}
			d.tokens[t] = true
		} else if t == tokenUnknownYet {
			unknown = true
		}
	}
	d.pending = unknown && len(d.tokens) == 0
	return d
}

func (d viewerDeclaration) has(t string) bool { return d.tokens[t] }

// String is the declaration as the log shows it: the tokens in a fixed order.
func (d viewerDeclaration) String() string {
	if d.pending {
		return tokenUnknownYet
	}
	var out []string
	for _, t := range []string{tokenHEVC8, tokenHEVC10, tokenHEVC8UHD, tokenHEVC10UHD, tokenHEVCHigh, tokenHDRPQ} {
		if d.tokens[t] {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// covers reports whether the declaration speaks for a Main (or, with
// tenBit, Main10) stream of the given class. A token covers its own depth
// and size and everything under them: hevc10 covers Main, a 2160 token
// covers the 1080 class of its depth (levels are nested).
func (d viewerDeclaration) covers(tenBit, uhd bool) bool {
	switch {
	case tenBit && uhd:
		return d.has(tokenHEVC10UHD)
	case tenBit:
		return d.has(tokenHEVC10) || d.has(tokenHEVC10UHD)
	case uhd:
		return d.has(tokenHEVC8UHD) || d.has(tokenHEVC10UHD)
	default:
		return d.has(tokenHEVC8) || d.has(tokenHEVC10) || d.has(tokenHEVC8UHD) || d.has(tokenHEVC10UHD)
	}
}

// sourceVideo is what content-prober already told us about the primary
// video: enough for the checks that need no look of our own.
type sourceVideo struct {
	codec  string // "" when the source has no video
	width  int
	height int
	index  int // input stream index (the one the run maps)
}

// over1080 is the class over 1080p: taller than 1080 or wider than 1920
// (the old gate looks at the height only, and still does).
func (v sourceVideo) over1080() bool { return v.height > 1080 || v.width > 1920 }

// Level of HEVC 4.1 (level_idc = 30 * level) and 5.1: the ceilings of the
// 1080 and 2160 tokens.
const (
	hevcLevel41 = 123
	hevcLevel51 = 153
)

// routeDecision is the outcome of videoRouteFor.
type routeDecision struct {
	passthrough bool
	reason      string
}

func oldRoute(reason string) routeDecision { return routeDecision{reason: reason} }

// videoRouteFor decides whether a session passes the source's HEVC through
// or takes the old route, and why. The checks go in order and the first
// that applies wins; the ones up to the size check need nothing but the
// declaration, the capability and content-prober's answer, and probe (the
// transcoder's own look at the source, sourceHEVCFacts) runs only past them.
//
// A failed probe is not read as "this source cannot pass": it is its own
// reason (probe_failed), the old route plays what it can, and what it
// refuses is answered as retryable (see openSessionWith). The same goes for
// a client whose check had not answered (declaration_pending).
func videoRouteFor(src sourceVideo, decl viewerDeclaration, capability passthroughCapability, probe func() (sourceHEVCFacts, error)) routeDecision {
	if len(decl.tokens) == 0 && !decl.pending {
		return oldRoute(reasonNoDeclaration)
	}
	if !capability.has("hevc") {
		return oldRoute(reasonPassthroughOff)
	}
	if src.codec != "hevc" {
		return oldRoute(reasonNotHEVC)
	}
	if decl.pending {
		return oldRoute(reasonDeclarationPending)
	}
	if src.width > 3840 || src.height > 2160 {
		return oldRoute(reasonTooLarge)
	}
	if src.over1080() && !decl.has(tokenHEVC8UHD) && !decl.has(tokenHEVC10UHD) {
		return oldRoute(reasonNeeds2160)
	}
	if probe == nil {
		return oldRoute(reasonProbeFailed)
	}
	f, err := probe()
	if err != nil {
		return oldRoute(reasonProbeFailed)
	}
	return routeForFacts(src, decl, f)
}

// routeForFacts is the part of videoRouteFor that reads the probe.
func routeForFacts(src sourceVideo, decl viewerDeclaration, f sourceHEVCFacts) routeDecision {
	if f.DOVI != nil {
		switch f.DOVI.Profile {
		case 5:
			return oldRoute(reasonDV5)
		case 7:
			return oldRoute(reasonDV7)
		}
		if f.DOVI.Profile != 8 || !doviBaseMatchesTransfer(f.DOVI.Compatibility, f.ColorTransfer) {
			return oldRoute(reasonDVBase)
		}
	}
	if f.DOVI == nil && f.RPU {
		return oldRoute(reasonDVUnknown)
	}
	if !passthroughPixFmts[f.PixFmt] {
		return oldRoute(reasonPixFmt)
	}
	if interlacedFieldOrders[f.FieldOrder] {
		return oldRoute(reasonInterlaced)
	}
	h, ok := parseHVCC(f.HVCC)
	if !ok {
		return oldRoute(reasonNoHVCC)
	}
	if h.profileSpace != 0 || (h.profileIdc != 1 && h.profileIdc != 2) {
		return oldRoute(reasonProfile)
	}
	if h.levelIdc > hevcLevel51 {
		return oldRoute(reasonTooLarge)
	}
	// Main10 signalled for 8-bit content still says Main10 in CODECS.
	tenBit := f.PixFmt == "yuv420p10le" || h.profileIdc == 2
	uhd := src.over1080() || h.levelIdc > hevcLevel41
	if !decl.covers(tenBit, uhd) {
		switch {
		case tenBit && !decl.covers(true, false):
			return oldRoute(reasonNeedsMain10)
		case uhd:
			return oldRoute(reasonNeeds2160)
		default:
			return oldRoute(reasonNeedsMain)
		}
	}
	if h.tierHigh && !decl.has(tokenHEVCHigh) {
		return oldRoute(reasonNeedsHighTier)
	}
	if f.ColorTransfer == transferPQ && !decl.has(tokenHDRPQ) {
		return oldRoute(reasonNeedsPQ)
	}
	if f.ColorTransfer == transferHLG {
		return oldRoute(reasonHLGLater)
	}
	return routeDecision{passthrough: true, reason: reasonOK}
}

// FFmpeg's names of the transfer characteristics that matter here.
const (
	transferPQ  = "smpte2084"
	transferHLG = "arib-std-b67"
)

// passthroughPixFmts: 4:2:0 at 8 or 10 bits, what Main and Main10 carry.
var passthroughPixFmts = map[string]bool{"yuv420p": true, "yuvj420p": true, "yuv420p10le": true}

// interlacedFieldOrders are ffprobe's field orders for field-coded video;
// progressive and an absent value (ffprobe leaves "unknown" out) are not.
var interlacedFieldOrders = map[string]bool{"tt": true, "bb": true, "tb": true, "bt": true}

// doviBaseMatchesTransfer reports whether a Dolby Vision 8 base layer is
// what its compatibility id says and the stream's transfer agrees: 1 is
// HDR10 (PQ), 2 SDR, 4 HLG. Anything else, or a transfer that contradicts
// it, may show wrong colours in a browser that plays the base layer.
func doviBaseMatchesTransfer(compat int, transfer string) bool {
	switch compat {
	case 1:
		return transfer == transferPQ
	case 2:
		return transfer != transferPQ && transfer != transferHLG
	case 4:
		return transfer == transferHLG
	}
	return false
}
