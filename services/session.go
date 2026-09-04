package services

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	sessionSegDuration = 4
	// seekQuantum defines the granularity of seek positions. Seek times are
	// rounded down to the nearest multiple of this value. This ensures that
	// viewers seeking to nearby positions share the same TranscodeRun (same
	// FFmpeg process, same segments on disk). 30s means max imprecision of
	// ~30s + keyframe gap, which is imperceptible in practice.
	seekQuantum = 30.0 // seconds
	// maxConsecutiveRestarts caps auto-restarts (EnsureRunning /
	// RestartForSegment) between successful segment reads. Without a cap a
	// session whose source keeps killing FFmpeg loops forever: the player
	// re-requests the segment, Touch() keeps the session alive, and each
	// attempt spawns a doomed FFmpeg (observed running for 34h straight).
	maxConsecutiveRestarts = 5
)

// ErrRestartLimit is returned by EnsureRunning/RestartForSegment once
// maxConsecutiveRestarts failed attempts accumulate without a single
// successfully produced segment. An explicit Seek or Start resets the
// counter — user action is allowed to try again.
var ErrRestartLimit = errors.New("too many failed auto-restarts")

// quantizeSeekTime rounds seek time down to the nearest seekQuantum boundary.
// seekTime=0 is never quantized (always starts from the beginning).
func quantizeSeekTime(t float64) float64 {
	if t <= 0 {
		return 0
	}
	return float64(int(t/seekQuantum)) * seekQuantum
}

// segPrefixPattern extracts the prefix and number from a segment filename.
// E.g., "v0-720-5.ts" → prefix="v0-720", num=5; "a0-42.ts" → prefix="a0", num=42.
var segPrefixPattern = regexp.MustCompile(`^([asv]\d+(?:-\d+)?)-(\d+)\.(ts|vtt)$`)

// redirectSegmentListParams redirects -segment_list to a .ffmpeg suffix
// so FFmpeg writes playlists with real durations to a separate file.
func redirectSegmentListParams(params []string) []string {
	result := make([]string, 0, len(params))
	for i := 0; i < len(params); i++ {
		if params[i] == "-segment_list" && i+1 < len(params) {
			result = append(result, params[i])
			i++
			result = append(result, params[i]+".ffmpeg")
			continue
		}
		result = append(result, params[i])
	}
	return result
}

// Session represents a single transcoding session.
// It delegates FFmpeg management to a shared TranscodeRun via RunManager.
// Multiple sessions for the same source+seekTime share one FFmpeg process.
type Session struct {
	id        string
	mu        sync.Mutex
	sourceURL string
	hashDir   string
	outputDir string // session-specific dir for master playlist: {hashDir}/sessions/{id}/
	h         *HLS
	duration  float64
	seekTime  float64
	lastAccess time.Time

	// Shared FFmpeg run
	run    *TranscodeRun
	runMgr *RunManager

	// Consecutive auto-restart attempts since the last successful segment
	// read (or explicit Start/Seek). Guarded by mu.
	restartFails int

	// capWarnOnce dedupes the "restart limit reached" warning: the message
	// is answered on EVERY playlist/segment request to a capped session, and
	// a single capped session was measured at 10-19K identical log lines per
	// day. One line per session carries the same signal.
	capWarnOnce sync.Once

	// Lifecycle
	closed bool
	logger *log.Entry
}

// SessionConfig holds parameters for creating a new session.
type SessionConfig struct {
	ID        string
	SourceURL string
	HashDir   string
	HLS       *HLS
	Duration  float64
	RunMgr    *RunManager
}

func NewSession(cfg SessionConfig) *Session {
	outputDir := filepath.Join(cfg.HashDir, "sessions", cfg.ID)
	return &Session{
		id:         cfg.ID,
		sourceURL:  cfg.SourceURL,
		hashDir:    cfg.HashDir,
		outputDir:  outputDir,
		h:          cfg.HLS,
		duration:   cfg.Duration,
		lastAccess: time.Now(),
		runMgr:     cfg.RunMgr,
		logger: log.WithFields(log.Fields{
			"sessionID": cfg.ID,
		}),
	}
}

// Start acquires a shared run at the given seek time.
func (s *Session) Start(seekTime float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session is closed")
	}

	s.seekTime = quantizeSeekTime(seekTime)
	s.restartFails = 0
	return s.acquireRunLocked()
}

