package services

import (
	"fmt"
	"os"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// Pacing keeps a run from getting too far ahead of its viewers. Measured on
// 2026-09-25 over 16 h of runs released for inactivity: copy runs produced
// p50 11 min of media and at least 93% of it was never watchable, audio-only
// runs 99% (p90 5.8 h ahead); every such minute is torrent read from a
// seeder, written to the node's disk and, for re-encodes, CPU the other runs
// on the pod could use. FFmpeg has no feedback input, so the run freezes it
// (SIGSTOP) once it is paceLead ahead of the furthest segment any viewer has
// asked for, and lets it go (SIGCONT) when a viewer gets within paceResume.
// Its source connection just idles meanwhile: neither torrent-http-proxy
// nor the seeder sets read/write timeouts, and -reconnect covers a drop.
//
// The first paceLead of every run is produced at full speed, so start-up
// (web-ui buffers 30 s before showing the player) is untouched.
// Variables so tests can run the loop at test speed.
var (
	pacePoll = time.Second
	// paceResumeGap is how much closer than the lead a viewer must come
	// before the run continues: a minute of hysteresis, so a run moves in
	// bursts of about a minute instead of a segment at a time.
	paceResumeGap = 60 * time.Second
	// paceResumeStall is how long a released run may go without starting a
	// new primary segment before the resume counts as stalled. A copy or
	// re-encode run with a live source starts one within a few seconds; on
	// 2026-09-25 a resumed run started none for 15 min.
	paceResumeStall = 30 * time.Second
)

// paceSegments converts a media duration to a count of sessionSegDuration
// segments.
func paceSegments(d time.Duration) int {
	return int(d / (sessionSegDuration * time.Second))
}

// noteDemand records that a viewer asked for segment n of this run.
func (r *TranscodeRun) noteDemand(n int) {
	r.mu.Lock()
	if n > r.demand {
		r.demand = n
	}
	r.mu.Unlock()
}

// primarySegmentPath is the file of segment n of the run's primary stream.
func (r *TranscodeRun) primarySegmentPath(n int) string {
	p := r.h.primary[0]
	name := fmt.Sprintf("%v%v-%v.%v", p.st, p.index, n, p.GetSegmentExtension())
	if p.r != nil {
		name = fmt.Sprintf("%v%v-%v-%v.%v", p.st, p.index, p.r.Height, n, p.GetSegmentExtension())
	}
	return r.outputDir + "/" + name
}

// firstMissingSegment is the lowest primary segment at or after the
// furthest viewer demand that FFmpeg has not started (segment files appear
// when FFmpeg opens them).
func (r *TranscodeRun) firstMissingSegment() int {
	r.mu.Lock()
	n := r.demand
	r.mu.Unlock()
	if n < 0 {
		n = 0
	}
	for fileExists(r.primarySegmentPath(n)) {
		n++
	}
	return n
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pace runs for one FFmpeg process (started by watchProcessLocked) and
// freezes and releases it against the viewers' demand until done closes.
func (r *TranscodeRun) pace(pid int, lead time.Duration, done <-chan struct{}, mode string) {
	leadSegs := paceSegments(lead)
	resumeSegs := paceSegments(lead - paceResumeGap)
	if resumeSegs < 1 {
		resumeSegs = 1
	}
	t := time.NewTicker(pacePoll)
	defer t.Stop()
	var pausedAt time.Time
	// After a resume: the first primary segment FFmpeg had not started when
	// it was let go (-1 when not watching), and whether the wait for it has
	// already been counted as a stall.
	var (
		resumedAt    time.Time
		waitSeg      = -1
		stallCounted bool
	)
	pause := func(on bool) {
		sig := syscall.SIGCONT
		if on {
			sig = syscall.SIGSTOP
		}
		next := -1
		if !on {
			// Found while FFmpeg is still frozen, so the segment it starts
			// right after SIGCONT is the one timed.
			next = r.firstMissingSegment()
		}
		if err := syscall.Kill(-pid, sig); err != nil {
			return
		}
		resumedAt, waitSeg, stallCounted = time.Now(), next, false
		r.mu.Lock()
		r.paused = on
		if on {
			pausedAt = time.Now()
		} else {
			d := time.Since(pausedAt)
			r.pausedFor += d
			metricRunPauseSeconds.WithLabelValues(mode).Add(d.Seconds())
		}
		r.mu.Unlock()
		if on {
			metricRunsPaused.Inc()
		} else {
			metricRunsPaused.Dec()
		}
		r.logger.WithField("paused", on).Debug("run: pacing")
	}
	defer func() {
		r.mu.Lock()
		was := r.paused
		r.mu.Unlock()
		if was {
			// The process is gone (done closed); account the pause without
			// signalling a pid that may be reused.
			r.mu.Lock()
			d := time.Since(pausedAt)
			r.pausedFor += d
			r.paused = false
			r.mu.Unlock()
			metricRunPauseSeconds.WithLabelValues(mode).Add(d.Seconds())
			metricRunsPaused.Dec()
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		r.mu.Lock()
		demand, paused := r.demand, r.paused
		r.mu.Unlock()
		if demand < 0 {
			demand = 0
		}
		if waitSeg >= 0 {
			since := time.Since(resumedAt)
			switch {
			case fileExists(r.primarySegmentPath(waitSeg)):
				metricRunResumeSegmentSeconds.WithLabelValues(mode).Observe(since.Seconds())
				waitSeg = -1
			case !stallCounted && since >= paceResumeStall:
				stallCounted = true
				metricRunResumeStalls.WithLabelValues(mode).Inc()
				r.logger.WithFields(log.Fields{
					"segment": waitSeg,
					"demand":  demand,
					"since":   since.Round(time.Second),
				}).Warn("run: no new segment after pacing let FFmpeg go")
			}
		}
		switch {
		case !paused && fileExists(r.primarySegmentPath(demand+leadSegs)):
			pause(true)
		case paused && !fileExists(r.primarySegmentPath(demand+resumeSegs)):
			pause(false)
		}
	}
}

// activeSpeed is media seconds over the time FFmpeg was allowed to run:
// FFmpeg's own speed= divides by wall time, frozen time included.
func activeSpeed(media float64, wall, paused time.Duration) (float64, bool) {
	active := wall - paused
	if paused <= 0 || active <= 0 {
		return 0, false
	}
	return media / active.Seconds(), true
}

// logPaceConfig is called once at start-up.
func logPaceConfig(lead time.Duration) {
	if lead <= 0 {
		log.Info("run pacing disabled")
		return
	}
	log.WithField("lead", lead).Info("run pacing: FFmpeg is held this far ahead of the furthest requested segment")
}
