package main

import (
	"context"
	"net/http"
	"regexp"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/content-transcoder/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	s.RegisterSnapshotFlags(app)
	s.RegisterCommonFlags(app)
	s.RegisterContentProberFlags(app)
	s.RegisterWebFlags(app)
	s.RegisterS3SessionFlags(app)
	s.RegisterS3StorageFlags(app)
	s.RegisterWebExpireFlags(app)
	s.RegisterServerWithErrorFlags(app)
	cs.RegisterProbeFlags(app)
	app.Action = run
}

func run(c *cli.Context) (err error) {
	// Setting ContentProbe
	contentProbe := s.NewContentProbe(c)

	// Setting HLSParser
	hls := s.NewHLSParser(c, contentProbe)

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Transcoder
	transcoder := s.NewTranscoder(hls)
	defer transcoder.Close()

	// Setting Web
	web := s.NewWeb(c, hls)
	defer web.Close()

	// Setting WebExpire
	webExpire := s.NewWebExpire(c)
	webExpire.Handle(web)

	// Setting Waiter
	waiter := s.NewWaiter(c, regexp.MustCompile(`\.m3u8$|index\.json|error\.log$`))
	waiter.Handle(web)
	defer waiter.Close()

	var transcodeServer cs.Servable = transcoder

	if c.Bool(s.UseSnapshotFlag) {
		httpClient := &http.Client{
			Timeout: time.Second * 60,
		}
		// Setting S3 Session
		s3Session := s.NewS3Session(c, httpClient)

		// Settings Key
		key := s.NewKey(c)

		// Setting S3 Client
		s3Client := s.NewS3Client(s3Session)

		// Setting S3 Storage
		s3Storage := s.NewS3Storage(c, s3Client, s3Session)

		// Setting TouchPool
		touchPool := s.NewTouchPool(s3Storage, key)
		touchPool.Handle(web)

		counter := s.NewCounter()

		initSizeFetcher := s.NewDownloadedSizeFetcher(context.Background(), s3Storage, key)

		// Setting DownloadedSizePool
		downloadedSizePool := s.NewDownloadSizePool(s3Storage, counter, key, initSizeFetcher)
		downloadedSizePool.Handle(web)

		// Setting OriginalSizeFetcher
		originalSizeFetcher := s.NewOriginalSizeFetcher(c, httpClient)

		// Setting Snapshotter
		snapshotter := s.NewSpapshotter(c, counter, s3Storage, key, transcoder, originalSizeFetcher, initSizeFetcher)
		defer snapshotter.Close()

		transcodeServer = snapshotter
	}

	server := s.NewServeWithError(c, cs.NewServe(probe, transcodeServer, web, waiter, webExpire), func(err error) {
		waiter.Close()
	})

	// And SERVE!
	err = server.Serve()
	if err != nil {
		log.WithError(err).Error("Got server error")
	}

	return err
}
