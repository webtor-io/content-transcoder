package services

import (
	"bufio"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/pkg/errors"
)

type Counter struct {
	count uint64
}

func NewCounter() *Counter {
	return &Counter{}
}

type responseWriterCounter struct {
	http.ResponseWriter
	c *Counter
}

func (s *responseWriterCounter) Write(buf []byte) (int, error) {
	n, err := s.ResponseWriter.Write(buf)
	s.c.Add(uint64(n))
	return n, err
}

func (w *responseWriterCounter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *responseWriterCounter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

func (s *Counter) Add(n uint64) {
	atomic.AddUint64(&s.count, n)
}

func (s *Counter) Count() uint64 {
	return s.count
}

func (s *Counter) NewResponseWriter(w http.ResponseWriter) *responseWriterCounter {
	return &responseWriterCounter{c: s, ResponseWriter: w}
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &responseWriterCounter{}
	_ http.Hijacker       = &responseWriterCounter{}
	_ http.Flusher        = &responseWriterCounter{}
)
