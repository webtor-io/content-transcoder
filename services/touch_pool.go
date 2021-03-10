package services

import (
	"context"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	touchTTL = 60
)

type TouchPool struct {
	sm     sync.Map
	st     *S3Storage
	expire time.Duration
	key    *Key
}

func NewTouchPool(st *S3Storage, key *Key) *TouchPool {
	return &TouchPool{
		expire: time.Duration(touchTTL) * time.Second,
		st:     st,
		key:    key,
	}
}

func (s *TouchPool) Touch() error {
	_, loaded := s.sm.LoadOrStore(s.key.Get(), true)
	if !loaded {
		t := NewToucher(context.Background(), s.st, s.key.Get())
		go func() {
			<-time.After(s.expire)
			s.sm.Delete(s.key.Get())
		}()
		return t.Touch()
	}
	return nil
}

func (s *TouchPool) Handle(h Handleable) {
	h.Handle(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r)
			go func() {
				if err := s.Touch(); err != nil {
					log.WithError(err).Errorf("Failed to touch key=%v", s.key.Get())
				}
			}()
		})
	})

}
