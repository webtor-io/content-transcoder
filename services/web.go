package services

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/webtor-io/gracenet"
)

const (
	webHostFlag   = "host"
	webPortFlag   = "port"
	webGraceFlag  = "grace"
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
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   webGraceFlag + ", ag",
		Usage:  "access grace in seconds",
		Value:  600,
		EnvVar: "GRACE",
	})

	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:  webPlayerFlag,
		Usage: "player",
	})
}

type Web struct {
	w      *Waiter
	h      *HLSParser
	host   string
	port   int
	grace  int
	player bool
	output string
	ln     net.Listener
}

func allowCORSHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h.ServeHTTP(w, r)
	})
}

func waitHandler(h http.Handler, wa *Waiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		select {
		case <-wa.Wait(r.Context(), r.URL.Path):
		case <-ctx.Done():
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			break
		}
		h.ServeHTTP(w, r)
	})
}

func getParam(headerName string, getName string, r *http.Request) string {
	param := r.Header.Get(headerName)
	if param != "" {
		return param
	}
	return r.URL.Query().Get(getName)
}

func indexPlaylistHandler(h *HLSParser) http.Handler {
	mp := ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mp == "" {
			hls, err := h.Get()
			if err != nil {
				log.WithError(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
			mp = hls.MakeMasterPlaylist()
		}
		w.Write([]byte(mp))
	})
}

func enrichPlaylistHandler(h http.Handler) http.Handler {
	re := regexp.MustCompile(`\.m3u8$`)
	re2 := regexp.MustCompile(`[asv][0-9]+(\-[0-9]+)?\.[0-9a-z]{2,4}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !re.MatchString(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}

		wi := NewResponseWrtierInterceptor(w)

		h.ServeHTTP(wi, r)

		b := wi.GetBufferedBytes()

		var sb strings.Builder
		scanner := bufio.NewScanner(bytes.NewReader(b))
		for scanner.Scan() {
			text := scanner.Text()
			if r.URL.RawQuery != "" {
				text = re2.ReplaceAllString(text, "$0?"+r.URL.RawQuery)
			}
			if text == "#EXT-X-MEDIA-SEQUENCE:0" {
				sb.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT")
				sb.WriteRune('\n')
			}
			sb.WriteString(text)
			sb.WriteRune('\n')
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%v", sb.Len()))
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(sb.String()))

		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
	})
}

func NewWeb(c *cli.Context, w *Waiter, h *HLSParser) *Web {
	return &Web{
		host:   c.String(webHostFlag),
		port:   c.Int(webPortFlag),
		grace:  c.Int(webGraceFlag),
		player: c.Bool(webPlayerFlag),
		output: c.String(outputFlag),
		w:      w,
		h:      h,
	}
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to bind address")
	}
	s.ln = ln
	agln := gracenet.NewGraceListener(ln, time.Duration(s.grace)*time.Second)
	mux := http.NewServeMux()
	if s.player {
		log.Info(fmt.Sprintf("Player available at http://%v/player/", addr))
		mux.Handle("/player/", http.StripPrefix("/player/", http.FileServer(http.Dir("./player"))))
	}
	mux.Handle("/index.m3u8", allowCORSHandler(enrichPlaylistHandler(indexPlaylistHandler(s.h))))
	fileH := http.FileServer(http.Dir(s.output))
	enrichH := enrichPlaylistHandler(fileH)
	waitH := waitHandler(enrichH, s.w)
	mux.Handle("/", allowCORSHandler(waitH))
	log.Infof("Serving Web at %v", addr)
	errCh := make(chan error)
	go func() {
		errCh <- http.Serve(agln, mux)
	}()
	select {
	case err := <-errCh:
		return errors.Wrapf(err, "Failed to serve Web")
	case <-agln.Expire():
		return errors.Errorf("Nobody here for so long...")
	}
}

func (s *Web) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
