package services

import (
	"context"
	"sync"
)

type DownloadedSizeFetcher struct {
	st     *S3Storage
	mux    sync.Mutex
	err    error
	inited bool
	key    *Key
	res    uint64
	ctx    context.Context
}

func NewDownloadedSizeFetcher(ctx context.Context, st *S3Storage, key *Key) *DownloadedSizeFetcher {
	return &DownloadedSizeFetcher{
		st:  st,
		ctx: ctx,
		key: key,
	}
}

func (s *DownloadedSizeFetcher) Fetch() (uint64, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.res, s.err
	}
	s.res, s.err = s.fetch()
	s.inited = true
	return s.res, s.err
}

func (s *DownloadedSizeFetcher) fetch() (uint64, error) {
	return s.st.FetchDownloadedSize(s.ctx, s.key.Get())
}
