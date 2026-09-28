package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	logtest "github.com/sirupsen/logrus/hooks/test"
	cp "github.com/webtor-io/content-prober/content-prober"
)

// What FFmpeg 8.1.2 printed when movenc refused a copied E-AC-3 packet
// (several independent substreams; a broken frame at 8.5 s): the output
// stream's name, the error, and the rest of the way out.
const copiedEC3MuxFailureTail = `[aost#1:0/copy @ 0xffff813b38d0] Error submitting a packet to the muxer: Invalid data found when processing input
Last message repeated 1 times
[out#1/hls @ 0xffff813e4e60] Error muxing a packet
[out#1/hls @ 0xffff813e4e60] Task finished with error code: -1094995529 (Invalid data found when processing input)
[out#1/hls @ 0xffff813e4e60] Terminating thread with return code -1094995529 (Invalid data found when processing input)
Conversion failed!`

func TestCopiedAudioMuxFailure(t *testing.T) {
	for _, c := range []struct {
		tail string
		want bool
	}{
		{copiedEC3MuxFailureTail, true},
		{"[aost#3:0/copy @ 0x1] Error submitting a packet to the muxer: Invalid argument", true},
		// Not a copied audio output: an encoder's, a video copy's, a
		// subtitle's.
		{"[aost#1:0/libfdk_aac @ 0x1] Error submitting a packet to the muxer: Invalid argument", false},
		{"[vost#0:0/copy @ 0x1] Error submitting a packet to the muxer: Invalid data found when processing input", false},
		{"[sost#2:0/webvtt @ 0x1] Error submitting a packet to the muxer: Invalid argument", false},
		// Other failures of a copied audio output, and the ADTS one (its
		// own matcher).
		{"[aost#1:0/copy @ 0x1] Non-monotonic DTS; previous: 5, current: 4", false},
		{"[adts @ 0x1] Scalable configurations are not allowed in ADTS\n[out#0/segment @ 0x1] Could not write header (incorrect codec parameters ?): Invalid data found when processing input", false},
		{"[out#1/hls @ 0x1] Error muxing a packet", false},
	} {
		if got := copiedAudioMuxFailure(c.tail); got != c.want {
			t.Errorf("%q: %v, want %v", c.tail, got, c.want)
		}
	}
}

// ec3Streams are an HEVC video with a stereo AAC (a0), an E-AC-3 5.1 at
// 640 kb/s (a1) and an AAC 5.1 without a rate (a2).
func ec3Streams() []*cp.Stream {
	return []*cp.Stream{
		{Index: 0, CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2, ChannelLayout: "stereo"},
		{Index: 2, CodecType: "audio", CodecName: "eac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: "640000"},
		{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6, ChannelLayout: "5.1"},
	}
}

// ec3Source is an HLS of ec3Streams, on passthrough or the old route.
func ec3Source(t *testing.T, passthrough bool) *HLS {
	t.Helper()
	h := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: ec3Streams()}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
	if passthrough && !h.usePassthrough() {
		t.Fatal("usePassthrough refused")
	}
	return h
}

