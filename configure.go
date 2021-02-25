package main

import (
	"regexp"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/content-transcoder/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	s.RegisterCommonFlags(app)
	s.RegisterContentProberFlags(app)
	s.RegisterWebFlags(app)
	cs.RegisterProbeFlags(app)
	app.Action = run
}

func run(c *cli.Context) (err error) {
	// Setting ContentProbe
	contentProbe := s.NewContentProbe(c)

	// Setting HLSParser
	hls := s.NewHLSParser(c, contentProbe)

	// Setting Transcoder
	transcoder := s.NewTranscoder(hls)
	defer transcoder.Close()

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Waiter
	waiter := s.NewWaiter(c, regexp.MustCompile(`\.m3u8$|index\.json$`))
	defer waiter.Close()

	// Setting Web
	web := s.NewWeb(c, waiter, hls)
	defer web.Close()

	// Setting Serve
	serve := cs.NewServe(probe, transcoder, waiter, web)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got server error")
	}

	return err
}
