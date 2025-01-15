package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/content-transcoder/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	app.Flags = s.RegisterCommonFlags(app.Flags)
	app.Flags = s.RegisterContentProberFlags(app.Flags)
	app.Flags = s.RegisterWebFlags(app.Flags)
	app.Flags = cs.RegisterProbeFlags(app.Flags)
	app.Flags = cs.RegisterPprofFlags(app.Flags)
	app.Flags = s.RegisterHLSFlags(app.Flags)
	app.Action = run
}

func run(c *cli.Context) (err error) {
	var servers []cs.Servable

	// Setting ContentProbe
	contentProbe := s.NewContentProbe(c)

	// Setting Probe
	probe := cs.NewProbe(c)
	if probe != nil {
		servers = append(servers, probe)
		defer probe.Close()
	}

	// Setting Pprof
	pprof := cs.NewPprof(c)
	if pprof != nil {
		servers = append(servers, pprof)
		defer pprof.Close()
	}

	// Setting TranscodePool
	transcodePool := s.NewTranscodePool()

	// Setting TouchMap
	touchMap := s.NewTouchMap()

	// Setting HLSBuilder
	hlsBuilder := s.NewHLSBuilder(c)

	// Setting Web
	web := s.NewWeb(c, contentProbe, hlsBuilder, transcodePool, touchMap)
	servers = append(servers, web)
	defer web.Close()

	// Setting Serve
	serve := cs.NewServe(servers...)

	// And SERVE!
	err = serve.Serve()

	if err != nil {
		log.WithError(err).Error("got server error")
		return err
	}
	return err
}
