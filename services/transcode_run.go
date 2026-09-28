package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	runGracefulStopTimeout = 2 * time.Second
)

// TranscodeRun represents a single shared FFmpeg process transcoding a source
// from a specific seek position. Multiple sessions can share a run.
type TranscodeRun struct {
	key       string  // identity: runKeyFor(hashDir, h, seekTime)
	hashDir   string
	seekTime  float64
	// onRealStart, when set, reports a freshly resolved real start to the
	// run manager, which remembers it per key: the manager reaps idle runs
	// out from under 10-minute sessions, and a re-created run must report
	// the same offset — not re-probe and, on a cold source, fall back to
	// the quantized value, moving the playlist tag mid-session.
	onRealStart func(key string, v float64, probed bool)
	// realStart is the movie time media time 0 of this run actually maps
	// to. For a copy-mode video the input seek lands on the keyframe at or
	// before seekTime (-noaccurate_seek; for an MKV with B-frames at or
	// before seekTime - 3/23 s; for an MPEG-TS, without an index, often the
	// next one after it), so the run starts up to a GOP away from the
	// quantized value; every side-loaded subtitle track
	// shifted by the quantized offset then runs ahead of the sound by that
	// difference (measured 1.657 s on stage). Resolved once per run by
	// probeRunStart before FFmpeg is spawned. A separate resolved flag, not
	// a zero sentinel: 0.000 is a real answer (a file whose only keyframe
	// before a 30 s seek is the first frame), and reading it as "not
	// resolved" would report the quantized seek exactly there. Guarded by
	// mu; written only through resolveRealStartOnce.
	realStart         float64
	realStartResolved bool
	// realStartProbed says realStart is the probe's answer, not the
	// quantized seek the copy route keeps after a failed or implausible
	// one: only then is the run's zero known, and only then may its
	// subtitles be cut there (startLocked).
	realStartProbed bool
	realStartOnce   sync.Once
	outputDir string // {hashDir}/runs/[{variant}-]seek-{seekTime}/
	sourceURL string
	h         *HLS

	mu       sync.Mutex
	refCount int
	cmd      *exec.Cmd
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	running  bool

	// completed reports that FFmpeg walked the source to its end and exited
	// cleanly. Such a run is finished, not dead: every segment it will ever
	// produce is already on disk. Callers must not restart it — restarting
	// rewrites the playlist from segment zero, which is what turned a 22h
	// audiobook (copied in 5.5 minutes, 235x realtime) into a restart loop
	// that ended in "transcoder restart limit reached".
	completed bool

	// stopReason is the run outcome (runOutcome*) the stopper wants recorded
	// for the process it is about to signal; empty while nobody is stopping
	// it. Read by reapProcess after the exit, so a process that ends on its
	// own is told apart from one we ended, and the idle reaper from a
	// shutdown. Guarded by mu.
	stopReason string

	// started is when the current FFmpeg process was spawned. Guarded by mu.
	started time.Time

	// generation names the FFmpeg process whose files are in outputDir. A
	// new process gets a new one, made before its arguments (startLocked):
	// a restart writes the segments again, from zero, under the same names,
	// and a passthrough process's init segments carry it in their names.
	// Segment validators are made of it (segmentETag), so a copy of a file
	// written by another process -- of another run, or an earlier one of
	// this run -- never validates. Set at construction too, so no run is
	// without one. Guarded by mu.
	generation string

	// fallbacks are the ParamOptions this source turned out to need: set
	// when a run died on a failure they cure (adtsScalableError,
	// timestampsFailure), and preset by the run manager for every later run
	// of the same source. Guarded by mu.
	fallbacks ParamOptions
	// onFallbacks tells the run manager what this source needs on this
	// route (under fallbackKey).
	onFallbacks func(key string, opts ParamOptions)

	// demand is the furthest segment number any viewer has asked this run
	// for, -1 before the first request; paused whether pace has FFmpeg
	// frozen, pausedFor the total time it was. Guarded by mu.
	demand    int
	paused    bool
	pausedFor time.Duration
	// mediaDemand is, for a passthrough run, the furthest segment number a
	// viewer asked for per stream playlist (pacing_media.go); nil before
	// the first request. Guarded by mu.
	mediaDemand map[string]int

	// firstSegment is how long the current process took to its first
	// segment, 0 until then (watchFirstSegment). Guarded by mu.
	firstSegment time.Duration

	// lastFailure is the stderr tail of the last failure that was logged,
	// with per-process addresses stripped (see sameFailure). Only
	// reapProcess touches it, and one process is reaped at a time.
	lastFailure string

	// lifecycle
	runCtx    context.Context
	runCancel context.CancelFunc

	logger *log.Entry
}

