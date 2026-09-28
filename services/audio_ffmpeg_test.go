package services

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestAudio_RealFFmpeg plays sessions that declare audio through the real
// handlers with the real FFmpeg and ffprobe (content-prober's local probe,
// the source probe, the real-start probes, the runs) against the sources of
// e2e/audio/gen.sh in AUDIO_MEDIA, served over HTTP, and checks what comes
// out with ffprobe: codec, channels and layout of the first audio
// rendition, its rate, the sample entry of an fMP4 init, the master's
// CODECS and CHANNELS, and after a seek to 35 (a run at 30) the cut on the
// outputs that need one, the audio's first timestamp and its length
// against the video's. Run it in the production image (e2e/audio/run.sh
// gotest):
//
//	GOOS=linux GOARCH=<docker's> CGO_ENABLED=0 go test -c -o ct.test ./services
//	docker run --rm -v $PWD/work:/w -e AUDIO_MEDIA=/w/media --entrypoint /w/ct.test \
//	  ghcr.io/webtor-io/content-transcoder:sha-079acfd -test.run TestAudio_RealFFmpeg -test.v
func TestAudio_RealFFmpeg(t *testing.T) {
	media := os.Getenv("AUDIO_MEDIA")
	if media == "" {
		t.Skip("AUDIO_MEDIA not set")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer srv.Close()
	const all = "hevc8,aac51,ac3,ec3"
	aac51 := audioWant{codec: "aac", channels: 6, layout: "5.1", entry: "mp4a", codecs: "mp4a.40.2", attr: "6"}
	stereo := audioWant{codec: "aac", channels: 2, layout: "stereo", entry: "mp4a", codecs: "mp4a.40.2"}
	for _, c := range []audioRealCase{
		// Passthrough (fMP4 audio), everything declared: E-AC-3 and AC-3
		// copied as they are, AAC 5.1 copied, the rest AAC 5.1.
		{file: "av_eac3_51.mkv", decode: all, route: videoRoutePassthrough, cut: true,
			want: audioWant{copy: true, codec: "eac3", channels: 6, layout: "5.1(side)", entry: "ec-3", codecs: "ec-3", attr: "6"}},
		{file: "av_ac3_51.mkv", decode: all, route: videoRoutePassthrough, cut: true,
			want: audioWant{copy: true, codec: "ac3", channels: 6, layout: "5.1(side)", entry: "ac-3", codecs: "ac-3", attr: "6"}},
		{file: "av_aac_51.mkv", decode: all, route: videoRoutePassthrough, cut: true,
			want: audioWant{copy: true, codec: "aac", channels: 6, layout: "5.1", entry: "mp4a", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_dts_51.mkv", decode: all, route: videoRoutePassthrough, cut: true, want: aac51},
		{file: "av_truehd_51.mkv", decode: all, route: videoRoutePassthrough, cut: true, want: aac51},
		{file: "av_flac_71.mkv", decode: all, route: videoRoutePassthrough, cut: true, want: aac51},
		{file: "av_aac_71.mkv", decode: all, route: videoRoutePassthrough, cut: true, want: aac51},
		{file: "av_aac_20.mkv", decode: all, route: videoRoutePassthrough, cut: true, noChange: true,
			want: audioWant{copy: true, codec: "aac", channels: 2, layout: "stereo", entry: "mp4a", codecs: "mp4a.40.2"}},
		// Passthrough without the Dolby tokens: 5.1 with aac51, stereo
		// without (as before).
		{file: "av_eac3_51.mkv", decode: "hevc8,aac51", route: videoRoutePassthrough, cut: true, want: aac51},
		{file: "av_ac3_51.mkv", decode: "hevc8,ec3", route: videoRoutePassthrough, cut: true, want: stereo, noChange: true},
		// The re-encode route (MPEG-TS audio): the audio-only declaration
		// takes the old route (no_hevc_declared), E-AC-3 is never copied into TS,
		// a copied AAC 5.1 is cut at the seek.
		{file: "av_eac3_51.mkv", decode: "aac51,ac3,ec3", route: videoRouteReencode, want: audioWant{codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_aac_51.mkv", decode: "aac51", route: videoRouteReencode, cut: true,
			want: audioWant{copy: true, codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_dts_51.mkv", decode: "aac51", route: videoRouteReencode, want: audioWant{codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_truehd_51.mkv", decode: "aac51", route: videoRouteReencode, want: audioWant{codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_flac_71.mkv", decode: "aac51", route: videoRouteReencode, want: audioWant{codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
		{file: "av_eac3_51.mkv", decode: "ec3", route: videoRouteReencode, want: audioWant{codec: "aac", channels: 2, layout: "stereo", codecs: "mp4a.40.2"}, noChange: true},
		// The copy route: AAC 5.1 copied into TS (not cut: the copy route
		// cuts subtitles only).
		{file: "av_h264_aac_51.mkv", decode: "aac51", route: videoRouteCopy,
			want: audioWant{copy: true, codec: "aac", channels: 6, layout: "5.1", codecs: "mp4a.40.2", attr: "6"}},
	} {
		t.Run(c.file+"/"+c.decode, func(t *testing.T) { realAudioSession(t, srv.URL, c) })
	}
}

type audioWant struct {
	copy     bool
	codec    string // ffprobe's codec_name of the output
	channels int
	layout   string
	entry    string // fMP4 sample entry; "" for TS
	codecs   string // the audio part of the master's CODECS
	attr     string // CHANNELS of the rendition, "" for none
}

type audioRealCase struct {
	file, decode, route string
	// cut: after a seek the rendition's output is cut at the run's zero
	// (-ss 0 before its -map).
	cut bool
	// noChange: the declaration changes no audio output (the old key).
	noChange bool
	want     audioWant
}

var (
	mediaChannels = regexp.MustCompile(`CHANNELS="([^"]+)"`)
	streamCodecs  = regexp.MustCompile(`#EXT-X-STREAM-INF:[^\n]*CODECS="([^"]+)"`)
)

// audioTrack fetches rendition a0 of the session as a player would (its
// init, if any, and every segment) into one file, and returns it, the
// playlist and the segments' total duration.
func audioTrack(t *testing.T, web *Web, sess *Session, dir, name string) (string, string, float64, int64) {
	t.Helper()
	pl := realGet(t, web, sess, "a0.m3u8").Body.String()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if m := mapURI.FindStringSubmatch(pl); m != nil {
		f.Write(realGet(t, web, sess, m[1]).Body.Bytes())
	}
	var bytes int64
	for _, line := range strings.Split(pl, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b := realGet(t, web, sess, strings.SplitN(line, "?", 2)[0]).Body.Bytes()
		bytes += int64(len(b))
		f.Write(b)
	}
	_, total := parseMediaPlaylist([]byte(pl))
	return f.Name(), pl, total, bytes
}

type probedStream struct {
	CodecName     string `json:"codec_name"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	StartTime     string `json:"start_time"`
}

func ffprobeAudio(t *testing.T, file string) probedStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries",
		"stream=codec_name,channels,channel_layout,start_time", "-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var r struct{ Streams []probedStream }
	if json.Unmarshal(out, &r) != nil || len(r.Streams) != 1 {
		t.Fatalf("ffprobe %s: %s", file, out)
	}
	return r.Streams[0]
}

// firstAudioPTS is the first audio packet's pts_time as the file has it
// (fMP4 with its edit list ignored, as hls.js does).
func firstAudioPTS(t *testing.T, file string, mp4 bool) float64 {
	t.Helper()
	args := []string{"-v", "error"}
	if mp4 {
		args = append(args, "-ignore_editlist", "1")
	}
	args = append(args, "-select_streams", "a:0", "-show_entries", "packet=pts_time", "-read_intervals", "%+#1", "-of", "csv=p=0", file)
	out, err := exec.Command("ffprobe", args...).Output()
	v, perr := strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(out)), ","), 64)
	if err != nil || perr != nil {
		t.Fatalf("first packet of %s: %v %q", file, err, out)
	}
	return v
}

// sampleEntry is the fourcc of the first sample entry of an fMP4 init and
// the types of the boxes inside it.
func sampleEntry(t *testing.T, file string) (string, []string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"moov", "trak", "mdia", "minf", "stbl", "stsd"} {
		var ok bool
		if b, ok = findBox(b, want); !ok {
			t.Fatalf("init without %s", want)
		}
	}
	size, typ, hdr, ok := boxHeader(b[8:])
	if !ok {
		t.Fatal("no sample entry")
	}
	// An AudioSampleEntry has 28 bytes of fields before its boxes.
	var inner []string
	for rest := b[8+hdr+28 : 8+size]; len(rest) >= 8; {
		s, ty, _, ok := boxHeader(rest)
		if !ok {
			break
		}
		inner = append(inner, ty)
		rest = rest[s:]
	}
	return typ, inner
}

// outputOf is the part of an FFmpeg command line that is the output of
// input stream m: from its -map to the next.
func outputOf(args, m string) string {
	i := strings.Index(args, " -map "+m+" ")
	if i < 0 {
		return ""
	}
	rest := args[i+1:]
	if j := strings.Index(rest[5:], " -map "); j >= 0 {
		return rest[:j+5]
	}
	return rest
}

func realAudioSession(t *testing.T, base string, c audioRealCase) {
	web, sm := realWeb(t)
	r := httptest.NewRequest(http.MethodPost, "/session?decode="+c.decode, nil)
	r.Header.Set("X-Source-Url", base+"/"+c.file)
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, r)
	var resp sessionCreateResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	if resp.VideoRoute != c.route {
		t.Fatalf("route %s/%s, want %s", resp.VideoRoute, resp.RouteReason, c.route)
	}
	sess := sm.Get(resp.ID)
	if got := sess.h.audioVariant() == ""; got != c.noChange {
		t.Errorf("audio variant %q", sess.h.audioVariant())
	}
	master := realGet(t, web, sess, "index.m3u8").Body.String()
	codecs := streamCodecs.FindStringSubmatch(master)
	if codecs == nil || !strings.HasSuffix(codecs[1], ","+c.want.codecs) {
		t.Errorf("CODECS %v, want the audio %s:\n%s", codecs, c.want.codecs, master)
	}
	attr := ""
	if m := mediaChannels.FindStringSubmatch(master); m != nil {
		attr = m[1]
	}
	if attr != c.want.attr {
		t.Errorf("CHANNELS %q, want %q", attr, c.want.attr)
	}
	waitCompleted(t, sess)
	dir := t.TempDir()
	mp4 := c.route == videoRoutePassthrough
	ext := "ts"
	if mp4 {
		ext = "mp4"
	}
	file, pl, total, bytes := audioTrack(t, web, sess, dir, "start."+ext)
	p := ffprobeAudio(t, file)
	if p.CodecName != c.want.codec || p.Channels != c.want.channels || p.ChannelLayout != c.want.layout {
		t.Errorf("output %s %d %s, want %s %d %s", p.CodecName, p.Channels, p.ChannelLayout, c.want.codec, c.want.channels, c.want.layout)
	}
	entry, inner := "", []string(nil)
	if mp4 {
		m := mapURI.FindStringSubmatch(pl)
		if m == nil {
			t.Fatalf("fMP4 audio without a MAP:\n%s", pl)
		}
		init := filepath.Join(dir, "init.mp4")
		os.WriteFile(init, realGet(t, web, sess, m[1]).Body.Bytes(), 0644)
		entry, inner = sampleEntry(t, init)
		if entry != c.want.entry {
			t.Errorf("sample entry %s %v, want %s", entry, inner, c.want.entry)
		}
	}
	rate := float64(bytes*8) / total / 1000
	args := strings.Join(sess.currentRun().cmd.Args, " ")
	copied := strings.Contains(outputOf(args, "0:1"), " -c:a copy ")
	if copied != c.want.copy {
		t.Errorf("copied %v, want %v: %s", copied, c.want.copy, args)
	}

	// Seek to 35: a run at 30.
	sw := httptest.NewRecorder()
	web.handler.ServeHTTP(sw, httptest.NewRequest(http.MethodPost, "/session/"+sess.id+"/seek?t=35", nil))
	if sw.Code != 200 {
		t.Fatalf("seek: %d %s", sw.Code, sw.Body.String())
	}
	sargs := strings.Join(sess.currentRun().cmd.Args, " ")
	if cut := strings.Contains(sargs, " -ss 0 -map 0:1 "); cut != c.cut {
		t.Errorf("after the seek the audio cut is %v, want %v: %s", cut, c.cut, sargs)
	}
	realGet(t, web, sess, "a0.m3u8")
	waitCompleted(t, sess)
	sfile, _, stotal, _ := audioTrack(t, web, sess, dir, "seek."+ext)
	vpl := realGet(t, web, sess, sess.h.primary[0].GetPlaylistName()).Body.String()
	_, vtotal := parseMediaPlaylist([]byte(vpl))
	first := firstAudioPTS(t, sfile, mp4)
	sp := ffprobeAudio(t, sfile)
	if sp.CodecName != c.want.codec || sp.Channels != c.want.channels {
		t.Errorf("after the seek: %s %d", sp.CodecName, sp.Channels)
	}
	if math.Abs(stotal-vtotal) > 0.3 {
		t.Errorf("after the seek the audio playlist has %.3f s, the video %.3f s", stotal, vtotal)
	}
	t.Log(fmt.Sprintf("RESULT %s decode=%s route=%s: %s %dch %s %.0f kb/s entry=%s%v CODECS=%s CHANNELS=%q; seek: offset=%s audio %.3f s video %.3f s, first audio pts %.3f",
		c.file, c.decode, c.route, p.CodecName, p.Channels, p.ChannelLayout, rate, entry, inner, codecs[1], attr,
		strings.TrimSpace(strings.SplitN(strings.SplitN(realGet(t, web, sess, "a0.m3u8").Body.String(), "#EXT-X-SESSION-OFFSET:", 2)[1], "\n", 2)[0]), stotal, vtotal, first))
}
