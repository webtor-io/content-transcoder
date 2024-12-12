package services

import (
	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	"os"
	"path"
	"time"
)

type TranscodePool struct {
	lazymap.LazyMap[bool]
}

func NewTranscodePool() *TranscodePool {
	return &TranscodePool{
		LazyMap: lazymap.New[bool](&lazymap.Config{
			Expire:      30 * time.Minute,
			StoreErrors: false,
		}),
	}
}

func (s *TranscodePool) IsDone(out string) bool {
	marker := path.Join(out, "done")
	log.Infof("checking if done marker %s exists", marker)
	if _, err := os.Stat(marker); err == nil {
		log.Infof("%s exists", marker)
		return true
	} else {
		log.WithError(err).Errorf("failed to check done marker %s", marker)
	}
	return false
}

func (s *TranscodePool) IsTranscoding(out string) bool {
	_, st := s.LazyMap.Status(out)
	return st
}

func (s *TranscodePool) Transcode(out string, h *HLS) error {
	_, err := s.LazyMap.Get(out, func() (bool, error) {
		tr := NewTranscoder(out, h)
		err := tr.Run()
		if err != nil {
			return false, err

		}
		return true, nil
	})
	return err
}
