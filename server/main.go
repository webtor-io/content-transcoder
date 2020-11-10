package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	joonix "github.com/joonix/log"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/webtor-io/gracenet"
	"gopkg.in/fsnotify.v1"

	u "net/url"

	"path/filepath"

	"github.com/pkg/errors"
)

type AccessLock struct {
	C      chan error
	closed bool
	mux    sync.Mutex
}

func NewAccessLock() *AccessLock {
	return &AccessLock{C: make(chan error)}
}
func (al *AccessLock) Unlocked() chan error {
	return al.C
}
func (al *AccessLock) Unlock() {
	al.mux.Lock()
	defer al.mux.Unlock()
	if !al.closed {
		close(al.C)
		al.closed = true
	}
}

func allowCORSHandler(h http.Handler, acao string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		h.ServeHTTP(w, r)
	})
}

func waitHandler(h http.Handler, ctx context.Context, path string, pattern string, lch chan string) (http.Handler, error) {
	var locks sync.Map
	re, err := regexp.Compile(pattern)
	// var mux sync.Mutex
	if err != nil {
		return nil, errors.Wrap(err, "Failed to compile regex for wait handler")
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to init watcher")
	}
	go func() {
		defer watcher.Close()
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				name := filepath.Base(event.Name)
				if re.MatchString(name) && event.Op == fsnotify.Create {
					log.WithField("name", name).Info("Got watcher event")
					l, ok := locks.Load(name)
					if ok {
						log.WithField("name", name).Info("Release lock")
						go func() {
							time.Sleep(500 * time.Millisecond)
							l.(*AccessLock).Unlock()
						}()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if err != nil {
					return
				}
			}
		}
	}()

	err = watcher.Add(path)
	if err != nil {
		return nil, errors.Wrap(err, "Failed add path for watcher")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if re.MatchString(r.URL.Path) {
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			name := filepath.Base(r.URL.Path)
			if _, err := os.Stat(path + r.URL.Path); os.IsNotExist(err) {
				log.WithField("name", name).Info("Add request lock")
				al, loaded := locks.LoadOrStore(name, NewAccessLock())
				var ticker *time.Ticker
				if !loaded {
					ticker := time.NewTicker(time.Second)
					go func() {
						for range ticker.C {
							lch <- name
						}
					}()
				}
				select {
				// case <-time.After(10 * time.Minute):
				case <-al.(*AccessLock).Unlocked():
				// case <-r.Context().Done():
				case <-ctx.Done():
					if ctx.Err() != nil {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					break
				}
				if ticker != nil {
					ticker.Stop()
				}
			}
			go func() {
				lch <- name
			}()
		}
		h.ServeHTTP(w, r)
	}), nil
}

func loggingHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.WithFields(log.Fields{
			"path":       r.URL.Path,
			"method":     r.Method,
			"remoteAddr": r.RemoteAddr,
		}).Info("Got HTTP request")
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

func enrichPlaylistHandler(h http.Handler, path string) http.Handler {
	re, _ := regexp.Compile(`\.m3u8$`)
	re2, _ := regexp.Compile(`\.[0-9a-z]{2,3}$`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "" || !re.MatchString(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		p := path + r.URL.Path
		file, err := os.Open(p)
		if err != nil {
			log.WithError(err).Fatal("Failed to open playlist")
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			text := scanner.Text()
			w.Write([]byte(text))
			if re2.MatchString(text) {
				w.Write([]byte("?"))
				w.Write([]byte(r.URL.RawQuery))
			}
			w.Write([]byte("\n"))
		}

		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
	})
}

