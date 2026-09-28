package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/bits"
	u "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// The output side of a passthrough session: the source's HEVC copied into
// HLS fMP4 (FFmpeg's hls muxer), its audio with it, subtitles as on the old
// route.
//
// Files of one process of a run, in the run's directory:
//
//	v0-<h>-init-<gen>.mp4   the video's init segment (moov with the hvcC)
//	v0-<h>-<n>.m4s          video segments, cut at keyframes
//	v0-<h>.m3u8.ffmpeg      its playlist, with #EXT-X-MAP naming the init
//	a<i>-init-<gen>.mp4, a<i>-<n>.m4s, a<i>.m3u8.ffmpeg   each audio track
//	s<i>-<n>.vtt, s<i>.m3u8.ffmpeg                        subtitles (segment muxer)
//
// <gen> is the process's generation (TranscodeRun.generation), made before
// its arguments are: a restart reuses the directory, and hlsenc opens the
// init file when it starts and fills it only at its first cut
// (hlsenc.c hls_mux_init, hls_write_packet). Under a fixed name the new
// process would empty the init the previous playlist still names; under
// its own name every playlist names an init only its own process writes.
// The segments are written to .tmp and renamed (temp_file), the playlist is
// rewritten whole on every cut, after the cut's init and segment are closed.

// passthroughSegmentExt is the extension of a passthrough session's audio
// and video segments.
const passthroughSegmentExt = "m4s"

// streamPrefix is what every file of the stream is named from: v0-2160,
// a1, s0.
func (h *HLSStream) streamPrefix() string {
	if h.r != nil {
		return fmt.Sprintf("%v%v-%v", h.st, h.index, h.r.Height)
	}
	return fmt.Sprintf("%v%v", h.st, h.index)
}

// initName is the init segment of an fMP4 stream as written by the process
// of generation gen.
func (h *HLSStream) initName(gen string) string {
	return h.streamPrefix() + "-init-" + gen + ".mp4"
}

// fmp4Params is the FFmpeg output of an fMP4 stream (the video or an audio
// track of a passthrough session). The playlist goes straight to its
// .ffmpeg name: redirectSegmentListParams only rewrites -segment_list.
func (h *HLSStream) fmp4Params(out, gen string, opts ParamOptions) []string {
	params := []string{"-map", fmt.Sprintf("0:%d", h.s.GetIndex())}
	params = append(params, h.codecParams(opts)...)
	return append(params,
		"-f", "hls",
		"-hls_time", strconv.Itoa(sessionSegDuration),
		// event forces it too; explicit in case the type ever changes.
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "temp_file",
		// A bare name: hlsenc writes it next to the playlist and puts it in
		// #EXT-X-MAP as given.
		"-hls_fmp4_init_filename", h.initName(gen),
		"-hls_segment_filename", fmt.Sprintf("%v/%v-%%d.%v", out, h.streamPrefix(), passthroughSegmentExt),
		h.GetPlaylistPath(out)+".ffmpeg",
	)
}

// errNoGeneration: a passthrough command names its init after the process
// it is for, so it cannot be built without one.
var errNoGeneration = errors.New("passthrough arguments need the process generation")

// buildPassthroughParams is the FFmpeg command of a passthrough run's
// process gen. The input side is the old route's (reconnect, threads,
// -xerror unless lenient); the seek is added by the run
// (injectPassthroughSeekParams). A variable so tests can stand in for it.
var buildPassthroughParams = func(h *HLS, in *u.URL, out string, opts ParamOptions, gen string) ([]string, error) {
	if gen == "" {
		return nil, errNoGeneration
	}
	params := []string{}
	if t := h.cfg.threads; t > 0 {
		params = append(params, "-filter_threads", strconv.Itoa(t), "-threads", strconv.Itoa(t))
	}
	params = append(params,
		"-reconnect", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "5",
		"-fix_sub_duration",
		"-i", in.String(),
	)
	if !opts.Lenient {
		params = append(params, "-xerror")
	}
	params = append(params, "-seekable", "1")
	for _, s := range h.primary {
		params = append(params, s.fmp4Params(out, gen, opts)...)
	}
	for _, s := range h.audio {
		params = append(params, s.fmp4Params(out, gen, opts)...)
	}
	for _, s := range h.subs {
		if !s.hasTextDecoder() {
			continue
		}
		params = append(params, s.ffmpegParams(out, opts)...)
	}
	return params, nil
}

