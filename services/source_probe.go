package services

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// The transcoder's own look at an HEVC source, before it may pass the
// video through: what content-prober does not keep (the hvcC with profile,
// tier and level, the Dolby Vision configuration record, the transfer) and
// what no prober reports at all (whether the first packets carry Dolby
// Vision RPUs). One ffprobe, run only for sessions that got past every
// cheaper check of videoRouteFor.
const (
	// sourceProbeTimeout bounds one attempt. It runs inside POST /session;
	// the head of the file has just been read by content-prober and the
	// player's warm-up, so it is expected to be quick (not measured).
	sourceProbeTimeout  = 5 * time.Second
	sourceProbeAttempts = 2
	// sourceProbeSize caps what ffprobe reads to find the streams.
	sourceProbeSize = "5000000"
	// sourceProbePackets is how many packets of the video are looked at
	// for RPUs.
	sourceProbePackets = 2
	// sourceProbeMaxOutput caps the JSON read back: two packets of a UHD
	// remux keyframe in base64 are a few MB.
	sourceProbeMaxOutput = 64 << 20
	// sourceFactsVersion names the layout of the cached file; a change of
	// what is extracted bumps it and old files are probed again.
	sourceFactsVersion = 1
)

// sourceHEVCFacts is what the probe found out about the video stream.
type sourceHEVCFacts struct {
	Version       int    `json:"v"`
	PixFmt        string `json:"pix_fmt"`
	FieldOrder    string `json:"field_order,omitempty"`
	ColorTransfer string `json:"color_transfer,omitempty"`
	// HVCC is the stream's extradata as found: an hvcC record for MKV and
	// MP4 sources, Annex B parameter sets (or nothing) for TS.
	HVCC []byte `json:"hvcc,omitempty"`
	// DOVI is the Dolby Vision configuration record, nil when there is none.
	DOVI *doviRecord `json:"dovi,omitempty"`
	// RPU: a Dolby Vision RPU or enhancement-layer NAL (types 62, 63) in the
	// first packets.
	RPU bool `json:"rpu"`
	// Packets is how many packets were looked at for RPUs.
	Packets int `json:"packets"`
}

type doviRecord struct {
	Profile       int `json:"profile"`
	Compatibility int `json:"compatibility"`
}

// hvccHeader is the fixed head of an HEVCDecoderConfigurationRecord
// (ISO/IEC 14496-15 8.3.3.1).
type hvccHeader struct {
	profileSpace int
	tierHigh     bool
	profileIdc   int
	compat       uint32
	constraint   [6]byte
	levelIdc     int
	lengthSize   int
}

// hvccMinSize: 23 bytes is the record with no parameter set arrays; one
// that has them is longer.
const hvccMinSize = 23

// parseHVCC reads the head of an hvcC record. ok is false for anything that
// is not one with parameter sets: too short, or not configurationVersion 1
// (Annex B extradata starts with a start code).
func parseHVCC(b []byte) (hvccHeader, bool) {
	if len(b) <= hvccMinSize || b[0] != 1 {
		return hvccHeader{}, false
	}
	h := hvccHeader{
		profileSpace: int(b[1] >> 6),
		tierHigh:     b[1]&0x20 != 0,
		profileIdc:   int(b[1] & 0x1f),
		compat:       binary.BigEndian.Uint32(b[2:6]),
		levelIdc:     int(b[12]),
		lengthSize:   int(b[21]&0x03) + 1,
	}
	copy(h.constraint[:], b[6:12])
	return h, true
}

// ffprobe's JSON, the parts read here.
type ffprobeSourceOutput struct {
	Streams []struct {
		Index         int                      `json:"index"`
		CodecName     string                   `json:"codec_name"`
		PixFmt        string                   `json:"pix_fmt"`
		FieldOrder    string                   `json:"field_order"`
		ColorTransfer string                   `json:"color_transfer"`
		Extradata     string                   `json:"extradata"`
		SideDataList  []map[string]interface{} `json:"side_data_list"`
	} `json:"streams"`
	Packets []struct {
		StreamIndex int    `json:"stream_index"`
		Data        string `json:"data"`
	} `json:"packets"`
}

// ffprobe's name for AV_PKT_DATA_DOVI_CONF (libavcodec/packet.c).
const doviSideDataType = "DOVI configuration record"

