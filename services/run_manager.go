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
	done chan struct{}
	closed bool
}

type managedRun struct {
	run       *TranscodeRun
	idleSince time.Time // set when refCount drops to 0
}

func NewRunManager() *RunManager {
	m := &RunManager{
		runs: make(map[string]*managedRun),
		done: make(chan struct{}),
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
func (m *RunManager) Acquire(hashDir string, seekTime float64, sourceURL string, h *HLS) (*TranscodeRun, error) {
	key := runKey(hashDir, seekTime)

	m.mu.Lock()
	if mr, ok := m.runs[key]; ok {
		mr.run.AddRef()
		mr.idleSince = time.Time{} // no longer idle
		m.mu.Unlock()

		// Ensure FFmpeg is running (may have been stopped by inactivity)
		if !mr.run.IsRunning() {
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
	run := newTranscodeRun(key, hashDir, seekTime, sourceURL, h)
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
	m.mu.Lock()
	var toCleanup []*TranscodeRun
	for key, mr := range m.runs {
		if mr.run.RefCount() <= 0 && !mr.idleSince.IsZero() && time.Since(mr.idleSince) > runGracePeriod {
			toCleanup = append(toCleanup, mr.run)
			delete(m.runs, key)
		}
	}
	m.mu.Unlock()

	for _, run := range toCleanup {
		log.WithField("runKey", run.key).Info("runManager: cleaning up idle run")
		run.Cleanup()
	}
}
