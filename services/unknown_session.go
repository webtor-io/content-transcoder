package services

import (
	"net/http"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// A request for a session this pod does not hold: the session expired after
// 10 min without a request (sessionInactivityExpiry), was lost with its pod
// on a rollout, or never lived here. Nothing on this pod can bring it back,
// so the answer is the 404 it always was -- but a playlist or segment
// request gets it only after unknownSessionDelay.
//
// Why wait. web-ui's player (hls-manager.js) calls hls.startLoad() at once
// on every fatal network error, and hls.js makes a 4xx fatal without a
// retry, so a tab left on a dead session asks again the moment its 404
// arrives: up to 66 requests a second per tab, for as long as the tab is
// open (2.27M 404s a day on 2026-09-29, 91% of them from four tabs). Each
// hls.js loader has one request in flight, so a 404 that takes 2 s caps
// such a tab at about 1.5 requests a second (three loaders) -- tabs running
// today's JavaScript included, from the day this is deployed.
//
// Who else gets this 404, checked 2026-09-29 before choosing what to hold:
//   - rest-api's CacheMap probe (index.m3u8?done=true) is answered by
//     legacyPlaylistHandler, not here; untouched.
//   - subtitle-translate reads a playlist 404 as "the session is over" and
//     stops (30 s fetch timeout): the same answer, 2 s later, once.
//   - web-ui's warmup (bufferSessionHLS) reads a 404 body as an empty
//     playlist and polls again every 2 s until its deadline: with the wait
//     it polls every 4 s, same outcome. No session got a 404 while it was
//     being warmed up: 0 of 1473 sessions created on 29.09 had one within
//     60 s of "created session", and all 1.93M session 404s in 21 h came
//     from external callers (role free/grace/...), none internal.
//   - a session asked on another pod while it lives: 7 requests in 21 h, 2 s
//     before a rollout closed the pod that held it -- gone either way.
//
// Only GET and HEAD of an HLS file (unknownSessionTarpitted) are held: that
// is what the hls.js loaders ask. The seek (POST, and the GET of its offset
// the player makes once at start) and DELETE are one-shot calls of our own
// code that act on the answer, never a loop (54 such 404s in 21 h): they are
// answered at once, like any other path.
//
// The wait shows in torrent-http-proxy's latency of the transcoder. The held
// 404 writes its body when the wait ends, so thp records it in
// webtor_http_proxy_request_ttfb_seconds{name="content-transcoder"} under
// status="400", at about 2 s. Dead tabs are there all day (6 to 25 dead ids
// in every 5-minute window of 28-29.09): replayed with the hold, the held
// 404s are about 13% of those observations and a p95 over all statuses sits
// at about 2 s for good, against 0.03 s, hiding every real slowdown under
// 2.5 s. TranscoderTTFBSlow and the transcoder TTFB panels must therefore
// select status!="400", and this ships together with that change (docs,
// "Unknown session"). Class 400 had 192 of the 34,348 answers slower than
// 1 s in the 7 days to 30.09; class 500 (segment and playlist timeouts)
// stays in, so the filter is !="400", not ="200".
//
// The wait stays under 2.5 s, a bucket bound of that histogram (default
// buckets): held answers land in (1, 2.5] and cannot by themselves push
// even an unfiltered p95 past 2.5 s, so not past the alert's 3 s. It must
// also stay under hls.js's 10 s time to first byte (fragLoadPolicy and
// playlistLoadPolicy, which web-ui does not override): past it the 404
// becomes a timeout, which hls.js does retry.

const (
	webUnknownSessionDelayFlag = "unknown-session-delay"
	// defaultUnknownSessionDelay: see above for why 2 s and not more.
	defaultUnknownSessionDelay = 2 * time.Second
)

// unknownSessionTarpitted reports whether a request for a session this pod
// does not hold waits before its 404: GET or HEAD of a playlist, segment or
// init -- what hls.js loaders ask for. subPath is the part after
// /session/{id}/.
func unknownSessionTarpitted(method, subPath string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	switch filepath.Ext(subPath) {
	case ".m3u8", ".ts", ".vtt", "." + passthroughSegmentExt, ".mp4":
		return true
	}
	return false
}

// sessionNotFound answers a request for session id, which this pod does not
// hold, with a 404 -- after unknownSessionDelay when the request is one
// unknownSessionTarpitted holds. A client that goes away during the wait
// ends it at once: nothing is written and the handler returns.
func (s *Web) sessionNotFound(w http.ResponseWriter, r *http.Request, id, subPath string) {
	if s.unknownSessionDelay <= 0 || !unknownSessionTarpitted(r.Method, subPath) {
		metricUnknownSessionTotal.WithLabelValues(unknownSessionImmediate).Inc()
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.unknownSessions.note(id, r.Method, subPath, s.unknownSessionDelay)
	t := time.NewTimer(s.unknownSessionDelay)
	defer t.Stop()
	select {
	case <-r.Context().Done():
		metricUnknownSessionTotal.WithLabelValues(unknownSessionCanceled).Inc()
		return
	case <-t.C:
	}
	metricUnknownSessionTotal.WithLabelValues(unknownSessionDelayed).Inc()
	http.Error(w, "session not found", http.StatusNotFound)
}

const (
	// unknownSessionLogEvery: one line per unknown session id per this
	// period, with the count of requests held since the last one.
	unknownSessionLogEvery = 10 * time.Minute
	// unknownSessionLogMax bounds the ids remembered for that: an id is
	// whatever a client puts in the URL. About 135 dead sessions were asked
	// for a day (2026-09-29).
	unknownSessionLogMax = 4096
	// unknownSessionIDMax bounds the length of an id as remembered and
	// logged; a real one is 32 hex digits. Without it the bound on the
	// count would not bound the memory: an id is as long as the URL.
	unknownSessionIDMax = 64
)

// unknownSessionLog lets the tarpit log an unknown session id at most once
// per unknownSessionLogEvery. One line per request would be the flood the
// tarpit is there to slow down -- a looping tab asks 1.5 to 66 times a
// second. Its zero value is ready to use.
type unknownSessionLog struct {
	mu      sync.Mutex
	entries map[string]*unknownSessionEntry
	now     func() time.Time // nil: time.Now
}

type unknownSessionEntry struct {
	logged time.Time // when the id was last logged
	held   int       // requests held since then that no line has counted
}

// note counts a held request for id and logs it, with the requests held
// since the id's last line, when that line is unknownSessionLogEvery old or
// there is none. Once unknownSessionLogMax ids are remembered and none is
// old enough to forget, a new id is counted in the metric only.
func (l *unknownSessionLog) note(id, method, subPath string, delay time.Duration) {
	if len(id) > unknownSessionIDMax {
		id = id[:unknownSessionIDMax]
	}
	held, ok := l.take(id)
	if !ok {
		return
	}
	log.WithFields(log.Fields{
		"sessionID": id,
		"method":    method,
		"file":      filepath.Base(subPath),
		"held":      held,
		"delay":     delay,
	}).Info("session: unknown session, 404 held (tarpit)")
}

// take is note without the logging: whether to log id now, and how many
// held requests the line covers (this one included).
func (l *unknownSessionLog) take(id string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if l.entries == nil {
		l.entries = map[string]*unknownSessionEntry{}
	}
	e := l.entries[id]
	if e == nil {
		if len(l.entries) >= unknownSessionLogMax {
			l.forget(now)
			if len(l.entries) >= unknownSessionLogMax {
				return 0, false
			}
		}
		l.entries[id] = &unknownSessionEntry{logged: now}
		return 1, true
	}
	e.held++
	if now.Sub(e.logged) < unknownSessionLogEvery {
		return 0, false
	}
	held := e.held
	e.logged, e.held = now, 0
	return held, true
}

// forget drops the ids whose last line is unknownSessionLogEvery old: a
// request for one of them would be logged anyway, and logs as a new id.
func (l *unknownSessionLog) forget(now time.Time) {
	for id, e := range l.entries {
		if now.Sub(e.logged) >= unknownSessionLogEvery {
			delete(l.entries, id)
		}
	}
}