// parseSourceProbe turns ffprobe's output for stream into facts. Anything
// short of the stream and at least one of its packets is an error: a
// probe that cannot be read is a failed probe, never an absent feature.
func parseSourceProbe(out []byte, stream int) (sourceHEVCFacts, error) {
	var o ffprobeSourceOutput
	if err := json.Unmarshal(out, &o); err != nil {
		return sourceHEVCFacts{}, errors.Wrap(err, "unparsable ffprobe output")
	}
	f := sourceHEVCFacts{Version: sourceFactsVersion}
	found := false
	for _, s := range o.Streams {
		if s.Index != stream {
			continue
		}
		found = true
		if s.CodecName != "hevc" {
			return sourceHEVCFacts{}, errors.Errorf("stream %d is %q, not hevc", stream, s.CodecName)
		}
		f.PixFmt, f.FieldOrder, f.ColorTransfer = s.PixFmt, s.FieldOrder, s.ColorTransfer
		if s.Extradata != "" {
			b, err := decodeDataDump(s.Extradata)
			if err != nil {
				return sourceHEVCFacts{}, errors.Wrap(err, "extradata")
			}
			f.HVCC = b
		}
		for _, sd := range s.SideDataList {
			if sd["side_data_type"] != doviSideDataType {
				continue
			}
			p, ok1 := sd["dv_profile"].(float64)
			c, ok2 := sd["dv_bl_signal_compatibility_id"].(float64)
			if !ok1 || !ok2 {
				return sourceHEVCFacts{}, errors.New("Dolby Vision record without profile or compatibility")
			}
			f.DOVI = &doviRecord{Profile: int(p), Compatibility: int(c)}
		}
	}
	if !found {
		return sourceHEVCFacts{}, errors.Errorf("stream %d not in ffprobe output", stream)
	}
	if f.PixFmt == "" {
		return sourceHEVCFacts{}, errors.New("no pixel format")
	}
	lengthSize := 0
	if h, ok := parseHVCC(f.HVCC); ok {
		lengthSize = h.lengthSize
	}
	for _, p := range o.Packets {
		if p.StreamIndex != stream {
			continue
		}
		b, err := decodeDataDump(p.Data)
		if err != nil {
			return sourceHEVCFacts{}, errors.Wrap(err, "packet data")
		}
		f.Packets++
		if hasDolbyVisionNAL(b, lengthSize) {
			f.RPU = true
		}
	}
	if f.Packets == 0 {
		return sourceHEVCFacts{}, errors.New("no packet of the stream")
	}
	return f, nil
}

// decodeDataDump reads ffprobe's -data_dump_format base64: a newline, then
// lines of base64, 60 bytes each (fftools/textformat/avtextformat.c,
// print_data_base64).
func decodeDataDump(s string) ([]byte, error) {
	var out []byte
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// HEVC NAL unit types Dolby Vision uses: 62 carries the RPU, 63 the
// enhancement layer (profile 7).
const (
	nalDolbyVisionRPU = 62
	nalDolbyVisionEL  = 63
)

// hasDolbyVisionNAL looks for NAL types 62/63 in a packet: NAL units
// prefixed with their length in lengthSize bytes (hvcC sources), or Annex B
// start codes when lengthSize is 0 or the lengths do not add up.
func hasDolbyVisionNAL(pkt []byte, lengthSize int) bool {
	isDV := func(nal []byte) bool {
		if len(nal) == 0 {
			return false
		}
		t := (nal[0] >> 1) & 0x3f
		return t == nalDolbyVisionRPU || t == nalDolbyVisionEL
	}
	if lengthSize > 0 {
		var found bool
		i, ok := 0, true
		for i < len(pkt) {
			if i+lengthSize > len(pkt) {
				ok = false
				break
			}
			n := 0
			for k := 0; k < lengthSize; k++ {
				n = n<<8 | int(pkt[i+k])
			}
			i += lengthSize
			if n <= 0 || i+n > len(pkt) {
				ok = false
				break
			}
			if isDV(pkt[i : i+n]) {
				found = true
			}
			i += n
		}
		if ok {
			return found
		}
	}
	for i := 0; i+3 < len(pkt); i++ {
		if pkt[i] == 0 && pkt[i+1] == 0 && pkt[i+2] == 1 && isDV(pkt[i+3:]) {
			return true
		}
	}
	return false
}

// sourceProbeArgs is the ffprobe command line for stream of sourceURL:
// the stream with its extradata and side data, and its first packets with
// their data. -analyzeduration is left alone: 0 would not shorten anything
// (FFmpeg reads 0 as its 5 s default, libavformat/demux.c); -probesize caps
// the read.
func sourceProbeArgs(sourceURL string, stream int) []string {
	return []string{
		"-v", "error",
		"-protocol_whitelist", "http,https,tcp,tls",
		"-reconnect", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "2",
		"-probesize", sourceProbeSize,
		"-select_streams", fmt.Sprintf("%d", stream),
		"-show_streams",
		"-show_packets", "-read_intervals", fmt.Sprintf("%%+#%d", sourceProbePackets),
		"-show_data", "-data_dump_format", "base64",
		"-of", "json",
		sourceURL,
	}
}

// runSourceProbe runs ffprobe once. A variable so tests stub the exec.
var runSourceProbe = ffprobeSource

func ffprobeSource(ctx context.Context, sourceURL string, stream int) ([]byte, error) {
	// The URL comes from a request header and goes to ffprobe as-is (an
	// exec argument, no shell); one that parses as an option is refused.
	if strings.HasPrefix(sourceURL, "-") {
		return nil, errors.New("source url cannot start with a dash")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.Wrap(err, "ffprobe not found")
	}
	ctx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath, sourceProbeArgs(sourceURL, stream)...)
	var stdout limitedBuffer
	stdout.max = sourceProbeMaxOutput
	cmd.Stdout = &stdout
	var stderr limitedBuffer
	stderr.max = 4096
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "ffprobe failed: %s", redactSecrets(strings.TrimSpace(string(stderr.b))))
	}
	if stdout.over {
		return nil, errors.New("ffprobe output over the limit")
	}
	return stdout.b, nil
}