// injectPassthroughSeekParams is the seek of a passthrough run: its input
// seek (copySeekInput), and the output's time zero moved from the
// quantized time to realStart, the first timestamp that seek gives. The
// audio outputs whose -map is in trimAudio start at that zero too.
//
// Not -ss <realStart>: FFmpeg's input seek goes back to the keyframe at or
// before the target, and for a format without AVFMT_SEEK_TO_PTS (matroska)
// with B-frames it first takes 3/23 s off the target
// (fftools/ffmpeg_demux.c, dts_heuristic): -ss at the keyframe itself
// lands on the one before, a whole GOP earlier. Measured on FFmpeg 8.1.2
// (10 s GOPs): -ss 20.020 on an MKV whose keyframe is at 20.020, and -ss
// 19.770 on an MP4 whose keyframe has that DTS, both started at 10.010.
//
// Without the offset every output takes the quantized time as zero and
// shifts its own negative timestamps away: video and audio begin at the
// keyframe, subtitles at the quantized time -- cues early by up to a GOP
// (cues at 21 s and 26 s after a seek to 25 over a keyframe at 20.02 came
// out at 0.000 and 5.000). With -itsoffset <seek-realStart> all outputs
// count from realStart (1.063 and 6.063 for realStart 19.937, the
// keyframe's DTS), and the video's first DTS is 0, so it is not shifted.
//
// The audio is not: the demuxer's seek lands the audio a little before the
// video's keyframe (the first copied AAC packet 162 ms before realStart on
// the e2e A/V source), and each output shifts its own negative timestamps
// to zero, so every seek played the audio that much late. -ss 0 on an audio
// output drops what comes before zero: a copied packet whose DTS is below
// the output's start time (fftools/ffmpeg_mux.c of_streamcopy), encoded
// samples through a trim at it (ffmpeg_filter.c insert_trim). Measured on
// FFmpeg 8.1.2 through the hls muxer: A/V after the seek from +162 ms to
// -8 ms (copied AAC) and from +184 ms to +43 ms (libfdk_aac, its priming),
// against -62 and -40 ms from the start. trimAudio is empty when realStart
// is not resolved: the video then starts at the keyframe before the zero,
// and audio cut at the zero would run ahead of it by up to a GOP.
func injectPassthroughSeekParams(params []string, seek, realStart float64, trimAudio []string) []string {
	trim := make(map[string]bool, len(trimAudio))
	for _, m := range trimAudio {
		trim[m] = true
	}
	result := make([]string, 0, len(params)+7+2*len(trimAudio))
	for i, p := range params {
		switch {
		case p == "-i":
			result = append(result, copySeekInput(seek)...)
			if d := seek - realStart; d > 0 {
				result = append(result, "-itsoffset", fmt.Sprintf("%.6f", d))
			}
		case p == "-map" && i+1 < len(params) && trim[params[i+1]]:
			result = append(result, "-ss", "0")
		}
		result = append(result, p)
	}
	return result
}

// passthroughAudioMaps are the -map values of the session's audio outputs.
func (h *HLS) passthroughAudioMaps() []string {
	var maps []string
	for _, a := range h.audio {
		maps = append(maps, fmt.Sprintf("0:%d", a.s.GetIndex()))
	}
	return maps
}

// probePassthroughStart resolves the real start of a passthrough run's
// seek: the earliest timestamp (DTS, else PTS) of the first video packet the
// run's own input seek gives, in movie time. A variable so tests stub the
// exec.
var probePassthroughStart = ffmpegSeekStart

