package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestRedactSecrets(t *testing.T) {
	in := "Input #0, matroska, from 'http://10.0.0.1:80/abc/f.mkv?api-key=8acbcf1e-732c-4574&token=eyJhbGciOi.x.y&download=true'"
	got := redactSecrets(in)
	for _, secret := range []string{"8acbcf1e-732c-4574", "eyJhbGciOi.x.y"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived: %s", secret, got)
		}
	}
	if !strings.Contains(got, "api-key=REDACTED") || !strings.Contains(got, "token=REDACTED") || !strings.Contains(got, "download=true") {
		t.Errorf("unexpected redaction: %s", got)
	}
}

// FFmpeg rewrites its progress line in place with \r, so a failed run's
// stderr is one huge "line" of stats followed by the actual error. The
// tail has to split on \r too, or the error drowns in progress output.
func TestStderrTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ffmpeg.err")
	var b strings.Builder
	b.WriteString("Input #0, matroska, from 'http://src/f.mkv?api-key=secret-key&token=x':\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("frame=  100 fps= 25 q=28.0 size=     256KiB time=00:00:04.00 bitrate= 524.3kbits/s speed=1.0x\r")
	}
	b.WriteString("\n[aost#6:0/libfdk_aac] Non-monotonic DTS; previous: 171675, current: 168030; Error submitting a packet to the muxer: Invalid argument\n")
	b.WriteString("[out#6/segment] Task finished with error code: -22 (Invalid argument)\n")
	b.WriteString("Conversion failed!\n")
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}

	got := stderrTail(path)
	lines := strings.Split(got, "\n")
	if len(lines) > stderrTailLines {
		t.Errorf("tail has %d lines, want at most %d", len(lines), stderrTailLines)
	}
	if lines[len(lines)-1] != "Conversion failed!" {
		t.Errorf("last line = %q, want the final error", lines[len(lines)-1])
	}
	if !strings.Contains(got, "Non-monotonic DTS") {
		t.Errorf("tail lost the cause:\n%s", got)
	}
	progress := 0
	for _, l := range lines {
		if len(l) > 200 {
			t.Errorf("line of %d bytes: progress redraws were not split", len(l))
		}
		if strings.HasPrefix(l, "frame=") {
			progress++
		}
	}
	if progress != 1 {
		t.Errorf("tail has %d progress lines, want only the last one:\n%s", progress, got)
	}
	if len(got) > stderrTailBytes {
		t.Errorf("tail is %d bytes, cap is %d", len(got), stderrTailBytes)
	}
	if stderrTail(filepath.Join(t.TempDir(), "missing")) != "" {
		t.Error("a missing stderr file yields an empty tail")
	}
}

func warnEntries(hook *logtest.Hook) []*log.Entry {
	var out []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel {
			out = append(out, e)
		}
	}
	return out
}

// A run FFmpeg ended on its own with an error is the one case where the
// reason lives only in ffmpeg.err -- and that file is recreated by the next
// auto-restart and removed on cleanup. It must reach the log. Runs we
// stopped ourselves (released_idle is ~1160 SIGKILLs a day) must not.
func TestReapProcessLogsStderrOfFailedRun(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()

	cases := []struct {
		name    string
		stop    func(r *TranscodeRun)
		cmd     []string
		wantLog bool
	}{
		{"failed run logs its stderr", nil, []string{"false"}, true},
		{"run we stopped stays quiet", func(r *TranscodeRun) { r.Stop() }, []string{"sleep", "30"}, false},
		{"clean finish stays quiet", nil, []string{"true"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hook.Reset()
			dir := t.TempDir()
			r := newTranscodeRun("test:seek:0.000", dir, 0, "", nil)
			if err := os.MkdirAll(r.outputDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte("Subtitle encoding currently only possible from text to text or bitmap to bitmap\n"), 0644); err != nil {
				t.Fatal(err)
			}
			startFakeProcess(t, r, c.cmd[0], c.cmd[1:]...)
			if c.stop != nil {
				c.stop(r)
			}
			select {
			case <-r.done:
			case <-time.After(5 * time.Second):
				t.Fatal("process was not reaped")
			}
			warns := warnEntries(hook)
			if !c.wantLog {
				if len(warns) != 0 {
					t.Fatalf("unexpected warning: %v", warns[0].Message)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("got %d warnings, want 1", len(warns))
			}
			if s, _ := warns[0].Data["stderr"].(string); !strings.Contains(s, "text to text or bitmap to bitmap") {
				t.Errorf("stderr field = %q", s)
			}
		})
	}
}

// A source FFmpeg cannot convert fails the same way on every auto-restart:
// the first run plus 5 restarts logged the same tail 6 times. The run logs
// a failure once and only again when the stderr says something else --
// ignoring the per-process addresses FFmpeg prints ("@ 0xffff7abe5d50").
func TestReapProcessLogsRepeatedFailureOnce(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	r := newTranscodeRun("test:seek:0.000", t.TempDir(), 0, "", nil)
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	fail := func(stderr string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.outputDir, "ffmpeg.err"), []byte(stderr), 0644); err != nil {
			t.Fatal(err)
		}
		startFakeProcess(t, r, "false")
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("process was not reaped")
		}
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}
	fail("[sost#3:0/webvtt @ 0xffff7abe5d50] Subtitle encoding currently only possible from text to text or bitmap to bitmap\n")
	fail("[sost#3:0/webvtt @ 0xffff91c2a010] Subtitle encoding currently only possible from text to text or bitmap to bitmap\n")
	if n := len(warnEntries(hook)); n != 1 {
		t.Fatalf("same failure twice: %d warnings, want 1", n)
	}
	fail("[in#0/matroska @ 0xffff12345678] Error during demuxing: I/O error\n")
	if n := len(warnEntries(hook)); n != 2 {
		t.Fatalf("a different failure: %d warnings, want 2", n)
	}
}
