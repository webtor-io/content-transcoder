package services

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics of the transcoder. Every failure mode counted here was,
// until now, visible only as a log line; the metric is named after the
// question a dashboard asks, and the label sets are closed (no session ids,
// hashes or paths — those are unbounded and belong in logs).
//
// Registered on the default registry via promauto; common-services serves it
// on the prom port (see configure.go). The variables are package-level so the
// hot paths pay one atomic add, and tests read them through testutil.

const metricsNamespace = "transcoder"

// Run outcomes: why an FFmpeg process is no longer running. Exactly one is
// recorded per process, when it is reaped (TranscodeRun.reapProcess).
const (
	runOutcomeFinished     = "finished"      // walked the source to its end, exit 0
	runOutcomeReleasedIdle = "released_idle" // stopped by the run reaper after the grace period
	runOutcomeKilled       = "killed"        // stopped by us for another reason (shutdown, explicit Stop)
	runOutcomeFailed       = "failed"        // exited on its own with an error or a signal we did not send
)

// FFmpeg exit reasons: how the process ended, regardless of why.
const (
	ffmpegExitOK     = "ok"     // exit status 0
	ffmpegExitError  = "error"  // non-zero exit status
	ffmpegExitSignal = "signal" // terminated by a signal (ours or the kernel's)
)

// Playlist wait outcomes (Session.WaitForPlaylist).
const (
	playlistWaitOK         = "ok"
	playlistWaitTimeout    = "timeout"
	playlistWaitNotRunning = "not_running"
	playlistWaitCanceled   = "canceled" // the client went away first
)

// Playlist kinds: a subtitle playlist is expected to lag (FFmpeg writes it
// when the first subtitle segment closes, minutes in on a slow source) and is
// waited for with a 5 s budget before an empty stub is served, so its
// timeouts are not the failure the variant's are and must not be summed
// with them.
const (
	playlistKindVariant  = "variant"
	playlistKindSubtitle = "subtitle"
)

// Run modes: what FFmpeg does to the primary stream, which is what decides
// how fast a run can go. copy remuxes h264 video (bound by the source),
// reencode encodes video to h264 (bound by the CPU), audio is an audio-only
// source, passthrough remuxes HEVC video into fMP4 (bound by the source,
// paced in media time: see pacing_media.go).
const (
	runModeCopy        = "copy"
	runModeReencode    = "reencode"
	runModeAudio       = "audio"
	runModePassthrough = "passthrough"
)

// runModes is every run mode, for registration.
var runModes = []string{runModeCopy, runModeReencode, runModeAudio, runModePassthrough}

// Run starts: from the beginning of the source, or from a seek.
const (
	runStartZero = "start"
	runStartSeek = "seek"
)

// FFmpeg failure causes, read off the stderr tail of a failed run. A closed
// set: the text itself is in the "run: ffmpeg failed" log line.
const (
	failureSubtitleBitmap = "subtitle_bitmap" // a bitmap subtitle sent to the webvtt encoder
	failureNoDecoder      = "no_decoder"      // a stream the build cannot decode
	failureTimestamps     = "timestamps"      // non-monotonic DTS, fatal under -xerror
	failureInvalidData    = "invalid_data"    // the demuxer rejected the source
	failureSource         = "source"          // reading the source failed (I/O, HTTP)
	failureSignal         = "signal"          // killed by a signal we did not send (OOM)
	failureOther          = "other"
)

// Source probe outcomes (ContentProbe).
const (
	probeOutcomeOK    = "ok"
	probeOutcomeError = "error"
)

// Results of resolving a copy or passthrough seek run's real start
// (TranscodeRun.resolveRealStart): the probe answered, failed (error or
// timeout), or named a keyframe that cannot be right.
const (
	realStartOK          = "ok"
	realStartFailed      = "failed"
	realStartImplausible = "implausible"
)

// Outcomes of the transcoder's own look at an HEVC source (sourceProber).
const (
	sourceProbeOK     = "ok"
	sourceProbeFailed = "failed"
)

// sourceProbeBuckets: the look is expected at about a second on a warm
// source; 1.5 s at p90 is the bar for switching passthrough on, and 10 s is
// two failed attempts.
var sourceProbeBuckets = []float64{0.1, 0.25, 0.5, 1, 1.5, 2, 3, 5, 10}

// segmentsServedBuckets: how many primary segments a session was served
// before it went away. <= 2 is a session that never really played.
var segmentsServedBuckets = []float64{0, 1, 2, 3, 5, 10, 20, 50, 100, 300, 1000}

