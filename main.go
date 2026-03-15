package main

import (
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// @title Content Transcoder API
// @version 1.0
// @description HTTP stream to HLS transcoder with session-based seek support. Each viewer creates a session which manages one HLS stream with FFmpeg. Multiple sessions for the same content share FFmpeg processes.
// @host localhost:8080
// @BasePath /
func main() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	app := cli.NewApp()
	app.Name = "content-transcoder"
	app.Usage = "runs content transcoder"
	app.Version = "0.0.1"
	configure(app)
	err := app.Run(os.Args)
	if err != nil {
		log.WithError(err).Fatal("Failed to serve application")
	}
}
