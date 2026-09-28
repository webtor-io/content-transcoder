package services

import (
	"strconv"
	"strings"

	cp "github.com/webtor-io/content-prober/content-prober"
)

// Audio outputs: what a session does with each audio track of the source.
//
// Without a declaration, and for every track the declaration does not
// change, it is what the transcoder always did: AAC with up to 2 channels
// is copied, every other track is encoded to AAC stereo (-ac 2). The audio
// tokens of the client's declaration (route.go) change it:
//
//   - aac51: AAC with 3 to 6 channels is copied; every other track with
//     more than 2 channels (E-AC-3, AC-3, DTS, TrueHD, AAC 7.1, FLAC ...)
//     is encoded to AAC 5.1 at aac51BitRate. AAC 5.1 goes into MPEG-TS
//     (ADTS channel configuration 6) as well as into fMP4.
//   - ec3, ac3: an E-AC-3 or AC-3 track with more than 2 channels is
//     copied, as it is (Atmos, E-AC-3 JOC, included), on fMP4 audio only:
//     hls.js 1.6.14 refuses E-AC-3 in MPEG-TS (tsdemuxer.ts, "Unsupported
//     EC-3 in M2TS"), and its AC-3 in TS depends on the build. The audio of
//     a passthrough session is fMP4; the old route's never is. AC-3 needs
//     ac3: an E-AC-3 decoder decodes AC-3, but the declaration says what
//     the browser's MediaSource takes, and ac3 is its answer for "ac-3",
//     the sample entry the copy carries.
//
// A stereo (or mono) track of any codec is left as it was: nothing is
// gained by copying it, and every copy leans on the browser's answer.
//
// audioOutputFor is the one decision. The run's arguments (codecParams),
// the seek cuts (reencodeSeekCuts, through codecParams), the master
// playlists (CODECS, CHANNELS, BANDWIDTH) and the run variant
// (audioVariant) all read it, so they cannot disagree.

// audioDecoders is what the client declared it decodes of the audio.
type audioDecoders struct {
	aac51, ac3, ec3 bool
}

// audioOutput is the decision for one audio track.
type audioOutput struct {
	// copy: the track goes out as it is (-c:a copy); otherwise it is
	// encoded to AAC with channels channels.
	copy bool
	// channels is the output's channel count: 2 or 6 for an encode, the
	// source's for a copy (0 when content-prober did not say).
	channels int
	// codecs is the output's RFC 6381 codec, as CODECS names it.
	codecs string
}

// CODECS values of the audio outputs. A copied AAC is mp4a.40.2 whatever
// its object type, as the old master has always said (hls.js takes the
// codec of fMP4 audio from its init and of TS audio from its ADTS headers).
const (
	codecsAAC = "mp4a.40.2"
	codecsEC3 = "ec-3"
	codecsAC3 = "ac-3"
)

// aac51BitRate is the rate AAC 5.1 is encoded at. Without -b:a libfdk_aac
// takes (96*sce + 128*cpe) * rate / 44, 2 SCE and 2 CPE for 5.1: 489 kb/s
// at 48 kHz (libavcodec/libfdk-aacenc.c). 384 kb/s is 64 kb/s per full
// channel, what the stereo default gives each channel, and the rate web-ui
// asks mediaCapabilities about when it declares aac51.
const aac51BitRate = 384_000

// Rates of the outputs whose rate is not set by us, for BANDWIDTH.
const (
	// encodedStereoBitRate: libfdk_aac's default for stereo is 140 kb/s at
	// 48 kHz; the allowance the passthrough master always counted.
	encodedStereoBitRate = passthroughAudioBandwidth
	// copiedStereoBitRate: a copied track of up to 2 channels whose rate
	// the probe does not know (p90 of copied stereo AAC in production:
	// 192 kb/s, 24 h of probes on 2026-09-28).
	copiedStereoBitRate = 192_000
	// copiedMultichannelBitRate: a copied track of more channels whose
	// rate the probe does not know -- in production that is AAC 5.1 in
	// MKV (53 of 107; E-AC-3 and AC-3 always have one): its p90 was
	// 449 kb/s, the largest 645 kb/s; 640 kb/s is AC-3's ceiling.
	copiedMultichannelBitRate = 640_000
)

