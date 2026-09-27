package services

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Serving a passthrough session: its master playlist, written from the
// output's init once there is one, and the init segments themselves. The
// .m4s segments go through sessionSegmentHandler like any segment.

// passthroughMasterTimeout bounds the wait for the init a passthrough
// master is written from: the same as for a variant playlist, which the
// init comes with (the first cut writes both).
var passthroughMasterTimeout = 5 * time.Minute

// passthroughInitWait bounds how long an init request waits for the
// running process's init to be complete, before a 404 the player retries.
// The playlist that names an init is written after it, so a player that
// read the name should not have to wait at all; this covers what that
// order does not promise (not measured).
var passthroughInitWait = 10 * time.Second

// passthroughMasterAttempts is how many processes a master request waits
// for: a process that dies before its first cut is restarted once more
// (within the session's restart budget) rather than failing the request --
// hls.js retries a master fewer times than a variant, and the first run of
// a source that dies on its timestamps at 0 comes back lenient.
const passthroughMasterAttempts = 2

// passthroughMaster makes sure a passthrough session's master playlist
// exists before it is read: it is written from the init of the run's
// current process (Session.writePassthroughMaster), so the run must be
// running -- a process that died before its init is restarted, as for a
// variant playlist. It answers the request itself, and returns false,
// when there is no master to serve.
func (s *Web) passthroughMaster(w http.ResponseWriter, r *http.Request, sess *Session) bool {
	if fileExists(filepath.Join(sess.outputDir, "index.m3u8")) {
		return true
	}
	var err error
	for attempt := 0; attempt < passthroughMasterAttempts; attempt++ {
		if !sess.IsRunning() && !s.ensureRunningFor(w, sess, "index.m3u8") {
			return false
		}
		err = sess.writePassthroughMaster(r.Context(), passthroughMasterTimeout)
		if !errors.Is(err, errPlaylistNotRunning) {
			break
		}
	}
	switch {
	case err == nil:
		return true
	case r.Context().Err() != nil:
		return false
	case errors.Is(err, errCodecsUnbuildable):
		http.Error(w, "master playlist not available", http.StatusInternalServerError)
	case errors.Is(err, errPlaylistNotRunning):
		// Not a timeout: every process ended before its init. The body is
		// the variant's for the same case.
		log.WithError(err).WithFields(log.Fields{
			"sessionID": sess.id,
			"attempts":  passthroughMasterAttempts,
		}).Error("session: passthrough master: the run ended before its init")
		http.Error(w, "playlist timeout", http.StatusGatewayTimeout)
	default:
		log.WithError(err).WithField("sessionID", sess.id).Error("session: passthrough master timeout")
		http.Error(w, "playlist timeout", http.StatusGatewayTimeout)
	}
	return false
}

// ensureRunningFor restarts the session's run for a request that needs it
// (the playlist handler's logic): false, with the request answered, only
// when the restart budget is spent.
func (s *Web) ensureRunningFor(w http.ResponseWriter, sess *Session, name string) bool {
	err := sess.EnsureRunning()
	if err == nil {
		return true
	}
	if errors.Is(err, ErrRestartLimit) {
		sess.capWarnOnce.Do(func() {
			metricRestartLimitReachedTotal.Inc()
			log.WithError(err).WithFields(log.Fields{
				"sessionID": sess.id,
				"file":      name,
			}).Warn("session: restart limit reached")
		})
		http.Error(w, "transcoder restart limit reached", http.StatusServiceUnavailable)
		return false
	}
	log.WithError(err).WithField("sessionID", sess.id).Error("session: failed to restart for " + name)
	return true
}

// initStreamPrefix is the stream prefix in an init segment's name, "" when
// name is not one.
func initStreamPrefix(name string) string {
	m := initNamePattern.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// sessionInitHandler serves an init segment of a passthrough session's run:
// once it is complete (TranscodeRun.initFile) -- never the empty file the
// running process holds until its first cut -- and only from the run's
// directory, where only a process of that run writes it.
//
// Its own branch, before any segment logic: no viewer demand, no restart
// for a segment. parseSegmentNumber reads the last "-" part of a name as
// the segment number, and a generation that happens to be all digits
// would read as one.
func (s *Web) sessionInitHandler(w http.ResponseWriter, r *http.Request, sess *Session, name string) {
	sess.Touch()
	m := initNamePattern.FindStringSubmatch(name)
	stream := sess.h.fmp4Stream(m[1])
	gen := m[2]
	if !sess.IsRunning() && !s.ensureRunningFor(w, sess, name) {
		return
	}
	deadline := time.NewTimer(passthroughInitWait)
	defer deadline.Stop()
	t := time.NewTicker(passthroughMasterPoll)
	defer t.Stop()
	for {
		run := sess.currentRun()
		if run == nil {
			http.Error(w, "init not found", http.StatusNotFound)
			return
		}
		path, complete, pending := run.initFile(stream, gen)
		if complete {
			serveInit(w, r, path, name, gen)
			return
		}
		if !pending {
			http.Error(w, "init not found", http.StatusNotFound)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			http.Error(w, "init not found", http.StatusNotFound)
			return
		case <-t.C:
		}
	}
}

// serveInit answers with a complete init segment. Its validator is the
// generation in its name and its size (segmentETag): an init is written
// once, by the process its name says.
func serveInit(w http.ResponseWriter, r *http.Request, path, name, gen string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "init not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "init not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", segmentETag(gen, fi.Size()))
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, name, time.Time{}, io.NewSectionReader(f, 0, fi.Size()))
}
