package services

import (
	"fmt"
	u "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cp "github.com/webtor-io/content-prober/content-prober"
)

const (
	HLSAACCodecFlag             = "hls-aac-codec"
	DisableVideoTranscodingFlag = "disable-video-transcoding"
	FFmpegThreadsFlag           = "ffmpeg-threads"
	PaceLeadFlag                = "pace-lead"
)

func RegisterHLSFlags(f []cli.Flag) []cli.Flag {
	return append(f, cli.StringFlag{
		Name:   HLSAACCodecFlag,
		Usage:  "specify the hls aac codec",
		EnvVar: "HLS_AAC_CODEC",
		Value:  "libfdk_aac",
	}, cli.BoolFlag{
		Name:   DisableVideoTranscodingFlag,
		Usage:  "disable video transcoding",
		EnvVar: "DISABLE_VIDEO_TRANSCODING",
	}, cli.IntFlag{
		Name:   FFmpegThreadsFlag,
		Usage:  "threads per FFmpeg decoder, encoder and filter graph; -1 sizes them from the container's CPU quota, 0 leaves them to FFmpeg",
		EnvVar: "FFMPEG_THREADS",
		Value:  -1,
	}, cli.DurationFlag{
		Name:   PaceLeadFlag,
		Usage:  "how far (media time) FFmpeg may get ahead of the furthest requested segment before it is frozen; 0 disables pacing",
		EnvVar: "PACE_LEAD",
		Value:  5 * time.Minute,
	})
}

type Rendition struct {
	Height   uint
	DefRate  uint
	Required bool
}

func (s *Rendition) adaptRate(h uint, hl uint, hh uint, bl uint, bh uint) uint {
	if h == hl {
		return bl
	}
	if h == hh {
		return bh
	}
	return uint(float64(h-hl)/float64(hh-hl)*float64(bh-bl)) + bl

}

// https://support.google.com/youtube/answer/1722171?hl=en#zippy=%2Cbitrate
var DefaultRenditions = []Rendition{
	{
		Height:   240,
		DefRate:  500,
		Required: true,
	},
	{
		Height:   360,
		DefRate:  1000,
		Required: true,
	},
	{
		Height:  480,
		DefRate: 2500,
	},
	{
		Height:  720,
		DefRate: 5000,
	},
	{
		Height:  1080,
		DefRate: 8000,
	},
}

func (s *Rendition) Rate() uint {
	h := s.Height
	for ri := range DefaultRenditions {
		if h <= DefaultRenditions[ri].Height {
			var hl, bl uint
			if ri != 0 {
				hl, bl = DefaultRenditions[ri-1].Height, DefaultRenditions[ri-1].DefRate
			}
			return s.adaptRate(h, hl, DefaultRenditions[ri].Height, bl, DefaultRenditions[ri].DefRate)
		}
	}
	return DefaultRenditions[len(DefaultRenditions)-1].DefRate
}

type StreamMode int

const (
	Online       StreamMode = 0
	MultiBitrate StreamMode = 1
)

type StreamType string

const (
	Audio    StreamType = "a"
	Video    StreamType = "v"
	Subtitle StreamType = "s"
)

// Content-level rejections: the source can never be transcoded by this
// deployment, as opposed to a transient internal failure. Surfaced to the
// client verbatim (HTTP 415) so upstream UIs can show a specific message.
var (
	ErrTranscodingDisabled    = errors.New("video transcoding is disabled")
	ErrResolutionNotSupported = errors.New("resolution over 1080p is not supported")
	ErrNoPlayableStreams      = errors.New("no video or audio stream")
)