func newTranscodeRun(key, hashDir string, seekTime float64, sourceURL string, h *HLS) *TranscodeRun {
	seekDir := fmt.Sprintf("seek-%.3f", seekTime)
	if v := h.runVariant(); v != "" {
		// A directory of its own: the old route's run at the same seek may
		// be writing next to it, on this pod or another of the node.
		seekDir = v + "-" + seekDir
	}
	outputDir := filepath.Join(hashDir, "runs", seekDir)
	runCtx, runCancel := context.WithCancel(context.Background())
	return &TranscodeRun{
		key:        key,
		hashDir:    hashDir,
		seekTime:   seekTime,
		outputDir:  outputDir,
		sourceURL:  sourceURL,
		h:          h,
		demand:     -1,
		generation: newRunGeneration(),
		runCtx:     runCtx,
		runCancel:  runCancel,
		logger: log.WithFields(log.Fields{
			"runKey": key,
		}),
	}
}

// AddRef increments the reference count.
func (r *TranscodeRun) AddRef() {
	r.mu.Lock()
	r.refCount++
	r.mu.Unlock()
}

// Release decrements the reference count and returns the new count.
func (r *TranscodeRun) Release() int {
	r.mu.Lock()
	r.refCount--
	n := r.refCount
	r.mu.Unlock()
	return n
}

// RefCount returns the current reference count.
func (r *TranscodeRun) RefCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refCount
}

// Start starts FFmpeg if not already running.
func (r *TranscodeRun) Start() error {
	// Before the lock: the probe shells out for up to 5 s, and r.mu is what
	// AddRef/RefCount take — the run manager holds its own global lock
	// across both, so a probe under r.mu stalled every Acquire and the
	// reaper (i.e. every session in the pod) for the probe's duration.
	r.resolveRealStartOnce()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked()
}

// resolveRealStartOnce resolves the run's real start exactly once per run
// object, without holding r.mu across the probe. Everything it reads
// (seekTime, sourceURL, h, runCtx) is immutable after construction. It
// runs on Start rather than on construction so a run preset from the
// manager's memory (see RunManager.Acquire) never probes at all — the
// offset a key once reported must not move when the run object is
// re-created, or every consumer of the playlist tag sees a new run.
func (r *TranscodeRun) resolveRealStartOnce() {
	r.realStartOnce.Do(func() {
		r.mu.Lock()
		done := r.realStartResolved
		r.mu.Unlock()
		if done || r.seekTime <= 0 || !r.isVideoCopy() {
			return
		}
		k, result := r.resolveRealStart()
		metricRunRealStartTotal.WithLabelValues(r.runMode(), result).Inc()
		// A passthrough run without an answer starts from the quantized
		// seek (what RealStart reports unresolved), as a copy run's seek
		// does, and keeps nothing: kept, one probe timing out on a cold
		// source fixed that offset -- up to a GOP off, subtitles with it --
		// for every later run of the key on the pod. The next run asks
		// again. The copy route keeps even a fallback, as it always has.
		if result != realStartOK && r.h.passthrough {
			return
		}
		probed := result == realStartOK
		r.mu.Lock()
		r.realStart = k
		r.realStartResolved = true
		r.realStartProbed = probed
		r.mu.Unlock()
		if r.onRealStart != nil {
			r.onRealStart(r.key, k, probed)
		}
	})
}

