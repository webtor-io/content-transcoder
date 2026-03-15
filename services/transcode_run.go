package services

import (
	"context"
	"fmt"
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
	key       string  // identity: hashDir + ":seek:" + seekTime
	hashDir   string
	seekTime  float64
	outputDir string // {hashDir}/runs/seek-{seekTime}/
	sourceURL string
	h         *HLS

	mu       sync.Mutex
	refCount int
	cmd      *exec.Cmd
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	running  bool

	// lifecycle
	runCtx    context.Context
	runCancel context.CancelFunc

	logger *log.Entry
}

func newTranscodeRun(key, hashDir string, seekTime float64, sourceURL string, h *HLS) *TranscodeRun {
	seekDir := fmt.Sprintf("seek-%.3f", seekTime)
	outputDir := filepath.Join(hashDir, "runs", seekDir)
	runCtx, runCancel := context.WithCancel(context.Background())
	return &TranscodeRun{
		key:       key,
		hashDir:   hashDir,
		seekTime:  seekTime,
		outputDir: outputDir,
		sourceURL: sourceURL,
		h:         h,
		runCtx:    runCtx,
		runCancel: runCancel,
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked()
}

func (r *TranscodeRun) startLocked() error {
	if r.running {
		return nil
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.Wrap(err, "ffmpeg not found")
	}

	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		return errors.Wrap(err, "failed to create run dir")
	}

	params, err := r.h.GetFFmpegParams(r.outputDir)
	if err != nil {
		return errors.Wrap(err, "failed to get ffmpeg params")
	}

	params = redirectSegmentListParams(params)

	if r.seekTime > 0 {
		params = injectSeekParams(params, r.seekTime, r.isVideoCopy())
		// Remove -xerror when seeking: AVI and other containers may produce
		// non-fatal errors during seek that -xerror would treat as fatal.
		params = removeParam(params, "-xerror")
	}

	r.ctx, r.cancel = context.WithCancel(r.runCtx)
	r.done = make(chan struct{})

	r.logger.WithFields(log.Fields{
		"seekTime": fmt.Sprintf("%.3f", r.seekTime),
		"params":   strings.Join(params, " "),
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

	r.running = true
	r.logger.WithFields(log.Fields{
		"pid":      r.cmd.Process.Pid,
		"seekTime": fmt.Sprintf("%.3f", r.seekTime),
	}).Info("run: ffmpeg started")

	go func() {
		defer outLog.Close()
		defer errLog.Close()
		defer close(r.done)
		waitErr := r.cmd.Wait()
		if waitErr != nil {
			r.logger.WithError(waitErr).Debug("run: ffmpeg exited with error")
		} else {
			r.logger.Info("run: ffmpeg finished normally")
		}
	}()

	return nil
}

// Stop stops FFmpeg.
func (r *TranscodeRun) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *TranscodeRun) stopLocked() {
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
	r.mu.Lock()
	r.stopLocked()
	r.runCancel()
	r.mu.Unlock()

	if err := os.RemoveAll(r.outputDir); err != nil {
		r.logger.WithError(err).Warn("run: failed to remove output dir")
	}
	r.logger.Info("run: cleaned up")
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

// isVideoCopy returns true if the primary video stream uses copy mode.
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
// (re-encode) start from the same keyframe → A/V sync.
//
// For re-encode mode: just -ss (accurate seek). FFmpeg decodes from the nearest
// keyframe and discards frames before the target, then starts encoding.
// Both streams start from the exact position → perfect sync.
//
// Input-level -ss is always used because output-level -ss (after -i) causes
// video segments to appear much later than audio when re-encoding.
func injectSeekParams(params []string, seekSec float64, videoCopy bool) []string {
	result := make([]string, 0, len(params)+4)
	seekStr := fmt.Sprintf("%.3f", seekSec)

	for i := 0; i < len(params); i++ {
		if params[i] == "-i" {
			result = append(result, "-ss", seekStr)
			if videoCopy {
				result = append(result, "-noaccurate_seek")
			}
		}
		result = append(result, params[i])
	}

	return result
}
