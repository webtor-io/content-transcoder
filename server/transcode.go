package main

import (
	"context"
	"fmt"
	"io/ioutil"

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

func transcodeEvent(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
	h := NewHLSTranscoder(probe, in, out, opts, 0)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				h.Ping()
			}

		}
	}()
	done, plCh, err := h.Run(ctx)
	if err != nil {
		return err
	}
	go func() {
		for pl := range plCh {
			fr := pl.Fragments[len(pl.Fragments)-1]
			log.Infof("%s with duration %f done", fr.Name, fr.Duration)
			err := flushPlaylist(pl)
			if err != nil {
				done <- err
				return
			}
		}
	}()
	return <-done
}

func startFragmentTranscoder(ctx context.Context, fr *HLSFragment, in string, mpl *HLSPlaylist, vttMpl *HLSPlaylist, probe *cp.ProbeReply, opts *Options) error {
	workDir := fmt.Sprintf("tmp/%d", fr.Num)
	h := NewHLSTranscoder(probe, in, workDir, opts, fr.Num)
	fr.Transcoder = h
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
			var err error
			if pl.IsVTT {
				vttMpl.ImportFragments(pl)

			} else {
				mpl.ImportFragments(pl)
				err = flushPlaylist(mpl)
			}
			if err != nil {
				done <- err
				return
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

func handleFragmentRequest(ctx context.Context, in string, name string, mpl *HLSPlaylist, vttMpl *HLSPlaylist, probe *cp.ProbeReply, opts *Options) error {
	for _, mfr := range mpl.Fragments {
		if mfr.Name == name {
			f := mfr
			found := false
			if f.State == Done {
				log.Info("Fragment is already transcoded")
				found = true
			}
			for index := 0; index < 3; index++ {
				if f.Transcoder != nil && f.Transcoder.IsAlive() {
					log.Info("Pinging transcoder")
					f.Transcoder.Ping()
					found = true
					break
				} else if f.Prev != nil {
					f = f.Prev
				} else {
					break
				}
			}
			if !found {
				log.Info("Transcoder not found near requested fragment, start new one")
				err := startFragmentTranscoder(ctx, mfr, in, mpl, vttMpl, probe, opts)
				if err != nil {
					return errors.Wrap(err, "Failed to start fragment transcoding")
				}
				if mpl.Done() {
					log.Info("All transcoding fragments done")
				}
			}
		}
	}
	return nil
}

func transcodeVOD(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
	res := make(chan error)
	indexPath := fmt.Sprintf("%s/index.m3u8", out)
	vttIndexPath := fmt.Sprintf("%s/index_vtt.m3u8", out)
	opts.forceTranscode = true
	mpl, err := NewHLSPlaylist(probe, opts.duration, "index", TS, out)
	if err != nil {
		return errors.Wrap(err, "Failed to build playlist")
	}
	vttMpl, err := NewHLSPlaylist(probe, opts.duration, "index", VTT, out)
	if err != nil {
		return errors.Wrap(err, "Failed to build vtt playlist")
	}
	err = mpl.Write(indexPath)
	if err != nil {
		return errors.Wrap(err, "Failed to write playlist")
	}
	err = vttMpl.Write(vttIndexPath)
	if err != nil {
		return errors.Wrap(err, "Failed to write vtt playlist")
	}
	// workIndexPath := fmt.Sprintf("%s/index.m3u8", workDir)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case name := <-ch:
				go func() {
					err := handleFragmentRequest(ctx, in, name, mpl, vttMpl, probe, opts)
					if err != nil {
						res <- err
					}
				}()
			}
		}
	}()
	return <-res
}

func Transcode(ctx context.Context, probe *cp.ProbeReply, in string, out string, opts *Options, ch chan string) error {
	if probe.GetFormat().GetDuration() == "" {
		return transcodeEvent(ctx, probe, in, out, opts, ch)
	}
	return transcodeVOD(ctx, probe, in, out, opts, ch)
}
