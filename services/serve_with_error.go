package services

import (
	"io/ioutil"
	"time"

	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

const (
	errorExpireFlag = "error-expire"
)

func RegisterServerWithErrorFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.Int64Flag{
		Name:   errorExpireFlag,
		Value:  600,
		EnvVar: "ERROR_EXPIRE",
	})
}

type ServeWithError struct {
	s            cs.Servable
	out          string
	expire       time.Duration
	errorHandler func(err error)
}

func NewServeWithError(c *cli.Context, s cs.Servable, eh func(err error)) *ServeWithError {
	return &ServeWithError{
		out:          c.String(outputFlag),
		expire:       time.Duration(c.Int64(errorExpireFlag)) * time.Second,
		s:            s,
		errorHandler: eh,
	}
}
func (s *ServeWithError) Serve() error {
	if err := s.s.Serve(); err != nil {
		ioutil.WriteFile(s.out+"/error.log", []byte(err.Error()), 0644)
		s.errorHandler(err)
		<-time.After(s.expire)
	}
	return nil
}
