package services

import (
	"sync"

	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/sirupsen/logrus"
)

type S3Client struct {
	sess   *S3Session
	s3     *s3.S3
	mux    sync.Mutex
	err    error
	inited bool
}

func NewS3Client(sess *S3Session) *S3Client {
	return &S3Client{sess: sess}
}

func (s *S3Client) Get() *s3.S3 {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.s3
	}
	s.s3 = s.get()
	s.inited = true
	return s.s3
}

func (s *S3Client) get() *s3.S3 {
	log.Info("Initializing S3 Client")
	return s3.New(s.sess.Get())
}