func (r *TranscodeRun) startLocked() error {
	if r.running {
		return nil
	}
	// A fresh process starts a fresh playlist, so any previous completion no
	// longer describes what is on disk.
	r.completed = false

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.Wrap(err, "ffmpeg not found")
	}

	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		return errors.Wrap(err, "failed to create run dir")
	}

	// The process's generation exists before its arguments: a passthrough
	// run names its init segments after it (passthrough_output.go).
	gen := newRunGeneration()
	params, err := r.h.ffmpegParamsFor(r.outputDir, r.fallbacks, gen)
	if err != nil {
		return errors.Wrap(err, "failed to get ffmpeg params")
	}

	params = redirectSegmentListParams(params)

	if r.seekTime > 0 {
		if r.h.passthrough {
			// resolveRealStartOnce ran before the lock (Start); without an
			// answer the real start is the quantized seek, the audio is
			// not cut at it, and the run behaves as a copy run's seek does.
			realStart, trimAudio := r.seekTime, []string(nil)
			if r.realStartResolved {
				realStart, trimAudio = r.realStart, r.h.passthroughAudioMaps()
			}
			params = injectPassthroughSeekParams(params, r.seekTime, realStart, trimAudio)
		} else if r.isVideoCopy() {
			// resolveRealStartOnce ran before the lock (Start); the copy
			// route keeps even a fallback, and without any the real start
			// is the quantized seek. The subtitles are cut only at a zero
			// the probe found: after a fallback the run's zero is not
			// known, and cut at the quantized seek they lost every cue
			// between the keyframe and it and ran early by the distance
			// (measured 10 s on a 10 s GOP, where the argv without the cut
			// serves them all 1.083 s early, as before the cut existed).
			realStart, cut := r.seekTime, []string(nil)
			if r.realStartResolved {
				realStart = r.realStart
			}
			if r.realStartProbed {
				cut = r.h.subtitleOutputMaps()
			}
			params = injectCopySeekParams(params, r.seekTime, realStart, cut)
		} else {
			params = injectSeekParams(params, r.seekTime, false)
			// The accurate seek trims only what is decoded: the outputs
			// that are not are cut at the seek point on the output side.
			params = cutAtOutputStart(params, r.h.reencodeSeekCuts(r.fallbacks))
		}
		// Remove -xerror when seeking: AVI and other containers may produce
		// non-fatal errors during seek that -xerror would treat as fatal.
		params = removeParam(params, "-xerror")
	}

	r.ctx, r.cancel = context.WithCancel(r.runCtx)
	r.done = make(chan struct{})

	r.logger.WithFields(log.Fields{
		"seekTime": fmt.Sprintf("%.3f", r.seekTime),
		"params":   redactSecrets(strings.Join(params, " ")),
	}).Info("run: starting ffmpeg")

	r.cmd = exec.CommandContext(r.ctx, ffmpegPath, params...)
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	outLog, err := os.Create(filepath.Join(r.outputDir, "ffmpeg.out"))
	if err != nil {
		close(r.done)
		return errors.Wrap(err, "failed to create ffmpeg stdout log")
	}
	errLog, err := os.Create(filepath.Join(r.outputDir, "ffmpeg.err"))
	if err != nil {
		outLog.Close()
		close(r.done)
		return errors.Wrap(err, "failed to create ffmpeg stderr log")
	}
	r.cmd.Stdout = outLog
	r.cmd.Stderr = errLog

	if err := r.cmd.Start(); err != nil {
		outLog.Close()
		errLog.Close()
		close(r.done)
		return errors.Wrap(err, "failed to start ffmpeg")
	}

	r.logger.WithFields(log.Fields{
		"pid":      r.cmd.Process.Pid,
		"seekTime": fmt.Sprintf("%.3f", r.seekTime),
	}).Info("run: ffmpeg started")
	r.watchProcessGenLocked(gen, outLog, errLog)

	return nil
}

// watchProcessLocked marks the process just started into r.cmd as running
// and reaps it in the background; the closers (its log files) are closed
// once it is gone. Caller holds mu. Split from startLocked so a test can
// put any process where FFmpeg goes and exercise the same bookkeeping.
func (r *TranscodeRun) watchProcessLocked(closers ...io.Closer) {
	r.watchProcessGenLocked(newRunGeneration(), closers...)
}

// watchProcessGenLocked is watchProcessLocked for a process whose
// generation was made before it started (startLocked).
func (r *TranscodeRun) watchProcessGenLocked(gen string, closers ...io.Closer) {
	r.running = true
	r.stopReason = ""
	r.started = time.Now()
	r.generation = gen
	r.demand = -1
	r.mediaDemand = nil
	r.pausedFor = 0
	metricRunsActive.Inc()
	if r.h != nil && r.h.cfg != nil && r.h.cfg.paceLead > 0 && len(r.h.primary) > 0 && r.cmd != nil && r.cmd.Process != nil {
		tm := currentPaceTiming()
		if r.h.passthrough {
			go r.paceMedia(r.cmd.Process.Pid, r.h.cfg.paceLead, r.done, tm)
		} else {
			go r.pace(r.cmd.Process.Pid, r.h.cfg.paceLead, r.done, r.runMode(), tm)
		}
	}
	if mode := r.runMode(); mode != "" {
		start := runStartZero
		if r.seekTime > 0 {
			start = runStartSeek
		}
		r.firstSegment = 0
		go watchFirstSegment(r.h.primary[0].GetPlaylistPath(r.outputDir)+".ffmpeg", r.started, r.done, mode, start, func(d time.Duration) {
			r.mu.Lock()
			r.firstSegment = d
			r.mu.Unlock()
		})
	}
	go r.reapProcess(closers...)
}