// textSubtitleCodecs are the subtitle codecs the webvtt encoder can take:
// text formats the production build (ffmpeg 8.1.2) has a decoder for. It is
// an allowlist on purpose. A bitmap subtitle (hdmv_pgs, dvd, dvb, xsub) in
// front of -c:s webvtt makes FFmpeg refuse the whole output ("Subtitle
// encoding currently only possible from text to text or bitmap to
// bitmap"), and a stream without a decoder (no codec name, or
// hdmv_text_subtitle, which is text but has no decoder) fails the same way
// ("Decoding requested, but no decoder found"). Either one took the video
// and audio of the run down with it: 129 of 132 sessions with a dvd/dvb
// track never played. eia_608 (cc_dec) is in: checked end to end, it writes
// cues. arib_caption decodes to text too (libaribb24) but was not tried on a
// real stream; it stays out until it is (1 stream in 5244 prod probes).
var textSubtitleCodecs = map[string]bool{
	"subrip":     true,
	"srt":        true,
	"ass":        true,
	"ssa":        true,
	"webvtt":     true,
	"mov_text":   true,
	"text":       true,
	"microdvd":   true,
	"subviewer":  true,
	"subviewer1": true,
	"sami":       true,
	"realtext":   true,
	"mpl2":       true,
	"pjs":        true,
	"jacosub":    true,
	"vplayer":    true,
	"stl":        true,
	"eia_608":    true,
}

type HLS struct {
	in      string
	primary []*HLSStream
	video   []*HLSStream
	audio   []*HLSStream
	subs    []*HLSStream
	cfg     *HLSConfig
}

// ParamOptions adjust a run's FFmpeg arguments beyond what the probe says.
type ParamOptions struct {
	// EncodeAudio encodes AAC audio the probe says can be copied. Set after
	// a copy failed on it (see adtsScalableError): some AAC streams carry a
	// configuration the mpegts muxer's ADTS headers cannot express.
	EncodeAudio bool
	// Lenient drops -xerror. It makes FFmpeg's routine timestamp repairs
	// fatal ("Non-monotonic DTS", "Invalid DTS"), which killed some sources
	// on every start while the seek runs of the same files (which never had
	// -xerror) played. Not the default: without -xerror a failed read of
	// the source ends the run like the end of the file (exit 0, a
	// completed run, no restart), so it is set only for a source that
	// failed on timestamps (see timestampsFailure).
	Lenient bool
}

func (h *HLS) GetFFmpegParams(out string) ([]string, error) {
	return h.GetFFmpegParamsWith(out, ParamOptions{})
}

func (h *HLS) GetFFmpegParamsWith(out string, opts ParamOptions) ([]string, error) {

	parsedURL, err := u.Parse(h.in)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to parse url")
	}
	if len(h.primary) == 0 {
		return nil, ErrNoPlayableStreams
	}
	if h.primary[0].s.GetCodecType() == "video" {
		if h.primary[0].s.GetCodecName() != "h264" {
			if h.cfg.disableVideoTranscoding {
				return nil, ErrTranscodingDisabled
			}
			if h.primary[0].s.GetHeight() > 1080 {
				return nil, ErrResolutionNotSupported
			}
		}
	}
	params := []string{}
	// Thread pools sized to the CPU the container may use (see
	// cpuQuotaThreads). FFmpeg sizes them from the cores it can see: 32 on
	// a worker node under a 1-CPU quota, 92-117 threads per run, and the
	// quota throttled 43% of periods. Measured on the prod EPYC at 1 CPU:
	// 1.6-1.9x faster 1080p x264, 860 -> 220 MB RSS. -threads before -i
	// sizes the decoders; the encoder gets its own below.
	if t := h.cfg.threads; t > 0 {
		params = append(params, "-filter_threads", strconv.Itoa(t), "-threads", strconv.Itoa(t))
	}
	// if h.sm == Online {
	// 	params = append(params, "-re")
	// }
	// Reconnect a dropped source connection (resuming with a Range request)
	// instead of failing: a run pacing holds frozen leaves its connection
	// idle, and torrent-http-proxy retries a lost seeder on another pod.
	params = append(params,
		"-reconnect", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "5",
		"-fix_sub_duration",
		"-i", parsedURL.String(),
		// "-err_detect", "ignore_err",
		// "-reconnect_at_eof", "1",
	)
	if !opts.Lenient {
		params = append(params, "-xerror")
	}
	params = append(params, "-seekable", "1")
	for _, s := range h.primary {
		params = append(params, s.ffmpegParams(out, opts)...)
	}
	for _, s := range h.audio {
		params = append(params, s.ffmpegParams(out, opts)...)
	}
	for _, s := range h.subs {
		// Subtitles FFmpeg cannot turn into webvtt keep their entry in the
		// master playlist (see MakeMasterPlaylist) but get no output: the
		// player's request for them falls through to the empty subtitle
		// playlist instead of the run failing for everyone.
		if !s.hasTextDecoder() {
			continue
		}
		params = append(params, s.ffmpegParams(out, opts)...)
	}
	return params, nil
}

