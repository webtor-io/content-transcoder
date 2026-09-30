package services

import (
	"encoding/binary"
	"io"
	"math"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Two samples of one passthrough video segment can share a presentation
// time: a CRA and a leading picture after it in decode order (RASL_N) at the
// same PTS, as FFmpeg copies them from the source (2026-09-30, 2 of 10 live
// passthrough runs). Chrome takes such an fMP4 fragment without an error and
// buffers nothing of it -- hls.js then loads it again and again, the player
// sits on a frozen picture and the transcoder answers 304s by the thousand.
// The same fragment with the duplicate's composition offset moved by one
// tick (1/timescale s, invisible) is taken whole (Chrome 154, MSE bench).
// A duplicate alone is not always dropped -- a fragment of another file with
// the same pattern was taken -- but it is never needed either.
//
// So a passthrough video segment goes out with every sample's presentation
// time unique within its track: the first sample (in decode order) at a time
// keeps it, a later one at the same time gets the next free tick, written
// over its trun's composition offset (uniquePTSPatches). The sizes stay, so
// the sidx, the data offsets and a Range request stay right; the file on
// disk stays as FFmpeg wrote it (patchedReaderAt). Not handled: a duplicate
// across two segments.

// ptsPatch is a 4-byte write at off over a segment's bytes: a trun's
// composition offset.
type ptsPatch struct {
	off int64
	b   [4]byte
}

// maxMoofSize bounds the moof read into memory: FFmpeg's for a 4 s segment
// of 60 fps is ~3 KB (240 samples); a moof larger than this is not one of
// ours.
const maxMoofSize = 1 << 20

// maxTrunSamples bounds a trun's sample count: a trun with no per-sample
// fields takes no bytes per sample, so its count is not bounded by the
// moof's size. A 4 s segment at 240 fps is 960.
const maxTrunSamples = 1 << 16

// Results of passthrough_segment_pts_total.
const (
	ptsResultFixed      = "fixed"      // samples moved apart
	ptsResultClean      = "clean"      // every presentation time unique already
	ptsResultUnreadable = "unreadable" // a moof not read: served as written
)

// uniquePTSPatches reads the fMP4 segment in r (size bytes) and returns the
// writes that make its samples' presentation times unique within each track,
// and how many samples they move. ok is false where a moof could not be
// read whole or a sample's time not known (no duration in its trun or tfhd,
// no tfdt, a duplicate without a composition offset to move): then there are
// no patches, and the segment goes out as written.
func uniquePTSPatches(r io.ReaderAt, size int64) (patches []ptsPatch, moved int, ok bool) {
	seen := map[uint32]map[int64]bool{}
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		n, _ := r.ReadAt(hdr[:], off)
		if n < 8 {
			return nil, 0, false
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		switch boxSize {
		case 0: // to the end
			boxSize = size - off
		case 1:
			if n < 16 {
				return nil, 0, false
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
		}
		if boxSize < 8 || boxSize > size-off {
			// A box past the end: a segment cut short. What came before it
			// is read; nothing after it is.
			return patches, moved, typ == "mdat"
		}
		if typ == "moof" {
			if boxSize > maxMoofSize {
				return nil, 0, false
			}
			moof := make([]byte, boxSize)
			if _, err := r.ReadAt(moof, off); err != nil && err != io.EOF {
				return nil, 0, false
			}
			p, m, mok := moofPTSPatches(moof, off, seen)
			if !mok {
				return nil, 0, false
			}
			patches = append(patches, p...)
			moved += m
		}
		off += boxSize
	}
	return patches, moved, true
}

// moofPTSPatches is uniquePTSPatches for one moof, whose bytes start at
// fileOff in the segment; seen holds the presentation times of each track
// in the moofs before it.
func moofPTSPatches(moof []byte, fileOff int64, seen map[uint32]map[int64]bool) (patches []ptsPatch, moved int, ok bool) {
	_, _, hdr, hok := boxHeader(moof)
	if !hok {
		return nil, 0, false
	}
	for pos := hdr; pos+8 <= len(moof); {
		size, typ, h, bok := boxHeader(moof[pos:])
		if !bok {
			return nil, 0, false
		}
		if typ == "traf" {
			p, m, tok := trafPTSPatches(moof[pos+h:pos+size], fileOff+int64(pos+h), seen)
			if !tok {
				return nil, 0, false
			}
			patches = append(patches, p...)
			moved += m
		}
		pos += size
	}
	return patches, moved, true
}

// trafPTSPatches is uniquePTSPatches for the body of one traf, at fileOff.
func trafPTSPatches(traf []byte, fileOff int64, seen map[uint32]map[int64]bool) (patches []ptsPatch, moved int, ok bool) {
	var (
		trackID  uint32
		haveTfhd bool
		dfltDur  uint32
		haveDflt bool
		dts      int64
		haveTfdt bool
		truns    [][2]int // body start, end within traf
	)
	for pos := 0; pos+8 <= len(traf); {
		size, typ, h, bok := boxHeader(traf[pos:])
		if !bok {
			return nil, 0, false
		}
		body := traf[pos+h : pos+size]
		switch typ {
		case "tfhd":
			if len(body) < 8 {
				return nil, 0, false
			}
			flags := binary.BigEndian.Uint32(body[:4]) & 0xffffff
			trackID = binary.BigEndian.Uint32(body[4:8])
			p := 8
			if flags&0x1 != 0 { // base_data_offset
				p += 8
			}
			if flags&0x2 != 0 { // sample_description_index
				p += 4
			}
			if flags&0x8 != 0 { // default_sample_duration
				if len(body) < p+4 {
					return nil, 0, false
				}
				dfltDur = binary.BigEndian.Uint32(body[p:])
				haveDflt = true
			}
			haveTfhd = true
		case "tfdt":
			if len(body) < 8 {
				return nil, 0, false
			}
			if body[0] == 1 {
				if len(body) < 12 {
					return nil, 0, false
				}
				dts = int64(binary.BigEndian.Uint64(body[4:12]))
			} else {
				dts = int64(binary.BigEndian.Uint32(body[4:8]))
			}
			haveTfdt = true
		case "trun":
			truns = append(truns, [2]int{pos + h, pos + size})
		}
		pos += size
	}
	if len(truns) == 0 {
		return nil, 0, true
	}
	// Without a tfdt the times of this traf are not comparable with the
	// other moofs'; without a tfhd the track is not known.
	if !haveTfhd || !haveTfdt {
		return nil, 0, false
	}
	times := seen[trackID]
	if times == nil {
		times = map[int64]bool{}
		seen[trackID] = times
	}
	for _, tr := range truns {
		body := traf[tr[0]:tr[1]]
		if len(body) < 8 {
			return nil, 0, false
		}
		version := body[0]
		flags := binary.BigEndian.Uint32(body[:4]) & 0xffffff
		count := int(binary.BigEndian.Uint32(body[4:8]))
		p := 8
		if flags&0x1 != 0 { // data_offset
			p += 4
		}
		if flags&0x4 != 0 { // first_sample_flags
			p += 4
		}
		per := 0
		for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if flags&f != 0 {
				per += 4
			}
		}
		if count > maxTrunSamples || len(body) < p+count*per {
			return nil, 0, false
		}
		hasDur, hasCTO := flags&0x100 != 0, flags&0x800 != 0
		if !hasDur && !haveDflt {
			return nil, 0, false
		}
		ctoAt := 0 // the composition offset's place within a sample's fields
		for _, f := range []uint32{0x100, 0x200, 0x400} {
			if flags&f != 0 {
				ctoAt += 4
			}
		}
		for i := 0; i < count; i++ {
			s := p + i*per
			dur := int64(dfltDur)
			if hasDur {
				dur = int64(binary.BigEndian.Uint32(body[s:]))
			}
			var cto int64
			if hasCTO {
				raw := binary.BigEndian.Uint32(body[s+ctoAt:])
				if version == 0 {
					cto = int64(raw)
				} else {
					cto = int64(int32(raw))
				}
			}
			pts := dts + cto
			if times[pts] {
				if !hasCTO {
					return nil, 0, false
				}
				for times[pts] {
					cto++
					pts++
				}
				if (version == 0 && cto > math.MaxUint32) || (version != 0 && cto > math.MaxInt32) {
					return nil, 0, false
				}
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(cto))
				patches = append(patches, ptsPatch{off: fileOff + int64(tr[0]+s+ctoAt), b: b})
				moved++
			}
			times[pts] = true
			dts += dur
		}
	}
	return patches, moved, true
}