// minSpeedMediaSeconds is how much media a run must have produced for its
// speed to be recorded (see reapProcess).
const minSpeedMediaSeconds = 30

// firstSegmentPoll is how often watchFirstSegment looks for the playlist.
const firstSegmentPoll = 200 * time.Millisecond

// watchFirstSegment records how long the process started at started took to
// close the first segment of the primary stream: FFmpeg's segment muxer
// writes the playlist file only then. A restart reuses the run dir, so a
// playlist left by the previous process does not count -- only one written
// after started. Gives up when the process ends first.
func watchFirstSegment(playlist string, started time.Time, done <-chan struct{}, mode, start string, found func(time.Duration)) {
	t := time.NewTicker(firstSegmentPoll)
	defer t.Stop()
	for {
		if fi, err := os.Stat(playlist); err == nil && !fi.ModTime().Before(started) {
			d := time.Since(started)
			metricRunFirstSegmentSeconds.WithLabelValues(mode, start).Observe(d.Seconds())
			found(d)
			return
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

// runMode is the run's metrics mode (runMode* constants), or "" when the run
// has no HLS description (tests).
func (r *TranscodeRun) runMode() string {
	if r.h == nil || len(r.h.primary) == 0 {
		return ""
	}
	if r.h.passthrough {
		return runModePassthrough
	}
	for _, s := range r.h.primary {
		if s.st == Video {
			if s.IsCopy() {
				return runModeCopy
			}
			return runModeReencode
		}
	}
	return runModeAudio
}

// reapProcess waits for the FFmpeg process started by startLocked, records
// how and why it ended, then closes done. It is the one place a process's
// end is counted: every stop path ends here too, so the counters add up to
// the number of processes. The closers are the process's log files.
func (r *TranscodeRun) reapProcess(closers ...io.Closer) {
	for _, c := range closers {
		defer c.Close()
	}
	defer close(r.done)
	waitErr := r.cmd.Wait()

	r.mu.Lock()
	stopReason := r.stopReason
	started := r.started
	firstSegment := r.firstSegment
	pausedFor := r.pausedFor
	if waitErr == nil {
		r.completed = true
	}
	r.mu.Unlock()

	outcome, reason := classifyExit(waitErr, stopReason)
	metricRunsActive.Dec()
	metricRunsTotal.WithLabelValues(outcome).Inc()
	metricFFmpegExitsTotal.WithLabelValues(reason).Inc()

	tail := stderrTail(filepath.Join(r.outputDir, "ffmpeg.err"))
	// Speed at the end, whatever ended it: FFmpeg's figure is cumulative
	// (media time over wall time since start). Under 30 s of media it is
	// mostly startup and says little about keeping up.
	mode := r.runMode()
	media, speed, progressOK := lastProgress(tail)
	// FFmpeg's speed counts wall time, the time pace held it frozen too;
	// the transcoder's speed is over the time it was allowed to run.
	if s, ok := activeSpeed(media, time.Since(started), pausedFor); progressOK && ok {
		speed = s
	}
	if mode != "" && progressOK && media >= minSpeedMediaSeconds {
		metricRunSpeed.WithLabelValues(mode).Observe(speed)
	}
	// One line per process, whatever ended it: the per-run numbers the
	// histograms aggregate, with the source path (credentials redacted) so a
	// run can be joined with how torrent-http-proxy served its source.
	endFields := log.Fields{
		"outcome":  outcome,
		"mode":     mode,
		"seekTime": fmt.Sprintf("%.3f", r.seekTime),
		"ranFor":   time.Since(started).Round(100 * time.Millisecond).String(),
		"source":   redactSecrets(r.sourceURL),
	}
	if firstSegment > 0 {
		endFields["firstSegment"] = firstSegment.Round(10 * time.Millisecond).String()
	}
	if progressOK {
		endFields["media"] = fmt.Sprintf("%.1f", media)
		endFields["speed"] = speed
	}
	if pausedFor > 0 {
		endFields["paused"] = pausedFor.Round(time.Second).String()
	}
	r.logger.WithFields(endFields).Info("run: ffmpeg ended")

	switch {
	case outcome == runOutcomeFailed:
		// FFmpeg gave up on its own. Its reason is only in ffmpeg.err, which
		// the next auto-restart truncates and the cleanup removes: until this
		// was logged, 15% of runs failed with no trace of why. Read before
		// done closes, so no restart can have truncated it yet.
		metricFFmpegFailuresTotal.WithLabelValues(failureCause(tail, reason)).Inc()
		// A copied AAC stream the mpegts muxer cannot wrap: every restart
		// would die the same way. The next start encodes the audio instead
		// (a restart the player's next request triggers anyway), and so do
		// later runs of the source.
		r.mu.Lock()
		before := r.fallbacks
		if strings.Contains(tail, adtsScalableError) {
			r.fallbacks.EncodeAudio = true
		}
		// Seek runs never have -xerror, so for them there is nothing to drop.
		if r.seekTime == 0 && timestampsFailure(tail) {
			r.fallbacks.Lenient = true
		}
		after := r.fallbacks
		r.mu.Unlock()
		if after != before {
			r.logger.WithFields(log.Fields{
				"encodeAudio": after.EncodeAudio,
				"lenient":     after.Lenient,
			}).Warn("run: switching FFmpeg options for this source from the next start")
			if r.onFallbacks != nil {
				r.onFallbacks(fallbackKey(r.hashDir, r.h), after)
			}
		}
		// A source that cannot be converted fails the same way on each of
		// its 5 auto-restarts: logged once, and again only if it changes.
		fields := log.Fields{
			"seekTime": fmt.Sprintf("%.3f", r.seekTime),
			"ranFor":   time.Since(started).Round(100 * time.Millisecond).String(),
			"stderr":   tail,
		}
		if key := failureKey(tail); key != r.lastFailure {
			r.lastFailure = key
			r.logger.WithError(waitErr).WithFields(fields).Warn("run: ffmpeg failed")
		} else {
			r.logger.WithError(waitErr).WithFields(fields).Debug("run: ffmpeg failed again the same way")
		}
	case waitErr != nil:
		// Stopped by us (released, killed): the exit status is our signal.
		r.logger.WithError(waitErr).WithField("outcome", outcome).Debug("run: ffmpeg exited with error")
	default:
		r.logger.Info("run: ffmpeg finished normally")
	}
}

// classifyExit maps a process exit to the run outcome and the exit reason.
// A clean exit is finished whoever asked for it (the segments are all
// there). Otherwise the outcome is the stopper's, or failed when nobody was
// stopping it: an error exit is FFmpeg giving up on the source, a signal we
// did not send is the kernel (OOM) or the node.
func classifyExit(waitErr error, stopReason string) (outcome, reason string) {
	if waitErr == nil {
		return runOutcomeFinished, ffmpegExitOK
	}
	reason = ffmpegExitError
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			reason = ffmpegExitSignal
		}
	}
	if stopReason != "" {
		return stopReason, reason
	}
	return runOutcomeFailed, reason
}

// Stop stops FFmpeg.
func (r *TranscodeRun) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(runOutcomeKilled)
}

