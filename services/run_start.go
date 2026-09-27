package services

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// probeRunStartTimeout bounds the keyframe lookup: it runs inside the seek
// POST, and a source that cannot answer one packet read in this time is not
// going to play either — the quantized fallback is fine there.
const probeRunStartTimeout = 5 * time.Second

// probeRunStart resolves the movie time a copy-route run starting at seek
// actually begins at, as a player sees it: the first frame FFmpeg's own
// seek gives (ffmpegSeekFirstFrame). A variable so tests stub the exec.
//
// Until this was FFmpeg the copy route asked ffprobe (-read_intervals
// "T%+#1"), whose seek is not FFmpeg's (see ffmpegSeekStart): measured on
// 8.1.2, a seek to 30 on a 24 fps MKV with 10 s GOPs and B-frames reported
// 30.000 while the run started at the keyframe at 20.000 -- the offset, and
// every side-loaded cue and time display with it, 10 s off. Modelled on the
// Cues of 89 production sources, about 10% of copy-route seeks were
// 1.4-10.4 s off that way, and a file with a start time by that much. The
// run's own arguments did not change: only the answer.
var probeRunStart = ffmpegSeekFirstFrame

// copySeekInput is the input seek of a run whose video is copied -- the
// copy route's and passthrough's -- at the quantized time seek:
// -noaccurate_seek, the demuxer's keyframe at or before it, -ss counted
// from the file's start time like every other run's, so a seek run and the
// run from 0 share a timeline whatever the file's start_time (measured: a
// source remuxed with start_time 5 put a seek to 30 on the keyframe at
// movie 20 and the offset at 19.917 -- with -seek_timestamp 1 the offset
// read 24.917). The runs and the probe of where they land
// (ffmpegSeekFirstPacket) all use exactly this, so they cannot drift apart.
func copySeekInput(seek float64) []string {
	return []string{"-ss", fmt.Sprintf("%.3f", seek), "-noaccurate_seek"}
}

// ffmpegSeekStart asks FFmpeg itself, with the runs' seek options
// (copySeekInput), for the first packet of stream (ffmpegSeekFirstPacket),
// and returns the earlier of its timestamps (the DTS) in movie time: where
// a passthrough run's -itsoffset puts its zero. Without -copyts they come
// relative to the seek point, as the run's do (fftools/ffmpeg_demux.c:
// ts_offset is minus the seek plus the file's start time), and a keyframe
// before it is negative: the real start is seek plus that. Measured on
// 8.1.2: -10.083 for a seek to 30 over the keyframe at movie 20.000 (DTS
// 19.917), on the source and on its copy remuxed with start_time 5.
//
// Not ffprobe: its -read_intervals seek has no dts heuristic
// (ffmpeg_demux.c takes 3/23 s off the target of a format without
// AVFMT_SEEK_TO_PTS, matroska, when a stream has a B-frame delay), so for an
// MKV with B-frames it names a keyframe the run does not start at whenever
// one lies in the last 3/23 s before the seek point -- 30.000 for a seek to
// 30 on a 10 s GOP, where FFmpeg starts at 20.000 -- and it reports no DTS
// for matroska, where FFmpeg guesses one (19.937 for a keyframe at 20.020
// with two frames of B-frame delay): with the PTS as zero, the video's
// first DTS is negative and its output shifts it away. Its timestamps are
// the file's, too, not counted from its start time.
func ffmpegSeekStart(ctx context.Context, sourceURL string, stream string, seek float64) (float64, error) {
	out, err := ffmpegSeekFirstPacket(ctx, sourceURL, stream, seek)
	if err != nil {
		return 0, err
	}
	first, err := parseFrameCRCStart(out)
	if err != nil {
		return 0, err
	}
	return seek + first, nil
}