// acquireRunLocked acquires a shared TranscodeRun for the current seekTime.
func (s *Session) acquireRunLocked() error {
	run, err := s.runMgr.Acquire(s.hashDir, s.seekTime, s.sourceURL, s.h)
	if err != nil {
		return err
	}
	s.run = run
	return nil
}

// releaseRunLocked releases the current run if any.
func (s *Session) releaseRunLocked() {
	if s.run != nil {
		s.runMgr.Release(s.run)
		s.run = nil
	}
}

// Seek releases the current run and acquires a new one at the new position.
func (s *Session) Seek(seekTime float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session is closed")
	}

	seekTime = quantizeSeekTime(seekTime)
	s.logger.WithField("seekTime", fmt.Sprintf("%.3f", seekTime)).Info("session: seeking")

	oldRun := s.run
	oldSeekTime := s.seekTime
	s.run = nil
	s.seekTime = seekTime
	s.restartFails = 0
	s.lastAccess = time.Now()

	if err := s.acquireRunLocked(); err != nil {
		// Restore old state on failure
		s.seekTime = oldSeekTime
		s.run = oldRun
		return err
	}

	// Release old run only after successful acquire
	if oldRun != nil {
		s.runMgr.Release(oldRun)
	}
	return nil
}

// Stop releases the run (FFmpeg may continue for other sessions).
func (s *Session) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseRunLocked()
}

// Close releases the run and removes the session's master playlist directory.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	s.releaseRunLocked()

	// Remove session directory (master playlist only)
	if err := os.RemoveAll(s.outputDir); err != nil {
		s.logger.WithError(err).Warn("session: failed to remove session dir")
	}

	s.logger.Info("session: closed")
}

// Touch updates the last access timestamp.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

// noteSegmentServed resets the auto-restart budget: a produced segment is
// proof the current run works, so earlier failed attempts stop counting.
func (s *Session) noteSegmentServed() {
	s.mu.Lock()
	s.restartFails = 0
	s.mu.Unlock()
}

// IsRunning returns true if the shared FFmpeg run is currently running.
func (s *Session) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run == nil {
		return false
	}
	return s.run.IsRunning()
}

// IsClosed returns true if the session has been closed.
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// SeekTime returns the current quantized seek offset in seconds.
func (s *Session) SeekTime() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seekTime
}

// LastAccess returns the last access timestamp.
func (s *Session) LastAccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccess
}

// EnsureRunning re-acquires the FFmpeg run if it's not currently running.
// Used by playlist handlers to restart after inactivity-based release.
func (s *Session) EnsureRunning() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session is closed")
	}
	if s.run != nil && s.run.IsRunning() {
		return nil
	}
	// "Not running" is not the same as "died". A run that reached the end of
	// the source has every segment on disk already; restarting it rewrites
	// the playlist from zero and the player never gets a stable one to play.
	if s.run != nil && s.run.IsCompleted() {
		return nil
	}
	if s.restartFails >= maxConsecutiveRestarts {
		return ErrRestartLimit
	}
	s.restartFails++

	s.logger.WithField("seekTime", fmt.Sprintf("%.3f", s.seekTime)).
		Info("session: auto-restarting run")

	s.releaseRunLocked()
	return s.acquireRunLocked()
}

// RestartForSegment releases the current run and acquires a new one
// at the calculated seek position for the given segment number.
func (s *Session) RestartForSegment(segNum int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("session is closed")
	}
	if s.run != nil && s.run.IsRunning() {
		return nil
	}
	// A segment missing behind a completed run is genuinely absent (past the
	// end of the source), not evidence that FFmpeg needs restarting.
	if s.run != nil && s.run.IsCompleted() {
		return nil
	}
	if s.restartFails >= maxConsecutiveRestarts {
		return ErrRestartLimit
	}
	s.restartFails++

	// Re-acquire at the same seekTime — segments are numbered relative to it.
	// Don't recalculate: the current seekTime is already the correct position
	// that produced these segment numbers.
	s.logger.WithFields(log.Fields{
		"segNum":   segNum,
		"seekTime": fmt.Sprintf("%.3f", s.seekTime),
	}).Info("session: auto-restarting for segment")

	s.releaseRunLocked()

	return s.acquireRunLocked()
}

// runIsCompleted reports whether the current run finished the source cleanly.
func (s *Session) runIsCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run != nil && s.run.IsCompleted()
}

