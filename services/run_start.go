package services

import (
	"context"
	"fmt"
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

// probeRunStart resolves the movie time a copy-mode run starting at seek
// actually begins at: the keyframe the demuxer seek lands on. A variable so
// tests stub the exec.
var probeRunStart = ffprobeRunStart

// ffprobeRunStart reads exactly one video packet at the seek point.
// -read_intervals seeks the way FFmpeg's own input seek does (backward, to
// the keyframe at or before the position, via the container index), so the
// first packet it reports is the keyframe the run will start from — one
// index read plus one packet, not a decode. Measured against the
// production image: for mkv and mp4 the answer equals the run's media
// time 0 for video, audio and the webvtt subtitle output; for mpegts the
// premise does not hold (the run starts elsewhere) and the guards below
// cannot tell — the quantized value was equally wrong there before, so
// this is a known limit, not a proof. Cost: ~4 range reads against the
// seeder (open, tail index, seek area) — the same reads the run itself is
// about to do, so warm in the common case.
func ffprobeRunStart(ctx context.Context, sourceURL string, seek float64) (float64, error) {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, errors.Wrap(err, "ffprobe not found")
	}
	ctx, cancel := context.WithTimeout(ctx, probeRunStartTimeout)
	defer cancel()
	// The URL comes from a request header; it has already been through the
	// session's own validation and is handed to FFmpeg as-is, but ffprobe
	// gets a protocol whitelist and an explicit non-option guard anyway —
	// they cost nothing and close the "URL that parses as a flag" shape.
	if strings.HasPrefix(sourceURL, "-") {
		return 0, errors.New("source url cannot start with a dash")
	}
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-protocol_whitelist", "http,https,tcp,tls",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,dts_time",
		"-of", "csv=p=0",
		"-read_intervals", fmt.Sprintf("%.3f%%+#1", seek),
		sourceURL,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, errors.Wrap(err, "ffprobe failed")
	}
	return parseFirstPacketTime(out)
}

// parseFirstPacketTime reads the first packet's time out of ffprobe's CSV.
// DTS when it is there, PTS otherwise: the muxer's zero point is the first
// timestamp it sees, which for a keyframe with B-frame delay is its DTS.
// Either is within a frame of the other — the error being fixed is a GOP.
func parseFirstPacketTime(out []byte) (float64, error) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "side_data") {
			continue
		}
		fields := strings.Split(line, ",")
		// pts_time,dts_time — in -show_entries order.
		var vals []float64
		for _, f := range fields {
			v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
			if err == nil {
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			continue
		}
		if len(vals) > 1 && vals[1] < vals[0] {
			return vals[1], nil
		}
		return vals[0], nil
	}
	return 0, errors.New("no packet in ffprobe output")
}
