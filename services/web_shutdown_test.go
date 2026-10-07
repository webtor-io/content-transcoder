package services

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
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

// A response still going out at the drain timeout (a segment to a reader
// that stopped) is cut with a reset, not a FIN queued behind the unsent tail
// of the send buffer: in a pod that tail and the FIN die with the network,
// and torrent-http-proxy waits for its keepalive to notice. The reset comes
// from common-services and needs the listener's connections to be plain
// *net.TCPConn: a Listen that wraps them turns it back into a FIN.
func TestWebCloseResetsResponsesCutAtTimeout(t *testing.T) {
	runMgr := NewRunManager()
	defer runMgr.CloseAll()
	web := &Web{host: "127.0.0.1", port: 0, sessionManager: NewSessionManager(runMgr), gs: cs.NewGracefulServer(200 * time.Millisecond)}

	var written atomic.Int64
	web.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32<<10)
		for {
			if _, err := w.Write(buf); err != nil {
				return
			}
			written.Add(int64(len(buf)))
		}
	})
	if err := web.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = web.Serve() }()

	c, err := net.Dial("tcp", web.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.(*net.TCPConn).SetReadBuffer(16 << 10)
	if _, err := fmt.Fprint(c, "GET /session/x/v0-0.ts HTTP/1.1\r\nHost: t\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Wait for the handler's writes to block on full socket buffers.
	for last := int64(-1); ; time.Sleep(50 * time.Millisecond) {
		cur := written.Load()
		if cur > 0 && cur == last {
			break
		}
		last = cur
	}

	web.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := io.Copy(io.Discard, c)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("client read %d of %d bytes and then %v, want a connection reset", n, written.Load(), err)
	}
}