// subtitleWithoutOutput reports whether name is the playlist of a subtitle
// track that is in the master playlist but gets no FFmpeg output (see
// textSubtitleCodecs): no playlist will ever appear for it.
func (h *HLS) subtitleWithoutOutput(name string) bool {
	for _, s := range h.subs {
		if s.GetPlaylistName() == name {
			return !s.hasTextDecoder()
		}
	}
	return false
}

// primaryVideoStreamSpecifier is the input stream the video output is
// mapped from, as an FFmpeg/ffprobe stream specifier, or "" when the primary
// output is not video. Probes that must look at the same stream as the run
// (the copy-mode keyframe lookup) use it instead of v:0, which also counts
// cover art.
func (h *HLS) primaryVideoStreamSpecifier() string {
	for _, p := range h.primary {
		if p.st == Video {
			return strconv.Itoa(int(p.s.GetIndex()))
		}
	}
	return ""
}

type HLSStream struct {
	index int
	st    StreamType
	s     *cp.Stream
	r     *Rendition
	force bool
	cfg   *HLSConfig
}

func (h *HLSStream) GetPlaylistPath(out string) string {
	return fmt.Sprintf("%v/%v", out, h.GetPlaylistName())
}

func (h *HLSStream) GetPlaylistName() string {
	if h.r != nil {
		return fmt.Sprintf("%v%v-%v.m3u8", h.st, h.index, h.r.Height)
	} else {
		return fmt.Sprintf("%v%v.m3u8", h.st, h.index)
	}
}

func (h *HLSStream) GetSegmentFormat() string {
	if h.st == Subtitle {
		return "webvtt"
	}
	return "mpegts"
}

func (h *HLSStream) GetCodecParams() []string {
	return h.codecParams(ParamOptions{})
}

func (h *HLSStream) codecParams(opts ParamOptions) []string {
	params := []string{
		fmt.Sprintf("-c:%v", h.st),
	}
	if h.st == Video && (h.force || h.s.GetCodecName() != "h264") {
		params = append(
			params,
			"h264",
			"-vf", fmt.Sprintf("scale=-2:%v", h.r.Height),
			"-profile:v", "high",
			"-preset", "veryfast",
			"-g", "48", "-keyint_min", "48",
			"-crf", "20",
			"-sc_threshold", "0",
			"-b:v", fmt.Sprintf("%vK", h.r.Rate()),
			"-maxrate", fmt.Sprintf("%vK", uint(float64(h.r.Rate())*1.3)),
			"-bufsize", fmt.Sprintf("%vK", uint(float64(h.r.Rate())*1.5)),
			"-pix_fmt", "yuv420p",
		)
		if h.cfg != nil && h.cfg.threads > 0 {
			params = append(params, "-threads", strconv.Itoa(h.cfg.threads))
		}
	} else if h.st == Audio && (h.s.GetCodecName() != "aac" || h.s.GetChannels() > 2 || opts.EncodeAudio) {
		params = append(
			params,
			h.cfg.aacCodec,
			"-ac", "2",
		)
	} else if h.st == Subtitle && h.s.GetCodecName() != "webvtt" {
		params = append(params, "webvtt")

	} else {
		params = append(params, "copy")
	}
	return params
}

// hasTextDecoder reports whether a subtitle stream can be converted to
// webvtt; every other stream type is always mapped.
func (h *HLSStream) hasTextDecoder() bool {
	return h.st != Subtitle || textSubtitleCodecs[h.s.GetCodecName()]
}

func (h *HLSStream) IsCopy() bool {
	codec := h.GetCodecParams()
	return len(codec) > 0 && codec[len(codec)-1] == "copy"
}

func (h *HLSStream) GetFFmpegParams(out string) []string {
	return h.ffmpegParams(out, ParamOptions{})
}

