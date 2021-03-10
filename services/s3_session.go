package services

import (
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// S3Client makes AWS SDK S3 Client from cli and environment variables
type S3Session struct {
	accessKeyID     string
	secretAccessKey string
	endpoint        string
	region          string
	noSSL           bool
	sess            *session.Session
	mux             sync.Mutex
	err             error
	cl              *http.Client
	inited          bool
}

const (
	awsAccessKeyID     = "aws-access-key-id"
	awsSecretAccessKey = "aws-secret-access-key"
	awsEndpoint        = "aws-endpoint"
	awsRegion          = "aws-region"
	awsNoSSL           = "aws-no-ssl"
)

func RegisterS3SessionFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   awsAccessKeyID,
		Usage:  "AWS Access Key ID",
		Value:  "",
		EnvVar: "AWS_ACCESS_KEY_ID",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   awsSecretAccessKey,
		Usage:  "AWS Secret Access Key",
		Value:  "",
		EnvVar: "AWS_SECRET_ACCESS_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   awsEndpoint,
		Usage:  "AWS Endpoint",
		Value:  "",
		EnvVar: "AWS_ENDPOINT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   awsRegion,
		Usage:  "AWS Region",
		Value:  "",
		EnvVar: "AWS_REGION",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   awsNoSSL,
		EnvVar: "AWS_NO_SSL",
	})
}

func NewS3Session(c *cli.Context, cl *http.Client) *S3Session {
	return &S3Session{
		accessKeyID:     c.String(awsAccessKeyID),
		secretAccessKey: c.String(awsSecretAccessKey),
		endpoint:        c.String(awsEndpoint),
		region:          c.String(awsRegion),
		noSSL:           c.Bool(awsNoSSL),
		cl:              cl,
	}
}

func (s *S3Session) Get() *session.Session {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.sess
	}
	s.sess = s.get()
	s.inited = true
	return s.sess
}

func (s *S3Session) get() *session.Session {
	log.Info("Initializing S3 Session")
	c := &aws.Config{
		Credentials:      credentials.NewStaticCredentials(s.accessKeyID, s.secretAccessKey, ""),
		Endpoint:         aws.String(s.endpoint),
		Region:           aws.String(s.region),
		DisableSSL:       aws.Bool(s.noSSL),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       s.cl,
	}
	return session.New(c)
}