// runOutputDir returns the output directory of the current run, or empty string.
func (s *Session) runOutputDir() string {
	if s.run != nil {
		return s.run.OutputDir()
	}
	return ""
}

// PlaylistForStream reads and cleans the FFmpeg-generated playlist for a stream.
func (s *Session) PlaylistForStream(name string) ([]byte, error) {
	dir := s.runOutputDir()
	if dir == "" {
		return nil, errors.New("no active run")
	}

	playlistPath := filepath.Join(dir, name)
	ffmpegPath := playlistPath + ".ffmpeg"

	data, err := os.ReadFile(ffmpegPath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	content = strings.Replace(content, "#EXT-X-ALLOW-CACHE:YES\n", "", 1)
	// ENDLIST is stripped while FFmpeg is still producing so the player keeps
	// polling for segments that do not exist yet. Once the run has finished
	// the source, the opposite is true: without ENDLIST the playlist reads as
	// live forever, and a player targeting the live edge of a stream that
	// already ended never starts.
	if !s.runIsCompleted() {
		content = strings.Replace(content, "#EXT-X-ENDLIST\n", "", 1)
	}

	if !strings.Contains(content, "#EXT-X-PLAYLIST-TYPE:") {
		content = strings.Replace(content, "#EXT-X-MEDIA-SEQUENCE:0\n",
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n", 1)
	}

	// Force players to start from the beginning (iOS Safari starts at live edge otherwise)
	if !strings.Contains(content, "#EXT-X-START:") {
		content = strings.Replace(content, "#EXTM3U\n",
			"#EXTM3U\n#EXT-X-START:TIME-OFFSET=0\n", 1)
	}

	// Movie-time offset of segment 0 in this variant. Downstream proxies
	// use this to compute movie_time per segment (offset + Σ EXTINF) without
	// querying session state. Quantized to seekQuantum (see Session.Start).
	// Players ignore unknown #EXT-X-* tags per HLS spec (RFC 8216 §3.1).
	if !strings.Contains(content, "#EXT-X-SESSION-OFFSET:") {
		content = strings.Replace(content, "#EXTM3U\n",
			fmt.Sprintf("#EXTM3U\n#EXT-X-SESSION-OFFSET:%.0f\n", s.seekTime), 1)
	}

	return []byte(content), nil
}

// SegmentPath returns the full path to a segment file in the shared run dir.
func (s *Session) SegmentPath(filename string) string {
	dir := s.runOutputDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, filename)
}

// WaitForPlaylist polls until the playlist file appears (max timeout).
// Returns early if the FFmpeg run is no longer active.
func (s *Session) WaitForPlaylist(ctx context.Context, name string, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		data, err := s.PlaylistForStream(name)
		if err == nil && len(data) > 0 {
			if isValidSessionPlaylist(data) {
				return data, nil
			}
		}

		// Don't wait forever if FFmpeg is no longer running
		if !s.IsRunning() {
			// One last check — file may have been written right before exit
			data, err := s.PlaylistForStream(name)
			if err == nil && len(data) > 0 && isValidSessionPlaylist(data) {
				return data, nil
			}
			return nil, errors.New("ffmpeg is not running and playlist not available")
		}

		select {
		case <-ticker.C:
		case <-deadline:
			return nil, errors.New("timeout waiting for playlist")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// isValidSessionPlaylist checks if the playlist has at least some segment entries.
func isValidSessionPlaylist(data []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lines := 0
	for scanner.Scan() {
		lines++
		if lines > 5 {
			return true
		}
	}
	return lines >= 4
}

// WaitForSegment polls until a segment file appears on disk with non-zero size.
// Returns early if the FFmpeg run is no longer active.
func (s *Session) WaitForSegment(ctx context.Context, filename string, timeout time.Duration) error {
	filePath := s.SegmentPath(filename)
	if filePath == "" {
		return errors.New("no active run")
	}
	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(filePath); err == nil && info.Size() > 0 {
			s.noteSegmentServed()
			return nil
		}

		// Don't wait forever if FFmpeg exited
		if !s.IsRunning() {
			if info, err := os.Stat(filePath); err == nil && info.Size() > 0 {
				s.noteSegmentServed()
				return nil
			}
			return errors.Errorf("ffmpeg exited, segment %s not available", filename)
		}

		select {
		case <-ticker.C:
		case <-deadline:
			return errors.Errorf("timeout waiting for segment %s", filename)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
