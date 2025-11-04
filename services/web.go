package services

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	webHostFlag   = "host"
	webPortFlag   = "port"
	webPlayerFlag = "player"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f, cli.StringFlag{
		Name:   webHostFlag + ", H",
		Usage:  "host",
		Value:  "",
		EnvVar: "WEB_HOST",
	}, cli.IntFlag{
		Name:   webPortFlag + ", P",
		Usage:  "port",
		Value:  8080,
		EnvVar: "WEB_PORT",
	}, cli.BoolFlag{
		Name:   webPlayerFlag,
		Usage:  "player",
		EnvVar: "PLAYER",
	})
}

type Web struct {
	host          string
	port          int
	player        bool
	output        string
	handler       http.Handler
	ln            net.Listener
	contentProbe  *ContentProbe
	transcodePool *TranscodePool
	touchMap      *TouchMap
	hlsBuilder    *HLSBuilder
}

func NewWeb(c *cli.Context, contentProbe *ContentProbe, hlsBuilder *HLSBuilder, transcodePool *TranscodePool, touchMap *TouchMap) *Web {
	we := &Web{
		host:          c.String(webHostFlag),
		port:          c.Int(webPortFlag),
		player:        c.Bool(webPlayerFlag),
		output:        c.String(OutputFlag),
		contentProbe:  contentProbe,
		transcodePool: transcodePool,
		touchMap:      touchMap,
		hlsBuilder:    hlsBuilder,
	}
	we.buildHandler()
	return we
}

func getSourceURL(r *http.Request) string {
	if r.Header.Get("X-Source-Url") != "" {
		return r.Header.Get("X-Source-Url")
	} else if r.URL.Query().Get("source_url") != "" {
		return r.URL.Query().Get("source_url")
	}
	return ""
}

type PlayerData struct {
	SourceURL string
}

func (s *Web) playerHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/player/" {
		path = "/player/index.html"
	}
	path = strings.TrimPrefix(path, "/")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		http.Error(w, "template file not found", http.StatusNotFound)
		return
	}
	tmpl, err := template.ParseFiles(path)
	if err != nil {
		http.Error(w, "unable to load template", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, &PlayerData{
		SourceURL: getSourceURL(r),
	})
	if err != nil {
		http.Error(w, "unable to render template", http.StatusInternalServerError)
		return
	}
}

func (s *Web) transcode(input string, output string) error {
	err := os.MkdirAll(output, 0755)
	if err != nil {
		return err
	}
	if s.transcodePool.IsDone(output) || s.transcodePool.IsTranscoding(output) {
		return nil
	}
	pr, err := s.contentProbe.Get(input, output)
	if err != nil {
		return err
	}
	hls := s.hlsBuilder.Build(input, pr)
	err = hls.MakeMasterPlaylist(output)
	if err != nil {
		return err
	}
	go func() {
		err = s.transcodePool.Transcode(output, hls)
		if err != nil {
			log.Error(err)
		}
	}()
	return nil
}

func (s *Web) transcodeHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.m3u8" {
			next.ServeHTTP(w, r)
			return
		}
		sourceURL := r.Context().Value(SourceURLContext).(string)
		out := r.Context().Value(OutputDirContext).(string)
		err := s.transcode(sourceURL, out)
		if err != nil {
			http.Error(w, "failed to start transcode", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Web) buildHandler() {
	mux := http.NewServeMux()
	if s.player {
		mux.HandleFunc("/player/", s.playerHandler)
	}
	var h http.Handler
	h = fileHandler()
	h = s.transcodeHandler(h)
	h = enrichPlaylistHandler(h)
	h = s.waitHandler(h)
	h = allowCORSHandler(h)
	h = s.touchHandler(h)
	h = s.doneHandler(h)
	h = setContextHandler(h, s.output)

	mux.Handle("/", h)
	s.handler = mux
}

type contextKey int

const (
	OutputDirContext contextKey = iota
	SourceURLContext
)

func setContextHandler(next http.Handler, output string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha1.New()
		sourceURL := getSourceURL(r)
		if sourceURL == "" {
			log.Error("empty source url")
			http.Error(w, "empty source url", http.StatusBadRequest)
			return
		}
		u, err := url.Parse(sourceURL)
		if err != nil {
			log.WithError(err).WithField("source_url", sourceURL).Error("unable to parse source url")
			http.Error(w, "failed to parse source url", http.StatusInternalServerError)
			return
		}
		h.Write([]byte(u.Path))
		hash := hex.EncodeToString(h.Sum(nil))
		dir, err := GetDir(output, hash)
		if err != nil {
			log.WithError(err).WithField("hash", hash).Error("unable to get dir")
			http.Error(w, "failed to get output dir", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), OutputDirContext, dir)
		ctx = context.WithValue(ctx, SourceURLContext, sourceURL)

		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "failed to bind address")
	}
	s.ln = ln
	log.Infof("serving Web at %v", addr)
	if s.player {
		log.Info(fmt.Sprintf("player available at http://%v/player/", addr))
	}
	return http.Serve(ln, s.handler)
}

func (s *Web) Close() {
	log.Info("closing Web")
	defer func() {
		log.Info("Web closed")
	}()
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *Web) waitHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".m3u8") || r.URL.Path == "/index.m3u8" {
			next.ServeHTTP(w, r)
			return
		}

		for {
			if r.Context().Err() != nil {
				return
			}
			wi := NewBufferedResponseWrtier(w)
			r.Header.Del("Range")
			next.ServeHTTP(wi, r)
			b := wi.GetBufferedBytes()
			log.Infof("waiting for %v current status %v", r.URL.Path, wi.statusCode)
			if wi.statusCode == http.StatusOK || wi.statusCode == http.StatusNotModified {
				w.Header().Set("Content-Length", fmt.Sprintf("%v", len(b)))
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				_, _ = w.Write(b)
				return
			}
			<-time.After(500 * time.Millisecond)
		}

	})
}

func (s *Web) touchHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.Context().Value(OutputDirContext).(string)
		_, _ = s.touchMap.Touch(out)
		next.ServeHTTP(w, r)
	})
}

func (s *Web) doneHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["done"]; ok {
			out := r.Context().Value(OutputDirContext).(string)
			if !s.transcodePool.IsDone(out) {
				w.WriteHeader(http.StatusNotFound)
			}
		} else {
			next.ServeHTTP(w, r)
		}
	})

}

func fileHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.Context().Value(OutputDirContext).(string)
		d := http.Dir(out)
		fs := http.FileServer(d)
		fs.ServeHTTP(w, r)
	})
}