// hevcCodecString is the RFC 6381 CODECS value of an HEVC stream from its
// hvcC (ISO/IEC 14496-15 Annex E): the sample entry, the profile space
// (none, A, B, C) and profile, the compatibility flags in reverse bit order
// in hex, the tier (L/H) and level, and the six constraint bytes in hex
// with the trailing zero ones left out. hvc1.1.6.L93.90 is Main level 3.1.
func hevcCodecString(fourcc string, h hvccHeader) string {
	var b strings.Builder
	b.WriteString(fourcc)
	b.WriteByte('.')
	if h.profileSpace > 0 {
		b.WriteByte(byte('A' + h.profileSpace - 1))
	}
	fmt.Fprintf(&b, "%d.%X.", h.profileIdc, bits.Reverse32(h.compat))
	if h.tierHigh {
		b.WriteByte('H')
	} else {
		b.WriteByte('L')
	}
	b.WriteString(strconv.Itoa(h.levelIdc))
	n := len(h.constraint)
	for n > 0 && h.constraint[n-1] == 0 {
		n--
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, ".%02X", h.constraint[i])
	}
	return b.String()
}

// hevcSampleEntries are the sample entries an fMP4 HEVC track can have.
var hevcSampleEntries = map[string]bool{"hvc1": true, "hev1": true}

// initHEVCConfig reads an fMP4 init segment: the fourcc of its HEVC sample
// entry and that entry's hvcC. The path is moov/trak/mdia/minf/stbl/stsd.
func initHEVCConfig(init []byte) (string, []byte, error) {
	path := []string{"moov", "trak", "mdia", "minf", "stbl", "stsd"}
	b := init
	for _, want := range path {
		body, ok := findBox(b, want)
		if !ok {
			return "", nil, errors.Errorf("no %s box", want)
		}
		b = body
	}
	// stsd: version and flags, entry count, then the sample entries.
	if len(b) < 8 {
		return "", nil, errors.New("short stsd")
	}
	b = b[8:]
	for len(b) >= 8 {
		size, typ, hdr, ok := boxHeader(b)
		if !ok {
			return "", nil, errors.New("bad sample entry")
		}
		if hevcSampleEntries[typ] {
			// A VisualSampleEntry has 78 bytes of fields before its boxes.
			const visualFields = 78
			if size < hdr+visualFields {
				return "", nil, errors.New("short sample entry")
			}
			hvcc, ok := findBox(b[hdr+visualFields:size], "hvcC")
			if !ok {
				return "", nil, errors.Errorf("%s without hvcC", typ)
			}
			return typ, hvcc, nil
		}
		b = b[size:]
	}
	return "", nil, errors.New("no HEVC sample entry")
}

// boxHeader reads the header of the ISO BMFF box at the start of b: its
// whole size, type and header length. ok is false for a box that does not
// fit in b.
func boxHeader(b []byte) (size int, typ string, hdr int, ok bool) {
	if len(b) < 8 {
		return 0, "", 0, false
	}
	s := uint64(binary.BigEndian.Uint32(b))
	typ = string(b[4:8])
	hdr = 8
	switch s {
	case 0: // to the end
		s = uint64(len(b))
	case 1: // 64-bit size
		if len(b) < 16 {
			return 0, "", 0, false
		}
		s = binary.BigEndian.Uint64(b[8:16])
		hdr = 16
	}
	if s < uint64(hdr) || s > uint64(len(b)) {
		return 0, "", 0, false
	}
	return int(s), typ, hdr, true
}

// findBox returns the body of the first box of type typ among the boxes in b.
func findBox(b []byte, typ string) ([]byte, bool) {
	for len(b) >= 8 {
		size, t, hdr, ok := boxHeader(b)
		if !ok {
			return nil, false
		}
		if t == typ {
			return b[hdr:size], true
		}
		b = b[size:]
	}
	return nil, false
}

