package services

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pkg/errors"
	cp "github.com/webtor-io/content-prober/content-prober"
)

func testStream(index int32, codecType, codec string) *cp.Stream {
	s := &cp.Stream{Index: index, CodecType: codecType, CodecName: codec}
	if codecType == "video" {
		s.Height = 1080
	}
	if codecType == "audio" {
		s.Channels = 2
	}
	return s
}

func testHLS(streams ...*cp.Stream) *HLS {
	return NewHLS("http://source/file.mkv", &cp.ProbeReply{Streams: streams}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
}

// mapTargets returns the value of every -map in FFmpeg's argument list.
func mapTargets(params []string) []string {
	var maps []string
	for i := 0; i+1 < len(params); i++ {
		if params[i] == "-map" {
			maps = append(maps, params[i+1])
		}
	}
	return maps
}

func ffmpegParams(t *testing.T, h *HLS) []string {
	t.Helper()
	params, err := h.GetFFmpegParams("/out")
	if err != nil {
		t.Fatal(err)
	}
	return params
}

// A PGS track before a text track shifted every later subtitle onto the
// wrong input stream: NewHLS numbered the kept subtitles without the PGS,
// but FFmpeg's 0:s:N counts every subtitle stream. [subrip, pgs, subrip]
// mapped 0:s:1 -- the PGS -- into the webvtt encoder, and FFmpeg refused
// the whole run ("Subtitle encoding currently only possible from text to
// text or bitmap to bitmap"). 140 of 686 failed runs in 50h were this
// shape; the source never played.
func TestGetFFmpegParamsMapsSubtitlesByStreamIndex(t *testing.T) {
	h := testHLS(
		testStream(0, "video", "h264"),
		testStream(1, "audio", "aac"),
		testStream(2, "subtitle", "subrip"),
		testStream(3, "subtitle", "hdmv_pgs_subtitle"),
		testStream(4, "subtitle", "subrip"),
	)
	got := mapTargets(ffmpegParams(t, h))
	want := []string{"0:0", "0:1", "0:2", "0:4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("-map targets = %v, want %v (stream 3 is the PGS)", got, want)
	}
}

// Only subtitles FFmpeg can decode as text go to the webvtt encoder.
// dvd_subtitle and dvb_subtitle are bitmaps (same refusal as PGS), and a
// stream the prober could not name has no decoder at all ("Decoding
// requested, but no decoder found") -- either one kills the run with the
// video and audio in it. 129 of 132 sessions with a dvd/dvb track failed.
//
// They keep their place in the master playlist: web-ui numbers subtitle
// tracks the same way (every non-PGS stream takes an s<N> slot, see
// embeddedSubtitleVisible in web-ui handlers/action/helper.go) and uses N
// as the hls.js track index. Dropping the entry would shift every later
// text track onto its neighbour's slot.
func TestGetFFmpegParamsSkipsSubtitlesWithoutTextDecoder(t *testing.T) {
	h := testHLS(
		testStream(0, "video", "h264"),
		testStream(1, "audio", "aac"),
		testStream(2, "subtitle", "dvd_subtitle"), // s0
		testStream(3, "subtitle", "subrip"),       // s1
		testStream(4, "subtitle", ""),             // s2
		testStream(5, "subtitle", "dvb_subtitle"), // s3
		testStream(6, "subtitle", "ass"),          // s4
	)
	params := ffmpegParams(t, h)
	if got, want := mapTargets(params), []string{"0:0", "0:1", "0:3", "0:6"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-map targets = %v, want %v", got, want)
	}
	joined := strings.Join(params, " ")
	for _, name := range []string{"/out/s1.m3u8", "/out/s4.m3u8"} {
		if !strings.Contains(joined, name) {
			t.Errorf("text subtitle output %s missing from params", name)
		}
	}
	for _, name := range []string{"/out/s0.m3u8", "/out/s2.m3u8", "/out/s3.m3u8"} {
		if strings.Contains(joined, name) {
			t.Errorf("undecodable subtitle output %s must not be in params", name)
		}
	}

	dir := t.TempDir()
	if err := h.MakeMasterPlaylist(dir); err != nil {
		t.Fatal(err)
	}
	master, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s0.m3u8", "s1.m3u8", "s2.m3u8", "s3.m3u8", "s4.m3u8"} {
		if !strings.Contains(string(master), `URI="`+name+`"`) {
			t.Errorf("master playlist lost the slot %s:\n%s", name, master)
		}
	}
}

// Negative control for the allowlist: every text codec the production
// build decodes (ffmpeg 8.1.2 -decoders) is still converted.
func TestGetFFmpegParamsKeepsTextSubtitles(t *testing.T) {
	for _, codec := range []string{"subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text", "microdvd", "subviewer", "subviewer1", "sami", "realtext", "mpl2", "pjs", "jacosub", "vplayer", "stl", "eia_608"} {
		h := testHLS(testStream(0, "video", "h264"), testStream(1, "subtitle", codec))
		if got := mapTargets(ffmpegParams(t, h)); len(got) != 2 || got[1] != "0:1" {
			t.Errorf("%s: -map targets = %v, want the subtitle mapped", codec, got)
		}
	}
}

// Cover art is a video stream too. NewHLS skips mjpeg/png, but 0:v:0 still
// counted it, so an mp4 with the cover first mapped the picture as the
// movie.
func TestGetFFmpegParamsSkipsCoverArtBeforeVideo(t *testing.T) {
	h := testHLS(
		testStream(0, "video", "mjpeg"),
		testStream(1, "video", "h264"),
		testStream(2, "audio", "aac"),
	)
	if got, want := mapTargets(ffmpegParams(t, h)), []string{"0:1", "0:2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-map targets = %v, want %v", got, want)
	}
	if got := h.primaryVideoStreamSpecifier(); got != "1" {
		t.Errorf("real-start probe stream = %q, want the mapped video \"1\"", got)
	}
}

// A probe with neither video nor audio used to panic on h.primary[0]
// (seen on POST /session, 2026-09-18). It is content the transcoder can
// never serve, so it is a 415 like the other content-level rejections.
func TestGetFFmpegParamsNoPlayableStreams(t *testing.T) {
	h := testHLS(testStream(0, "subtitle", "subrip"), testStream(1, "attachment", "ttf"))
	_, err := h.GetFFmpegParams("/out")
	if !errors.Is(err, ErrNoPlayableStreams) {
		t.Fatalf("err = %v, want ErrNoPlayableStreams", err)
	}
	if unsupportedContentReason(err) == "" {
		t.Error("no playable streams must surface as a 415 reason")
	}
}