// stopLocked stops FFmpeg, recording reason as the run's outcome (see
// stopReason). Caller holds mu.
func (r *TranscodeRun) stopLocked(reason string) {
	if !r.running {
		return
	}

	// Check if already exited
	if r.done != nil {
		select {
		case <-r.done:
			r.running = false
			return
		default:
		}
	}

	r.stopReason = reason
	r.cancel()

	if r.cmd != nil && r.cmd.Process != nil {
		pid := r.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)

		r.mu.Unlock()
		select {
		case <-r.done:
		case <-time.After(runGracefulStopTimeout):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-r.done
		}
		r.mu.Lock()
	} else if r.done != nil {
		r.mu.Unlock()
		<-r.done
		r.mu.Lock()
	}

	r.running = false
}

// Cleanup stops FFmpeg and removes the output directory.
func (r *TranscodeRun) Cleanup() {
	r.cleanup(runOutcomeKilled)
}

// cleanup is Cleanup with the run outcome to record if FFmpeg is still
// running: the idle reaper passes released_idle, everything else is killed.
func (r *TranscodeRun) cleanup(reason string) {
	r.mu.Lock()
	r.stopLocked(reason)
	r.runCancel()
	r.mu.Unlock()

	if err := os.RemoveAll(r.outputDir); err != nil {
		r.logger.WithError(err).Warn("run: failed to remove output dir")
	}
	r.logger.Info("run: cleaned up")
}