// Fields of passthrough_codecs_mismatch_total: which part of the output's
// hvcC differs from the source's the route was decided on, or unbuildable
// when no CODECS could be read off the output at all.
const (
	codecsMismatchProfile     = "profile"
	codecsMismatchTier        = "tier"
	codecsMismatchLevel       = "level"
	codecsMismatchUnbuildable = "unbuildable"
)

// codecsMismatches lists what of out, the output's hvcC, differs from src,
// what the route was decided on: outputHVCC's reading of the source, which
// merges the parameter sets as FFmpeg's writer does (ff_isom_write_hvcc;
// the source record's head plays no part). A difference means that reading
// is wrong, and a decision made on one while the player is told the other
// is a bug to hear about.
func codecsMismatches(src, out hvccHeader) []string {
	var m []string
	if src.profileSpace != out.profileSpace || src.profileIdc != out.profileIdc {
		m = append(m, codecsMismatchProfile)
	}
	if src.tierHigh != out.tierHigh {
		m = append(m, codecsMismatchTier)
	}
	if src.levelIdc != out.levelIdc {
		m = append(m, codecsMismatchLevel)
	}
	return m
}

// passthroughAudioBandwidth is what an audio rendition is counted at in
// BANDWIDTH when the declaration leaves the audio as it was: copied AAC is
// at most 2 channels, encoded AAC is 2 channels at libfdk_aac's default
// rate. An allowance, not measured per source.
const passthroughAudioBandwidth = 192_000

// passthroughAudioAllowance is the audio part of a passthrough session's
// BANDWIDTH when its run uses opts: none without audio renditions,
// passthroughAudioBandwidth for the audio every session had, and the
// largest rate of its audio outputs under opts (audioBandwidth: a copied
// E-AC-3 at its own rate, AAC 5.1 at 384 kb/s) when the declaration
// changes them.
func (h *HLS) passthroughAudioAllowance(opts ParamOptions) int64 {
	switch {
	case len(h.audio) == 0:
		return 0
	case h.audioVariant() == "":
		return passthroughAudioBandwidth
	}
	return h.audioBandwidth(opts)
}

// passthroughBandwidth is the BANDWIDTH of a passthrough variant: the
// larger of the source's average bit rate (content-prober's, the whole file
// with every track, 0 when unknown) and the first video segment's rate with
// the audio allowance (passthroughAudioAllowance). Not Rendition.Rate,
// which stops at 8 Mbit/s.
func passthroughBandwidth(sourceBitRate, seg0Bytes int64, seg0Seconds float64, audio int64) int64 {
	bw := sourceBitRate
	if seg0Seconds > 0 && seg0Bytes > 0 {
		measured := int64(float64(seg0Bytes*8) / seg0Seconds)
		measured += audio
		if measured > bw {
			bw = measured
		}
	}
	if bw <= 0 {
		bw = 1
	}
	return bw
}

// passthroughMasterPlaylist is passthroughMasterFor a run without fallback
// options.
func (s *HLS) passthroughMasterPlaylist(videoCodecs string, bandwidth int64) string {
	return s.passthroughMasterFor(videoCodecs, bandwidth, ParamOptions{})
}

