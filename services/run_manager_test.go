package services

import (
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