func run(c *cli.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c.String("input") == "" {
		return errors.New("No url defined")
	}

	parsedURL, err := u.Parse(c.String("input"))
	if err != nil {
		return errors.Wrap(err, "Failed to parse url")
	}

	in := parsedURL.String()
	out := c.String("o")

	addr := fmt.Sprintf("%s:%d", c.String("host"), c.Int("port"))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to bind address")
	}
	defer ln.Close()
	agln := gracenet.NewGraceListener(ln, time.Duration(c.Int("access-grace"))*time.Second)
	paddr := fmt.Sprintf("%s:%d", c.String("host"), c.Int("probe-port"))
	pln, err := net.Listen("tcp", paddr)
	if err != nil {
		return errors.Wrap(err, "Failed to bind address")
	}
	defer pln.Close()
	httpError := make(chan error, 1)
	ch := make(chan string)
	go func() {
		log.WithFields(log.Fields{
			"addr":           addr,
			"accessGrace":    c.Int("access-grace"),
			"transcodeGrace": c.Int("transcode-grace"),
		}).Info("Start listening")
		mux := http.NewServeMux()
		if c.Bool("player") {
			log.Info(fmt.Sprintf("Player available at http://localhost:%d/player/", c.Int("port")))
			mux.Handle("/player/", http.StripPrefix("/player/", http.FileServer(http.Dir("./player"))))
		}
		pattern := "index.*"
		fileH := http.FileServer(http.Dir(c.String("o")))
		enrichH := enrichPlaylistHandler(fileH, out)
		waitH, err := waitHandler(enrichH, ctx, out, pattern, ch)
		if err != nil {
			httpError <- err
			return
		}
		mux.Handle("/", loggingHandler(allowCORSHandler(waitH, c.String("access-control-allow-origin"))))
		err = http.Serve(agln, mux)
		httpError <- err
	}()

	go func() {
		err := transcode(ctx, c, in, out, ch)
		if err != nil {
			log.WithError(err).Warn("Got transcoding error")
			cancel()
		}
	}()

	probeError := make(chan error, 1)
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})
		mux.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})
		err := http.Serve(pln, mux)
		probeError <- err
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-agln.Expire():
		log.Info("Nobody here for so long!")
	case sig := <-sigs:
		log.WithField("signal", sig).Info("Got syscall")
	case err := <-probeError:
		return errors.Wrap(err, "Got probe service error")
	case err := <-httpError:
		return errors.Wrap(err, "Got http error")
	}
	log.Info("Shooting down... at last!")
	return nil
}

func transcode(ctx context.Context, c *cli.Context, in string, out string, ch chan string) error {
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Int("probe-timeout"))*time.Second)
	defer cancel()

	pr, err := ProbeAndStore(probeCtx, c, fmt.Sprintf("%s/%s", out, "index.json"))
	if err != nil {
		return errors.Wrap(err, "Failed to probe and store")
	}

	opts := OptionsFromString(c.String("extra"))
	opts.grace = time.Duration(c.Int("transcode-grace")) * time.Second
	opts.preset = c.String("preset")

	err = Transcode(ctx, pr, in, out, opts, ch)
	if err != nil {
		return errors.Wrap(err, "Failed to transcode")
	}
	return nil
}

func main() {
	log.SetFormatter(joonix.NewFormatter())
	app := cli.NewApp()
	app.Name = "content-transcoder-server"
	app.Usage = "runs content transcoder"
	app.Version = "0.0.1"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "host, H",
			Usage: "listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  "port, P",
			Usage: "listening port",
			Value: 8080,
		},
		cli.IntFlag{
			Name:  "probe-port, pP",
			Usage: "probe port",
			Value: 8081,
		},
		cli.StringFlag{
			Name:   "input, i, url",
			Usage:  "input (url)",
			EnvVar: "INPUT, SOURCE_URL, URL",
		},
		cli.StringFlag{
			Name:  "output, o",
			Usage: "output (local path)",
			Value: "out",
		},
		cli.StringFlag{
			Name:   "content-prober-host, cpH",
			Usage:  "hostname of the content prober service",
			EnvVar: "CONTENT_PROBER_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   "content-prober-port, cpP",
			Usage:  "port of the content prober service",
			Value:  50051,
			EnvVar: "CONTENT_PROBER_SERVICE_PORT",
		},
		cli.IntFlag{
			Name:   "access-grace, ag",
			Usage:  "access grace in seconds",
			Value:  600,
			EnvVar: "GRACE",
		},
		cli.StringFlag{
			Name:   "preset",
			Usage:  "transcode preset",
			Value:  "ultrafast",
			EnvVar: "PRESET",
		},
		cli.IntFlag{
			Name:   "transcode-grace, tg",
			Usage:  "transcode grace in seconds",
			Value:  5,
			EnvVar: "TRANSCODE_GRACE",
		},
		cli.IntFlag{
			Name:   "probe-timeout, pt",
			Usage:  "probe timeout in seconds",
			Value:  600,
			EnvVar: "PROBE_TIMEOUT",
		},
		cli.StringFlag{
			Name:   "job-id",
			Usage:  "job id",
			Value:  "",
			EnvVar: "JOB_ID",
		},
		cli.StringFlag{
			Name:   "info-hash",
			Usage:  "info hash",
			Value:  "",
			EnvVar: "INFO_HASH",
		},
		cli.StringFlag{
			Name:   "file-path",
			Usage:  "file path",
			Value:  "",
			EnvVar: "FILE_PATH",
		},
		cli.StringFlag{
			Name:   "extra",
			Usage:  "extra",
			Value:  "",
			EnvVar: "EXTRA",
		},
		cli.BoolFlag{
			Name:  "player",
			Usage: "player",
		},
	}
	app.Action = run
	err := app.Run(os.Args)
	if err != nil {
		log.WithError(err).Fatal("Failed to serve application")
	}
}
