package services

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"gopkg.in/fsnotify.v1"
)

type Waiter struct {
	path       string
	re         *regexp.Regexp
	locks      sync.Map
	doneCh     chan error
	w          *fsnotify.Watcher
	transcoder *Transcoder
}

func NewWaiter(c *cli.Context, re *regexp.Regexp, transcoder *Transcoder) *Waiter {
	return &Waiter{
		path:       c.String(outputFlag),
		re:         re,
		doneCh:     make(chan error),
		transcoder: transcoder,
	}
}

func (s *Waiter) Serve() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.Wrap(err, "Failed to init watcher")
	}
	err = watcher.Add(s.path)
	if err != nil {
		return errors.Wrap(err, "Failed add path for watcher")
	}
	s.w = watcher
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				name := filepath.Base(event.Name)
				if s.re.MatchString(name) && event.Op == fsnotify.Create {
					log.WithField("name", name).Info("Got watcher event")
					l, ok := s.locks.Load(name)
					if ok {
						log.WithField("name", name).Info("Release lock")
						go func() {
							<-time.After(500 * time.Millisecond)
							l.(*AccessLock).Unlock()
						}()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if err != nil {
					s.doneCh <- err
					return
				}
			}
		}
	}()
	log.Info("Starting Waiter")
	return <-s.doneCh
}

func (s *Waiter) Wait(ctx context.Context, path string) chan error {
	errCh := make(chan error)
	go func() {
		if !s.re.MatchString(path) || s.transcoder.IsFinished() {
			errCh <- nil
		} else if _, err := os.Stat(s.path + path); os.IsNotExist(err) {
			log.WithField("name", s.path+path).Info("Add request lock")
			al, _ := s.locks.LoadOrStore(path, NewAccessLock())
			select {
			case <-al.(*AccessLock).Unlocked():
				errCh <- nil
				break
			case <-ctx.Done():
				errCh <- ctx.Err()
				break
			}
		} else {
			errCh <- nil
		}
	}()
	return errCh
}

func (s *Waiter) Close() {
	close(s.doneCh)
	if s.w != nil {
		s.w.Close()
	}
}
