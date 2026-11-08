package services

import (
	"os"
	"testing"
	"time"
)

// A restarted process reuses the run dir; the previous process's segments
// are still there and must not freeze it.
func TestRestartNotFrozenByStaleSegments(t *testing.T) {
	fastPacing(t)
	r := pacedRun(t, 20*time.Second) // lead 5 segs
	writeSegments(t, r, 100)         // previous process got to segment 99
	old := time.Now().Add(-time.Minute)
	for i := 0; i < 100; i++ {
		if err := os.Chtimes(r.primarySegmentPath(i), old, old); err != nil {
			t.Fatal(err)
		}
	}
	startFakeProcess(t, r, "sleep", "30")
	pid := r.cmd.Process.Pid
	never(t, "restarted process frozen by its predecessor's segments", func() bool { return stopped(t, pid) })
	writeSegments(t, r, 6) // its own segments 0..5: demand 0 + lead 5
	eventually(t, "frozen on its own progress", func() bool { return stopped(t, pid) })
	r.Stop()
}
