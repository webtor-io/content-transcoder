package services

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	sessionInactivityRelease = 60 * time.Second // release run after 60s inactivity
	sessionInactivityExpiry  = 10 * time.Minute  // remove session after 10min inactivity
	sessionReaperInterval    = 10 * time.Second
)

// SessionManager manages all active transcoding sessions.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	runMgr   *RunManager
	done     chan struct{}
	closed   bool
}

func NewSessionManager(runMgr *RunManager) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*Session),
		runMgr:   runMgr,
		done:     make(chan struct{}),
	}
	go m.reaper()
	return m
}

// Create creates a new session and returns it.
func (m *SessionManager) Create(cfg SessionConfig) *Session {
	if cfg.ID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		cfg.ID = hex.EncodeToString(b)
	}
	cfg.RunMgr = m.runMgr

	s := NewSession(cfg)

	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	log.WithFields(log.Fields{
		"sessionID": s.id,
		"sourceURL": s.sourceURL,
		"duration":  s.duration,
	}).Info("sessionManager: created session")

	return s
}

// Get returns a session by ID, or nil if not found.
func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Close removes and cleans up a specific session.
func (m *SessionManager) Close(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		s.Close()
		log.WithField("sessionID", id).Info("sessionManager: closed session")
	}
}

// CloseAll stops all sessions and the reaper.
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.done)

	sessions := make(map[string]*Session, len(m.sessions))
	for k, v := range m.sessions {
		sessions[k] = v
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}

	log.Info("sessionManager: closed all sessions")
}

// reaper periodically checks sessions for inactivity.
func (m *SessionManager) reaper() {
	ticker := time.NewTicker(sessionReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.checkInactivity()
		}
	}
}

// checkInactivity releases runs for inactive sessions and removes expired ones.
func (m *SessionManager) checkInactivity() {
	m.mu.Lock()
	var toRelease []*Session
	var toRemove []string
	for id, s := range m.sessions {
		lastAccess := s.LastAccess()
		idle := time.Since(lastAccess)

		if idle > sessionInactivityExpiry {
			toRemove = append(toRemove, id)
			continue
		}

		if idle > sessionInactivityRelease && s.IsRunning() {
			toRelease = append(toRelease, s)
		}
	}
	m.mu.Unlock()

	for _, s := range toRelease {
		s.logger.Info("sessionManager: releasing run due to inactivity")
		s.Stop()
	}

	for _, id := range toRemove {
		m.Close(id)
	}
}
