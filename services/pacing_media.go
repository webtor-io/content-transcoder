package services

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// Pacing of a passthrough run, in media time.
//
// The old pacing (pacing.go) counts segments: demand is the furthest segment
// number of any stream a viewer asked for, and the run freezes once the
// primary stream's segment demand+lead exists. That holds while every
// stream is cut every 4 s. A passthrough run copies the video, which the
// muxer can only cut at keyframes -- every 10 s for a typical x265 GOP --
// while the audio is still cut every 4 s. The audio's numbers then run 2.5x
// ahead of the video's, the audio sets the demand, and the run would freeze
// at video segment (t+30)/4+75: 2.5(t+30)+750 s of media for a viewer at t,
// 14 min ahead at the start and an hour ahead at t = 30 min (5.7-20 GB of a
// 4K source per run).
//
// So a passthrough run measures both sides in seconds of media, each stream
// by its own playlist: a request for segment n of a stream is a viewer at
// the start of that segment (the EXTINF sum before it), or at the stream's
// edge when n is not listed yet; production is the EXTINF sum of the
// primary playlist. The run is frozen when production reaches demand +
// lead, and let go when it falls under demand + lead - paceResumeGap.
//
// Only passthrough runs are paced this way; copy, reencode and audio runs
// keep the segment pacing they had.

// paceResumeStallMedia is how long a released passthrough run may go without
// finishing a primary segment before the resume counts as stalled. Longer
// than paceResumeStall: a copied 4K GOP can be 60 MB of source to read
// before its segment closes. Provisional, to be set from the first days of
// passthrough runs.
var paceResumeStallMedia = 60 * time.Second

// segmentStreamPattern splits a segment file name into its stream prefix
// and number: "v0-2160-12.m4s" is segment 12 of v0-2160, "a1-3.m4s" segment
// 3 of a1, "s0-7.vtt" segment 7 of s0.
var segmentStreamPattern = regexp.MustCompile(`^([asv][0-9]+(?:-[0-9]+)?)-([0-9]+)\.[0-9a-z]{2,4}$`)

// segmentStreamPlaylist is the stream playlist a segment file belongs to
// and the segment's number.
func segmentStreamPlaylist(filename string) (string, int, bool) {
	m := segmentStreamPattern.FindStringSubmatch(filename)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1] + ".m3u8", n, true
}

// hasStreamPlaylist reports whether name is the playlist of one of h's
// streams: the only files the pace loop reads.
func (h *HLS) hasStreamPlaylist(name string) bool {
	for _, group := range [][]*HLSStream{h.primary, h.audio, h.subs} {
		for _, s := range group {
			if s.GetPlaylistName() == name {
				return true
			}
		}
	}
	return false
}

// noteMediaDemand records that a viewer asked a passthrough run for the
// segment file filename. Other runs ignore it (they have noteDemand).
func (r *TranscodeRun) noteMediaDemand(filename string) {
	if r.h == nil || !r.h.passthrough {
		return
	}
	stream, n, ok := segmentStreamPlaylist(filename)
	if !ok || !r.h.hasStreamPlaylist(stream) {
		return
	}
	r.mu.Lock()
	if r.mediaDemand == nil {
		r.mediaDemand = map[string]int{}
	}
	if cur, seen := r.mediaDemand[stream]; !seen || n > cur {
		r.mediaDemand[stream] = n
	}
	r.mu.Unlock()
}

// mediaSegment is one entry of a media playlist: the segment's number (-1
// when its name has none), where it starts and how long it is, in seconds
// of media from the start of the run.
type mediaSegment struct {
	n          int
	start, dur float64
}

// parseMediaPlaylist reads the segments of an HLS media playlist in order,
// and their total duration.
func parseMediaPlaylist(data []byte) ([]mediaSegment, float64) {
	var segs []mediaSegment
	total := 0.0
	dur := -1.0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			d, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || d < 0 {
				d = 0
			}
			dur = d
		case line == "" || strings.HasPrefix(line, "#"):
		case dur >= 0:
			name := line
			if i := strings.IndexByte(name, '?'); i >= 0 {
				name = name[:i]
			}
			n := -1
			if _, num, ok := segmentStreamPlaylist(filepath.Base(name)); ok {
				n = num
			}
			segs = append(segs, mediaSegment{n: n, start: total, dur: dur})
			total += dur
			dur = -1
		}
	}
	return segs, total
}