// ffmpegSeekFirstFrame is where a copy-route seek run starts for a player:
// the PTS of the first video packet FFmpeg's own seek gives
// (ffmpegSeekFirstPacket), in movie time -- the keyframe the run starts
// with. Not its DTS, where the run's video output puts its zero (the
// segment muxer shifts its first negative DTS to 0): hls.js places TS
// video by its PTS and plays the first one at media time 0. Measured in
// Chrome 154 with hls.js 1.6.14, a seek to 35 on a 24 fps MKV with 10 s
// GOPs and two frames of B-frame delay: the keyframe at movie 20.000 (DTS
// 19.917, PTS 0.083 in the served TS) played at media time 0.000, and with
// the DTS as the offset every frame was 83 ms early by it. A packet without
// a PTS gives its DTS.
func ffmpegSeekFirstFrame(ctx context.Context, sourceURL string, stream string, seek float64) (float64, error) {
	out, err := ffmpegSeekFirstPacket(ctx, sourceURL, stream, seek)
	if err != nil {
		return 0, err
	}
	dts, pts, hasDTS, hasPTS, err := frameCRCFirst(out)
	if err != nil {
		return 0, err
	}
	if !hasPTS && hasDTS {
		pts = dts
	}
	return seek + pts, nil
}

// ffmpegSeekFirstPacket runs FFmpeg with the runs' seek options
// (copySeekInput) and returns the framecrc of the first packet of stream.
func ffmpegSeekFirstPacket(ctx context.Context, sourceURL string, stream string, seek float64) ([]byte, error) {
	// The URL comes from a request header and goes to FFmpeg as-is (an
	// exec argument, no shell); one that parses as an option is refused.
	if strings.HasPrefix(sourceURL, "-") {
		return nil, errors.New("source url cannot start with a dash")
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.Wrap(err, "ffmpeg not found")
	}
	ctx, cancel := context.WithTimeout(ctx, probeRunStartTimeout)
	defer cancel()
	args := []string{"-nostdin", "-v", "error", "-protocol_whitelist", "http,https,tcp,tls"}
	args = append(args, copySeekInput(seek)...)
	args = append(args, "-i", sourceURL, "-map", "0:"+stream, "-c", "copy", "-frames:v", "1", "-f", "framecrc", "-")
	out, err := exec.CommandContext(ctx, ffmpegPath, args...).Output()
	if err != nil {
		return nil, errors.Wrap(err, "ffmpeg failed")
	}
	return out, nil
}

// parseFrameCRCStart reads the first packet of FFmpeg's framecrc output
// (frameCRCFirst) and returns the earlier of its dts and pts, in seconds.
func parseFrameCRCStart(out []byte) (float64, error) {
	dts, pts, hasDTS, hasPTS, err := frameCRCFirst(out)
	if err != nil {
		return 0, err
	}
	if hasDTS && (!hasPTS || dts < pts) {
		return dts, nil
	}
	return pts, nil
}

// frameCRCFirst reads the first packet of FFmpeg's framecrc output:
// "#tb 0: num/den", then "stream, dts, pts, duration, size, crc" in that
// time base (libavformat/framecrcenc.c; a missing timestamp is INT64_MIN).
// It returns its dts and pts in seconds and which of them it has; one of
// them at least.
func frameCRCFirst(out []byte) (dts, pts float64, hasDTS, hasPTS bool, err error) {
	var num, den int64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#tb 0:") {
			tb := strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:"))
			if i := strings.IndexByte(tb, '/'); i > 0 {
				num, _ = strconv.ParseInt(tb[:i], 10, 64)
				den, _ = strconv.ParseInt(tb[i+1:], 10, 64)
			}
			continue
		}
		if line == "" || line[0] == '#' {
			continue
		}
		if num <= 0 || den <= 0 {
			return 0, 0, false, false, errors.New("framecrc without a time base")
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			return 0, 0, false, false, errors.Errorf("unexpected framecrc line %q", line)
		}
		var ts [2]float64
		var has [2]bool
		for k, f := range fields[1:3] {
			v, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
			if err != nil || v == math.MinInt64 {
				continue
			}
			ts[k], has[k] = float64(v)*float64(num)/float64(den), true
		}
		if !has[0] && !has[1] {
			return 0, 0, false, false, errors.New("first packet without timestamps")
		}
		return ts[0], ts[1], has[0], has[1], nil
	}
	return 0, 0, false, false, errors.New("no packet in framecrc output")
}
