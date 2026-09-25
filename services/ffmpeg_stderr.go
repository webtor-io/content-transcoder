package services

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// adtsScalableError is FFmpeg refusing to wrap a copied AAC stream in the
// ADTS headers mpegts needs: the stream's AudioSpecificConfig says it depends
// on a core coder. Seen on m4b audiobooks, which failed on every run.
const adtsScalableError = "Scalable configurations are not allowed in ADTS"

// timestampsFailure reports a run -xerror killed over timestamps FFmpeg
// would otherwise repair: "Non-monotonic DTS ... Error submitting a packet"
// and "Invalid DTS ... replacing by guess" in its stderr (2026-09-25: about
// 8 of 34 failing sources a day, all on seek-0 runs, the only ones with
// -xerror).
func timestampsFailure(tail string) bool {
	return strings.Contains(tail, "Non-monotonic DTS") || strings.Contains(tail, "Invalid DTS")
}

const (
	// stderrTailBytes bounds what is read back from ffmpeg.err: the cause
	// of a failure sits near the end, and a long run's stderr grew to 1.7 MB
	// of progress output.
	stderrTailBytes = 16384
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
		if l == "" || (isProgressLine(l) && i != lastProgress) || isMuxerNoise(l) {
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

// muxerNoisePattern matches what the segment muxer prints on its way out for
// every output -- one playlist rewrite and one size summary each. A run with
// a dozen outputs pushed the actual error out of the tail with them.
var muxerNoisePattern = regexp.MustCompile(`Opening '.*' for writing|muxing overhead`)

func isMuxerNoise(l string) bool {
	return muxerNoisePattern.MatchString(l)
}

// isProgressLine reports FFmpeg's periodic stats line ("frame= ..." with
// video, "size= ..." for audio-only outputs).
func isProgressLine(l string) bool {
	return strings.HasPrefix(l, "frame=") || strings.HasPrefix(l, "size=")
}

// failureCause names why a failed run died, from its stderr tail and how it
// ended (see the failure* constants). Order matters: the specific causes
// before the generic ones they can co-occur with ("Invalid argument" follows
// most of them).
func failureCause(tail string, exitReason string) string {
	if exitReason == ffmpegExitSignal {
		return failureSignal
	}
	switch {
	case strings.Contains(tail, "only possible from text to text or bitmap to bitmap"):
		return failureSubtitleBitmap
	case strings.Contains(tail, "no decoder found"), strings.Contains(tail, "Decoder not found"):
		return failureNoDecoder
	case strings.Contains(tail, "Non-monotonic DTS"):
		return failureTimestamps
	case strings.Contains(tail, "Invalid data found when processing input"):
		return failureInvalidData
	case strings.Contains(tail, "I/O error"), strings.Contains(tail, "HTTP error"),
		strings.Contains(tail, "Server returned"), strings.Contains(tail, "Connection reset"),
		strings.Contains(tail, "Connection refused"), strings.Contains(tail, "Error in the pull function"):
		return failureSource
	}
	return failureOther
}

var (
	progressTimePattern  = regexp.MustCompile(`time=(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	progressSpeedPattern = regexp.MustCompile(`speed=\s*([0-9.]+)x`)
)

// lastProgress reads media time (seconds) and speed off the last progress
// line in a stderr tail; ok is false when there is none or it has no speed
// yet ("speed=N/A" in the first moments).
func lastProgress(tail string) (mediaSec float64, speed float64, ok bool) {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !isProgressLine(lines[i]) {
			continue
		}
		t := progressTimePattern.FindStringSubmatch(lines[i])
		s := progressSpeedPattern.FindStringSubmatch(lines[i])
		if t == nil || s == nil {
			return 0, 0, false
		}
		h, _ := strconv.ParseFloat(t[1], 64)
		m, _ := strconv.ParseFloat(t[2], 64)
		sec, _ := strconv.ParseFloat(t[3], 64)
		speed, err := strconv.ParseFloat(s[1], 64)
		if err != nil {
			return 0, 0, false
		}
		return h*3600 + m*60 + sec, speed, true
	}
	return 0, 0, false
}
