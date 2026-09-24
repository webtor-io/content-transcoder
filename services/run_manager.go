package services

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	runGracePeriod      = 30 * time.Second // keep idle run alive for reuse
	runReaperInterval   = 10 * time.Second
)

// RunManager manages shared TranscodeRun instances.
// Runs are keyed by (hashDir, seekTime) — sessions with the same source
// and seek position share a single FFmpeg process.
type RunManager struct {
	mu   sync.Mutex
	runs map[string]*managedRun
	// realStarts remembers, per run key, the real start a run once
	// reported (see rememberRealStart): the offset a key answers must
	// survive the run object being reaped.
	realStarts map[string]float64
	// encodeAudio remembers, per source (hashDir), that its AAC audio
	// cannot be copied (TranscodeRun.encodeAudio), so a seek does not spend
	// a failed run to find out again.
	encodeAudio map[string]bool
	done        chan struct{}
	closed     bool
}

type managedRun struct {
	run       *TranscodeRun
	idleSince time.Time // set when refCount drops to 0
}

func NewRunManager() *RunManager {
	m := &RunManager{
		runs:       make(map[string]*managedRun),
		realStarts:  make(map[string]float64),
		encodeAudio: make(map[string]bool),
		done:        make(chan struct{}),
	}
	go m.reaper()
	return m
}

func runKey(hashDir string, seekTime float64) string {
	return fmt.Sprintf("%s:seek:%.3f", hashDir, seekTime)
}

// Acquire returns an existing run or creates a new one.
// The returned run has its refCount incremented.
// If the run is new, FFmpeg is started automatically.
// ResolvedStart is the real start a run for this (hashDir, seekTime) once
// reported, if any run on this pod has resolved one.
func (m *RunManager) ResolvedStart(hashDir string, seekTime float64) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.realStarts[runKey(hashDir, seekTime)]
	return v, ok
}

func (m *RunManager) rememberRealStart(key string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A cap, not an LRU: entries are 16 bytes and a pod restarts on every
	// deploy, but an unbounded map keyed by every (hash, seek) ever played
	// is still a leak by shape.
	if len(m.realStarts) > 8192 {
		m.realStarts = map[string]float64{}
	}
	m.realStarts[key] = v
}

func (m *RunManager) rememberEncodeAudio(hashDir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.encodeAudio) > 8192 {
		m.encodeAudio = map[string]bool{}
	}
	m.encodeAudio[hashDir] = true
}

// newRunLocked builds a run wired into this manager's real-start memory:
// it reports what it resolves (rememberRealStart), and it starts preset
// with the offset this key once reported, surviving the run object — the
// reaper deletes idle runs under 10-minute sessions, and a re-created run
// must not re-probe: a probe against a source gone cold falls back to the
// quantized seek, and the playlist tag moving mid-session reads as a new
// run to every consumer. Caller holds m.mu.
func (m *RunManager) newRunLocked(key, hashDir string, seekTime float64, sourceURL string, h *HLS) *TranscodeRun {
	run := newTranscodeRun(key, hashDir, seekTime, sourceURL, h)
	run.onRealStart = m.rememberRealStart
	run.onEncodeAudio = m.rememberEncodeAudio
	run.encodeAudio = m.encodeAudio[hashDir]
	if v, ok := m.realStarts[key]; ok {
		run.realStart = v
		run.realStartResolved = true
	}
	return run
}

func (m *RunManager) Acquire(hashDir string, seekTime float64, sourceURL string, h *HLS) (*TranscodeRun, error) {
	key := runKey(hashDir, seekTime)

	m.mu.Lock()
	if mr, ok := m.runs[key]; ok {
		mr.run.AddRef()
		mr.idleSince = time.Time{} // no longer idle
		m.mu.Unlock()

		// Ensure FFmpeg is running (may have been stopped by inactivity).
		// A completed run is left alone: its segments are all on disk, and
		// restarting it would truncate the playlist under viewers already
		// watching it.
		if !mr.run.IsRunning() && !mr.run.IsCompleted() {
			if err := mr.run.Start(); err != nil {
				m.Release(mr.run)
				return nil, err
			}
		}

		log.WithFields(log.Fields{
			"runKey":   key,
			"refCount": mr.run.RefCount(),
		}).Info("runManager: reusing existing run")
		return mr.run, nil
	}

	// Create new run
	run := m.newRunLocked(key, hashDir, seekTime, sourceURL, h)
	run.AddRef()
	m.runs[key] = &managedRun{run: run}
	m.mu.Unlock()

	if err := run.Start(); err != nil {
		m.mu.Lock()
		delete(m.runs, key)
		m.mu.Unlock()
		return nil, err
	}

	log.WithFields(log.Fields{
		"runKey": key,
	}).Info("runManager: created new run")
	return run, nil
}

// Release decrements the run's refCount. When it reaches 0, a grace period
// starts. If no new session acquires the run within the grace period, it is
// cleaned up by the reaper.
func (m *RunManager) Release(run *TranscodeRun) {
	n := run.Release()

	if n <= 0 {
		m.mu.Lock()
		if mr, ok := m.runs[run.key]; ok && mr.run == run {
			mr.idleSince = time.Now()
		}
		m.mu.Unlock()

		log.WithFields(log.Fields{
			"runKey": run.key,
		}).Info("runManager: run idle, grace period started")
	}
}

// CloseAll stops all runs and the reaper.
func (m *RunManager) CloseAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.done)

	runs := make(map[string]*managedRun, len(m.runs))
	for k, v := range m.runs {
		runs[k] = v
	}
	m.runs = make(map[string]*managedRun)
	m.mu.Unlock()

	for _, mr := range runs {
		mr.run.Cleanup()
	}

	log.Info("runManager: closed all runs")
}

func (m *RunManager) reaper() {
	ticker := time.NewTicker(runReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.cleanupIdleRuns()
		}
	}
}

func (m *RunManager) cleanupIdleRuns() {
	// Snapshot under m.mu, count references outside it: RefCount takes the
	// run's own mutex, and holding the manager-wide lock across a run
	// mutex means one slow run operation stalls every Acquire/Release in
	// the pod for its duration.
	m.mu.Lock()
	candidates := make(map[string]*managedRun, len(m.runs))
	for key, mr := range m.runs {
		if !mr.idleSince.IsZero() && time.Since(mr.idleSince) > runGracePeriod {
			candidates[key] = mr
		}
	}
	m.mu.Unlock()

	var toCleanup []*TranscodeRun
	for key, mr := range candidates {
		if mr.run.RefCount() > 0 {
			continue
		}
		m.mu.Lock()
		// Re-checked under the lock: an Acquire may have taken the run
		// back between the count and here — idleSince is zeroed there, so
		// a re-acquired run never passes.
		if cur, ok := m.runs[key]; ok && cur == mr && !mr.idleSince.IsZero() && time.Since(mr.idleSince) > runGracePeriod && mr.run.RefCount() <= 0 {
			toCleanup = append(toCleanup, mr.run)
			delete(m.runs, key)
		}
		m.mu.Unlock()
	}

	for _, run := range toCleanup {
		log.WithField("runKey", run.key).Info("runManager: cleaning up idle run")
		run.cleanup(runOutcomeReleasedIdle)
	}
}
