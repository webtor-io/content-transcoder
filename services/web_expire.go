package services

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	log "github.com/sirupsen/logrus"
)

const (
	webGraceFlag = "grace"
)

func RegisterWebExpireFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   webGraceFlag + ", ag",
		Usage:  "access grace in seconds",
		Value:  600,
		EnvVar: "GRACE",
	})
}

type WebExpire struct {
	t *time.Timer
	d time.Duration
}

func NewWebExpire(c *cli.Context) *WebExpire {
	d := time.Duration(c.Int(webGraceFlag)) * time.Second
	return &WebExpire{d: d, t: time.NewTimer(d)}
}

func (s *WebExpire) Reset() {
	s.t.Reset(s.d)
}

func (s *WebExpire) NewResponseWriter(w http.ResponseWriter) *expireResponseWriter {
	return &expireResponseWriter{ResponseWriter: w, e: s}
}

func (s *WebExpire) Expire() <-chan time.Time {
	return s.t.C
}

func (s *WebExpire) Serve() error {
	<-s.Expire()
	log.Info("Nobody here for so long...")
	return nil
}

func (s *WebExpire) Handle(h Handleable) {
	h.Handle(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ew := s.NewResponseWriter(w)
			h.ServeHTTP(ew, r)
		})
	})
}

type expireResponseWriter struct {
	http.ResponseWriter
	e *WebExpire
}

func (s *expireResponseWriter) Write(buf []byte) (int, error) {
	n, err := s.ResponseWriter.Write(buf)
	s.e.Reset()
	return n, err
}

func (w *expireResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *expireResponseWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &expireResponseWriter{}
	_ http.Hijacker       = &expireResponseWriter{}
	_ http.Flusher        = &expireResponseWriter{}
)
