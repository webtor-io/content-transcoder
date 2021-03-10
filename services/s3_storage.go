package services

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type S3Storage struct {
	bucket       string
	bucketSpread bool
	sess         *S3Session
	cl           *S3Client
}

type fileWalk chan string

func (f fileWalk) Walk(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if !info.IsDir() {
		f <- path
	}
	return nil
}

const (
	awsBucket = "aws-bucket"
)

func RegisterS3StorageFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   awsBucket,
		Usage:  "AWS Bucket",
		Value:  "",
		EnvVar: "AWS_BUCKET",
	})
}

func NewS3Storage(c *cli.Context, cl *S3Client, sess *S3Session) *S3Storage {
	return &S3Storage{
		bucket: c.String(awsBucket),
		cl:     cl,
		sess:   sess,
	}
}

func (s *S3Storage) uploadFile(ctx context.Context, uploader *s3manager.Uploader, path string, key string, out string) error {
	rel, err := filepath.Rel(out, path)
	if err != nil {
		return errors.Wrapf(err, "Failed to get relative path=%v", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.Wrapf(err, "Failed to open file path=%v", path)
	}
	defer file.Close()
	result, err := uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket: &s.bucket,
		Key:    aws.String(filepath.Join(key, rel)),
		Body:   file,
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to upload file path=%v", path)
	}
	log.Infof("File uploaded path=%v to location=%v", path, result.Location)
	return nil
}

func (s *S3Storage) Upload(ctx context.Context, key string, out string) error {
	walker := make(fileWalk)
	walkErrCh := make(chan error)
	go func() {
		// Gather the files to upload by walking the path recursively
		if err := filepath.Walk(out, walker.Walk); err != nil {
			walkErrCh <- errors.Wrapf(err, "Failed to walk")
			return
		}
		close(walker)
	}()

	uploadErrCh := make(chan error)
	// For each file found walking, upload it to S3
	go func() {
		uploader := s3manager.NewUploader(s.sess.Get())
		for path := range walker {
			err := s.uploadFile(ctx, uploader, path, key, out)
			if err != nil {
				uploadErrCh <- errors.Wrapf(err, "Failed to upload file path=%v", path)
			}
		}
		uploadErrCh <- nil
	}()

	select {
	case err := <-walkErrCh:
		return err
	case err := <-uploadErrCh:
		if err != nil {
			return err
		}
	}
	return s.SetDoneMarker(ctx, key)
}

func (s *S3Storage) SetDoneMarker(ctx context.Context, key string) (err error) {
	key = "done/" + key
	log.Infof("Store done marker bucket=%v key=%v", s.bucket, key)
	_, err = s.cl.Get().PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("")),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to store done marker bucket=%v key=%v", s.bucket, key)
	}
	return
}

func (s *S3Storage) CheckDoneMarker(ctx context.Context, key string) (bool, error) {
	key = "done/" + key
	log.Infof("Check done marker bucket=%v key=%v", s.bucket, key)
	_, err := s.cl.Get().GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return false, nil
		}
		return false, errors.Wrapf(err, "Failed to check done marker bucket=%v key=%v", s.bucket, key)
	}
	return true, nil
}

func (s *S3Storage) StoreDownloadedSize(ctx context.Context, key string, i uint64) (err error) {
	key = "downloaded_size/" + key
	log.Infof("Store downloaded size bucket=%v key=%v size=%v", s.bucket, key, i)
	_, err = s.cl.Get().PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(strconv.Itoa(int(i)))),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to store downloaded size bucket=%v key=%v", s.bucket, key)
	}
	return
}

func (s *S3Storage) FetchDownloadedSize(ctx context.Context, key string) (uint64, error) {
	key = "downloaded_size/" + key
	r, err := s.cl.Get().GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return 0, nil
		}
		return 0, errors.Wrapf(err, "Failed to fetch downloaded size bucket=%v key=%v", s.bucket, key)
	}
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	i, err := strconv.Atoi(string(data))
	log.Infof("Size fetch completed bucket=%v key=%v size=%v", s.bucket, key, i)
	return uint64(i), nil
}

func (s *S3Storage) Touch(ctx context.Context, key string) (err error) {
	key = "touch/" + key
	log.Infof("Touching bucket=%v key=%v", s.bucket, key)
	_, err = s.cl.Get().PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(fmt.Sprintf("%v", time.Now().Unix()))),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to touch bucket=%v key=%v", s.bucket, key)
	}
	return
}
