package services

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type Transcoder struct {
	cmd *exec.Cmd
	h   *HLS
	out string
}

func NewTranscoder(out string, h *HLS) *Transcoder {
	return &Transcoder{
		out: out,
		h:   h,
	}
}

func (s *Transcoder) Stop() error {
	if s.cmd != nil {
		log.Infof("killing ffmpeg with pid %d", s.cmd.Process.Pid)
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

func (s *Transcoder) isHLSIndexFinished(str *HLSStream) (bool, error) {
	path := filepath.Join(s.out, str.GetPlaylistName())

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func(file *os.File) {
		_ = file.Close()
	}(file)

	allSegmentsExist := true
	foundEndlist := false

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "#EXT-X-ENDLIST" {
			foundEndlist = true
			break
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		segmentPath := filepath.Join(s.out, line)
		if _, err = os.Stat(segmentPath); os.IsNotExist(err) {
			allSegmentsExist = false
			break
		}
	}

	if err = scanner.Err(); err != nil {
		return false, err
	}

	return foundEndlist && allSegmentsExist, nil
}

func (s *Transcoder) isFinished() (bool, error) {
	for _, stream := range s.h.video {
		ok, err := s.isHLSIndexFinished(stream)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for _, stream := range s.h.audio {
		ok, err := s.isHLSIndexFinished(stream)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for _, stream := range s.h.subs {
		ok, err := s.isHLSIndexFinished(stream)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (s *Transcoder) Run() (err error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.Wrap(err, "failed to find ffmpeg")
	}

	params, err := s.h.GetFFmpegParams(s.out)
	if err != nil {
		return errors.Wrap(err, "failed to get ffmpeg params")
	}

	log.Infof("got ffmpeg params %-v", params)

	s.cmd = exec.Command(ffmpeg, params...)

	outLog, err := os.Create(fmt.Sprintf("%v/%v", s.out, "ffmpeg.out"))
	if err != nil {
		return errors.Wrapf(err, "failed create %v", fmt.Sprintf("%v/%v", s.out, "ffmpeg.out"))
	}
	defer func(outLog *os.File) {
		_ = outLog.Close()
	}(outLog)

	errLog, err := os.Create(fmt.Sprintf("%v/%v", s.out, "ffmpeg.err"))
	if err != nil {
		return errors.Wrapf(err, "failed create %v", fmt.Sprintf("%v/%v", s.out, "ffmpeg.out"))
	}
	defer func(errLog *os.File) {
		_ = errLog.Close()
	}(errLog)

	s.cmd.Stdout = io.MultiWriter(os.Stdout, outLog)
	s.cmd.Stderr = io.MultiWriter(os.Stderr, errLog)
	//s.cmd.Stdout = outLog
	//s.cmd.Stderr = errLog
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = s.cmd.Start()
	if err != nil {
		return errors.Wrap(err, "failed to start ffmpeg")
	}
	log.Info("starting Transcoder")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan any, 1)
	go func() {
		select {
		case <-sigs:
			s.Stop()
			return
		case <-done:
			return
		}
	}()

	err = s.cmd.Wait()
	if err != nil {
		log.WithError(err).Warn("got error while transcoding")
	}
	log.Info("transcoding finished")
	close(done)
	ok, err := s.isFinished()
	if err != nil {
		return errors.Wrap(err, "failed to check if ffmpeg finished")
	}
	if !ok {
		return errors.New("ffmpeg not finished")
	}
	err = os.WriteFile(fmt.Sprintf("%v/%v", s.out, "done"), []byte{}, 0644)
	if err != nil {
		return errors.Wrap(err, "failed to put done marker")
	}
	return nil
}
