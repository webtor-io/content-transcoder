package services

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type Transcoder struct {
	cmd          *exec.Cmd
	h            *HLSParser
	ch           chan error
	finished     bool
	toCompletion bool
}

func NewTranscoder(c *cli.Context, h *HLSParser) *Transcoder {
	return &Transcoder{
		h:            h,
		ch:           make(chan error),
		toCompletion: c.Bool(ToCompletionFlag),
	}
}

func (s *Transcoder) Close() error {
	if s.cmd != nil {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
	close(s.ch)
	return nil
}
func (s *Transcoder) IsFinished() bool {
	return s.finished
}
func (s *Transcoder) Serve() (err error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.Wrap(err, "Failed to find ffmpeg")
	}
	hls, err := s.h.Get()
	if err != nil {
		return errors.Wrap(err, "Failed to get hls")
	}

	params, err := hls.GetFFmpegParams()
	if err != nil {
		return errors.Wrap(err, "Failed to get ffmpeg params")
	}

	err = hls.MakeMasterPlaylist()
	if err != nil {
		return errors.Wrap(err, "Failed to make master playlist")
	}

	log.Infof("Got ffmpeg params %-v", params)

	s.cmd = exec.Command(ffmpeg, params...)

	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	err = s.cmd.Start()
	if err != nil {
		return errors.Wrap(err, "Failed to start ffmpeg")
	}
	log.Info("Starting Transcoder")
	err = s.cmd.Wait()
	if err != nil {
		return errors.Wrap(err, "Failed to transcode")
	}
	log.Info("Transcoding finished")
	s.finished = true
	if s.toCompletion {
		return nil
	}
	<-s.ch
	return nil
}
