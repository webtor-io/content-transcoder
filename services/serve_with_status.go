package services

import (
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

const (
	statusExpireFlag = "status-expire"
)

func RegisterServerWithErrorFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.Int64Flag{
		Name:   statusExpireFlag,
		Value:  300,
		EnvVar: "STATUS_EXPIRE",
	})
}

type ServeWithStatus struct {
	s            cs.Servable
	out          string
	expire       time.Duration
	endHandler   func(err error)
	toCompletion bool
}

func NewServeWithStatus(c *cli.Context, s cs.Servable, eh func(err error)) *ServeWithStatus {
	return &ServeWithStatus{
		out:          c.String(OutputFlag),
		expire:       time.Duration(c.Int64(statusExpireFlag)) * time.Second,
		s:            s,
		endHandler:   eh,
		toCompletion: c.Bool(ToCompletionFlag),
	}
}
func (s *ServeWithStatus) Serve() error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	if err := s.s.Serve(); err != nil {
		log.WithError(err).Error("Setting error status")
		os.Create(s.out + "/error")
		ioutil.WriteFile(s.out+"/error.log", []byte(err.Error()), 0644)
		s.endHandler(err)
		select {
		case <-sigs:
		case <-time.After(s.expire):
		}
	} else if s.toCompletion {
		log.Info("Setting done status")
		os.Create(s.out + "/done")
		s.endHandler(nil)
		select {
		case <-sigs:
		case <-time.After(s.expire):
		}
	}
	return nil
}
