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
// Vision RPUs, the transfer the bitstream itself signals). One ffprobe, run
// only for sessions that got past every cheaper check of videoRouteFor.
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
	// what is extracted bumps it and old files are probed again. 2: the
	// transfer of the decoded frames (FrameColorTransfer).
	sourceFactsVersion = 2
)

// sourceHEVCFacts is what the probe found out about the video stream.
type sourceHEVCFacts struct {
	Version    int    `json:"v"`
	PixFmt     string `json:"pix_fmt"`
	FieldOrder string `json:"field_order,omitempty"`
	// ColorTransfer is ffprobe's stream-level transfer: the container's
	// when it has colour information (an MKV Colour element, an MP4 colr
	// box), else the bitstream's. Read the source's with transfer().
	ColorTransfer string `json:"color_transfer,omitempty"`
	// FrameColorTransfer is the transfer of the decoded frames: the HEVC
	// VUI's (or an alternative-transfer SEI's), whatever the container
	// says -- an MKV whose Colour element has a matrix and no transfer
	// reports "unknown" at stream level over a PQ bitstream (measured on
	// ffprobe 8.1.2). Frames is how many frames were decoded.
	FrameColorTransfer string `json:"frame_color_transfer,omitempty"`
	Frames             int    `json:"frames"`
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

// transfer is the source's transfer as the route reads it: of the
// container's (ColorTransfer) and the bitstream's (FrameColorTransfer), the
// one a browser would show wrong if the other were believed.
func (f sourceHEVCFacts) transfer() string {
	return strongerTransfer(f.ColorTransfer, f.FrameColorTransfer)
}

// transferRank orders transfers by what believing another costs: HLG
// first (never passed through), then PQ (needs hdr-pq, labelled PQ), then
// the rest.
func transferRank(t string) int {
	switch t {
	case transferHLG:
		return 2
	case transferPQ:
		return 1
	}
	return 0
}

// strongerTransfer is b when a names no transfer or b ranks above it, else a.
func strongerTransfer(a, b string) string {
	if a == "" || a == "unknown" || transferRank(b) > transferRank(a) {
		return b
	}
	return a
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

// HEVC NAL unit types of the parameter sets and SEI (ITU-T H.265 7.4.2.2).
const (
	nalVPS       = 32
	nalSPS       = 33
	nalPPS       = 34
	nalSEIPrefix = 39
	nalSEISuffix = 40
)

// outputHVCC is the hvcC a passthrough output of a source with hvcC b will
// carry: the source's head with profile, tier, level and the flags as
// FFmpeg's hvcC writer derives them from the parameter sets, which is what
// the output's CODECS is built from. hevc_mp4toannexb turns the record's
// arrays into Annex B extradata (libavcodec/bsf/hevc_mp4toannexb.c), and
// movenc writes an hvcC from that anew (ff_isom_write_hvcc,
// libavformat/hevc.c): from its defaults (flags all set, profile, tier and
// level 0) it merges the profile_tier_level of every base-layer VPS and SPS
// (hvcc_update_ptl) and never reads the source record's head.
//
// ok is false when the output cannot be made right: not an hvcC, arrays
// that do not parse, a NAL type hevc_mp4toannexb refuses in extradata (the
// run would fail), or no base-layer VPS, SPS and PPS in the record -- movenc
// drops the parameter sets from the samples of an hvc1 track, so a set that
// is only in-band would be in neither place.
func outputHVCC(b []byte) (hvccHeader, bool) {
	h, ok := parseHVCC(b)
	if !ok {
		return hvccHeader{}, false
	}
	merged := hevcPTL{compat: 0xffffffff, constraint: 1<<48 - 1}
	var seen [64]bool
	p := hvccMinSize
	for n := int(b[22]); n > 0; n-- {
		if p+3 > len(b) {
			return hvccHeader{}, false
		}
		count := int(binary.BigEndian.Uint16(b[p+1:]))
		p += 3
		for ; count > 0; count-- {
			if p+2 > len(b) {
				return hvccHeader{}, false
			}
			size := int(binary.BigEndian.Uint16(b[p:]))
			p += 2
			if size < 2 || p+size > len(b) {
				return hvccHeader{}, false
			}
			nal := b[p : p+size]
			p += size
			typ := int(nal[0]>>1) & 0x3f
			switch typ {
			case nalVPS, nalSPS, nalPPS, nalSEIPrefix, nalSEISuffix:
			default:
				return hvccHeader{}, false
			}
			if layer := int(nal[0]&1)<<5 | int(nal[1]>>3); layer != 0 {
				continue
			}
			if typ == nalVPS || typ == nalSPS {
				ptl, ok := parsePSProfileTierLevel(nalRBSP(nal), typ)
				if !ok {
					return hvccHeader{}, false
				}
				merged.update(ptl)
			}
			seen[typ] = true
		}
	}
	if !seen[nalVPS] || !seen[nalSPS] || !seen[nalPPS] {
		return hvccHeader{}, false
	}
	h.profileSpace, h.tierHigh, h.profileIdc, h.levelIdc = merged.space, merged.tier, merged.profile, merged.level
	h.compat = merged.compat
	for i := range h.constraint {
		h.constraint[i] = byte(merged.constraint >> (40 - 8*i))
	}
	return h, true
}

// hevcPTL is the general part of a profile_tier_level() (H.265 7.3.3).
type hevcPTL struct {
	space      int
	tier       bool
	profile    int
	compat     uint32
	constraint uint64 // 48 bits
	level      int
}

// update merges ptl into p the way FFmpeg's hvcC writer does
// (hvcc_update_ptl): the last profile space, the higher tier and the level
// within it, the higher profile, the flags every set has.
func (p *hevcPTL) update(ptl hevcPTL) {
	p.space = ptl.space
	if !p.tier && ptl.tier {
		p.level = ptl.level
	} else if ptl.level > p.level {
		p.level = ptl.level
	}
	p.tier = p.tier || ptl.tier
	if ptl.profile > p.profile {
		p.profile = ptl.profile
	}
	p.compat &= ptl.compat
	p.constraint &= ptl.constraint
}

// parsePSProfileTierLevel reads the general profile_tier_level of a
// base-layer VPS or SPS from its RBSP (NAL header included): after 32 bits
// of VPS fields, or 8 of SPS fields (H.265 7.3.2.1, 7.3.2.2).
func parsePSProfileTierLevel(rbsp []byte, typ int) (hevcPTL, bool) {
	skip := 8
	if typ == nalVPS {
		skip = 32
	}
	r := bitReader{b: rbsp, pos: 16 + skip}
	var ptl hevcPTL
	ptl.space = int(r.read(2))
	ptl.tier = r.read(1) == 1
	ptl.profile = int(r.read(5))
	ptl.compat = uint32(r.read(32))
	ptl.constraint = r.read(48)
	ptl.level = int(r.read(8))
	return ptl, !r.over
}

// nalRBSP is a NAL unit with its emulation prevention bytes taken out (the
// 03 of every 00 00 03 after the 2-byte header), as FFmpeg reads it
// (ff_nal_unit_extract_rbsp).
func nalRBSP(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for i, c := range nal {
		if i >= 2 {
			if zeros >= 2 && c == 3 {
				zeros = 0
				continue
			}
			if c == 0 {
				zeros++
			} else {
				zeros = 0
			}
		}
		out = append(out, c)
	}
	return out
}

// bitReader reads big-endian bit fields; over is set by a read past the end.
type bitReader struct {
	b    []byte
	pos  int
	over bool
}

func (r *bitReader) read(n int) uint64 {
	var v uint64
	for ; n > 0; n-- {
		if r.pos >= 8*len(r.b) {
			r.over = true
			return 0
		}
		v = v<<1 | uint64(r.b[r.pos/8]>>(7-uint(r.pos%8))&1)
		r.pos++
	}
	return v
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
	// With both -show_packets and -show_frames ffprobe prints one array of
	// both, in read order, each entry with its type (fftools/ffprobe.c,
	// SECTION_ID_PACKETS_AND_FRAMES); with one of them, its own array.
	PacketsAndFrames []ffprobeSourceEntry `json:"packets_and_frames"`
	Packets          []ffprobeSourceEntry `json:"packets"`
	Frames           []ffprobeSourceEntry `json:"frames"`
}

// ffprobeSourceEntry is a packet (Data) or a frame (MediaType,
// ColorTransfer) of ffprobe's JSON.
type ffprobeSourceEntry struct {
	Type          string `json:"type"`
	StreamIndex   int    `json:"stream_index"`
	Data          string `json:"data"`
	MediaType     string `json:"media_type"`
	ColorTransfer string `json:"color_transfer"`
}

// packetsAndFrames splits ffprobe's entries into packets and frames.
func (o ffprobeSourceOutput) packetsAndFrames() (packets, frames []ffprobeSourceEntry) {
	packets = append(packets, o.Packets...)
	frames = append(frames, o.Frames...)
	for _, e := range o.PacketsAndFrames {
		switch e.Type {
		case "packet":
			packets = append(packets, e)
		case "frame":
			frames = append(frames, e)
		}
	}
	return packets, frames
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
	packets, frames := o.packetsAndFrames()
	for _, p := range packets {
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
	// The frames' transfer: the first that names one, or a stronger one
	// (transferRank) should they differ.
	for _, fr := range frames {
		if fr.StreamIndex != stream || fr.MediaType != "video" {
			continue
		}
		f.Frames++
		f.FrameColorTransfer = strongerTransfer(f.FrameColorTransfer, fr.ColorTransfer)
	}
	// Without a decoded frame the bitstream's transfer is unknown, and the
	// container's alone is what hid PQ: a look that could not see is a
	// failed look, not an SDR source.
	if f.Frames == 0 {
		return sourceHEVCFacts{}, errors.New("no frame of the stream decoded")
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
// the stream with its extradata and side data, its first packets with
// their data, and the keyframe among them decoded (the interval's packets
// go to the decoder, flushed at the interval's end, fftools/ffprobe.c
// read_interval_packets; -skip_frame nokey leaves the others undecoded):
// the bitstream's own transfer, which the stream level hides whenever the
// container has colour information of its own. The decode costs 8-26 ms on
// the synthetic e2e sources and 0.6 s on a 7.3 MB 4K 10-bit keyframe (2
// CPUs, one decoder thread; measured on 8.1.2), once per source and node
// (the facts are cached). -analyzeduration is left alone: 0 would not
// shorten anything (FFmpeg reads 0 as its 5 s default,
// libavformat/demux.c); -probesize caps the read.
func sourceProbeArgs(sourceURL string, stream int) []string {
	return []string{
		"-v", "error",
		"-protocol_whitelist", "http,https,tcp,tls",
		"-reconnect", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "2",
		"-probesize", sourceProbeSize,
		"-select_streams", fmt.Sprintf("%d", stream),
		"-show_streams",
		"-show_packets", "-show_frames", "-skip_frame", "nokey",
		"-read_intervals", fmt.Sprintf("%%+#%d", sourceProbePackets),
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
