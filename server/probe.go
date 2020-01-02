package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	u "net/url"
	"os/exec"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cp "github.com/webtor-io/content-prober/content-prober"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func remoteProbe(ctx context.Context, source string, host string, port int, meta map[string]string) (*cp.ProbeReply, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, errors.Wrap(err, "Failed to dial probing service")
	}
	defer conn.Close()
	cl := cp.NewContentProberClient(conn)

	md := metadata.New(meta)
	ctx = metadata.NewOutgoingContext(ctx, md)
	req := cp.ProbeRequest{
		Url: source,
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

func localProbe(ctx context.Context, source string) (*cp.ProbeReply, error) {
	done := make(chan error)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.Wrap(err, "Unable to find ffprobe")
	}
	parsedURL, err := u.Parse(source)
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
	err = cmd.Start()
	if err != nil {
		return nil, errors.Wrap(err, "Unable to start ffprobe")
	}

	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
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

func Probe(ctx context.Context, c *cli.Context) (*cp.ProbeReply, error) {
	source := c.String("input")
	if c.String("content-prober-host") == "" {
		return localProbe(ctx, source)
	}
	return remoteProbe(ctx, source, c.String("cpH"), c.Int("cpP"), map[string]string{
		"job-id":    c.String("job-id"),
		"file-path": c.String("file-path"),
		"info-hash": c.String("info-hash"),
	})
}

func ProbeAndStore(ctx context.Context, c *cli.Context, output string) (*cp.ProbeReply, error) {
	pr, err := Probe(ctx, c)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to probe")
	}

	json, err := json.Marshal(pr)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert probe result to json")
	}

	err = ioutil.WriteFile(output, json, 0644)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to write probe result")
	}

	return pr, nil
}
