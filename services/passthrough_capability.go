package services

import (
	"os"
	"sort"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	PassthroughVideoCodecsFlag     = "passthrough-video-codecs"
	PassthroughVideoCodecsFileFlag = "passthrough-video-codecs-file"
)

func RegisterPassthroughFlags(f []cli.Flag) []cli.Flag {
	return append(f, cli.StringFlag{
		Name:   PassthroughVideoCodecsFlag,
		Usage:  "source video codecs this transcoder hands to players as they are, comma-separated (hevc); empty passes none through",
		EnvVar: "PASSTHROUGH_VIDEO_CODECS",
	}, cli.StringFlag{
		Name:   PassthroughVideoCodecsFileFlag,
		Usage:  "file with the same list, re-read on every new session when it changes; overrides the flag (for a mounted ConfigMap: switch without a rollout)",
		EnvVar: "PASSTHROUGH_VIDEO_CODECS_FILE",
	})
}

// passthroughBuildCodecs are the codecs this build can write as they are
// (passthrough_output.go). A codec the configuration lists and this build
// cannot write is left out, and the start-up line says so.
var passthroughBuildCodecs = map[string]bool{"hevc": true}

// passthroughKnownCodecs are the codec names the configuration may carry.
var passthroughKnownCodecs = map[string]bool{"hevc": true}

// passthroughCapability answers "which source video codecs does this
// transcoder hand over as they are". The zero value passes none.
type passthroughCapability struct {
	codecs map[string]bool
}

func (c passthroughCapability) has(codec string) bool { return c.codecs[codec] }

// list is the codecs passed through, sorted; never nil -- an empty
// capability is an empty list (GET /capabilities answers [] for it, not
// null).
func (c passthroughCapability) list() []string {
	out := make([]string, 0, len(c.codecs))
	for k := range c.codecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c passthroughCapability) String() string { return strings.Join(c.list(), ",") }

// parsePassthroughCodecs reads a codec list (commas or whitespace). It
// returns the capability -- the listed codecs this build can write -- and,
// for the log, what was listed but cannot be used.
func parsePassthroughCodecs(s string) (passthroughCapability, []string) {
	c := passthroughCapability{}
	var dropped []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
		t = strings.ToLower(t)
		if !passthroughKnownCodecs[t] || !passthroughBuildCodecs[t] {
			dropped = append(dropped, t)
			continue
		}
		if c.codecs == nil {
			c.codecs = map[string]bool{}
		}
		c.codecs[t] = true
	}
	return c, dropped
}

// passthroughCapabilitySource keeps the capability current. With a file it
// is re-read when the file changes (checked on every new session: a stat),
// so a ConfigMap switches passthrough on and off with no pod restart --
// sessions already open keep the route they got. A file that cannot be
// read keeps the last value it had (the flag's until it was read once), and
// says so in the log once per failure.
type passthroughCapabilitySource struct {
	flagValue string
	file      string

	mu      sync.Mutex
	current passthroughCapability
	read    os.FileInfo // the file as last read
	failure string      // the last read failure logged
	// fileRead: the file has been read once and its value logged. The
	// start-up line speaks for the flag; without a line of its own, a
	// file that says the same (the empty ConfigMap of stage 1) was never
	// seen to be read at all.
	fileRead bool
}

func newPassthroughCapabilitySource(flagValue, file string) *passthroughCapabilitySource {
	s := &passthroughCapabilitySource{flagValue: flagValue, file: file}
	c, dropped := parsePassthroughCodecs(flagValue)
	s.current = c
	src := "flag"
	if file != "" {
		src = "flag, until the file is read"
	}
	logPassthroughCapability(c, dropped, src)
	if file != "" {
		s.Current()
	}
	return s
}

// Current returns the capability for a session being opened now. A nil
// source (no configuration at all) passes nothing through.
func (s *passthroughCapabilitySource) Current() passthroughCapability {
	if s == nil {
		return passthroughCapability{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == "" {
		return s.current
	}
	fi, err := os.Stat(s.file)
	if err != nil {
		s.readFailed(err)
		return s.current
	}
	if s.read != nil && os.SameFile(s.read, fi) && fi.ModTime().Equal(s.read.ModTime()) && fi.Size() == s.read.Size() {
		return s.current
	}
	b, err := os.ReadFile(s.file)
	if err != nil {
		s.readFailed(err)
		return s.current
	}
	s.read, s.failure = fi, ""
	c, dropped := parsePassthroughCodecs(string(b))
	if !s.fileRead || c.String() != s.current.String() || len(dropped) > 0 {
		logPassthroughCapability(c, dropped, s.file)
	}
	s.current, s.fileRead = c, true
	return c
}

func (s *passthroughCapabilitySource) readFailed(err error) {
	if err.Error() == s.failure {
		return
	}
	s.failure = err.Error()
	log.WithError(err).WithFields(log.Fields{
		"file": s.file,
		"kept": s.current.String(),
	}).Warn("passthrough capability: cannot read the file, keeping the last value")
}

// logPassthroughCapability is the line that explains, at start-up and on
// every change, why HEVC is or is not passed through.
func logPassthroughCapability(c passthroughCapability, dropped []string, source string) {
	state := "off"
	if c.has("hevc") {
		state = "on"
	}
	e := log.WithFields(log.Fields{
		"codecs": c.String(),
		"source": source,
	})
	if len(dropped) > 0 {
		e = e.WithField("ignored", strings.Join(dropped, ",")).
			WithField("buildable", buildableCodecs())
	}
	e.Info("HEVC passthrough: " + state)
}

func buildableCodecs() string {
	var out []string
	for k := range passthroughBuildCodecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
