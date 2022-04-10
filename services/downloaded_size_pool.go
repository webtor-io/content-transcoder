package services

import (
	"context"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	downloadedSizeTTL       = 60
	downloadedSizeStoreDiff = 10_000_000
)

type DownloadedSizePool struct {
	sm      sync.Map
	st      *S3Storage
	expire  time.Duration
	counter *Counter
	key     *Key
	isf     *DownloadedSizeFetcher
}

func NewDownloadSizePool(st *S3Storage, counter *Counter, key *Key, isf *DownloadedSizeFetcher) *DownloadedSizePool {
	return &DownloadedSizePool{
		expire:  time.Duration(downloadedSizeTTL) * time.Second,
		st:      st,
		counter: counter,
		key:     key,
		isf:     isf,
	}
}

func (s *DownloadedSizePool) Push(size uint64) error {
	_, loaded := s.sm.LoadOrStore(s.key.Get(), true)
	if !loaded {
		ds := NewDownloadedSizePusher(context.Background(), s.st, s.key.Get(), size)
		go func() {
			<-time.After(s.expire)
			s.sm.Delete(s.key.Get())
		}()
		return ds.Push()
	}
	return nil
}

func (s *DownloadedSizePool) Handle(h Handleable) {
	key := s.key.Get()
	var ci uint64 = 0
	h.Handle(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cw := s.counter.NewResponseWriter(w)
			h.ServeHTTP(cw, r)
			go func() {
				i, err := s.isf.Fetch()
				if err != nil {
					log.WithError(err).Errorf("Failed to get initial downloaded size key=%v", key)
					return
				}
				if s.counter.Count()-ci > downloadedSizeStoreDiff {
					ci = s.counter.Count()
					total := i + ci
					if err := s.Push(total); err != nil {
						log.WithError(err).Errorf("Failed to push downloaded size=%v key=%v", total, key)
					}
				}
			}()
		})
	})

}