// IsCompleted reports whether FFmpeg finished the source cleanly. Distinct
// from IsRunning: both are false for a run that was killed or released, and
// only the completed one has a whole playlist behind it.
func (r *TranscodeRun) IsCompleted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completed
}

// IsRunning returns true if FFmpeg is currently running.
func (r *TranscodeRun) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running && r.done != nil {
		select {
		case <-r.done:
			r.running = false
		default:
		}
	}
	return r.running
}

// OutputDir returns the directory where segments are written.
func (r *TranscodeRun) OutputDir() string {
	return r.outputDir
}

// Generation names the FFmpeg process that writes the run's files now (see
// generation).
func (r *TranscodeRun) Generation() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

// newRunGeneration returns a name no other run process gets: not on this
// pod, and not on another pod of the node, which shares the output dir.
func newRunGeneration() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isVideoCopy returns true if the primary video stream uses copy mode.
// RealStart is the movie time this run's media time 0 maps to: the
// keyframe an input seek with -noaccurate_seek actually landed on for a
// copy-mode video, and the exact seek time everywhere else (re-encode uses
// an accurate seek, and an unseeked run starts at zero).
func (r *TranscodeRun) RealStart() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.realStartResolved {
		return r.realStart
	}
	return r.seekTime
}

// realStartSpan bounds how far from the quantized seek a run can plausibly
// start: a broken index can name anything, and 60 s is well past any GOP
// we transcode.
const realStartSpan = 60

// resolveRealStart asks probeRunStart for the keyframe and falls back to
// the quantized seek time on any answer that cannot be right: an error, a
// time before the file, or one implausibly far from the seek point
// (realStartSpan) -- and, for passthrough, any time after it. result says
// which (realStart*).
//
// A copy-route answer after the seek point is where the run starts: the
// probe seeks exactly as the run does (copySeekInput), and a format without
// an index (MPEG-TS) lands on the next keyframe after it
// (ffmpegSeekFirstFrame). Refused, a seek to 35 on a TS with keyframes at
// 25 and 35 reported 30.000 for a run starting at 35.021: every
// side-loaded cue 5 s early. Passthrough's -itsoffset only moves the zero
// back (injectPassthroughSeekParams), and it keeps its old guard.
func (r *TranscodeRun) resolveRealStart() (float64, string) {
	stream := "v:0"
	if r.h != nil {
		if sp := r.h.primaryVideoStreamSpecifier(); sp != "" {
			stream = sp
		}
	}
	probe := probeRunStart
	if r.h != nil && r.h.passthrough {
		// The same seek, its own answer: a passthrough run counts from the
		// first DTS (its -itsoffset), the copy route's TS from the first
		// frame (ffmpegSeekFirstFrame).
		probe = probePassthroughStart
	}
	k, err := probe(r.runCtx, r.sourceURL, stream, r.seekTime)
	if err != nil {
		r.logger.WithError(err).Warn("run: failed to resolve the real start, reporting the quantized seek")
		return r.seekTime, realStartFailed
	}
	after := float64(realStartSpan)
	if r.h != nil && r.h.passthrough {
		after = 0
	}
	if k < 0 || k-r.seekTime > after || r.seekTime-k > realStartSpan {
		r.logger.WithField("keyframe", fmt.Sprintf("%.3f", k)).Warn("run: implausible keyframe, reporting the quantized seek")
		return r.seekTime, realStartImplausible
	}
	return k, realStartOK
}

func (r *TranscodeRun) isVideoCopy() bool {
	if r.h == nil {
		return false
	}
	for _, s := range r.h.primary {
		if s.st == Video && s.IsCopy() {
			return true
		}
	}
	return false
}

// removeParam removes all occurrences of a flag from the params list.
func removeParam(params []string, flag string) []string {
	result := make([]string, 0, len(params))
	for _, p := range params {
		if p != flag {
			result = append(result, p)
		}
	}
	return result
}