// passthroughMasterFor is the master playlist of a passthrough session
// whose run uses opts: the renditions as on the old route, and one variant
// described by the output (CODECS from its init, VIDEO-RANGE from the
// transfer the route was decided on, the audio's CODECS and CHANNELS from
// what the run's options make of each track -- after a muxer refused a
// copied E-AC-3 that is the AAC the run encodes from the next start).
//
// Every audio rendition is in the one group, and CODECS lists each audio
// codec they have, once, in rendition order (audioCodecs: "mp4a.40.2,ec-3"
// for AAC stereo then a copied E-AC-3), as RFC 8216 4.3.4.2 asks of a
// variant. hls.js 1.6.14 keeps the
// variant only if MediaSource takes every codec it lists
// (level-controller.ts, areCodecsMediaSourceSupported) -- the declaration
// is that answer -- and builds the SourceBuffer of an alternate audio
// track from the codec in the track's own init (passthrough-remuxer.ts
// getParsedTrackCodec, buffer-controller.ts pickMostCompleteCodecName: a
// parsed "ec-3" or "mp4a.40.2" wins over the variant's; the variant's is
// taken as the track's only when it lists one audio codec), changing the
// SourceBuffer's type when a switch changes the codec (changeType).
func (s *HLS) passthroughMasterFor(videoCodecs string, bandwidth int64, opts ParamOptions) string {
	changed := s.audioVariant() != ""
	var res strings.Builder
	res.WriteString("#EXTM3U\n")
	for _, a := range s.audio {
		res.WriteString(a.masterMedia(changed, opts))
		res.WriteRune('\n')
	}
	for _, su := range s.subs {
		res.WriteString(su.MakeMasterPlaylist())
		res.WriteRune('\n')
	}
	v := s.primaryVideo()
	codecs := videoCodecs
	if len(s.audio) > 0 {
		codecs += "," + s.audioCodecs(opts)
	}
	videoRange := "SDR"
	if s.passFacts != nil && s.passFacts.transfer() == transferPQ {
		videoRange = "PQ"
	}
	fmt.Fprintf(&res, `#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS="%s",VIDEO-RANGE=%s`,
		bandwidth, v.s.GetWidth(), v.s.GetHeight(), codecs, videoRange)
	if len(s.audio) > 0 {
		res.WriteString(`,AUDIO="audio"`)
	}
	if len(s.subs) > 0 {
		res.WriteString(`,SUBTITLES="subtitles"`)
	}
	res.WriteRune('\n')
	res.WriteString(v.GetPlaylistName())
	res.WriteRune('\n')
	return res.String()
}

// initNamePattern is the name of an init segment: the stream's prefix and
// the generation of the process that wrote it.
var initNamePattern = regexp.MustCompile(`^([av][0-9]+(?:-[0-9]+)?)-init-([0-9a-f]{16})\.mp4$`)

// fmp4Stream is the fMP4 stream of the session whose files start with
// prefix, nil when there is none (the old route has none: only
// usePassthrough makes a stream fMP4).
func (h *HLS) fmp4Stream(prefix string) *HLSStream {
	if h == nil {
		return nil
	}
	for _, group := range [][]*HLSStream{h.primary, h.audio} {
		for _, s := range group {
			if s.fmp4 && s.streamPrefix() == prefix {
				return s
			}
		}
	}
	return nil
}

// ownsSegment reports whether name is a segment file of one of the
// session's fMP4 streams.
func (h *HLS) ownsSegment(name string) bool {
	m := segmentStreamPattern.FindStringSubmatch(name)
	if m == nil || !strings.HasSuffix(name, "."+passthroughSegmentExt) {
		return false
	}
	return h.fmp4Stream(m[1]) != nil
}

// processState is the generation of the run's current FFmpeg process and
// whether it is still running, read together.
func (r *TranscodeRun) processState() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running && r.done != nil {
		select {
		case <-r.done:
			r.running = false
		default:
		}
	}
	return r.generation, r.running
}

// initFile is the init segment of stream s written by process gen of this
// run: its path, whether it is complete, and -- when it is not -- whether
// it may still become so (it is the running process's own, before its
// first cut). The running process's init is complete once its playlist
// names it: hlsenc writes the init whole and closes it at the first cut,
// before it writes that cut's playlist, and names it in every playlist it
// writes. Any other process's init is final: whole if it is not empty.
func (r *TranscodeRun) initFile(s *HLSStream, gen string) (path string, complete, pending bool) {
	name := s.initName(gen)
	path = filepath.Join(r.OutputDir(), name)
	cur, running := r.processState()
	if gen == cur && running {
		pl, err := os.ReadFile(filepath.Join(r.OutputDir(), s.GetPlaylistName()+".ffmpeg"))
		if err != nil || !bytes.Contains(pl, []byte(`URI="`+name+`"`)) {
			return path, false, true
		}
		return path, true, false
	}
	fi, err := os.Stat(path)
	return path, err == nil && fi.Mode().IsRegular() && fi.Size() > 0, false
}

