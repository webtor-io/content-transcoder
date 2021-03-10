package services

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	log "github.com/sirupsen/logrus"
)

const (
	snapshotDownloadRatioFlag = "snapshot-download-ratio"
)

func RegisterSnapshotFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.Float64Flag{
		Name:   snapshotDownloadRatioFlag,
		Value:  2.0,
		EnvVar: "SNAPSHOT_DOWNLOAD_RATIO",
	})
}

type Snapshotter struct {
	run           bool
	downloadRatio float64
	counter       *Counter
	out           string
	storage       *S3Storage
	key           *Key
	stopCh        chan error
	transcoder    *Transcoder
	osf           *OriginalSizeFetcher
	dsf           *DownloadedSizeFetcher
	ch            chan error
}

func NewSpapshotter(c *cli.Context, co *Counter, st *S3Storage, key *Key, transcoder *Transcoder, osf *OriginalSizeFetcher, dsf *DownloadedSizeFetcher) *Snapshotter {
	return &Snapshotter{
		downloadRatio: c.Float64(snapshotDownloadRatioFlag),
		counter:       co,
		out:           c.String(outputFlag),
		storage:       st,
		key:           key,
		stopCh:        make(chan error),
		transcoder:    transcoder,
		osf:           osf,
		dsf:           dsf,
		ch:            make(chan error),
	}
}

func (s *Snapshotter) Serve() error {
	errCh := make(chan error)
	go func() {
		err := s.transcoder.Serve()
		if err != nil {
			<-time.After(5 * time.Minute)
			errCh <- err
			return
		}

	}()
	go func() {
		err := s.snapshot()
		if err != nil {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-s.ch:
		return nil
	}
}

func (s *Snapshotter) snapshot() error {
	waitCh := make(chan error)
	go func() {
		for {
			var err error
			s.run, err = s.shouldRun()
			if err != nil {
				waitCh <- err
				break
			}
			if s.transcoder.IsFinished() {
				waitCh <- nil
				break
			}
			<-time.After(10 * time.Second)
		}
	}()
	err := <-waitCh
	if err != nil {
		return err
	}
	if !s.run {
		return nil
	}
	defer func() {
		s.run = false
		close(s.stopCh)
	}()
	log.Info("Staring to build snapshot")
	err = s.storage.Upload(context.Background(), s.key.Get(), s.out)
	if err != nil {
		return errors.Wrapf(err, "Failed to make snapshot")
	}
	log.Info("Snapshot finished")
	return nil
}

func (s *Snapshotter) shouldRun() (bool, error) {
	// return true, nil
	return false, nil
	os, err := s.osf.Fetch()
	if err != nil {
		return false, errors.Wrapf(err, "Failed to fetch original size")
	}
	ds, err := s.dsf.Fetch()
	if err != nil {
		return false, errors.Wrapf(err, "Failed to fetch downloaded size")
	}
	return float64(s.counter.Count()+ds)/float64(os) > s.downloadRatio, nil
}

func (s *Snapshotter) Close() {
	log.Info("Closing Snapshotter")
	defer func() {
		log.Info("Snapshotter closed")
	}()
	if !s.run {
		close(s.ch)
		return
	}
	<-s.stopCh
	close(s.ch)
}