// readMediaPlaylist reads the playlist FFmpeg writes for a stream of the
// run; a missing one is a stream with nothing produced yet.
func (r *TranscodeRun) readMediaPlaylist(name string) ([]mediaSegment, float64) {
	data, err := os.ReadFile(filepath.Join(r.outputDir, name+".ffmpeg"))
	if err != nil {
		return nil, 0
	}
	return parseMediaPlaylist(data)
}

// mediaPaceReading is what the pace loop of a passthrough run reads each
// poll, in seconds of media from the start of the run.
type mediaPaceReading struct {
	produced float64 // finished media of the primary stream
	segments int     // how many primary segments that is
	demand   float64 // the furthest point a viewer asked for; 0 before any request
}

func (r *TranscodeRun) readMediaPace() mediaPaceReading {
	r.mu.Lock()
	want := make(map[string]int, len(r.mediaDemand))
	for k, v := range r.mediaDemand {
		want[k] = v
	}
	r.mu.Unlock()
	primary := r.h.primary[0].GetPlaylistName()
	psegs, ptotal := r.readMediaPlaylist(primary)
	rd := mediaPaceReading{produced: ptotal, segments: len(psegs)}
	for stream, n := range want {
		segs, total := psegs, ptotal
		if stream != primary {
			segs, total = r.readMediaPlaylist(stream)
		}
		// Not listed yet: the viewer is waiting at this stream's edge.
		at := total
		for _, s := range segs {
			if s.n == n {
				at = s.start
				break
			}
		}
		if at > rd.demand {
			rd.demand = at
		}
	}
	return rd
}

// mediaPaceWantsFrozen is the pacing rule of a passthrough run: freeze once
// production is lead seconds past demand, let go once it is less than
// resumeLead past it (hysteresis: the run moves in bursts, not a segment at
// a time).
func mediaPaceWantsFrozen(paused bool, rd mediaPaceReading, lead, resumeLead float64) bool {
	if !paused {
		return rd.produced >= rd.demand+lead
	}
	return rd.produced >= rd.demand+resumeLead
}

// paceMedia is pace for a passthrough run: the same freezing (SIGSTOP /
// SIGCONT on the process group) and the same accounting, with demand and
// production in media time (mediaPaceWantsFrozen). Metrics carry mode
// passthrough, and a resume counts as stalled when no primary segment
// finishes within paceResumeStallMedia.
func (r *TranscodeRun) paceMedia(pid int, lead time.Duration, done <-chan struct{}, tm paceTiming) {
	const mode = runModePassthrough
	leadS := lead.Seconds()
	resumeS := (lead - tm.resumeGap).Seconds()
	if resumeS < sessionSegDuration {
		resumeS = sessionSegDuration
	}
	t := time.NewTicker(tm.poll)
	defer t.Stop()
	var pausedAt time.Time
	// After a resume: how many primary segments were finished when FFmpeg
	// was let go (-1 when not watching), and whether the wait for the next
	// one has already been counted as a stall.
	var (
		resumedAt    time.Time
		waitFrom     = -1
		stallCounted bool
	)
	pause := func(on bool, rd mediaPaceReading) {
		sig := syscall.SIGCONT
		if on {
			sig = syscall.SIGSTOP
		}
		from := -1
		if !on {
			// Read while FFmpeg is still frozen, so the segment it finishes
			// after SIGCONT is the one timed.
			from = rd.segments
		}
		if err := syscall.Kill(-pid, sig); err != nil {
			return
		}
		resumedAt, waitFrom, stallCounted = time.Now(), from, false
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
		r.logger.WithFields(log.Fields{
			"paused":   on,
			"produced": rd.produced,
			"demand":   rd.demand,
		}).Debug("run: pacing (media time)")
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
		rd := r.readMediaPace()
		r.mu.Lock()
		paused := r.paused
		r.mu.Unlock()
		if waitFrom >= 0 {
			since := time.Since(resumedAt)
			switch {
			case rd.segments > waitFrom:
				metricRunResumeSegmentSeconds.WithLabelValues(mode).Observe(since.Seconds())
				waitFrom = -1
			case !stallCounted && since >= tm.resumeStallMedia:
				stallCounted = true
				metricRunResumeStalls.WithLabelValues(mode).Inc()
				r.logger.WithFields(log.Fields{
					"produced": rd.produced,
					"demand":   rd.demand,
					"since":    since.Round(time.Second),
				}).Warn("run: no new segment after pacing let FFmpeg go")
			}
		}
		if want := mediaPaceWantsFrozen(paused, rd, leadS, resumeS); want != paused {
			pause(want, rd)
		}
	}
}