// injectSeekParams adds -ss before -i (input-level seek).
//
// For copy-mode video: adds -noaccurate_seek so both video (copy) and audio
// (re-encode) start from the same keyframe → A/V sync (injectCopySeekParams
// builds the copy route's seek on it).
//
// For re-encode mode: just -ss (accurate seek). FFmpeg decodes from the nearest
// keyframe and discards what comes before the target -- but only for the
// streams it decodes: the trim is a filter at the input of the stream's
// filter graph (fftools/ffmpeg_filter.c insert_trim). A copied stream has no
// graph and starts at the keyframe the demuxer landed on, up to a GOP
// earlier, and so do subtitles; the run cuts those outputs itself
// (cutAtOutputStart).
//
// Input-level -ss is always used because output-level -ss (after -i) causes
// video segments to appear much later than audio when re-encoding.
func injectSeekParams(params []string, seekSec float64, videoCopy bool) []string {
	result := make([]string, 0, len(params)+4)
	seekStr := fmt.Sprintf("%.3f", seekSec)

	for i := 0; i < len(params); i++ {
		if params[i] == "-i" {
			if videoCopy {
				// Exactly what probeRunStart asks FFmpeg with.
				result = append(result, copySeekInput(seekSec)...)
			} else {
				result = append(result, "-ss", seekStr)
			}
		}
		result = append(result, params[i])
	}

	return result
}

// injectCopySeekParams is the seek of a copy-route run: the input seek
// (copySeekInput, exactly what probeRunStart asks FFmpeg with: -noaccurate_seek
// starts the copied video and the audio from the same keyframe), every
// output's time zero moved from the quantized seek to realStart, and the
// outputs whose -map is in cut (the subtitles) started at that zero.
//
// Without the offset every output takes the quantized time as zero and
// shifts its own negative timestamps away (libavformat/mux.c,
// avoid_negative_ts): the video and the audio begin at the keyframe the
// offset names, the subtitles at the first cue the demuxer hands over or
// the quantized time -- cues early against #EXT-X-SESSION-OFFSET by up to a
// GOP. A run that landed on the file's first frame (offset 0.000, the only
// keyframe before the seek point is the first) served every cue early by
// the first cue's time, and subtitle-translate reads offset 0 as the run
// from the start, whose translation it stores for good. Measured on FFmpeg
// 8.1.2, cues at 1, 21, 26 and 33 s on an MKV with keyframes at 0 and 30:
// a seek to 30 served them at 0, 20, 25 and 32 with the offset 0.000; with
// -itsoffset 30 and the cut at 1, 21, 26 and 33 -- the playlist and every
// segment byte for byte the run from the start's. Keyframes every 10 s,
// the same seek (run at 20): 21, 26, 33 s from 0, 5, 12 to 1, 6, 13.
//
// The video is not changed by it: realStart is the video's first PTS, so
// its first DTS is still negative by the B-frame delay and its output
// shifts that away as it shifted the larger one before -- the same
// timestamps. Nor is the audio where the demuxer's seek hands it over from
// a little before the keyframe (measured: the first AAC packet at 19.925
// for the keyframe at 20.000, every video and audio segment and playlist
// byte for byte the same with and without the offset). Where the first
// audio packet comes after the keyframe (audio starting later than the
// video; no B-frames) the audio output no longer shifts to zero and counts
// from realStart, as in the run from the start: measured, a copied track
// starting 0.5 s after the video used to play 0.479 s early, and a run
// landing on 0 now serves the run from the start's audio byte for byte. -ss 0 on a
// subtitle output drops the cues that start before the zero, encoded
// (fftools/ffmpeg_enc.c do_subtitle_out) or copied webvtt (ffmpeg_mux.c
// of_streamcopy) alike, and shifts neither: with the offset those are the
// cues before the keyframe, which would otherwise be negative and move
// every cue after them (mov_text in MP4 seeks to the cue on screen, with
// its own start). The one on screen at the keyframe is lost with them, as
// on the re-encode route. Measured for SRT, ASS, copied WebVTT and mov_text:
// every cue served at its movie time minus the offset.
//
// Only a zero moved back (realStart before the seek point), as passthrough
// does (injectPassthroughSeekParams). When the run starts after the seek
// point (MPEG-TS, see resolveRealStart) the demuxer hands over the audio
// from before the keyframe -- measured on a TS, the first AAC packet at
// 29.739 for a seek to 30 that started the video at 35.021 -- and a zero
// moved forward to the keyframe made that audio negative: its output
// shifted the whole track, 5.2 s late against the picture. There the cut
// still keeps the cues from before the seek point from moving the rest,
// and the cues run late against the offset by the distance to the keyframe,
// as they did before.
func injectCopySeekParams(params []string, seek, realStart float64, cut []string) []string {
	params = injectSeekParams(params, seek, true)
	if d := seek - realStart; d > 0 {
		result := make([]string, 0, len(params)+2)
		for _, p := range params {
			if p == "-i" {
				result = append(result, "-itsoffset", fmt.Sprintf("%.6f", d))
			}
			result = append(result, p)
		}
		params = result
	}
	return cutAtOutputStart(params, cut)
}