func (h *HLSStream) ffmpegParams(out string, opts ParamOptions) []string {

	// Mapped by the input stream's own index. h.index is this stream's
	// number among the streams NewHLS keeps (it names the outputs: v0, a1,
	// s2), and FFmpeg's 0:<type>:<n> counts every stream of the type --
	// the two disagree as soon as NewHLS skips one. A PGS track before a
	// text track mapped the PGS into the webvtt encoder and failed the run
	// (140 of 686 failed runs in 50h); cover art before the video mapped
	// the picture as the movie.
	params := []string{
		"-map", fmt.Sprintf("0:%d", h.s.GetIndex()),
		"-f", "segment",
		"-segment_time", "4",
		"-segment_list_type", "hls",
		"-segment_list", h.GetPlaylistPath(out),
		"-muxdelay", "0",
		"-segment_format", h.GetSegmentFormat(),
	}

	// For transcoded streams (not copy), force exact segment boundaries
	codec := h.codecParams(opts)
	if codec[len(codec)-1] != "copy" {
		params = append(params, "-break_non_keyframes", "1")
	}

	params = append(params, codec...)
	if h.r != nil {
		params = append(params, fmt.Sprintf("%v/%v%v-%v-%%d.%v", out, h.st, h.index, h.r.Height, h.GetSegmentExtension()))
	} else {
		params = append(params, fmt.Sprintf("%v/%v%v-%%d.%v", out, h.st, h.index, h.GetSegmentExtension()))
	}

	return params
}
func (h *HLSStream) GetSegmentExtension() string {
	if h.st == Subtitle {
		return "vtt"
	}
	return "ts"
}

func (h *HLSStream) GetName() string {
	n := "Track"
	if h.st == Subtitle {
		n = "Subtitle"
	}
	name := fmt.Sprintf("%v #%v", n, h.index+1)
	if title, ok := h.s.Tags["title"]; ok {
		name = title
	}
	if lang, ok := h.s.Tags["language"]; ok {
		name = name + fmt.Sprintf(" (%v)", lang)
	}
	return name
}

func (h *HLSStream) GetLanguage() string {
	lang := "eng"
	if title, ok := h.s.Tags["language"]; ok {
		lang = title
	}
	return lang
}

func (h *HLSStream) MakeMasterPlaylist() string {
	t := "AUDIO"
	if h.st == Subtitle {
		t = "SUBTITLES"
	}
	extra := ""
	if h.st == Audio && h.index == 0 {
		extra = ",AUTOSELECT=YES,DEFAULT=YES"
	}
	return fmt.Sprintf(
		`#EXT-X-MEDIA:TYPE=%v,GROUP-ID="%v",LANGUAGE="%v",NAME="%v"%v,URI="%v"`,
		t, strings.ToLower(t), h.GetLanguage(), h.GetName(), extra, h.GetPlaylistName(),
	)
}

func NewHLSStream(index int, st StreamType, s *cp.Stream, r *Rendition, cfg *HLSConfig, force bool) *HLSStream {
	return &HLSStream{
		index: index,
		st:    st,
		s:     s,
		r:     r,
		cfg:   cfg,
		force: force,
	}
}

func (s *HLS) getNextRendition(height uint) *Rendition {
	for ri := range DefaultRenditions {
		if height < DefaultRenditions[ri].Height {
			return &DefaultRenditions[ri]
		}
	}
	return nil
}

func (s *HLS) getRenditions(height uint) []Rendition {
	if height > DefaultRenditions[len(DefaultRenditions)-1].Height {
		height = DefaultRenditions[len(DefaultRenditions)-1].Height
	}
	rs := []Rendition{}
	for ri := range DefaultRenditions {
		if height >= DefaultRenditions[ri].Height {
			rs = append(rs, DefaultRenditions[ri])
		}
	}
	if rs[len(rs)-1].Height < height {
		ex := float64(height-rs[len(rs)-1].Height) / float64(s.getNextRendition(height).Height-rs[len(rs)-1].Height)
		if !rs[len(rs)-1].Required && ex < 0.3 {
			rs = rs[:len(rs)-1]
		}
		rs = append(rs, Rendition{Height: height})
	}
	return rs
}

