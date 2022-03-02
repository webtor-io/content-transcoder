package services

import (
	"fmt"
	"io/ioutil"
	u "net/url"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	cp "github.com/webtor-io/content-prober/content-prober"
)

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

type HLSParser struct {
	in     string
	out    string
	probe  *ContentProbe
	r      *HLS
	err    error
	inited bool
	mux    sync.Mutex
	sm     StreamMode
}

func NewHLSParser(c *cli.Context, pr *ContentProbe) *HLSParser {
	sms := c.String(StreamModeFlag)
	sm := Online
	if sms == "multibitrate" {
		sm = MultiBitrate
	}
	return &HLSParser{
		in:    c.String(inputFlag),
		out:   c.String(OutputFlag),
		probe: pr,
		sm:    sm,
	}
}

func (s *HLSParser) Get() (*HLS, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.r, s.err
	}
	s.r, s.err = s.get()
	s.inited = true
	return s.r, s.err
}

func (s *HLSParser) get() (*HLS, error) {
	pr, err := s.probe.Get()

	if err != nil {
		return nil, errors.Wrap(err, "Failed to get probe")
	}

	return NewHLS(s.in, s.out, pr, s.sm), nil
}

type StreamType string

const (
	Audio    StreamType = "a"
	Video    StreamType = "v"
	Subtitle StreamType = "s"
)

type HLS struct {
	in      string
	out     string
	primary []*HLSStream
	video   []*HLSStream
	audio   []*HLSStream
	subs    []*HLSStream
	sm      StreamMode
}

func (h *HLS) GetFFmpegParams() ([]string, error) {

	parsedURL, err := u.Parse(h.in)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to parse url")
	}
	if h.primary[0].s.GetCodecType() == "video" {
		// if h.primary.s.GetCodecName() == "hevc" {
		// 	return nil, errors.Errorf("hevc codec is not supported")
		// }
		if h.primary[0].s.GetHeight() > 1080 {
			return nil, errors.Errorf("resoulution over 1080p is not supported")
		}
	}
	params := []string{}
	// if h.sm == Online {
	// 	params = append(params, "-re")
	// }
	params = append(params,
		"-i", parsedURL.String(),
		// "-err_detect", "ignore_err",
		// "-reconnect_at_eof", "1",
		"-xerror",
		"-seekable", "1",
	)
	for _, s := range h.primary {
		params = append(params, s.GetFFmpegParams()...)
	}
	for _, s := range h.audio {
		params = append(params, s.GetFFmpegParams()...)
	}
	for _, s := range h.subs {
		params = append(params, s.GetFFmpegParams()...)
	}
	return params, nil
}

type HLSStream struct {
	index int
	st    StreamType
	out   string
	s     *cp.Stream
	r     *Rendition
	force bool
}

func (h *HLSStream) GetPlaylistPath() string {
	return fmt.Sprintf("%v/%v", h.out, h.GetPlaylistName())
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
		)
	} else if h.st == Audio && (h.s.GetCodecName() != "aac" || h.s.GetChannels() > 2) {
		params = append(
			params,
			"libfdk_aac",
			"-ac", "2",
		)
	} else if h.st == Subtitle && h.s.GetCodecName() != "webvtt" {
		params = append(params, "webvtt")
	} else {
		params = append(params, "copy")
	}
	return params
}

func (h *HLSStream) GetFFmpegParams() []string {

	params := []string{
		"-map", fmt.Sprintf("0:%v:%v", h.st, h.index),
		"-f", "segment",
		"-segment_time", "4",
		"-segment_list_type", "hls",
		"-segment_list", h.GetPlaylistPath(),
		"-muxdelay", "0",
		"-segment_format", h.GetSegmentFormat(),
	}

	params = append(params, h.GetCodecParams()...)
	if h.r != nil {
		params = append(params, fmt.Sprintf("%v/%v%v-%v-%%d.%v", h.out, h.st, h.index, h.r.Height, h.GetSegmentExtension()))
	} else {
		params = append(params, fmt.Sprintf("%v/%v%v-%%d.%v", h.out, h.st, h.index, h.GetSegmentExtension()))
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

func NewHLSStream(index int, st StreamType, out string, s *cp.Stream, r *Rendition, force bool) *HLSStream {
	return &HLSStream{
		index: index,
		st:    st,
		out:   out,
		s:     s,
		r:     r,
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

func NewHLS(in string, out string, probe *cp.ProbeReply, sm StreamMode) *HLS {
	h := &HLS{
		in:    in,
		out:   out,
		video: []*HLSStream{},
		audio: []*HLSStream{},
		subs:  []*HLSStream{},
		sm:    sm,
	}
	vi := 0
	ai := 0
	si := 0
	for _, s := range probe.GetStreams() {
		if s.GetCodecType() == "video" && s.GetCodecName() != "mjpeg" && s.GetCodecName() != "png" && vi < 1 {
			if sm == Online {
				h.video = append(h.video, NewHLSStream(vi, Video, out, s, &Rendition{Height: uint(s.GetHeight())}, false))
			} else if sm == MultiBitrate {
				rs := h.getRenditions(uint(s.GetHeight()))
				for ri := range rs {
					h.video = append(h.video, NewHLSStream(vi, Video, out, s, &rs[ri], true))
				}
				if len(h.video) == 0 {
					h.video = append(h.video, NewHLSStream(vi, Video, out, s, &Rendition{
						Height: uint(s.GetHeight()),
					}, true))
				}
			}
			vi++
		} else if s.GetCodecType() == "audio" {
			h.audio = append(h.audio, NewHLSStream(ai, Audio, out, s, nil, false))
			ai++
		} else if s.GetCodecType() == "subtitle" && s.GetCodecName() != "hdmv_pgs_subtitle" {
			h.subs = append(h.subs, NewHLSStream(si, Subtitle, out, s, nil, false))
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

func (s *HLS) MakeMasterPlaylist() error {
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
	return ioutil.WriteFile(s.out+"/index.m3u8", []byte(res.String()), 0644)
}
