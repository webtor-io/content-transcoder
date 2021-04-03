package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	webHostFlag   = "host"
	webPortFlag   = "port"
	webPlayerFlag = "player"
)

func RegisterWebFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   webHostFlag + ", H",
		Usage:  "host",
		EnvVar: "HOST",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   webPortFlag + ", P",
		Usage:  "port",
		Value:  8080,
		EnvVar: "PORT",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:  webPlayerFlag,
		Usage: "player",
	})
}

type Web struct {
	h       *HLSParser
	host    string
	port    int
	grace   int
	player  bool
	output  string
	handler http.Handler
	ln      net.Listener
}

func getParam(headerName string, getName string, r *http.Request) string {
	param := r.Header.Get(headerName)
	if param != "" {
		return param
	}
	return r.URL.Query().Get(getName)
}

func NewWeb(c *cli.Context, h *HLSParser) *Web {
	we := &Web{
		host:   c.String(webHostFlag),
		port:   c.Int(webPortFlag),
		grace:  c.Int(webGraceFlag),
		player: c.Bool(webPlayerFlag),
		output: c.String(outputFlag),
		h:      h,
	}
	we.buildHandler()
	return we
}

func (s *Web) Handle(h func(h http.Handler) http.Handler) {
	s.handler = h(s.handler)
}

type Handleable interface {
	Handle(h func(h http.Handler) http.Handler)
}

func (s *Web) buildHandler() {
	mux := http.NewServeMux()
	if s.player {
		mux.Handle("/player/", http.StripPrefix("/player/", http.FileServer(http.Dir("./player"))))
	}
	fileH := http.FileServer(http.Dir(s.output))
	enrichH := enrichPlaylistHandler(fileH)
	corsH := allowCORSHandler(enrichH)

	mux.Handle("/", corsH)
	s.handler = mux
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to bind address")
	}
	s.ln = ln
	log.Infof("Serving Web at %v", addr)
	if s.player {
		log.Info(fmt.Sprintf("Player available at http://%v/player/", addr))
	}
	return http.Serve(ln, s.handler)
}

func (s *Web) Close() {
	log.Info("Closing Web")
	defer func() {
		log.Info("Web closed")
	}()
	if s.ln != nil {
		s.ln.Close()
	}
}