// secondsBuckets covers the waits this service does: a playlist appears in
// a few seconds on a warm source and in tens of seconds on a cold torrent;
// beyond a minute the player has already given up.
var secondsBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60}

// firstSegmentBuckets covers the first 4 s segment of a run: a copy run on a
// warm source has it in about a second, a 1080p re-encode on one CPU in
// 10-25 s (measured 2026-09-24), a cold torrent in minutes.
var firstSegmentBuckets = []float64{1, 2, 4, 8, 15, 30, 60, 120}

// speedBuckets are FFmpeg's speed (media seconds per wall second). 1 is the
// line that matters: below it playback outruns the transcoder. Copy runs go
// at tens to hundreds.
var speedBuckets = []float64{0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 3, 5, 10, 30, 100}

var (
	metricSessionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "sessions_total",
		Help:      "Sessions created.",
	})
	metricSessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "sessions_active",
		Help:      "Sessions currently held by the session manager (idle ones included until the 10-minute expiry).",
	})
	metricRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "runs_total",
		Help:      "FFmpeg processes that ended, by why: finished (source fully transcoded), released_idle (reaped after the grace period), killed (stopped by us for another reason), failed (died on its own).",
	}, []string{"outcome"})
	metricRunsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "runs_active",
		Help:      "FFmpeg processes currently running.",
	})
	metricFFmpegExitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "ffmpeg_exits_total",
		Help:      "FFmpeg process exits, by how: ok (status 0), error (non-zero status), signal (terminated by a signal).",
	}, []string{"reason"})
	metricPlaylistWaitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "playlist_waits_total",
		Help:      "Waits for a playlist to appear, by outcome (ok, timeout, not_running, canceled) and kind (variant, subtitle).",
	}, []string{"outcome", "kind"})
	metricPlaylistWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "playlist_wait_seconds",
		Help:      "Time until a playlist appeared, successful waits only, by kind.",
		Buckets:   secondsBuckets,
	}, []string{"kind"})
	metricAutoRestartsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "auto_restarts_total",
		Help:      "FFmpeg auto-restart attempts charged to a session's restart budget (a playlist or segment request found the run dead).",
	})
	metricRestartLimitReachedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "restart_limit_reached_total",
		Help:      "Sessions that exhausted the auto-restart budget and were answered 503 (once per session, like the log line).",
	})
	metricRunFirstSegmentSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "run_first_segment_seconds",
		Help:      "Time from FFmpeg's start to the first segment of the primary stream (its playlist appearing), by mode (copy, reencode, audio) and start (start, seek). Runs that end before it are not counted.",
		Buckets:   firstSegmentBuckets,
	}, []string{"mode", "start"})
	metricRunSpeed = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "run_speed",
		Help:      "FFmpeg's speed (media time over wall time) when a run ends, for runs that produced at least 30 s of media, by mode. Below 1 the viewer outruns the transcoder.",
		Buckets:   speedBuckets,
	}, []string{"mode"})
	metricRunPauseSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "run_pause_seconds_total",
		Help:      "Time FFmpeg processes spent frozen by pacing (ahead of every viewer by the pace lead), by mode.",
	}, []string{"mode"})
	metricRunResumeSegmentSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "run_resume_segment_seconds",
		Help:      "Time from pacing letting a frozen FFmpeg go (SIGCONT) to it starting the next primary segment, by mode. Resumes that never get one are only in run_resume_stalls_total.",
		Buckets:   firstSegmentBuckets,
	}, []string{"mode"})
	metricRunResumeStalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "run_resume_stalls_total",
		Help:      "Resumes after which FFmpeg started no new primary segment within 30 s, by mode; counted once, when the 30 s pass.",
	}, []string{"mode"})
	metricRunsPaused = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "runs_paused",
		Help:      "FFmpeg processes currently frozen by pacing.",
	})
	metricFFmpegFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "ffmpeg_failures_total",
		Help:      "Failed runs (runs_total{outcome=failed}) by the cause read off FFmpeg's stderr: subtitle_bitmap, no_decoder, timestamps, invalid_data, source, signal, other.",
	}, []string{"cause"})
	metricSourceOpenSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "source_open_seconds",
		Help:      "Time to probe a source (its first read; cached probe results are not counted), by outcome.",
		Buckets:   secondsBuckets,
	}, []string{"outcome"})
	metricVideoRouteTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "video_route_total",
		Help:      "POST /session answers by the video route (passthrough, copy, reencode, audio; refused for the 415 of a video the route would have to encode or the 503 of a failed source check, error for any other failure) and the reason it was chosen.",
	}, []string{"route", "reason"})
	metricSourceProbeSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "source_probe_seconds",
		Help:      "Time of the transcoder's own look at an HEVC source before passthrough, retries included, by result (ok, failed); cached results are not counted.",
		Buckets:   sourceProbeBuckets,
	}, []string{"result"})
	metricPassthroughCodecsMismatch = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "passthrough_codecs_mismatch_total",
		Help:      "Passthrough master playlists whose output HEVC configuration (the init's hvcC, which CODECS is built from) differs from what the route was decided on (the source's parameter sets merged as FFmpeg's hvcC writer merges them), by field (profile, tier, level); unbuildable when the init gave no CODECS at all and the master was refused. Expected 0.",
	}, []string{"field"})
	metricRunRealStartTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "run_real_start_total",
		Help:      "Probes of where a seek run of a copied video (copy, passthrough) really starts, by run mode and result (ok; failed: error or timeout; implausible: a keyframe after the seek or over 60 s before it). Not ok: the run reports the quantized seek, its timeline and subtitles off by up to a GOP.",
	}, []string{"mode", "result"})
	metricSessionSegmentsServed = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "session_segments_served",
		Help:      "Primary-stream segments served to a session over its life, observed when it is removed, by video route.",
		Buckets:   segmentsServedBuckets,
	}, []string{"route"})
)