// limitedBuffer keeps at most max bytes and notes that there were more.
type limitedBuffer struct {
	b    []byte
	max  int
	over bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - len(l.b); room < len(p) {
		l.over = true
		if room > 0 {
			l.b = append(l.b, p[:room]...)
		}
		return len(p), nil
	}
	l.b = append(l.b, p...)
	return len(p), nil
}

// sourceProber runs the probe for a (source, stream) at most once at a
// time, keeps what it found next to the source's other cached probe
// (hashDir, shared by the pods of a node), and never keeps a failure: the
// next session asks again.
type sourceProber struct {
	mu       sync.Mutex
	inflight map[string]*sourceProbeCall
}

type sourceProbeCall struct {
	done  chan struct{}
	facts sourceHEVCFacts
	err   error
}

func newSourceProber() *sourceProber {
	return &sourceProber{inflight: map[string]*sourceProbeCall{}}
}

func sourceFactsPath(hashDir string, stream int) string {
	return filepath.Join(hashDir, fmt.Sprintf("source-video-%d.json", stream))
}

// Facts returns the probe of stream of sourceURL, from hashDir's cache or
// by running ffprobe (up to sourceProbeAttempts times).
func (p *sourceProber) Facts(sourceURL, hashDir string, stream int) (sourceHEVCFacts, error) {
	if p == nil {
		return sourceHEVCFacts{}, errors.New("no source prober")
	}
	path := sourceFactsPath(hashDir, stream)
	if f, ok := readSourceFacts(path); ok {
		return f, nil
	}
	p.mu.Lock()
	if c, ok := p.inflight[path]; ok {
		p.mu.Unlock()
		<-c.done
		return c.facts, c.err
	}
	c := &sourceProbeCall{done: make(chan struct{})}
	p.inflight[path] = c
	p.mu.Unlock()

	c.facts, c.err = probeSourceFacts(sourceURL, stream)
	if c.err == nil {
		writeSourceFacts(path, c.facts)
	}
	p.mu.Lock()
	delete(p.inflight, path)
	p.mu.Unlock()
	close(c.done)
	return c.facts, c.err
}

func probeSourceFacts(sourceURL string, stream int) (sourceHEVCFacts, error) {
	started := time.Now()
	var err error
	for attempt := 0; attempt < sourceProbeAttempts; attempt++ {
		var out []byte
		out, err = runSourceProbe(context.Background(), sourceURL, stream)
		if err == nil {
			var f sourceHEVCFacts
			f, err = parseSourceProbe(out, stream)
			if err == nil {
				metricSourceProbeSeconds.WithLabelValues(sourceProbeOK).Observe(time.Since(started).Seconds())
				return f, nil
			}
		}
	}
	metricSourceProbeSeconds.WithLabelValues(sourceProbeFailed).Observe(time.Since(started).Seconds())
	log.WithError(err).WithFields(log.Fields{
		"source": redactSecrets(sourceURL),
		"stream": stream,
		"took":   time.Since(started).Round(10 * time.Millisecond).String(),
	}).Warn("source probe: failed")
	return sourceHEVCFacts{}, err
}

func readSourceFacts(path string) (sourceHEVCFacts, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sourceHEVCFacts{}, false
	}
	var f sourceHEVCFacts
	if json.Unmarshal(b, &f) != nil || f.Version != sourceFactsVersion {
		return sourceHEVCFacts{}, false
	}
	return f, true
}

// writeSourceFacts writes the cache file whole or not at all: the pods of a
// node share hashDir and may read it at any moment.
func writeSourceFacts(path string, f sourceHEVCFacts) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
