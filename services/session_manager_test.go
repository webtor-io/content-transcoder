package services

import (
	"sync"
	"testing"
	"time"
)

func newTestManager(t *testing.T) (*SessionManager, *RunManager) {
	t.Helper()
	rm := NewRunManager()
	sm := NewSessionManager(rm)
	t.Cleanup(func() {
		sm.CloseAll()
		rm.CloseAll()
	})
	return sm, rm
}

func TestSessionManagerCreateAndGet(t *testing.T) {
	m, _ := newTestManager(t)

	dir := t.TempDir()
	s := m.Create(SessionConfig{
		ID:        "sess-1",
		SourceURL: "http://example.com/v.mkv",
		HashDir:   dir,
		Duration:  100,
	})

	if s == nil {
		t.Fatal("Create returned nil")
	}
	if s.id != "sess-1" {
		t.Errorf("id: got %q, want %q", s.id, "sess-1")
	}

	got := m.Get("sess-1")
	if got != s {
		t.Error("Get should return the same session")
	}

	if m.Get("nonexistent") != nil {
		t.Error("Get should return nil for unknown ID")
	}
}

func TestSessionManagerAutoID(t *testing.T) {
	m, _ := newTestManager(t)

	dir := t.TempDir()
	s := m.Create(SessionConfig{
		SourceURL: "http://example.com/v.mkv",
		HashDir:   dir,
	})

	if s.id == "" {
		t.Error("auto-generated ID should not be empty")
	}
	if len(s.id) != 32 {
		t.Errorf("auto-generated ID should be 32 hex chars, got %d: %q", len(s.id), s.id)
	}
}

func TestSessionManagerClose(t *testing.T) {
	m, _ := newTestManager(t)

	dir := t.TempDir()
	s := m.Create(SessionConfig{ID: "sess-close", HashDir: dir})

	m.Close("sess-close")

	if m.Get("sess-close") != nil {
		t.Error("session should be removed after Close")
	}
	if !s.IsClosed() {
		t.Error("session should be closed")
	}

	// Double close is safe
	m.Close("sess-close")
}

func TestSessionManagerCloseAll(t *testing.T) {
	rm := NewRunManager()
	m := NewSessionManager(rm)

	dir := t.TempDir()
	s1 := m.Create(SessionConfig{ID: "s1", HashDir: dir})
	s2 := m.Create(SessionConfig{ID: "s2", HashDir: dir})
	s3 := m.Create(SessionConfig{ID: "s3", HashDir: dir})

	m.CloseAll()
	rm.CloseAll()

	if !s1.IsClosed() || !s2.IsClosed() || !s3.IsClosed() {
		t.Error("all sessions should be closed")
	}
	if m.Get("s1") != nil || m.Get("s2") != nil || m.Get("s3") != nil {
		t.Error("all sessions should be removed")
	}
}

func TestSessionManagerConcurrentCreate(t *testing.T) {
	m, _ := newTestManager(t)

	dir := t.TempDir()
	const n = 50
	var wg sync.WaitGroup
	sessions := make([]*Session, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sessions[idx] = m.Create(SessionConfig{
				SourceURL: "http://example.com/v.mkv",
				HashDir:   dir,
			})
		}(i)
	}
	wg.Wait()

	ids := make(map[string]bool)
	for _, s := range sessions {
		if s == nil {
			t.Fatal("nil session")
		}
		if ids[s.id] {
			t.Errorf("duplicate ID: %s", s.id)
		}
		ids[s.id] = true
		if m.Get(s.id) == nil {
			t.Errorf("session %s not found", s.id)
		}
	}
}

func TestSessionManagerConcurrentCreateAndClose(t *testing.T) {
	m, _ := newTestManager(t)

	dir := t.TempDir()
	const n = 30
	var wg sync.WaitGroup

	sessions := make([]*Session, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sessions[idx] = m.Create(SessionConfig{HashDir: dir})
		}(i)
	}
	wg.Wait()

	var wg2 sync.WaitGroup
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func(idx int) {
			defer wg2.Done()
			if idx%2 == 0 {
				m.Close(sessions[idx].id)
			} else {
				_ = m.Get(sessions[idx].id)
				sessions[idx].Touch()
			}
		}(i)
	}
	wg2.Wait()

	for i := 0; i < n; i++ {
		if i%2 == 0 {
			if m.Get(sessions[i].id) != nil {
				t.Errorf("session %d should be removed", i)
			}
		} else {
			if m.Get(sessions[i].id) == nil {
				t.Errorf("session %d should still exist", i)
			}
		}
	}
}

func TestSessionManagerManySessionsStress(t *testing.T) {
	rm := NewRunManager()
	m := NewSessionManager(rm)

	dir := t.TempDir()
	const n = 100
	sessions := make([]*Session, n)

	for i := 0; i < n; i++ {
		sessions[i] = m.Create(SessionConfig{HashDir: dir})
	}

	for i, s := range sessions {
		if m.Get(s.id) == nil {
			t.Fatalf("session %d not found", i)
		}
	}

	m.CloseAll()
	rm.CloseAll()

	for _, s := range sessions {
		if !s.IsClosed() {
			t.Errorf("session %s not closed", s.id)
		}
	}
}

func TestSessionTouchAndLastAccess(t *testing.T) {
	dir := t.TempDir()
	rm := NewRunManager()
	defer rm.CloseAll()

	s := NewSession(SessionConfig{ID: "test-touch", HashDir: dir, RunMgr: rm})

	t1 := s.LastAccess()
	time.Sleep(10 * time.Millisecond)
	s.Touch()
	t2 := s.LastAccess()

	if !t2.After(t1) {
		t.Error("Touch should update lastAccess")
	}
}