// Every label value is registered at start so each series exists at 0 from
// the first scrape: a counter that first appears at 1 has no previous
// sample for increase()/rate() to compare against, and the very first
// failure of a kind — the one worth alerting on — would be invisible.
func init() {
	for _, o := range []string{runOutcomeFinished, runOutcomeReleasedIdle, runOutcomeKilled, runOutcomeFailed} {
		metricRunsTotal.WithLabelValues(o)
	}
	for _, r := range []string{ffmpegExitOK, ffmpegExitError, ffmpegExitSignal} {
		metricFFmpegExitsTotal.WithLabelValues(r)
	}
	for _, k := range []string{playlistKindVariant, playlistKindSubtitle} {
		metricPlaylistWaitSeconds.WithLabelValues(k)
		for _, o := range []string{playlistWaitOK, playlistWaitTimeout, playlistWaitNotRunning, playlistWaitCanceled} {
			metricPlaylistWaitsTotal.WithLabelValues(o, k)
		}
	}
	for _, o := range []string{probeOutcomeOK, probeOutcomeError} {
		metricSourceOpenSeconds.WithLabelValues(o)
	}
	for _, m := range runModes {
		metricRunSpeed.WithLabelValues(m)
		metricRunPauseSeconds.WithLabelValues(m)
		metricRunResumeSegmentSeconds.WithLabelValues(m)
		metricRunResumeStalls.WithLabelValues(m)
		for _, s := range []string{runStartZero, runStartSeek} {
			metricRunFirstSegmentSeconds.WithLabelValues(m, s)
		}
	}
	for _, c := range []string{failureSubtitleBitmap, failureNoDecoder, failureTimestamps, failureInvalidData, failureSource, failureSignal, failureOther} {
		metricFFmpegFailuresTotal.WithLabelValues(c)
	}
	for _, r := range []string{sourceProbeOK, sourceProbeFailed} {
		metricSourceProbeSeconds.WithLabelValues(r)
	}
	for _, m := range []string{runModeCopy, runModePassthrough} {
		for _, r := range []string{realStartOK, realStartFailed, realStartImplausible} {
			metricRunRealStartTotal.WithLabelValues(m, r)
		}
	}
	for _, r := range []string{videoRoutePassthrough, videoRouteCopy, videoRouteReencode, videoRouteAudio} {
		metricSessionSegmentsServed.WithLabelValues(r)
	}
	for _, f := range []string{codecsMismatchProfile, codecsMismatchTier, codecsMismatchLevel, codecsMismatchUnbuildable} {
		metricPassthroughCodecsMismatch.WithLabelValues(f)
	}
	// The route series a dashboard compares from the first scrape: every
	// reason on the two routes it can end in, the reasons a copy or audio
	// source can get, and the one way to passthrough.
	metricVideoRouteTotal.WithLabelValues(videoRoutePassthrough, reasonOK)
	for _, reason := range routeReasons {
		if reason == reasonOK {
			continue
		}
		metricVideoRouteTotal.WithLabelValues(videoRouteReencode, reason)
		metricVideoRouteTotal.WithLabelValues(videoRouteRefused, reason)
	}
	for _, reason := range []string{reasonNoDeclaration, reasonPassthroughOff, reasonNotHEVC} {
		metricVideoRouteTotal.WithLabelValues(videoRouteCopy, reason)
		metricVideoRouteTotal.WithLabelValues(videoRouteAudio, reason)
	}
}
