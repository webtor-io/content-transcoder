package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	u "net/url"
	"os/exec"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	cp "github.com/webtor-io/content-prober/content-prober"
	"google.golang.org/grpc"
)

const (
	contentProberHostFlag    = "content-prober-host"
	contentProberPortFlag    = "content-prober-port"
	contentProberTimeoutFlag = "content-prober-timeout"
)

func RegisterContentProberFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   contentProberHostFlag,
		Usage:  "hostname of the content prober service",
		EnvVar: "CONTENT_PROBER_SERVICE_HOST",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   contentProberPortFlag,
		Usage:  "port of the content prober service",
		Value:  50051,
		EnvVar: "CONTENT_PROBER_SERVICE_PORT",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   contentProberTimeoutFlag,
		Usage:  "probe timeout in seconds",
		Value:  600,
		EnvVar: "CONTENT_PROBER_TIMEOUT",
	})
}

type ContentProbe struct {
	host    string
	port    int
	timeout int
	input   string
	mux     sync.Mutex
	err     error
	r       *cp.ProbeReply
	inited  bool
	out     string
}

func NewContentProbe(c *cli.Context) *ContentProbe {
	return &ContentProbe{
		host:    c.String(contentProberHostFlag),
		port:    c.Int(contentProberPortFlag),
		timeout: c.Int(contentProberTimeoutFlag),
		input:   c.String(inputFlag),
		out:     c.String(outputFlag),
	}
}

func (s *ContentProbe) Get() (*cp.ProbeReply, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.r, s.err
	}
	s.r, s.err = s.get()
	s.inited = true
	return s.r, s.err
}

func (s *ContentProbe) get() (pr *cp.ProbeReply, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()
	if s.host == "" {
		pr, err = s.localProbe(ctx)
	} else {

		pr, err = s.remoteProbe(ctx)
	}

	if err != nil {
		return nil, errors.Wrap(err, "Failed to probe")
	}

	json, err := json.Marshal(pr)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert probe result to json")
	}

	err = ioutil.WriteFile(s.out+"/index.json", json, 0644)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to write probe result")
	}

	return
}

func (s *ContentProbe) remoteProbe(ctx context.Context) (*cp.ProbeReply, error) {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, errors.Wrap(err, "Failed to dial probing service")
	}
	defer conn.Close()
	cl := cp.NewContentProberClient(conn)

	req := cp.ProbeRequest{
		Url: s.input,
	}
	log := log.WithField("request", req)
	log.Info("Sending probing request")

	r, err := cl.Probe(ctx, &req)
	if err != nil {
		return nil, errors.Wrap(err, "Probing failed")
	}
	log.WithField("reply", r).Info("Got probing reply")

	return r, nil

}

func (s *ContentProbe) localProbe(ctx context.Context) (*cp.ProbeReply, error) {
	done := make(chan error)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.Wrap(err, "Unable to find ffprobe")
	}
	parsedURL, err := u.Parse(s.input)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to parse url")
	}
	cmdText := fmt.Sprintf("%s -show_format -show_streams -print_format json '%s'", ffprobe, parsedURL.String())
	log.WithField("cmd", cmdText).Info("Running ffprobe command")
	cmd := exec.Command(ffprobe, "-show_format", "-show_streams", "-print_format", "json", parsedURL.String())
	var bufOut bytes.Buffer
	var bufErr bytes.Buffer
	cmd.Stdout = &bufOut
	cmd.Stderr = &bufErr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	if err != nil {
		return nil, errors.Wrap(err, "Unable to start ffprobe")
	}

	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil, errors.Wrap(ctx.Err(), "Context done")
	case err := <-done:
		output := bufOut.String()
		stdErr := bufErr.String()
		if err != nil {
			return nil, errors.Wrapf(err, "Probing failed with err=%v", stdErr)
		}
		var rep cp.ProbeReply
		err = json.Unmarshal([]byte(output), &rep)
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to unmarshal output=%v", output)
		}
		log.WithField("output", rep).Info("Probing finished")
		return &rep, nil
	}

}
