package services

import (
	"bufio"
	"bytes"
	"net"
	"net/http"

	"github.com/pkg/errors"
)

type bufferedResponseWriter struct {
	http.ResponseWriter
	statusCode int
	buf        bytes.Buffer
}

func NewBufferedResponseWrtier(w http.ResponseWriter) *bufferedResponseWriter {
	return &bufferedResponseWriter{
		statusCode:     http.StatusOK,
		ResponseWriter: w,
	}
}

func (w *bufferedResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *bufferedResponseWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *bufferedResponseWriter) GetBufferedBytes() []byte {
	return w.buf.Bytes()
}

func (w *bufferedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *bufferedResponseWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &bufferedResponseWriter{}
	_ http.Hijacker       = &bufferedResponseWriter{}
	_ http.Flusher        = &bufferedResponseWriter{}
)
