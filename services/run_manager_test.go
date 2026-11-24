package services

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRunKey(t *testing.T) {
	got := runKey("/data/abc123", 100.5)
	want := "/data/abc123:seek:100.500"
	if got != want {
		t.Errorf("runKey: got %q, want %q", got, want)
	}
}

// Two runs of one key -- the old and the new pod of a rollout on one node,
// or a reaped run still being cleaned up and its successor -- write to one
// hashDir. Cleaning one up must leave the other's files where they are.
func TestRunCleanupLeavesSameKeyRunFiles(t *testing.T) {
	hashDir := t.TempDir()
	key := runKey(hashDir, 0)
	old := newTranscodeRun(key, hashDir, 0, "", nil)
	cur := newTranscodeRun(key, hashDir, 0, "", nil)
	if err := os.MkdirAll(cur.OutputDir(), 0755); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(cur.OutputDir(), "v0-720-0.ts")
	if err := os.WriteFile(seg, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	old.Cleanup()
	if _, err := os.Stat(seg); err != nil {
		t.Fatalf("the other run's segment is gone after a cleanup (dirs %s, %s): %v", old.OutputDir(), cur.OutputDir(), err)
	}
}

// A start the route refuses (415, the 503 of a failed source check) made
// the run's directory before it refused, and every run has a directory of
// its own: each refused request left one behind.
func TestRunManagerRefusedStartLeavesNoRunDir(t *testing.T) {
	fakeFFmpeg(t)
	m := NewRunManager()
	defer m.CloseAll()
	hashDir := t.TempDir()
	h := hevcHLS(t, 3840, 2160, false, nil)
	for i := 0; i < 3; i++ {
		if _, err := m.Acquire(hashDir, 0, "http://src/movie.mkv", h); !errors.Is(err, ErrResolutionNotSupported) {
			t.Fatalf("acquire %d: %v, want %v", i, err, ErrResolutionNotSupported)
		}
	}
	entries, err := os.ReadDir(filepath.Join(hashDir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d run directories left by refused starts, want 0", len(entries))
	}
	if len(m.runs) != 0 {
		t.Errorf("%d runs left in the manager, want 0", len(m.runs))
	}
}

// A session of the key that took the new run while its start was failing
// owns it from then on: the failed start must not clean up a run that
// session may have started.
func TestRunManagerFailedStartLeavesTakenRun(t *testing.T) {
	m := NewRunManager()
	defer m.CloseAll()
	hashDir := t.TempDir()
	key := runKey(hashDir, 0)
	m.mu.Lock()
	run := m.newRunLocked(key, hashDir, 0, "", nil)
	run.AddRef() // the session whose start failed
	run.AddRef() // the session that took the run meanwhile
	m.runs[key] = &managedRun{run: run}
	m.mu.Unlock()
	if err := os.MkdirAll(run.OutputDir(), 0755); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(run.OutputDir(), "v0-720-0.ts")
	if err := os.WriteFile(seg, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	m.dropFailedStart(key, run)
	if _, err := os.Stat(seg); err != nil {
		t.Errorf("the taken run's segment is gone: %v", err)
	}
	if mr := m.runs[key]; mr == nil || mr.run != run {
		t.Error("the taken run left the manager")
	}
	if n := run.RefCount(); n != 1 {
		t.Errorf("refCount %d, want 1", n)
	}
}

func TestRunManagerAcquireRelease(t *testing.T) {
	rm := NewRunManager()
	defer rm.CloseAll()

	dir := t.TempDir()
	// Acquire creates a new run (will fail to start FFmpeg but that's ok for ref counting)
	run := newTranscodeRun(runKey(dir, 0), dir, 0, "http://example.com/v.mkv", nil)
	run.AddRef()

	if run.RefCount() != 1 {
		t.Errorf("refCount: got %d, want 1", run.RefCount())
	}

	run.AddRef()
	if run.RefCount() != 2 {
		t.Errorf("refCount: got %d, want 2", run.RefCount())
	}

	n := run.Release()
	if n != 1 {
		t.Errorf("Release: got %d, want 1", n)
	}

	n = run.Release()
	if n != 0 {
		t.Errorf("Release: got %d, want 0", n)
	}
}

func TestRunManagerCloseAll(t *testing.T) {
	rm := NewRunManager()

	// Just verify CloseAll doesn't panic
	rm.CloseAll()

	// Double close is safe
	rm.CloseAll()
}

func TestRunManagerConcurrentOperations(t *testing.T) {
	rm := NewRunManager()
	defer rm.CloseAll()

	dir := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	runs := make([]*TranscodeRun, n)

	// Create runs concurrently
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runs[idx] = newTranscodeRun(runKey(dir, float64(idx)), dir, float64(idx), "", nil)
			runs[idx].AddRef()
		}(i)
	}
	wg.Wait()

	// Release all concurrently
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runs[idx].Release()
		}(i)
	}
	wg.Wait()
}

func TestInjectSeekParamsInRun(t *testing.T) {
	params := []string{"-fix_sub_duration", "-i", "http://example.com/v.mkv", "-c:v", "copy"}
	got := injectSeekParams(params, 50.0, true)

	want := []string{"-fix_sub_duration", "-ss", "50.000", "-noaccurate_seek", "-i", "http://example.com/v.mkv", "-c:v", "copy"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