// audioOutputFor decides the output of audio stream s for a client that
// decodes d, on fMP4 (a passthrough session) or MPEG-TS; opts.EncodeAudio
// (the fallback after an ADTS failure) forces an encode: 5.1 with aac51,
// stereo without.
func audioOutputFor(s *cp.Stream, d audioDecoders, fmp4 bool, opts ParamOptions) audioOutput {
	codec, ch := s.GetCodecName(), int(s.GetChannels())
	if !opts.EncodeAudio {
		switch {
		case codec == "aac" && ch <= 2:
			return audioOutput{copy: true, channels: ch, codecs: codecsAAC}
		case codec == "aac" && ch <= 6 && d.aac51:
			return audioOutput{copy: true, channels: ch, codecs: codecsAAC}
		case codec == "eac3" && ch > 2 && d.ec3 && fmp4:
			return audioOutput{copy: true, channels: ch, codecs: codecsEC3}
		case codec == "ac3" && ch > 2 && d.ac3 && fmp4:
			return audioOutput{copy: true, channels: ch, codecs: codecsAC3}
		}
	}
	if ch > 2 && d.aac51 {
		return audioOutput{channels: 6, codecs: codecsAAC}
	}
	return audioOutput{channels: 2, codecs: codecsAAC}
}

// audioOutput is the decision for this stream (an audio stream: a
// rendition, or the primary of an audio-only source).
func (h *HLSStream) audioOutput(opts ParamOptions) audioOutput {
	return audioOutputFor(h.s, h.decoders, h.fmp4, opts)
}

// audioCodecParams are the codec options of the stream's output.
func (h *HLSStream) audioCodecParams(opts ParamOptions) []string {
	o := h.audioOutput(opts)
	if o.copy {
		return []string{"copy"}
	}
	params := []string{h.cfg.aacCodec, "-ac", strconv.Itoa(o.channels)}
	if o.channels == 6 {
		params = append(params, "-b:a", strconv.Itoa(aac51BitRate/1000)+"k")
	}
	return params
}

// bitRate is what the output of s is counted at in BANDWIDTH: the rate an
// encode is made at, a copy's own rate from the probe (the stream's
// bit_rate, else the BPS statistics tag mkvmerge writes), or an allowance.
func (o audioOutput) bitRate(s *cp.Stream) int64 {
	if !o.copy {
		if o.channels == 6 {
			return aac51BitRate
		}
		return encodedStereoBitRate
	}
	if br := streamBitRate(s); br > 0 {
		return br
	}
	if o.channels > 2 {
		return copiedMultichannelBitRate
	}
	return copiedStereoBitRate
}

// streamBitRate is the stream's average bit rate as the probe has it, 0
// when it has none.
func streamBitRate(s *cp.Stream) int64 {
	for _, v := range []string{s.GetBitRate(), s.GetTags()["BPS"], s.GetTags()["BPS-eng"]} {
		if br, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && br > 0 {
			return br
		}
	}
	return 0
}

// variantCode names the output in the run variant: c for a copy, the
// channel count for an encode.
func (o audioOutput) variantCode() string {
	if o.copy {
		return "c"
	}
	return strconv.Itoa(o.channels)
}

// audioStreams are the session's audio outputs: the primary of an
// audio-only source, then the renditions.
func (h *HLS) audioStreams() []*HLSStream {
	var out []*HLSStream
	for _, p := range h.primary {
		if p.st == Audio {
			out = append(out, p)
		}
	}
	return append(out, h.audio...)
}

// useAudioDecoders gives every audio output of the session what the client
// declared it decodes. Set once, before the session exists: the run key,
// the master and every run's arguments read it.
func (h *HLS) useAudioDecoders(d audioDecoders) {
	for _, s := range h.audioStreams() {
		s.decoders = d
	}
}

// audioVariant names the session's audio outputs when the declaration
// changes any of them, "" when every one is what it is without a
// declaration: "a" and one code per output, in order (variantCode), so two
// sessions of the same source share runs exactly when their audio
// arguments are the same. Decided without fallbacks: they are the run's,
// learned under the key this names.
func (h *HLS) audioVariant() string {
	if h == nil {
		return ""
	}
	changed := false
	var b strings.Builder
	for _, s := range h.audioStreams() {
		o := s.audioOutput(ParamOptions{})
		if o != audioOutputFor(s.s, audioDecoders{}, s.fmp4, ParamOptions{}) {
			changed = true
		}
		b.WriteString(o.variantCode())
	}
	if !changed {
		return ""
	}
	return "a" + b.String()
}

// audioCodecs is the CODECS part of the session's audio: every codec its
// audio outputs have, once, in their order; mp4a.40.2 when it has none
// (what the old master has always said).
func (h *HLS) audioCodecs() string {
	var codecs []string
	seen := map[string]bool{}
	for _, s := range h.audioStreams() {
		c := s.audioOutput(ParamOptions{}).codecs
		if !seen[c] {
			seen[c] = true
			codecs = append(codecs, c)
		}
	}
	if len(codecs) == 0 {
		return codecsAAC
	}
	return strings.Join(codecs, ",")
}

// audioBandwidth is the largest rate of the session's audio outputs: a
// player plays one rendition at a time.
func (h *HLS) audioBandwidth() int64 {
	var bw int64
	for _, s := range h.audioStreams() {
		if r := s.audioOutput(ParamOptions{}).bitRate(s.s); r > bw {
			bw = r
		}
	}
	return bw
}
