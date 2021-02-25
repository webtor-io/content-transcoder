package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"

	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"
)

type HLSPlaylist struct {
	Name             string          `json:"name"`
	Duration         float64         `json:duration"`
	FragmentDuration int             `json:"fragment_duration"`
	Ended            bool            `json:"ended"`
	Output           string          `json:"output"`
	Start            int             `json:"start"`
	IsVTT            bool            `json:"is_vtt"`
	PlaylistType     HLSPlaylistType `json:"type"`
	FragmentType     HLSFragmentType `json:"fragment_type"`
	Fragments        []*HLSFragment  `json:"fragments"`
}

type HLSFragment struct {
	// Transcoder *HLSTranscoder   `json:"-"`
	State    HLSFragmentState `json:"state"`
	Duration float64          `json:"duration"`
	Num      int              `json:"num"`
	Name     string           `json:"name"`
	Prev     *HLSFragment     `json:"-"`
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

type HLSPlaylistType string

const (
	VOD   HLSPlaylistType = "vod"
	Event HLSPlaylistType = "event"
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

// func (mpl *HLSPlaylist) ImportFragments(pl *HLSPlaylist) error {
// 	for _, fr := range pl.Fragments {
// 		found := false
// 		for _, mfr := range mpl.Fragments {
// 			if fr.Num == mfr.Num {
// 				if mfr.State == New && fr.State == Done {
// 					os.Rename(fmt.Sprintf("%s/%s", pl.Output, fr.Name), fmt.Sprintf("%s/%s", mpl.Output, mfr.Name))
// 				}
// 				mfr.State = fr.State
// 				// mfr.Transcoder = fr.Transcoder
// 				found = true
// 			}
// 		}
// 		if !found {
// 			mpl.Fragments = append(mpl.Fragments, fr)
// 			os.Rename(fmt.Sprintf("%s/%s", pl.Output, fr.Name), fmt.Sprintf("%s/%s", mpl.Output, fr.Name))
// 		}
// 	}
// 	if !mpl.Ended && pl.Ended {
// 		var total float64 = 0
// 		for _, fr := range mpl.Fragments {
// 			total = total + fr.Duration
// 		}
// 		logrus.Infof("total=%v duration=%v", total, mpl.Duration)
// 		if total >= mpl.Duration-10 {
// 			mpl.Ended = true
// 		}
// 	}
// 	return nil
// }

// func NewHLSPlaylistFromTranscoder(h *HLSTranscoder, isVTT bool) (*HLSPlaylist, error) {
// 	pl := &HLSPlaylist{Ended: false, Output: h.out, IsVTT: isVTT}
// 	index := h.Index()
// 	if isVTT {
// 		index = h.VTTIndex()
// 	}
// 	file, err := os.Open(index)
// 	if err != nil {
// 		return nil, errors.Wrap(err, "Failed to open transcoder index file")
// 	}
// 	defer file.Close()

// 	scanner := bufio.NewScanner(file)

// 	num := h.start

// 	pl.Start = h.start

// 	fr := &HLSFragment{}

// 	for scanner.Scan() {
// 		line := scanner.Text()
// 		matches := targetDurationRegex.FindStringSubmatch(line)
// 		if len(matches) > 1 {
// 			i, err := strconv.Atoi(strings.TrimSpace(matches[1]))
// 			if err != nil {
// 				return nil, errors.Wrap(err, "Failed to parse #EXT-X-TARGETDURATION value")
// 			}
// 			pl.FragmentDuration = i
// 		}
// 		if endRegex.MatchString(line) {
// 			pl.Ended = true
// 		}
// 		matches = extinfRegex.FindStringSubmatch(line)
// 		if len(matches) > 1 {
// 			i, err := strconv.ParseFloat(strings.TrimSpace(matches[1]), 64)
// 			if err != nil {
// 				return nil, errors.Wrap(err, "Failed to parse #EXTINF value")
// 			}
// 			fr.Duration = i
// 		}
// 		if nameRegex.MatchString(line) {
// 			fr.Name = line
// 		}
// 		fr.State = Done
// 		fr.Num = num
// 		fr.Transcoder = h
// 		if fr.Valid() {
// 			pl.Fragments = append(pl.Fragments, fr)
// 			fr = &HLSFragment{}
// 			num++
// 		}
// 	}

// 	if err := scanner.Err(); err != nil {
// 		return nil, errors.Wrap(err, "Got scanner error")
// 	}
// 	return pl, nil
// }
func (pl *HLSPlaylist) FragmentExtension() string {
	ext := "ts"
	if pl.FragmentType == FMP4 {
		ext = "m4s"
	}
	if pl.FragmentType == VTT {
		ext = "vtt"
	}
	return ext
}
func generateVODFragments(pl *HLSPlaylist) {
	ext := pl.FragmentExtension()
	intD := int(pl.Duration)
	num := intD / pl.FragmentDuration
	rest := intD % pl.FragmentDuration
	var prev *HLSFragment
	for index := 0; index < num; index++ {
		fr := &HLSFragment{
			State:    New,
			Duration: float64(pl.FragmentDuration),
			Num:      index,
			Name:     fmt.Sprintf("%s%d.%s", pl.Name, index, ext),
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
			Name:     fmt.Sprintf("%s%d.%s", pl.Name, num, ext),
			Prev:     prev,
		})
	}
}
func NewHLSPlaylist(plType HLSPlaylistType, plDuration float64, frType HLSFragmentType, frDuration int, name string, out string) *HLSPlaylist {
	pl := &HLSPlaylist{Name: name, PlaylistType: plType, Duration: plDuration, FragmentDuration: frDuration,
		Ended: false, Output: out, FragmentType: frType}
	if plType == VOD {
		pl.Ended = true
		generateVODFragments(pl)
	}
	return pl
}

func (p *HLSPlaylist) FindFragmentByNum(num int) *HLSFragment {
	for _, fr := range p.Fragments {
		if fr.Num == num {
			return fr
		}
	}
	return nil
}

func (pl *HLSPlaylist) writeHeader(w *bufio.Writer) {
	fmt.Fprintln(w, "#EXTM3U")
	fmt.Fprintln(w, "#EXT-X-VERSION:3")
	fmt.Fprintln(w, fmt.Sprintf("#EXT-X-TARGETDURATION:%d", pl.FragmentDuration))
	fmt.Fprintln(w, "#EXT-X-MEDIA-SEQUENCE:0")
	if pl.PlaylistType == VOD {
		fmt.Fprintln(w, "#EXT-X-PLAYLIST-TYPE:VOD")
	}
	if pl.PlaylistType == Event {
		fmt.Fprintln(w, "#EXT-X-PLAYLIST-TYPE:EVENT")
	}
}

func (pl *HLSPlaylist) writeFooter(w *bufio.Writer) {
	if pl.Ended {
		fmt.Fprintln(w, "#EXT-X-ENDLIST")
	}
}

func (fr *HLSFragment) write(w *bufio.Writer) {
	fmt.Fprintln(w, fmt.Sprintf("#EXTINF:%f,", fr.Duration))
	fmt.Fprintln(w, fr.Name)
}

func (pl *HLSPlaylist) Write() error {
	suffix := ""
	if pl.FragmentType == VTT {
		suffix = "_vtt"
	}
	path := fmt.Sprintf("%s/%s%s.m3u8", pl.Output, pl.Name, suffix)
	logrus.Infof("Write playlist to path=%s", path)
	file, err := os.Create(path)
	if err != nil {
		return errors.Wrap(err, "Failed to create playlist file")
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	pl.writeHeader(w)
	for _, f := range pl.Fragments {
		f.write(w)
	}
	pl.writeFooter(w)
	return w.Flush()
}
