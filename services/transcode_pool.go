package services

import (
	"fmt"
	"github.com/webtor-io/lazymap"
	"os"
	"time"
)

type TranscodePool struct {
	lazymap.LazyMap
}

func NewTranscodePool() *TranscodePool {
	return &TranscodePool{
		LazyMap: lazymap.New(&lazymap.Config{
			Expire:      30 * time.Minute,
			ErrorExpire: 10 * time.Second,
		}),
	}
}

func (s *TranscodePool) IsDone(out string) bool {
	if _, err := os.Stat(fmt.Sprintf("%v/done", out)); err == nil {
		return true
	}
	return false
}

func (s *TranscodePool) IsTranscoding(out string) bool {
	_, st := s.LazyMap.Status(out)
	return st
}

func (s *TranscodePool) Transcode(out string, h *HLS) error {
	_, err := s.LazyMap.Get(out, func() (interface{}, error) {
		tr := NewTranscoder(out, h)
		err := tr.Run()
		return nil, err
	})
	return err
}
