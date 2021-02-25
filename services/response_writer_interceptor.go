package services

import (
	"bufio"
	"bytes"
	"net"
	"net/http"

	"github.com/pkg/errors"
)

type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode int
	buf        bytes.Buffer
}

func NewResponseWrtierInterceptor(w http.ResponseWriter) *responseWriterInterceptor {
	return &responseWriterInterceptor{
		statusCode:     http.StatusOK,
		ResponseWriter: w,
	}
}

func (w *responseWriterInterceptor) WriteHeader(statusCode int) {
}

func (w *responseWriterInterceptor) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *responseWriterInterceptor) GetBufferedBytes() []byte {
	return w.buf.Bytes()
}

func (w *responseWriterInterceptor) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *responseWriterInterceptor) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &responseWriterInterceptor{}
	_ http.Hijacker       = &responseWriterInterceptor{}
	_ http.Flusher        = &responseWriterInterceptor{}
)
