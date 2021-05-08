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

type HLSParser struct {
	in     string
	out    string
	probe  *ContentProbe
	r      *HLS
	err    error
	inited bool
	mux    sync.Mutex
}

func NewHLSParser(c *cli.Context, pr *ContentProbe) *HLSParser {
	return &HLSParser{
		in:    c.String(inputFlag),
		out:   c.String(outputFlag),
		probe: pr,
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

	return NewHLS(s.in, s.out, pr), nil
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
	primary *HLSStream
	video   []*HLSStream
	audio   []*HLSStream
	subs    []*HLSStream
}

func (h *HLS) GetFFmpegParams() ([]string, error) {

	parsedURL, err := u.Parse(h.in)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to parse url")
	}
	if h.primary.s.GetCodecType() == "video" {
		if h.primary.s.GetCodecName() == "hevc" {
			return nil, errors.Errorf("hevc codec is not supported")
		}
		if h.primary.s.GetHeight() > 1080 {
			return nil, errors.Errorf("resoulution over 1080p is not supported")
		}
	}
	params := []string{
		"-i", parsedURL.String(),
		"-err_detect", "ignore_err",
		"-reconnect_at_eof", "1",
		"-reconnect_streamed", "1",
		"-seekable", "1",
	}
	params = append(params, h.primary.GetFFmpegParams()...)
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
}

func (h *HLSStream) GetPlaylistPath() string {
	return fmt.Sprintf("%v/%v", h.out, h.GetPlaylistName())
}

func (h *HLSStream) GetPlaylistName() string {
	return fmt.Sprintf("%v%v.m3u8", h.st, h.index)
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
	if h.st == Video {
		params = append(
			params,
			"h264",
			"-preset", "veryfast",
			"-b:v", "2M",
			"-maxrate", "2M",
			"-bufsize", "1M",
		)
	} else if h.st == Audio {
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
		"-segment_time", "10",
		"-segment_list_type", "hls",
		"-segment_list", h.GetPlaylistPath(),
		"-muxdelay", "0",
		"-segment_format", h.GetSegmentFormat(),
	}

	params = append(params, h.GetCodecParams()...)
	params = append(params, fmt.Sprintf("%v/%v%v-%%d.%v", h.out, h.st, h.index, h.GetSegmentExtension()))

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

func NewHLSStream(index int, st StreamType, out string, s *cp.Stream) *HLSStream {
	return &HLSStream{
		index: index,
		st:    st,
		out:   out,
		s:     s,
	}
}

func NewHLS(in string, out string, probe *cp.ProbeReply) *HLS {
	h := &HLS{
		in:    in,
		out:   out,
		video: []*HLSStream{},
		audio: []*HLSStream{},
		subs:  []*HLSStream{},
	}
	vi := 0
	ai := 0
	si := 0
	for _, s := range probe.GetStreams() {
		if s.GetCodecType() == "video" && s.GetCodecName() != "mjpeg" && s.GetCodecName() != "png" {
			h.video = append(h.video, NewHLSStream(vi, Video, out, s))
			vi++
		} else if s.GetCodecType() == "audio" {
			h.audio = append(h.audio, NewHLSStream(ai, Audio, out, s))
			ai++
		} else if s.GetCodecType() == "subtitle" {
			h.subs = append(h.subs, NewHLSStream(si, Subtitle, out, s))
			si++
		}
	}
	if len(h.video) > 0 {
		h.primary = h.video[0]
	} else if len(h.audio) > 0 {
		h.primary = h.audio[0]
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
	res.WriteString(`#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1,CODECS="avc1.42e00a,mp4a.40.2"`)
	if len(s.audio) > 0 {
		res.WriteString(`,AUDIO="audio"`)
	}
	if len(s.subs) > 0 {
		res.WriteString(`,SUBTITLES="subtitles"`)
	}
	res.WriteRune('\n')
	res.WriteString(s.primary.GetPlaylistName())
	return ioutil.WriteFile(s.out+"/index.m3u8", []byte(res.String()), 0644)
}