// cutAtOutputStart puts -ss 0 before the -map of every output whose map is
// in maps: an output start time of 0, counted like every timestamp of the
// run from its zero (the input seek point, or the copy route's realStart
// by -itsoffset), so that output drops what the demuxer hands it from
// before that point -- a copied packet whose DTS is below it
// (fftools/ffmpeg_mux.c of_streamcopy), a subtitle whose start is
// (ffmpeg_enc.c do_subtitle_out). Neither is shifted by it: 0 is where the
// output starts. The same placement as the passthrough route's audio cut
// (injectPassthroughSeekParams).
func cutAtOutputStart(params []string, maps []string) []string {
	if len(maps) == 0 {
		return params
	}
	cut := make(map[string]bool, len(maps))
	for _, m := range maps {
		cut[m] = true
	}
	result := make([]string, 0, len(params)+2*len(maps))
	for i, p := range params {
		if p == "-map" && i+1 < len(params) && cut[params[i+1]] {
			result = append(result, "-ss", "0")
		}
		result = append(result, p)
	}
	return result
}

// reencodeSeekCuts are the -map values of the outputs a seek run of a
// re-encoded video cuts at the seek point (cutAtOutputStart), with the
// run's current options: every audio track that is copied (the decision of
// audioOutputFor, through codecParams: AAC up to 2 channels, up to 6 with
// aac51), and every subtitle output.
//
// The input seek lands on the keyframe at or before the seek point (for an
// MKV with B-frames at or before the seek point minus 3/23 s,
// fftools/ffmpeg_demux.c dts_heuristic), and everything from there on is
// counted from the seek point, so what comes before it is negative. The
// re-encoded video is trimmed to the seek point; a copied AAC track is not,
// and its output -- a segment muxer of its own, which cannot take negative
// timestamps -- shifts its first packet to zero (libavformat/mux.c,
// avoid_negative_ts make_non_negative). Measured on FFmpeg 8.1.2, a seek to
// 35 (run at 30) on a 10 s GOP MKV: the copied audio started at movie
// 19.755, the video at 30.000, and hls.js, which places both by their PTS,
// played the sound 10.16 s late for the whole run, the audio playlist
// 10 s longer than the video's. With the cut: -83 ms, against -62 ms from
// the start. An encoded track goes through the trim like the video
// (EncodeAudio makes every track one) and is left alone. An audio-only
// source has no h.audio (its track is the primary) and no h.subs, and so
// no cut.
//
// Subtitles never go through a filter graph, encoded to webvtt or copied,
// and matroskadec does not skip a subtitle block before the keyframe it
// seeks to (matroskadec.c, skip_to_keyframe is for the other tracks): the
// cues between where the demuxer landed and the seek point reach the
// output with negative times, and the output shifts them to zero like the
// audio's. Every cue after them comes late by the same amount against
// #EXT-X-SESSION-OFFSET, which on this route is the seek point: measured on
// 8.1.2, cues at 21 and 26 s were served after a seek to 35 (run at 30),
// the one at 33 s shown at 12.000 instead of 3.000 -- 9 s late, in hls.js
// and in subtitle-translate's movie time (cue + offset) alike. With the cut
// an encoded cue that starts before zero is dropped (ffmpeg_enc.c
// do_subtitle_out), a copied webvtt one like the audio: the cues land on
// the offset's timeline, and the one still on screen at the seek point is
// lost (FFmpeg compares the cue's start).
func (h *HLS) reencodeSeekCuts(opts ParamOptions) []string {
	var maps []string
	for _, a := range h.audio {
		if c := a.codecParams(opts); c[len(c)-1] == "copy" {
			maps = append(maps, fmt.Sprintf("0:%d", a.s.GetIndex()))
		}
	}
	return append(maps, h.subtitleOutputMaps()...)
}

// subtitleOutputMaps are the -map values of the session's subtitle
// outputs; a track without a decoder has none (ffmpegParamsFor).
func (h *HLS) subtitleOutputMaps() []string {
	var maps []string
	for _, s := range h.subs {
		if s.hasTextDecoder() {
			maps = append(maps, fmt.Sprintf("0:%d", s.s.GetIndex()))
		}
	}
	return maps
}
