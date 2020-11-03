package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	cp "github.com/webtor-io/content-prober/content-prober"
	"gopkg.in/fsnotify.v1"
)

type HLSTranscoder struct {
	probe              *cp.ProbeReply
	in                 string
	out                string
	options            *Options
	cmd                *exec.Cmd
	state              TranscodeState
	mux                sync.Mutex
	gt                 *time.Timer
	name               string
	start              int
	videoStrm          *cp.Stream
	audioStrm          *cp.Stream
	subtitleStrm       *cp.Stream
	fragmentsProcessed int
	shouldSuspend      bool
	lazy               bool
}

type TranscodeState string

const (
	NotStarted TranscodeState = "NotStarted"
	Running    TranscodeState = "Running"
	Suspended  TranscodeState = "Suspended"
	Killed     TranscodeState = "Killed"
	Finished   TranscodeState = "Finished"
	Failed     TranscodeState = "Failed"
)

func NewHLSTranscoder(probe *cp.ProbeReply, in string, out string, opts *Options, start int) *HLSTranscoder {
	return &HLSTranscoder{
		probe:              probe,
		in:                 in,
		out:                out,
		options:            opts,
		state:              NotStarted,
		name:               "index",
		start:              start,
		fragmentsProcessed: 0,
		shouldSuspend:      false,
	}
}

func (h *HLSTranscoder) Suspend() error {
	h.mux.Lock()
	defer h.mux.Unlock()
	if h.state == Suspended {
		return nil
	}
	if h.state != Running {
		return fmt.Errorf("Failed to suspend transcoding state=%v", h.state)
	}
	h.cmd.Process.Signal(syscall.SIGSTOP)
	log.Info("Transcoding suspended")
	h.state = Suspended
	return nil
}
func (h *HLSTranscoder) Kill() error {
	h.mux.Lock()
	defer h.mux.Unlock()
	if !h.IsAlive() {
		return fmt.Errorf("Failed to kill transcoding state=%v", h.state)
	}
	h.state = Killed
	h.cmd.Process.Signal(syscall.SIGKILL)
	return nil
}
func (h *HLSTranscoder) IsAlive() bool {
	return h.state == Running || h.state == Suspended
}
func (h *HLSTranscoder) Ping() {
	if h.state == Running {
		h.shouldSuspend = false
		h.gt.Reset(h.options.grace)
	}
	if h.state == Suspended {
		h.cmd.Process.Signal(syscall.SIGCONT)
		log.Info("Transcoding continued")
		h.state = Running
		h.gt.Reset(h.options.grace)
	}
}
func (h *HLSTranscoder) Index() string {
	return fmt.Sprintf("%s/%s.%s", h.out, h.name, "m3u8")
}
func (h *HLSTranscoder) VTTIndex() string {
	return fmt.Sprintf("%s/%s_vtt.%s", h.out, h.name, "m3u8")
}
func (h *HLSTranscoder) watchUpdates(ctx context.Context) (chan error, chan *HLSPlaylist, error) {
	ch := make(chan *HLSPlaylist)
	done := make(chan error)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to start playlist watcher")
	}
	go func() {
		defer watcher.Close()
		// defer close(ch)
		var pl *HLSPlaylist
		var vttPl *HLSPlaylist
		for {
			select {
			case <-ctx.Done():
				done <- errors.Wrap(ctx.Err(), "Context done")
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == h.Index() {
					pl, err := NewHLSPlaylistFromTranscoder(h, false)
					if err != nil {
						done <- errors.Wrap(err, "Failed to load playlist")
						return
					}
					ch <- pl
				}
				if event.Name == h.VTTIndex() {
					vttPl, err := NewHLSPlaylistFromTranscoder(h, true)
					if err != nil {
						done <- errors.Wrap(err, "Failed to load vtt playlist")
						return
					}
					ch <- vttPl
				}
				if pl != nil && pl.Ended && (h.subtitleStrm == nil || (vttPl != nil && vttPl.Ended)) {
					// log.Info("Playlist ended")
					done <- nil
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if err != nil {
					done <- errors.Wrap(err, "Got watcher error")
					return
				}
			}
		}
	}()

	err = watcher.Add(h.out)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed add playlist to watcher")
	}
	return done, ch, nil
}
func (h *HLSTranscoder) Run(ctx context.Context) (chan error, chan *HLSPlaylist, error) {
	h.mux.Lock()
	defer h.mux.Unlock()
	if h.state != NotStarted {
		return nil, nil, fmt.Errorf("Failed to start transcoding state=%v", h.state)
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to find ffmpeg")
	}

	err = os.MkdirAll(h.out, os.ModePerm)

	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to make work directory")
	}

	ffmpegParams, videoStrm, audioStrm, subtitleStrm := genFFmpegParams(h.in, h.Index(), h.options, h.probe, h.start)
	h.videoStrm = videoStrm
	h.audioStrm = audioStrm
	h.subtitleStrm = subtitleStrm

	log.WithField("ffmpegParams", ffmpegParams).Info("Got ffmpeg params")

	h.cmd = exec.Command(ffmpeg, ffmpegParams...)

	h.cmd.Stdout = os.Stdout
	h.cmd.Stderr = os.Stderr

	err = h.cmd.Start()
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to start ffmpeg")
	}
	done := make(chan error)
	log.Info("Transcoding have just started")
	go func() {
		done <- h.cmd.Wait()
	}()
	h.gt = time.NewTimer(h.options.grace)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.gt.C:
				if h.options.lazyTranscoding {
					h.shouldSuspend = true
				}
			}
		}
	}()
	resPlCh := make(chan *HLSPlaylist)
	plDone, plCh, err := h.watchUpdates(ctx)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to watch updates")
	}
	res := make(chan error)
	go func() {
		for {
			select {
			case pl := <-plCh:
				resPlCh <- pl
				if h.fragmentsProcessed < len(pl.Fragments) {
					h.fragmentsProcessed = len(pl.Fragments)
					if h.shouldSuspend {
						h.Suspend()
					}
				}
			case err := <-plDone:
				if err != nil {
					res <- errors.Wrap(err, "Watch updates done")
					return
				}
			}
		}
	}()
	go func() {
		defer h.gt.Stop()
		for {
			select {
			case <-ctx.Done():
				h.Kill()
				res <- errors.Wrap(ctx.Err(), "Context done")
				return
			case err := <-done:
				if err != nil && h.state != Killed {
					h.state = Failed
					res <- errors.Wrap(err, "Transcoding failed")
				}
				log.Info("Transcoding finished")
				h.state = Finished
				res <- nil
				return
			}
		}
	}()
	h.state = Running
	return res, resPlCh, nil
}

