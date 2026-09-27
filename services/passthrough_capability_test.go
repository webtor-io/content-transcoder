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

// buildableHEVC makes this build able to write hevc passthrough for the
// test: the decisions are tested as they will run once the output side is
// in.
func buildableHEVC(t *testing.T) {
	t.Helper()
	orig := passthroughBuildCodecs
	passthroughBuildCodecs = map[string]bool{"hevc": true}
	t.Cleanup(func() { passthroughBuildCodecs = orig })
}

func capabilityLines(hook *logtest.Hook) []string {
	var out []string
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "HEVC passthrough:") || strings.HasPrefix(e.Message, "passthrough capability:") {
			line := e.Message
			if v, ok := e.Data["ignored"]; ok {
				line += " ignored=" + v.(string)
			}
			out = append(out, line)
		}
	}
	return out
}

// writeCapability writes the file with a modification time of its own:
// the source notices a change by it (and by size and inode).
func writeCapability(t *testing.T, path, content string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The file switches passthrough on and off with no restart: every session
// opened after the change sees it.
func TestPassthroughCapability_FileReloadsWithoutRestart(t *testing.T) {
	buildableHEVC(t)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	file := filepath.Join(t.TempDir(), "codecs")
	t0 := time.Now().Add(-time.Hour)
	writeCapability(t, file, "", t0)

	s := newPassthroughCapabilitySource("", file)
	if s.Current().has("hevc") {
		t.Fatal("empty file: on")
	}
	writeCapability(t, file, "hevc\n", t0.Add(time.Minute))
	if !s.Current().has("hevc") {
		t.Fatal("file says hevc: still off")
	}
	writeCapability(t, file, " ", t0.Add(2*time.Minute))
	if s.Current().has("hevc") {
		t.Fatal("file emptied: still on")
	}
	writeCapability(t, file, "hevc", t0.Add(3*time.Minute))
	if !s.Current().has("hevc") {
		t.Fatal("back on: still off")
	}
	// Unchanged file, many sessions: no re-read, no log line.
	before := len(capabilityLines(hook))
	for i := 0; i < 5; i++ {
		s.Current()
	}
	if got := len(capabilityLines(hook)); got != before {
		t.Errorf("unchanged file logged %d more lines", got-before)
	}
	want := []string{"HEVC passthrough: off", "HEVC passthrough: on", "HEVC passthrough: off", "HEVC passthrough: on"}
	if got := capabilityLines(hook); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("log lines %q, want %q", got, want)
	}
}

// A ConfigMap mounted as a directory swaps a symlink to a new directory;
// the file then is another inode even when size and mtime match.
func TestPassthroughCapability_ConfigMapSymlinkSwap(t *testing.T) {
	buildableHEVC(t)
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour)
	for _, v := range []struct{ name, content string }{{"..v1", "hevc"}, {"..v2", "none"}} {
		if err := os.Mkdir(filepath.Join(dir, v.name), 0755); err != nil {
			t.Fatal(err)
		}
		writeCapability(t, filepath.Join(dir, v.name, "codecs"), v.content, at)
	}
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/codecs", filepath.Join(dir, "codecs")); err != nil {
		t.Fatal(err)
	}
	s := newPassthroughCapabilitySource("", filepath.Join(dir, "codecs"))
	if !s.Current().has("hevc") {
		t.Fatal("v1 says hevc")
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink("..v2", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if s.Current().has("hevc") {
		t.Error("after the swap to v2 (same size and mtime): still on")
	}
}

// A file that cannot be read keeps what was last read, logged once; the
// flag's value stands until the file was read once.
func TestPassthroughCapability_UnreadableFileKeepsLastValue(t *testing.T) {
	buildableHEVC(t)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	file := filepath.Join(t.TempDir(), "codecs")

	s := newPassthroughCapabilitySource("hevc", file) // not there yet
	if !s.Current().has("hevc") {
		t.Fatal("before the file exists the flag stands")
	}
	writeCapability(t, file, "", time.Now().Add(-time.Hour))
	if s.Current().has("hevc") {
		t.Fatal("the file overrides the flag")
	}
	writeCapability(t, file, "hevc", time.Now().Add(-time.Minute))
	if !s.Current().has("hevc") {
		t.Fatal("file on")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if !s.Current().has("hevc") {
			t.Fatal("a vanished file must keep the last value")
		}
	}
	warns := 0
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.HasPrefix(e.Message, "passthrough capability: cannot read") {
			warns++
		}
	}
	// Once when the file was missing at start, once now.
	if warns != 2 {
		t.Errorf("%d warnings, want one per failure (2)", warns)
	}
}

func TestPassthroughCapability_FlagOnly(t *testing.T) {
	buildableHEVC(t)
	if !newPassthroughCapabilitySource("HEVC", "").Current().has("hevc") {
		t.Error("flag hevc (any case): off")
	}
	if newPassthroughCapabilitySource("", "").Current().has("hevc") {
		t.Error("empty flag: on")
	}
	c, dropped := parsePassthroughCodecs("hevc, av1\th264")
	if !c.has("hevc") || c.has("av1") || strings.Join(dropped, ",") != "av1,h264" {
		t.Errorf("codecs %v dropped %v", c, dropped)
	}
	var nilSource *passthroughCapabilitySource
	if nilSource.Current().has("hevc") {
		t.Error("no configuration: on")
	}
	if (&HLSBuilder{}).PassthroughCapability().has("hevc") {
		t.Error("a builder without configuration passes hevc")
	}
}

// This build writes hevc as it is: configured, it is on. A codec the build
// cannot write stays off, and the start-up line says why.
func TestPassthroughCapability_BuildAllowlist(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	if !newPassthroughCapabilitySource("hevc", "").Current().has("hevc") {
		t.Fatal("hevc configured and off in a build with the output side")
	}
	if got := capabilityLines(hook); len(got) != 1 || got[0] != "HEVC passthrough: on" {
		t.Errorf("start-up line %q", got)
	}
	hook.Reset()
	orig := passthroughBuildCodecs
	passthroughBuildCodecs = map[string]bool{}
	defer func() { passthroughBuildCodecs = orig }()
	if newPassthroughCapabilitySource("hevc", "").Current().has("hevc") {
		t.Fatal("hevc on in a build that cannot write it")
	}
	if got := capabilityLines(hook); len(got) != 1 || got[0] != "HEVC passthrough: off ignored=hevc" {
		t.Errorf("start-up line %q", got)
	}
}
