package services

import (
	"context"
	"sync"
)

type DownloadedSizePusher struct {
	st     *S3Storage
	mux    sync.Mutex
	err    error
	inited bool
	key    string
	size   uint64
	ctx    context.Context
}

func NewDownloadedSizePusher(ctx context.Context, st *S3Storage, key string, size uint64) *DownloadedSizePusher {
	return &DownloadedSizePusher{
		st:   st,
		ctx:  ctx,
		key:  key,
		size: size,
	}
}

func (s *DownloadedSizePusher) Push() error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.err
	}
	s.err = s.push()
	s.inited = true
	return s.err
}

func (s *DownloadedSizePusher) push() error {
	return s.st.StoreDownloadedSize(s.ctx, s.key, s.size)
}