// patchedReaderAt is r with patches written over the bytes it reads.
type patchedReaderAt struct {
	r       io.ReaderAt
	patches []ptsPatch
}

func (p patchedReaderAt) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.r.ReadAt(b, off)
	for _, pt := range p.patches {
		for i := range pt.b {
			if at := pt.off + int64(i) - off; at >= 0 && at < int64(n) {
				b[at] = pt.b[i]
			}
		}
	}
	return n, err
}

// isPassthroughVideoSegment is whether filename is a passthrough session's
// video segment (v0-<h>-<n>.m4s): the one kind of segment whose presentation
// times are the source's, copied.
func isPassthroughVideoSegment(filename string) bool {
	return strings.HasPrefix(filename, "v") && strings.HasSuffix(filename, "."+passthroughSegmentExt)
}

// segmentETagUniquePTS is segmentETag of a segment served with moved
// samples: another validator than the bytes on disk had, so a copy a browser
// took before the samples were moved does not validate. "pts1" names this
// way of moving them.
func segmentETagUniquePTS(generation string, size int64) string {
	return strings.TrimSuffix(segmentETag(generation, size), `"`) + `-pts1"`
}

// uniquePTSSegment is the segment at path (size bytes, written by the run
// process of generation) as it is to be served: f itself, or f with its
// samples moved apart, and its ETag. Each segment is counted once per
// process of this pod (passthrough_segment_pts_total), and a run whose
// samples were moved is logged once.
func uniquePTSSegment(f io.ReaderAt, size int64, path, generation string) (io.ReaderAt, string) {
	patches, moved, ok := uniquePTSPatches(f, size)
	result := ptsResultClean
	switch {
	case !ok:
		result = ptsResultUnreadable
	case moved > 0:
		result = ptsResultFixed
	}
	key := path + "|" + generation + "|" + result
	if ptsSeen.first(key) {
		metricPassthroughSegmentPTS.WithLabelValues(result).Inc()
		if result != ptsResultClean && ptsSeen.first(filepath.Dir(path)+"|"+generation+"|log") {
			log.WithFields(log.Fields{
				"segment":    filepath.Base(path),
				"run":        filepath.Base(filepath.Dir(path)),
				"generation": generation,
				"moved":      moved,
				"result":     result,
			}).Info("passthrough: samples sharing a presentation time moved apart (first segment of the run)")
		}
	}
	if !ok || moved == 0 {
		return f, segmentETag(generation, size)
	}
	return patchedReaderAt{r: f, patches: patches}, segmentETagUniquePTS(generation, size)
}

// ptsSeen remembers which segments were counted and which runs logged, at
// most ptsSeenMax keys: past that it starts again, and a segment served
// after that is counted again -- a count of segments served, not of files.
var ptsSeen = &seenSet{max: 8192}

type seenSet struct {
	mu  sync.Mutex
	m   map[string]struct{}
	max int
}

// first is whether key is new, remembering it.
func (s *seenSet) first(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return false
	}
	if s.m == nil || len(s.m) >= s.max {
		s.m = map[string]struct{}{}
	}
	s.m[key] = struct{}{}
	return true
}