func genFFmpegParams(in string, out string, options *Options, pr *cp.ProbeReply, start int) ([]string, *cp.Stream, *cp.Stream, *cp.Stream) {
	var params []string

	if start != 0 {
		params = append(
			params,
			"-ss", fmt.Sprintf("%d", options.duration*start),
		)
	}

	params = append(
		params,
		// "-y",
		"-i", in,
		"-hls_playlist_type", "event",
		// "-sn",
		"-preset", options.preset,
		"-tune", "zerolatency",
		"-copyts", "-muxdelay", "0",
		// "-maxrate", "2000000",
		// "-bufsize", "1500000",
		// "-crf", "23",
		// "-hls_segment_type", "fmp4",
		"-hls_time", fmt.Sprintf("%d", options.duration),
		"-start_number", fmt.Sprintf("%d", start),
		"-err_detect", "ignore_err",
		"-reconnect_at_eof", "1",
		"-reconnect_streamed", "1",
		"-seekable", "1",
		// "-hls_flags", "round_durations",
	)

	var audioStrm *cp.Stream
	var videoStrm *cp.Stream
	var subtitleStrm *cp.Stream

	for _, strm := range pr.GetStreams() {
		if strm.GetCodecType() == "video" && strm.GetCodecName() != "mjpeg" && strm.GetCodecName() != "png" && videoStrm == nil {
			videoStrm = strm
		}
		if strm.GetCodecType() == "audio" && audioStrm == nil {
			audioStrm = strm
		}
		if strm.GetCodecType() == "subtitle" && subtitleStrm == nil {
			subtitleStrm = strm
		}
		if audioStrm != nil && videoStrm != nil && subtitleStrm != nil {
			break
		}
	}
	if options.audioChNum != 0 {
		for _, strm := range pr.GetStreams() {
			if strm.GetIndex() == int32(options.audioChNum) && strm.GetCodecType() == "audio" {
				audioStrm = strm
			}
		}
	}
	if options.subChNum != 0 {
		for _, strm := range pr.GetStreams() {
			if strm.GetIndex() == int32(options.subChNum) && strm.GetCodecType() == "subtitle" {
				subtitleStrm = strm
			}
		}
	}
	if videoStrm != nil {
		params = append(params, "-vcodec")
		// bitrate, err := strconv.Atoi(pr.GetFormat().GetBitRate())
		if options.forceTranscode || videoStrm.GetCodecName() != options.videoCodec {
			// if options.forceTranscode || videoStrm.GetCodecName() != options.videoCodec || err == nil && bitrate > 2_000_000 {
			params = append(params, options.videoCodec)
			// https://gist.github.com/kuntau/a7cbe28df82380fd3467#gistcomment-2388045
			// params = append(params, "-crf", "27", "-x264-params", "cabac=1:ref=5:analyse=0x133:me=umh:subme=9:chroma-me=1:deadzone-inter=21:deadzone-intra=11:b-adapt=2:rc-lookahead=60:vbv-maxrate=10000:vbv-bufsize=10000:qpmax=69:bframes=5:b-adapt=2:direct=auto:crf-max=51:weightp=2:merange=24:chroma-qp-offset=-1:sync-lookahead=2:psy-rd=1.00,0.15:trellis=2:min-keyint=23:partitions=all")
			params = append(params, "-b:v", "2M")
			params = append(params, "-maxrate", "2M")
			params = append(params, "-bufsize", "1M")
			// params = append(params, "-x264-params", "keyint=240:scenecut=0")
			// params = append(params, "-r", "24")
		} else {
			params = append(params, "copy")
		}
		params = append(params, "-map")
		params = append(params, fmt.Sprintf("0:%v", videoStrm.GetIndex()))
	}
	if audioStrm != nil {
		params = append(params, "-acodec")
		if options.forceTranscode || audioStrm.GetCodecName() != options.audioCodec {
			params = append(params, "libfdk_aac")
			params = append(params, "-ac")
			params = append(params, "2")
			// params = append(params, "-ar", "44100")
			// params = append(params, "-b:a", "128k")
		} else {
			params = append(params, "copy")
		}
		params = append(params, "-map")
		params = append(params, fmt.Sprintf("0:%v", audioStrm.GetIndex()))
	}
	if subtitleStrm != nil {
		params = append(params, "-scodec")
		if options.forceTranscode || videoStrm.GetCodecName() != options.subtitleCodec {
			params = append(params, options.subtitleCodec)
		} else {
			params = append(params, "copy")
		}
		params = append(params, "-map")
		params = append(params, fmt.Sprintf("0:%v", subtitleStrm.GetIndex()))
	}

	params = append(
		params,
		out,
	)
	return params, videoStrm, audioStrm, subtitleStrm
}
