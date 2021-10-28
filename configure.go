package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/pkg/errors"
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
	outLog, err := os.Create(fmt.Sprintf("%v/%v", c.String(s.OutputFlag), "output.log"))
	if err != nil {
		return errors.Wrapf(err, "Failed create %v", fmt.Sprintf("%v/%v", c.String(s.OutputFlag), "output.log"))
	}
	defer outLog.Close()
	log.SetOutput(io.MultiWriter(os.Stdout, outLog))

	// Setting ContentProbe
	contentProbe := s.NewContentProbe(c)

	// Setting HLSParser
	hls := s.NewHLSParser(c, contentProbe)

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Transcoder
	transcoder := s.NewTranscoder(c, hls)
	defer transcoder.Close()

	// Setting Web
	web := s.NewWeb(c, hls)
	defer web.Close()

	// Setting Waiter
	waiter := s.NewWaiter(c, regexp.MustCompile(`\.m3u8$|index\.json|error\.log$`))
	waiter.Handle(web)
	defer waiter.Close()

	var transcodeServer cs.Servable = transcoder

	if c.Bool(s.UseSnapshotFlag) {
		httpClient := &http.Client{
			Timeout: time.Second * 600,
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

	servers := []cs.Servable{probe, transcodeServer, web, waiter}

	if c.Int(s.WebGraceFlag) > 0 {
		// Setting WebExpire
		webExpire := s.NewWebExpire(c)
		webExpire.Handle(web)
		servers = append(servers, webExpire)
	}

	server := s.NewServeWithStatus(c, cs.NewServe(servers...), func(err error) {
		waiter.Close()
	})

	// And SERVE!
	err = server.Serve()
	if err != nil {
		log.WithError(err).Error("Got server error")
	}

	return err
}