func NewHLS(in string, probe *cp.ProbeReply, cfg *HLSConfig) *HLS {
	h := &HLS{
		in:    in,
		video: []*HLSStream{},
		audio: []*HLSStream{},
		subs:  []*HLSStream{},
		cfg:   cfg,
	}
	vi := 0
	ai := 0
	si := 0
	for _, s := range probe.GetStreams() {
		if s.GetCodecType() == "video" && s.GetCodecName() != "mjpeg" && s.GetCodecName() != "png" && vi < 1 {
			if cfg.sm == Online {
				h.video = append(h.video, NewHLSStream(vi, Video, s, &Rendition{Height: uint(s.GetHeight())}, cfg, false))
			} else if cfg.sm == MultiBitrate {
				rs := h.getRenditions(uint(s.GetHeight()))
				for ri := range rs {
					h.video = append(h.video, NewHLSStream(vi, Video, s, &rs[ri], cfg, true))
				}
				if len(h.video) == 0 {
					h.video = append(h.video, NewHLSStream(vi, Video, s, &Rendition{
						Height: uint(s.GetHeight()),
					}, cfg, true))
				}
			}
			vi++
		} else if s.GetCodecType() == "audio" {
			h.audio = append(h.audio, NewHLSStream(ai, Audio, s, nil, cfg, false))
			ai++
		} else if s.GetCodecType() == "subtitle" && s.GetCodecName() != "hdmv_pgs_subtitle" {
			h.subs = append(h.subs, NewHLSStream(si, Subtitle, s, nil, cfg, false))
			si++
		}
	}
	if len(h.video) > 0 {
		h.primary = h.video
	} else if len(h.audio) > 0 {
		h.primary = []*HLSStream{h.audio[0]}
		h.audio = []*HLSStream{}
		h.subs = []*HLSStream{}
	}
	return h
}

func (s *HLS) MakeMasterPlaylist(out string) error {
	var res strings.Builder
	res.WriteString("#EXTM3U\n")
	for _, a := range s.audio {
		res.WriteString(fmt.Sprintln(a.MakeMasterPlaylist()))
	}
	for _, su := range s.subs {
		res.WriteString(fmt.Sprintln(su.MakeMasterPlaylist()))
	}
	for _, p := range s.primary {
		var rate uint = 1
		if p.r != nil {
			rate = p.r.Rate() * 1000
		}
		res.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%v,CODECS=\"avc1.42e00a,mp4a.40.2\"", rate))
		if len(s.audio) > 0 {
			res.WriteString(`,AUDIO="audio"`)
		}
		if len(s.subs) > 0 {
			res.WriteString(`,SUBTITLES="subtitles"`)
		}
		res.WriteRune('\n')
		res.WriteString(p.GetPlaylistName())
		res.WriteRune('\n')
	}
	return os.WriteFile(out+"/index.m3u8", []byte(res.String()), 0644)
}

type HLSBuilder struct {
	aacCodec                string
	disableVideoTranscoding bool
	threads                 int
	paceLead                time.Duration
}

type HLSConfig struct {
	sm                      StreamMode
	aacCodec                string
	disableVideoTranscoding bool
	// threads per FFmpeg decoder, encoder and filter graph; 0 leaves the
	// sizing to FFmpeg.
	threads int
	// paceLead is how far ahead of its viewers a run may get (pacing.go);
	// 0 disables pacing.
	paceLead time.Duration
}

func NewHLSBuilder(c *cli.Context) *HLSBuilder {
	threads := c.Int(FFmpegThreadsFlag)
	if threads < 0 {
		threads = cpuQuotaThreads()
	}
	log.WithField("threads", threads).Info("hls: FFmpeg threads per decoder/encoder (0 = FFmpeg decides)")
	paceLead := c.Duration(PaceLeadFlag)
	logPaceConfig(paceLead)
	return &HLSBuilder{
		aacCodec:                c.String(HLSAACCodecFlag),
		disableVideoTranscoding: c.Bool(DisableVideoTranscodingFlag),
		threads:                 threads,
		paceLead:                paceLead,
	}
}

func (s *HLSBuilder) Build(in string, probe *cp.ProbeReply) *HLS {
	return NewHLS(in, probe, &HLSConfig{
		sm:                      Online,
		aacCodec:                s.aacCodec,
		disableVideoTranscoding: s.disableVideoTranscoding,
		threads:                 s.threads,
		paceLead:                s.paceLead,
	})
}
