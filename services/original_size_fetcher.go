package services

import (
	"net/http"
	"sync"

	"github.com/urfave/cli"
)

type OriginalSizeFetcher struct {
	cl     *http.Client
	inited bool
	mux    sync.Mutex
	res    uint64
	err    error
	url    string
}

func NewOriginalSizeFetcher(c *cli.Context, cl *http.Client) *OriginalSizeFetcher {
	return &OriginalSizeFetcher{
		url: c.String(inputFlag),
		cl:  cl,
	}
}

func (s *OriginalSizeFetcher) Fetch() (uint64, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.res, s.err
	}
	s.res, s.err = s.fetch()
	s.inited = true
	return s.res, s.err
}

func (s *OriginalSizeFetcher) fetch() (uint64, error) {
	res, err := s.cl.Head(s.url)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	return uint64(res.ContentLength), nil
}
