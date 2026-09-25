package main

import (
	"os"
	"path/filepath"

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
	app.Flags = cs.RegisterPromFlags(app.Flags)
	app.Flags = cs.RegisterPprofFlags(app.Flags)
	app.Flags = s.RegisterHLSFlags(app.Flags)
	app.Flags = cs.RegisterShutdownFlags(app.Flags)
	app.Action = run
}

func cleanOutputDir(c *cli.Context) error {
	output := c.String(s.OutputFlag)
	log.WithField("output", output).Info("cleaning output directory")
	dirs, err := filepath.Glob(output)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func run(c *cli.Context) (err error) {
	s.ConfigureDebug(c)

	if c.Bool(s.CleanOnStartupFlag) {
		if err := cleanOutputDir(c); err != nil {
			log.WithError(err).Error("failed to clean output directory")
			return err
		}
	}

	var servers []cs.Servable

	// Setting ContentProbe
	contentProbe := s.NewContentProbe(c)

	// Setting Probe
	probe := cs.NewProbe(c)
	if probe != nil {
		servers = append(servers, probe)
		defer probe.Close()
	}

	// Setting Prom: serves the default registry (services/metrics.go) on the
	// prom port; the chart already exposes it as httpprom.
	prom := cs.NewProm(c)
	if prom != nil {
		servers = append(servers, prom)
		defer prom.Close()
	}

	// Setting Pprof
	pprof := cs.NewPprof(c)
	if pprof != nil {
		servers = append(servers, pprof)
		defer pprof.Close()
	}

	// Setting TouchMap
	touchMap := s.NewTouchMap()

	// Setting HLSBuilder
	hlsBuilder := s.NewHLSBuilder(c)

	// Setting RunManager
	runManager := s.NewRunManager()

	// Setting SessionManager
	sessionManager := s.NewSessionManager(runManager)

	// Setting Web
	web := s.NewWeb(c, contentProbe, hlsBuilder, sessionManager, touchMap)
	// Bound before any servable starts (see Web.Listen).
	if err := web.Listen(); err != nil {
		return err
	}
	servers = append(servers, web)
	defer runManager.CloseAll()

	// Setting Serve
	serve := cs.NewServe(servers...)

	// And SERVE!
	err = serve.Serve()

	// Drain here, not in a defer: defers run in reverse, and the deferred
	// runManager.CloseAll would stop every FFmpeg before the requests
	// waiting on them are done. A defer would also hold a panic for the whole
	// shutdown timeout.
	web.Close()

	if err != nil {
		log.WithError(err).Error("got server error")
		return err
	}
	return err
}
