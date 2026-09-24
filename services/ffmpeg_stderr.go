package services

import (
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	// stderrTailBytes bounds what is read back from ffmpeg.err: the cause
	// of a failure sits in the last few lines, and a long run's stderr grew
	// to 1.7 MB of progress output.
	stderrTailBytes = 4096
	// stderrTailLines is how many lines of that are logged.
	stderrTailLines = 15
)

// secretParamPattern matches the credentials the source URL carries in its
// query (the internal api-key, the viewer's token).
var secretParamPattern = regexp.MustCompile(`((?:api-key|token)=)[^&\s'"}]+`)

// redactSecrets replaces credential values in a source URL, or in any text
// that quotes one (FFmpeg params, ffprobe output), with REDACTED.
func redactSecrets(s string) string {
	return secretParamPattern.ReplaceAllString(s, "${1}REDACTED")
}

// stderrTail returns the last stderrTailLines non-empty lines of an FFmpeg
// stderr file, secrets redacted, or "" when it cannot be read. FFmpeg
// redraws its progress line with \r, so lines are split on \r as well as
// \n -- otherwise the whole stats history reads as one line -- and only the
// last progress line is kept: it shows how far and how fast the run got,
// the ones before it only push the error out of the tail.
func stderrTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	seeked := false
	if fi, err := f.Stat(); err == nil && fi.Size() > stderrTailBytes {
		if _, err := f.Seek(fi.Size()-stderrTailBytes, io.SeekStart); err != nil {
			return ""
		}
		seeked = true
	}
	data, err := io.ReadAll(io.LimitReader(f, stderrTailBytes))
	if err != nil {
		return ""
	}
	all := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' || r == '\r' })
	// Reading from the middle of the file starts mid-line.
	if seeked && len(all) > 1 {
		all = all[1:]
	}
	lastProgress := -1
	for i, l := range all {
		if isProgressLine(strings.TrimSpace(l)) {
			lastProgress = i
		}
	}
	var lines []string
	for i, l := range all {
		l = strings.TrimSpace(l)
		if l == "" || (isProgressLine(l) && i != lastProgress) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) > stderrTailLines {
		lines = lines[len(lines)-stderrTailLines:]
	}
	return redactSecrets(strings.Join(lines, "\n"))
}

// ffmpegAddrPattern matches the context addresses FFmpeg prints in its log
// prefixes ("[sost#3:0/webvtt @ 0xffff7abe5d50]"), different in every process.
var ffmpegAddrPattern = regexp.MustCompile(`@ 0x[0-9a-f]+`)

// failureKey is a stderr tail with what differs between two runs of the same
// failure removed: addresses and the progress line (how far it got).
func failureKey(tail string) string {
	var lines []string
	for _, l := range strings.Split(tail, "\n") {
		if !isProgressLine(l) {
			lines = append(lines, ffmpegAddrPattern.ReplaceAllString(l, "@"))
		}
	}
	return strings.Join(lines, "\n")
}

// isProgressLine reports FFmpeg's periodic stats line ("frame= ..." with
// video, "size= ..." for audio-only outputs).
func isProgressLine(l string) bool {
	return strings.HasPrefix(l, "frame=") || strings.HasPrefix(l, "size=")
}
