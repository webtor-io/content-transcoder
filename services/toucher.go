package services

import (
	"context"
	"sync"
)

type Toucher struct {
	st     *S3Storage
	mux    sync.Mutex
	err    error
	inited bool
	ctx    context.Context
	key    string
}

func NewToucher(ctx context.Context, st *S3Storage, key string) *Toucher {
	return &Toucher{
		st:  st,
		ctx: ctx,
		key: key,
	}
}

func (s *Toucher) Touch() error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.err
	}
	s.err = s.touch()
	s.inited = true
	return s.err
}

func (s *Toucher) touch() (err error) {
	err = s.st.Touch(s.ctx, s.key)
	return
}
