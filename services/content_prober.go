package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	u "net/url"
	"os"
	"os/exec"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	cp "github.com/webtor-io/content-prober/content-prober"
	"github.com/webtor-io/lazymap"
	"google.golang.org/grpc"
)

const (
	contentProberHostFlag    = "content-prober-host"
	contentProberPortFlag    = "content-prober-port"
	contentProberTimeoutFlag = "content-prober-timeout"
)

func RegisterContentProberFlags(f []cli.Flag) []cli.Flag {
	return append(f, cli.StringFlag{
		Name:   contentProberHostFlag,
		Usage:  "hostname of the content prober service",
		EnvVar: "CONTENT_PROBER_SERVICE_HOST",
	}, cli.IntFlag{
		Name:   contentProberPortFlag,
		Usage:  "port of the content prober service",
		Value:  50051,
		EnvVar: "CONTENT_PROBER_SERVICE_PORT",
	}, cli.IntFlag{
		Name:   contentProberTimeoutFlag,
		Usage:  "probe timeout in seconds",
		Value:  600,
		EnvVar: "CONTENT_PROBER_TIMEOUT",
	})
}

type ContentProbe struct {
	lazymap.LazyMap
	host    string
	port    int
	timeout int
}

func NewContentProbe(c *cli.Context) *ContentProbe {
	return &ContentProbe{
		host:    c.String(contentProberHostFlag),
		port:    c.Int(contentProberPortFlag),
		timeout: c.Int(contentProberTimeoutFlag),
		LazyMap: lazymap.New(&lazymap.Config{
			Expire:      30 * time.Minute,
			ErrorExpire: 10 * time.Second,
		}),
	}
}

func (s *ContentProbe) Get(input string, out string) (*cp.ProbeReply, error) {
	pr, err := s.LazyMap.Get(input+out, func() (interface{}, error) {
		return s.get(input, out)
	})
	return pr.(*cp.ProbeReply), err
}

func (s *ContentProbe) get(input string, out string) (pr *cp.ProbeReply, err error) {
	probeFilePath := out + "/index.json"

	// Check if the file already exists
	if _, err := os.Stat(probeFilePath); err == nil {
		// File exists, read its content
		fileContent, err := os.ReadFile(probeFilePath)
		if err != nil {
			return nil, errors.Wrap(err, "failed to read existing probe result")
		}
		// Unmarshal JSON content into ProbeReply
		pr = &cp.ProbeReply{}
		err = json.Unmarshal(fileContent, pr)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal existing probe result")
		}
		log.WithField("file", probeFilePath).Info("using existing probe result")
		return pr, nil
	}

	// File does not exist, proceed with probing
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()
	if s.host == "" {
		pr, err = s.localProbe(ctx, input)
	} else {

		pr, err = s.remoteProbe(ctx, input)
	}

	if err != nil {
		return nil, errors.Wrap(err, "failed to probe")
	}

	json, err := json.Marshal(pr)

	if err != nil {
		return nil, errors.Wrap(err, "failed to convert probe result to json")
	}

	err = os.WriteFile(probeFilePath, json, 0644)

	if err != nil {
		return nil, errors.Wrap(err, "failed to write probe result")
	}

	return
}

func (s *ContentProbe) remoteProbe(ctx context.Context, input string) (*cp.ProbeReply, error) {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial probing service")
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)
	cl := cp.NewContentProberClient(conn)

	req := cp.ProbeRequest{
		Url: input,
	}
	log := log.WithField("request", req)
	log.Info("sending probing request")

	r, err := cl.Probe(ctx, &req)
	if err != nil {
		return nil, errors.Wrap(err, "probing failed")
	}
	log.WithField("reply", r).Info("got probing reply")

	return r, nil

}

func (s *ContentProbe) localProbe(ctx context.Context, input string) (*cp.ProbeReply, error) {
	done := make(chan error)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.Wrap(err, "unable to find ffprobe")
	}
	parsedURL, err := u.Parse(input)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse url")
	}
	cmdText := fmt.Sprintf("%s -show_format -show_streams -print_format json '%s'", ffprobe, parsedURL.String())
	log.WithField("cmd", cmdText).Info("running ffprobe command")
	cmd := exec.Command(ffprobe, "-show_format", "-show_streams", "-print_format", "json", parsedURL.String())
	var bufOut bytes.Buffer
	var bufErr bytes.Buffer
	cmd.Stdout = &bufOut
	cmd.Stderr = &bufErr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	if err != nil {
		return nil, errors.Wrap(err, "unable to start ffprobe")
	}

	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil, errors.Wrap(ctx.Err(), "context done")
	case err := <-done:
		output := bufOut.String()
		stdErr := bufErr.String()
		if err != nil {
			return nil, errors.Wrapf(err, "probing failed with err=%v", stdErr)
		}
		var rep cp.ProbeReply
		err = json.Unmarshal([]byte(output), &rep)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to unmarshal output=%v", output)
		}
		log.WithField("output", rep).Info("probing finished")
		return &rep, nil
	}

}
