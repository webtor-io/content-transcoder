package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	cp "bitbucket.org/vintikzzzz/content-prober/content-prober"
	"github.com/pkg/errors"
)

type HLSPlaylist struct {
	Duration  int            `json:"duration"`
	Ended     bool           `json:"ended"`
	Output    string         `json:"output"`
	Start     int            `json:"start"`
	IsVTT     bool           `json:"is_vtt"`
	Fragments []*HLSFragment `json:"fragments"`
}

type HLSFragment struct {
	Transcoder *HLSTranscoder   `json:"-"`
	State      HLSFragmentState `json:"state"`
	Duration   float64          `json:"duration"`
	Num        int              `json:"num"`
	Name       string           `json:"name"`
	Prev       *HLSFragment     `json:"-"`
}
type HLSFragmentState string

const (
	New        HLSFragmentState = "New"
	InProgress HLSFragmentState = "InProgress"
	Done       HLSFragmentState = "Done"
)

type HLSFragmentType string

const (
	TS   HLSFragmentType = "ts"
	FMP4 HLSFragmentType = "fmp4"
	VTT  HLSFragmentType = "vtt"
)

var targetDurationRegex = regexp.MustCompile(`#EXT-X-TARGETDURATION:(\d+)`)
var endRegex = regexp.MustCompile(`#EXT-X-ENDLIST`)
var extinfRegex = regexp.MustCompile(`#EXTINF:([\d\.]+),`)
var nameRegex = regexp.MustCompile(`^[^#]+`)

func (fr *HLSFragment) Valid() bool {
	return fr.Duration != 0 && fr.Name != ""
}

func (pl *HLSPlaylist) Done() bool {
	for _, fr := range pl.Fragments {
		if fr.State != Done {
			return false
		}
	}
	return true
}

func (mpl *HLSPlaylist) ImportFragments(pl *HLSPlaylist) error {
	for _, mfr := range mpl.Fragments {
		for _, fr := range pl.Fragments {
			if fr.Num == mfr.Num {
				if mfr.State == New && fr.State == Done {
					os.Rename(fmt.Sprintf("%s/%s", pl.Output, fr.Name), fmt.Sprintf("%s/%s", mpl.Output, mfr.Name))
				}
				mfr.State = fr.State
				mfr.Transcoder = fr.Transcoder
			}
		}
	}
	return nil
}

func NewHLSPlaylistFromTranscoder(h *HLSTranscoder, isVTT bool) (*HLSPlaylist, error) {
	pl := &HLSPlaylist{Ended: false, Output: h.out, IsVTT: isVTT}
	index := h.Index()
	if isVTT {
		index = h.VTTIndex()
	}
	file, err := os.Open(index)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to open transcoder index file")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	num := h.start

	pl.Start = h.start

	fr := &HLSFragment{}

	for scanner.Scan() {
		line := scanner.Text()
		matches := targetDurationRegex.FindStringSubmatch(line)
		if len(matches) > 1 {
			i, err := strconv.Atoi(strings.TrimSpace(matches[1]))
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse #EXT-X-TARGETDURATION value")
			}
			pl.Duration = i
		}
		if endRegex.MatchString(line) {
			pl.Ended = true
		}
		matches = extinfRegex.FindStringSubmatch(line)
		if len(matches) > 1 {
			i, err := strconv.ParseFloat(strings.TrimSpace(matches[1]), 64)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse #EXTINF value")
			}
			fr.Duration = i
		}
		if nameRegex.MatchString(line) {
			fr.Name = line
		}
		fr.State = Done
		fr.Num = num
		fr.Transcoder = h
		if fr.Valid() {
			pl.Fragments = append(pl.Fragments, fr)
			fr = &HLSFragment{}
			num++
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, errors.Wrap(err, "Got scanner error")
	}
	return pl, nil
}
func NewHLSPlaylist(probe *cp.ProbeReply, duration int, name string, t HLSFragmentType, out string) (*HLSPlaylist, error) {
	ext := "ts"
	if t == FMP4 {
		ext = "m4s"
	}
	if t == VTT {
		ext = "vtt"
	}
	d, err := strconv.ParseFloat(probe.GetFormat().GetDuration(), 64)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse format duration")
	}
	i := int(d)
	num := i / duration
	rest := i % duration
	pl := &HLSPlaylist{Duration: duration, Ended: true, Output: out}
	var prev *HLSFragment
	for index := 0; index < num; index++ {
		fr := &HLSFragment{
			State:    New,
			Duration: float64(duration),
			Num:      index,
			Name:     fmt.Sprintf("%s%d.%s", name, index, ext),
			Prev:     prev,
		}
		prev = fr
		pl.Fragments = append(pl.Fragments, fr)
	}
	if rest != 0 {
		pl.Fragments = append(pl.Fragments, &HLSFragment{
			State:    New,
			Duration: float64(rest),
			Num:      num,
			Name:     fmt.Sprintf("%s%d.%s", name, num, ext),
			Prev:     prev,
		})
	}
	return pl, nil
}

func (p *HLSPlaylist) FindFragmentByNum(num int) *HLSFragment {
	for _, fr := range p.Fragments {
		if fr.Num == num {
			return fr
		}
	}
	return nil
}

func (p *HLSPlaylist) writeHeader(w *bufio.Writer) {
	fmt.Fprintln(w, "#EXTM3U")
	fmt.Fprintln(w, "#EXT-X-VERSION:3")
	fmt.Fprintln(w, fmt.Sprintf("#EXT-X-TARGETDURATION:%d", p.Duration))
	fmt.Fprintln(w, "#EXT-X-MEDIA-SEQUENCE:0")
	fmt.Fprintln(w, "#EXT-X-PLAYLIST-TYPE:VOD")
}

func (p *HLSPlaylist) writeFooter(w *bufio.Writer) {
	fmt.Fprintln(w, "#EXT-X-ENDLIST")
}

func (f *HLSFragment) write(w *bufio.Writer) {
	fmt.Fprintln(w, fmt.Sprintf("#EXTINF:%f,", f.Duration))
	fmt.Fprintln(w, f.Name)
}

func (p *HLSPlaylist) Write(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return errors.Wrap(err, "Failed to create playlist file")
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	p.writeHeader(w)
	for _, f := range p.Fragments {
		f.write(w)
	}
	p.writeFooter(w)
	return w.Flush()
}
