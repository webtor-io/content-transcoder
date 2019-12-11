package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"strconv"

	cp "bitbucket.org/vintikzzzz/content-prober/content-prober"

	"encoding/json"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func flushPlaylist(pl *HLSPlaylist) error {
	output := fmt.Sprintf("%s/index.m3u8.json", pl.Output)
	json, err := json.Marshal(pl)

	if err != nil {
		return errors.Wrap(err, "Failed to convert playlist to json")
	}

	err = ioutil.WriteFile(output, json, 0644)

	if err != nil {
		return errors.Wrap(err, "Failed to flush playlist json to file")
	}
	return nil
}

// func transcodeEvent(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
// 	h := NewHLSTranscoder(probe, in, out, opts, 0)
// 	go func() {
// 		for {
// 			select {
// 			case <-ctx.Done():
// 				return
// 			case <-ch:
// 				h.Ping()
// 			}

// 		}
// 	}()
// 	done, plCh, err := h.Run(ctx)
// 	if err != nil {
// 		return err
// 	}
// 	go func() {
// 		for pl := range plCh {
// 			fr := pl.Fragments[len(pl.Fragments)-1]
// 			log.Infof("%s with duration %f done", fr.Name, fr.Duration)
// 			err := flushPlaylist(pl)
// 			if err != nil {
// 				done <- err
// 				return
// 			}
// 		}
// 	}()
// 	return <-done
// }

func startFragmentTranscoder(ctx context.Context, num int, in string, mpl *HLSPlaylist, vttMpl *HLSPlaylist, probe *cp.ProbeReply, opts *Options) error {
	workDir := fmt.Sprintf("tmp/%d", num)
	h := NewHLSTranscoder(probe, in, workDir, opts, num)
	// fr.Transcoder = h
	done, plCh, err := h.Run(ctx)
	if err != nil {
		return errors.Wrap(err, "Failed to start transcoder")
	}
	go func() {
		for pl := range plCh {
			if len(pl.Fragments) == 0 {
				continue
			}
			fr := pl.Fragments[len(pl.Fragments)-1]
			log.WithField("transcoder", pl.Start).Infof("%s with duration %f done", fr.Name, fr.Duration)
			if pl.IsVTT {
				vttMpl.ImportFragments(pl)
				err := vttMpl.Write()
				if err != nil {
					done <- err
					return
				}
			} else {
				mpl.ImportFragments(pl)
				err := mpl.Write()
				if err != nil {
					done <- err
					return
				}
				err = flushPlaylist(mpl)
				if err != nil {
					done <- err
					return
				}
			}
			next := mpl.FindFragmentByNum(fr.Num + 1)
			if !pl.IsVTT && next != nil && next.State == Done {
				log.WithField("transcoder", pl.Start).Infof("Next fragment is already transcoded, terminating...")
				h.Kill()
				return
			}
		}
	}()
	return <-done
}

// func handleFragmentRequest(ctx context.Context, in string, name string, mpl *HLSPlaylist, vttMpl *HLSPlaylist, probe *cp.ProbeReply, pingingOnly bool, opts *Options) error {
// 	for _, mfr := range mpl.Fragments {
// 		if mfr.Name == name {
// 			f := mfr
// 			found := false
// 			if f.State == Done {
// 				log.Info("Fragment is already transcoded")
// 				found = true
// 			}
// 			for index := 0; index < 3; index++ {
// 				if f.Transcoder != nil && f.Transcoder.IsAlive() {
// 					log.Info("Pinging transcoder")
// 					f.Transcoder.Ping()
// 					found = true
// 					break
// 				} else if f.Prev != nil {
// 					f = f.Prev
// 				} else {
// 					break
// 				}
// 			}
// 			if !found && !pingingOnly {
// 				log.Info("Transcoder not found near requested fragment, start new one")
// 				err := startFragmentTranscoder(ctx, mfr.Num, in, mpl, vttMpl, probe, opts)
// 				if err != nil {
// 					return errors.Wrap(err, "Failed to start fragment transcoding")
// 				}
// 				if mpl.Done() {
// 					log.Info("All transcoding fragments done")
// 				}
// 			}
// 		}
// 	}
// 	return nil
// }

// func transcodeVOD(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
// 	d, err := strconv.ParseFloat(probe.GetFormat().GetDuration(), 64)
// 	if err != nil {
// 		return errors.Wrap(err, "Failed to parse format duration")
// 	}
// 	plDuration := d
// 	res := make(chan error)
// 	frDuration := opts.duration
// 	name := "index"
// 	plType := VOD
// 	mpl := NewHLSPlaylist(plType, plDuration, TS, frDuration, name, out)
// 	vttMpl := NewHLSPlaylist(plType, plDuration, VTT, frDuration, name, out)
// 	err = mpl.Write()
// 	if err != nil {
// 		return errors.Wrap(err, "Failed to write playlist")
// 	}
// 	err = vttMpl.Write()
// 	if err != nil {
// 		return errors.Wrap(err, "Failed to write vtt playlist")
// 	}
// 	go func() {
// 		for {
// 			select {
// 			case <-ctx.Done():
// 				return
// 			case name := <-ch:
// 				go func() {
// 					err := handleFragmentRequest(ctx, in, name, mpl, vttMpl, probe, false, opts)
// 					if err != nil {
// 						res <- err
// 					}
// 				}()
// 			}
// 		}
// 	}()
// 	startFragmentTranscoder(ctx, 0, in, mpl, vttMpl, probe, opts)
// 	return <-res
// }

func Transcode(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
	var videoStrm *cp.Stream
	for _, strm := range probe.GetStreams() {
		if strm.GetCodecType() == "video" {
			videoStrm = strm
		}
	}
	if videoStrm.GetCodecName() == "hevc" && opts.dropHEVC {
		return errors.Errorf("HEVC not supported")
	}
	d, err := strconv.ParseFloat(probe.GetFormat().GetDuration(), 64)
	if err != nil {
		return errors.Wrap(err, "Failed to parse format duration")
	}
	plDuration := d
	res := make(chan error)
	frDuration := opts.duration
	name := "index"
	plType := Event
	mpl := NewHLSPlaylist(plType, plDuration, TS, frDuration, name, out)
	vttMpl := NewHLSPlaylist(plType, plDuration, VTT, frDuration, name, out)
	err = mpl.Write()
	if err != nil {
		return errors.Wrap(err, "Failed to write playlist")
	}
	err = vttMpl.Write()
	if err != nil {
		return errors.Wrap(err, "Failed to write vtt playlist")
	}

	startFragmentTranscoder(ctx, 0, in, mpl, vttMpl, probe, opts)
	return <-res
}