// A muxer that refuses a copy only the client's declaration made (E-AC-3,
// AC-3, AAC 5.1) fails every restart the same way: the run encodes the
// audio from the next start, without -xerror (the decoder reads the same
// bitstream), and its variant remembers it -- on a seek run too, for the
// variant's runs from 0. A session whose declaration copies nothing new --
// none, or HEVC tokens alone -- fails as it always did: nothing learned,
// nothing remembered.
func TestCopiedAudioMuxFailure_EncodesDeclaredCopies(t *testing.T) {
	for _, c := range []struct {
		name        string
		passthrough bool
		decode      string
		learn       bool
	}{
		{"passthrough, ec3: E-AC-3 copied", true, "hevc8,ec3", true},
		{"passthrough, aac51: AAC 5.1 copied", true, "hevc8,aac51", true},
		{"old route, aac51: AAC 5.1 copied into TS", false, "aac51", true},
		{"passthrough, nothing declared", true, "", false},
		{"passthrough, HEVC tokens only", true, "hevc8,hevc10", false},
		{"old route, nothing declared", false, "", false},
		// ec3 on the old route: E-AC-3 is never copied into TS, the
		// declaration changes no output.
		{"old route, ec3", false, "ec3", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := ec3Source(t, c.passthrough)
			h.useAudioDecoders(decl(c.decode).audioDecoders())
			dir := t.TempDir()
			m := NewRunManager()
			defer m.CloseAll()
			m.mu.Lock()
			r := m.newRunLocked(runKeyFor(dir, h, 30), dir, 30, "http://src/movie.mkv", h)
			m.mu.Unlock()
			if err := os.MkdirAll(r.outputDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte(copiedEC3MuxFailureTail+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			startFakeProcess(t, r, "false")
			<-r.done
			want := ParamOptions{EncodeAudio: c.learn, Lenient: c.learn}
			if got := r.options(); got != want {
				t.Errorf("options after the failure %+v, want %+v", got, want)
			}
			if got := m.rememberedFallbacks(dir, h); got != want {
				t.Errorf("remembered for the variant %+v, want %+v", got, want)
			}
		})
	}
}

// surroundPassthroughSession is a passthrough session of ec3Source whose
// declaration is decode, with a finished run that wrote the FFmpeg 8.1.2
// video init and a first segment of 12.5 MB over 10.01 s.
func surroundPassthroughSession(t *testing.T, decode string) (*Session, *TranscodeRun) {
	t.Helper()
	return passthroughSessionOf(t, ec3Source(t, true), decode)
}

// passthroughSessionOf is surroundPassthroughSession for the passthrough
// HLS h.
func passthroughSessionOf(t *testing.T, h *HLS, decode string) (*Session, *TranscodeRun) {
	t.Helper()
	f := sdrMain10()
	h.passFacts = &f
	h.useAudioDecoders(decl(decode).audioDecoders())
	dir := t.TempDir()
	runMgr := NewRunManager()
	t.Cleanup(runMgr.CloseAll)
	sess := NewSession(SessionConfig{ID: "a", HashDir: dir, HLS: h, RunMgr: runMgr})
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := newCompletedRun(t, dir)
	run.h = h
	sess.run = run
	passthroughRunDir(t, run.OutputDir(), "v0-1080", run.Generation(), fixtureInit(t, "main10-init.mp4"), 12_500_000)
	return sess, run
}

func readMaster(t *testing.T, sess *Session) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sess.outputDir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The passthrough master names the audio the run makes: a copied E-AC-3
// the run learned to encode is AAC in CODECS, CHANNELS and BANDWIDTH --
// built after the fallback, or rewritten on the next read when the run
// learned it after the master went out.
func TestPassthroughMaster_FollowsTheRunsAudio(t *testing.T) {
	const (
		// 12.5 MB over 10.01 s: 9,990,009 bit/s of video.
		copied = "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="a0.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",CHANNELS="6",URI="a1.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #3",CHANNELS="6",URI="a2.m3u8"` + "\n" +
			`#EXT-X-STREAM-INF:BANDWIDTH=10630009,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L63.90,mp4a.40.2,ec-3",VIDEO-RANGE=SDR,AUDIO="audio"` + "\n" +
			"v0-1080.m3u8\n"
		// EncodeAudio with aac51: every track AAC, the E-AC-3 and the AAC
		// 5.1 at 384 kb/s.
		encoded = "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="a0.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #2",CHANNELS="6",URI="a1.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="Track #3",CHANNELS="6",URI="a2.m3u8"` + "\n" +
			`#EXT-X-STREAM-INF:BANDWIDTH=10374009,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L63.90,mp4a.40.2",VIDEO-RANGE=SDR,AUDIO="audio"` + "\n" +
			"v0-1080.m3u8\n"
	)
	// Written before the run learned it, rewritten on the next read.
	sess, run := surroundPassthroughSession(t, "hevc8,aac51,ec3")
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := readMaster(t, sess); got != copied {
		t.Errorf("master\n got %q\nwant %q", got, copied)
	}
	sess.refreshMaster()
	if got := readMaster(t, sess); got != copied {
		t.Errorf("rewritten with nothing changed:\n%s", got)
	}
	run.mu.Lock()
	run.fallbacks.EncodeAudio = true
	run.mu.Unlock()
	sess.refreshMaster()
	if got := readMaster(t, sess); got != encoded {
		t.Errorf("after the fallback\n got %q\nwant %q", got, encoded)
	}

	// Built after the run learned it: AAC from the start.
	sess, run = surroundPassthroughSession(t, "hevc8,aac51,ec3")
	run.fallbacks.EncodeAudio = true
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := readMaster(t, sess); got != encoded {
		t.Errorf("built after the fallback\n got %q\nwant %q", got, encoded)
	}

	// A session declaring ec3 alone on the run a session with aac51
	// started (an E-AC-3-only source: the same arguments without
	// fallbacks, so the same run): the run encodes 5.1, and the master says
	// so -- not the stereo the session's own declaration would give.
	eac3Only := func() *HLS {
		h := NewHLS("http://src/movie.mkv", &cp.ProbeReply{Streams: []*cp.Stream{
			{Index: 0, CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080},
			{Index: 1, CodecType: "audio", CodecName: "eac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: "640000"},
		}}, &HLSConfig{sm: Online, aacCodec: "libfdk_aac"})
		if !h.usePassthrough() {
			t.Fatal("usePassthrough refused")
		}
		return h
	}
	sess, run = passthroughSessionOf(t, eac3Only(), "hevc8,ec3")
	starter := eac3Only()
	starter.passFacts = sess.h.passFacts
	starter.useAudioDecoders(decl("hevc8,aac51,ec3").audioDecoders())
	if runKeyFor("/h", starter, 0) != runKeyFor("/h", sess.h, 0) {
		t.Fatal("the two declarations do not share a run")
	}
	run.h = starter
	run.fallbacks.EncodeAudio = true
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := readMaster(t, sess); !strings.Contains(got, `NAME="Track #1",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="6",`) || !strings.Contains(got, `BANDWIDTH=10374009,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L63.90,mp4a.40.2",`) {
		t.Errorf("on a run another declaration started:\n%s", got)
	}

	// ec3 without aac51 on its own run: the fallback is AAC stereo.
	sess, run = surroundPassthroughSession(t, "hevc8,ec3")
	run.fallbacks.EncodeAudio = true
	if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := readMaster(t, sess); !strings.Contains(got, `NAME="Track #2",CHANNELS="2",`) || !strings.Contains(got, `CODECS="hvc1.2.4.L63.90,mp4a.40.2",`) {
		t.Errorf("ec3 alone after the fallback:\n%s", got)
	}

	// Nothing declared, HEVC tokens only: the master is what it always
	// was, whatever the run's options, and is never touched again.
	hook := logtest.NewGlobal()
	defer hook.Reset()
	for _, d := range []string{"", "hevc8"} {
		sess, run = surroundPassthroughSession(t, d)
		if err := sess.writePassthroughMaster(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		before := readMaster(t, sess)
		run.mu.Lock()
		run.fallbacks = ParamOptions{EncodeAudio: true, Lenient: true}
		run.mu.Unlock()
		sess.refreshMaster()
		if got := readMaster(t, sess); got != before || strings.Contains(got, "CHANNELS") {
			t.Errorf("decode=%q: master changed with the options:\n%s\n%s", d, before, got)
		}
	}
	if n := rewrites(hook); n != 0 {
		t.Errorf("masters without declared audio rewritten %d times", n)
	}
}

// rewrites counts refreshMaster's rewrites in hook.
func rewrites(hook *logtest.Hook) int {
	n := 0
	for _, e := range hook.AllEntries() {
		if e.Message == "session: master rewritten for the audio the run makes now" {
			n++
		}
	}
	return n
}

// The old route's master of a declared session is written for the options
// its variant's runs start with (what the run manager remembers), and
// follows what its run learns later; without a declaration it is what it
// always was.
func TestOldRouteMaster_FollowsTheRunsAudio(t *testing.T) {
	fakeFFmpeg(t)
	web, sm, out := goldenWeb(t, nil)
	src := goldenSource{"hevc-ec3-aac51", ec3Streams()}
	writeGoldenProbes(t, out, []goldenSource{src})
	open := func(decode string) *Session {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/session?decode="+decode, nil)
		r.Header.Set("X-Source-Url", goldenSourceURL(src.name))
		w := httptest.NewRecorder()
		web.handler.ServeHTTP(w, r)
		var resp sessionCreateResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
			t.Fatalf("POST: %d %s", w.Code, w.Body.String())
		}
		return sm.Get(resp.ID)
	}
	master := func(sess *Session) string {
		t.Helper()
		w := httptest.NewRecorder()
		web.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/session/"+sess.id+"/index.m3u8", nil))
		return w.Body.String()
	}
	// AAC 5.1 copied (its BPS unknown: 640 kb/s), then encoded (384).
	copiedAAC51 := `NAME="Track #3",CHANNELS="6",URI="a2.m3u8"`
	sess := open("aac51")
	if m := master(sess); !strings.Contains(m, copiedAAC51) || !strings.Contains(m, "BANDWIDTH=8640000,") {
		t.Errorf("copied:\n%s", m)
	}
	run := sess.currentRun()
	run.mu.Lock()
	run.fallbacks.EncodeAudio = true
	run.mu.Unlock()
	sess.runMgr.rememberFallbacks(fallbackKey(sess.hashDir, sess.h), ParamOptions{EncodeAudio: true})
	if m := master(sess); !strings.Contains(m, copiedAAC51) || !strings.Contains(m, "BANDWIDTH=8384000,") {
		t.Errorf("after the run learned EncodeAudio:\n%s", m)
	}
	// A new session of the variant: written for EncodeAudio from the start.
	sess2 := open("aac51")
	if b, _ := os.ReadFile(filepath.Join(sess2.outputDir, "index.m3u8")); !strings.Contains(string(b), "BANDWIDTH=8384000,") {
		t.Errorf("a new session of the variant:\n%s", b)
	}
	// Without a declaration: the old master, whatever the source learned,
	// never touched again.
	plain := open("")
	before := master(plain)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	fi, _ := os.Stat(filepath.Join(plain.outputDir, "index.m3u8"))
	sess.runMgr.rememberFallbacks(fallbackKey(plain.hashDir, plain.h), ParamOptions{EncodeAudio: true})
	prun := plain.currentRun()
	prun.mu.Lock()
	prun.fallbacks.EncodeAudio = true
	prun.mu.Unlock()
	if after := master(plain); after != before || strings.Contains(after, "CHANNELS") {
		t.Errorf("no declaration:\n%s\n%s", before, after)
	}
	if fi2, _ := os.Stat(filepath.Join(plain.outputDir, "index.m3u8")); rewrites(hook) != 0 || fi2.Mode() != fi.Mode() || !os.SameFile(fi, fi2) || fi.Mode().Perm() != 0644 {
		t.Errorf("the master without a declaration was rewritten (%d, %v -> %v)", rewrites(hook), fi.Mode(), fi2.Mode())
	}
}

// A process that makes other audio than the one before it (a fallback
// turned the declared E-AC-3 copy into an encode) starts without the old
// process's playlist of that output: served in its place it named the old
// "ec-3" init, which Chrome appended and failed on while the master said
// AAC. The outputs it makes as before keep theirs, and so does every
// output of a session whose declaration changes no audio.
func TestRestart_DropsThePlaylistOfAChangedAudioOutput(t *testing.T) {
	fakeFFmpeg(t)
	for _, c := range []struct {
		decode string
		drop   map[string]bool
	}{
		// a0 AAC stereo copied, a1 E-AC-3 copied, a2 AAC 5.1 copied: all
		// three become encodes.
		{"hevc8,aac51,ec3", map[string]bool{"a0": true, "a1": true, "a2": true}},
		// ec3 alone: a0 copied as always but encoded now, a1 likewise; a2
		// was an encode and stays one.
		{"hevc8,ec3", map[string]bool{"a0": true, "a1": true}},
		{"hevc8", nil},
		{"", nil},
	} {
		h := ec3Source(t, true)
		h.useAudioDecoders(decl(c.decode).audioDecoders())
		standInPassthroughParams(t)
		r := newTranscodeRun("k", t.TempDir(), 0, "http://src/movie.mkv", h)
		t.Cleanup(r.Cleanup)
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{"a0", "a1", "a2", "v0-1080"} {
			os.WriteFile(filepath.Join(r.OutputDir(), a+".m3u8.ffmpeg"), []byte("#EXTM3U\n"), 0644)
		}
		r.Stop()
		r.mu.Lock()
		r.fallbacks = ParamOptions{EncodeAudio: true, Lenient: true}
		r.mu.Unlock()
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{"a0", "a1", "a2", "v0-1080"} {
			if gone := !fileExists(filepath.Join(r.OutputDir(), a+".m3u8.ffmpeg")); gone != c.drop[a] {
				t.Errorf("decode=%q: %s playlist removed %v, want %v", c.decode, a, gone, c.drop[a])
			}
		}
		// Restarted with the same options again: nothing more goes.
		os.WriteFile(filepath.Join(r.OutputDir(), "a1.m3u8.ffmpeg"), []byte("#EXTM3U\n"), 0644)
		r.Stop()
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(r.OutputDir(), "a1.m3u8.ffmpeg")) {
			t.Errorf("decode=%q: a1 removed on a restart that changed nothing", c.decode)
		}
		r.Stop()
	}
}