// currentRun is the session's run right now, nil when it has none.
func (s *Session) currentRun() *TranscodeRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

// errCodecsUnbuildable: the output's init has no HEVC configuration a CODECS
// value can be made of. The master is not guessed: a wrong CODECS fails in
// the player at once.
var errCodecsUnbuildable = errors.New("CODECS cannot be built from the output init")

// passthroughMasterPoll is how often the master waits look at the run.
var passthroughMasterPoll = 200 * time.Millisecond

// writePassthroughMaster writes the master playlist of a passthrough
// session once the video init of its run's current process is complete,
// waiting up to timeout. A master already there is kept: the video's
// configuration is the source's, the same for every run of the session
// (its audio part follows the run: refreshMaster).
func (s *Session) writePassthroughMaster(ctx context.Context, timeout time.Duration) error {
	master := filepath.Join(s.outputDir, "index.m3u8")
	if fileExists(master) {
		return nil
	}
	v := s.h.primaryVideo()
	if v == nil {
		return errors.New("passthrough session without a video")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	t := time.NewTicker(passthroughMasterPoll)
	defer t.Stop()
	for {
		if run := s.currentRun(); run != nil {
			gen, _ := run.processState()
			path, complete, pending := run.initFile(v, gen)
			if complete {
				return s.buildPassthroughMaster(run, path)
			}
			if !pending {
				return errPlaylistNotRunning
			}
		} else {
			return errPlaylistNotRunning
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errPlaylistWaitTimeout
		case <-t.C:
		}
	}
}

// buildPassthroughMaster makes the master from the video init at initPath,
// the run's first video segment and the audio the run makes (runHLS with
// the run's options: what its process makes -- or, after one died on a
// fallback, the next one will; the options change only between
// processes), counts any difference from the source's hvcC, and writes it
// whole (a temporary file renamed over it). What it was made of is kept
// for refreshMaster.
func (s *Session) buildPassthroughMaster(run *TranscodeRun, initPath string) error {
	logger := s.logger.WithField("init", filepath.Base(initPath))
	init, err := os.ReadFile(initPath)
	if err != nil {
		return err
	}
	fourcc, raw, err := initHEVCConfig(init)
	var out hvccHeader
	ok := err == nil
	if ok {
		out, ok = parseHVCC(raw)
	}
	if !ok {
		metricPassthroughCodecsMismatch.WithLabelValues(codecsMismatchUnbuildable).Inc()
		logger.WithError(err).Error("passthrough: no CODECS in the output init")
		return errCodecsUnbuildable
	}
	codecs := hevcCodecString(fourcc, out)
	if s.h.passFacts != nil {
		if src, ok := outputHVCC(s.h.passFacts.HVCC); ok {
			if diff := codecsMismatches(src, out); len(diff) > 0 {
				for _, f := range diff {
					metricPassthroughCodecsMismatch.WithLabelValues(f).Inc()
				}
				logger.WithFields(log.Fields{
					"source": hevcCodecString(fourcc, src),
					"output": codecs,
					"fields": strings.Join(diff, ","),
				}).Warn("passthrough: the output's HEVC configuration differs from the source's the route was decided on")
			}
		}
	}
	v := s.h.primaryVideo()
	var seg0Bytes int64
	var seg0Seconds float64
	if segs, _ := run.readMediaPlaylist(v.GetPlaylistName()); len(segs) > 0 {
		seg0Seconds = segs[0].dur
		if fi, err := os.Stat(filepath.Join(run.OutputDir(), fmt.Sprintf("%s-%d.%s", v.streamPrefix(), segs[0].n, passthroughSegmentExt))); err == nil {
			seg0Bytes = fi.Size()
		}
	}
	opts, h := run.options(), s.runHLS(run)
	in := passthroughMasterInputs{videoCodecs: codecs, seg0Bytes: seg0Bytes, seg0Seconds: seg0Seconds}
	bw := in.bandwidth(h, opts)
	if err := s.writeMasterFile(h.passthroughMasterFor(codecs, bw, opts)); err != nil {
		return err
	}
	s.mu.Lock()
	s.ptMaster = &in
	s.masterAudio = h.audioSignature(opts)
	s.mu.Unlock()
	logger.WithFields(log.Fields{"codecs": codecs, "bandwidth": bw}).Info("passthrough: master playlist written")
	return nil
}

// passthroughMasterInputs is what a passthrough master is made of besides
// the audio: the video's CODECS from the init and the first video segment
// its rate was measured on.
type passthroughMasterInputs struct {
	videoCodecs string
	seg0Bytes   int64
	seg0Seconds float64
}

// bandwidth is the master's BANDWIDTH with the audio of a run using opts.
func (in passthroughMasterInputs) bandwidth(h *HLS, opts ParamOptions) int64 {
	return passthroughBandwidth(h.sourceBitRate, in.seg0Bytes, in.seg0Seconds, h.passthroughAudioAllowance(opts))
}

// writeMasterFile writes the session's master whole: a temporary file
// renamed over it, so a reader gets the old one or the new one.
func (s *Session) writeMasterFile(data string) error {
	tmp, err := os.CreateTemp(s.outputDir, "index.m3u8.tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Errorf("write master: %v %v", werr, cerr)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.outputDir, "index.m3u8")); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// passthroughRefPattern matches the references in a passthrough session's
// playlists as whole tokens: the init segment names, and playlist and
// segment names. The generation in an init's name is hex, and the old
// pattern (playlistFilePattern, unanchored) finds "a12.mp4" at the end of
// one ending in a12 -- a prefix would land mid-name -- or nothing at all in
// one ending in, say, e90, and the init goes out without the token.
// Bounded left by the line start, a quote or a slash, right by a quote or
// the line end.
var passthroughRefPattern = regexp.MustCompile(`(^|["/])([av][0-9]+(?:-[0-9]+)?-init-[0-9a-f]{16}\.mp4|[asv][0-9]+(?:-[0-9]+){0,2}\.(?:m3u8|m4s|vtt))("|$)`)

// rewritePassthroughRefs applies f to every reference in a passthrough
// session's playlist (passthroughRefPattern). A function, not a template:
// the query it appends is the client's, and a $ in it is not a group.
func rewritePassthroughRefs(data []byte, f func(ref string) string) []byte {
	var sb strings.Builder
	for _, line := range strings.SplitAfter(string(data), "\n") {
		nl := strings.HasSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\n")
		if line == "" && !nl {
			continue
		}
		line = passthroughRefPattern.ReplaceAllStringFunc(line, func(m string) string {
			sub := passthroughRefPattern.FindStringSubmatch(m)
			return sub[1] + f(sub[2]) + sub[3]
		})
		sb.WriteString(line)
		sb.WriteRune('\n')
	}
	return []byte(sb.String())
}

// enrichPlaylistDataFor is enrichPlaylistData for a session's playlist: the
// old route's playlists keep the old pattern, byte for byte; a passthrough
// session's references (init names among them) are matched as tokens.
func enrichPlaylistDataFor(h *HLS, data []byte, rawQuery string) []byte {
	if h == nil || !h.passthrough {
		return enrichPlaylistData(data, rawQuery)
	}
	if rawQuery == "" {
		return data
	}
	return rewritePassthroughRefs(data, func(ref string) string { return ref + "?" + rawQuery })
}

// prefixPlaylistRefsFor is prefixPlaylistRefs for a session's playlist, the
// same way as enrichPlaylistDataFor.
func prefixPlaylistRefsFor(h *HLS, data []byte, prefix string) []byte {
	if h == nil || !h.passthrough {
		return prefixPlaylistRefs(data, prefix)
	}
	if prefix == "" {
		return data
	}
	return rewritePassthroughRefs(data, func(ref string) string { return prefix + ref })
}
