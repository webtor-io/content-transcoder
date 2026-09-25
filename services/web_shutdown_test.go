package services

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cs "github.com/webtor-io/common-services"
)

// Close drains in-flight requests before it closes the sessions: a request
// still being answered when SIGTERM lands must find its session (and the
// FFmpeg behind it) alive, and get its response.
func TestWebCloseDrainsBeforeClosingSessions(t *testing.T) {
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	sm := NewSessionManager(runMgr)
	web := &Web{host: "127.0.0.1", port: 0, sessionManager: sm, gs: cs.NewGracefulServer(5 * time.Second)}

	var sessionsOpenAtEnd atomic.Bool
	started := make(chan struct{})
	web.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		sm.mu.Lock()
		sessionsOpenAtEnd.Store(!sm.closed)
		sm.mu.Unlock()
		_, _ = w.Write([]byte("segment"))
	})
	if err := web.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = web.Serve() }()

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + web.ln.Addr().String() + "/session/x/v0-0.ts")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()
	<-started
	web.Close()

	r := <-res
	if r.err != nil || r.body != "segment" {
		t.Fatalf("in-flight request was not drained: %q %v", r.body, r.err)
	}
	if !sessionsOpenAtEnd.Load() {
		t.Error("sessions were closed under a request still in flight")
	}
	sm.mu.Lock()
	closed := sm.closed
	sm.mu.Unlock()
	if !closed {
		t.Error("Close must close the sessions after the drain")
	}
}
