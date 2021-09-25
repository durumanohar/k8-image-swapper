package registry

import (
	"encoding/base64"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	"github.com/dgraph-io/ristretto"
	"github.com/go-co-op/gocron"
	"github.com/rs/zerolog/log"
)

var execCommand = exec.Command

type ECRClient struct {
	client    ecriface.ECRAPI
	ecrDomain string
	authToken []byte
	cache     *ristretto.Cache
	scheduler *gocron.Scheduler
}

func (e *ECRClient) Credentials() string {
	return string(e.authToken)
}

func (e *ECRClient) CreateRepository(name string) error {
	if _, found := e.cache.Get(name); found {
		return nil
	}

	_, err := e.client.CreateRepository(&ecr.CreateRepositoryInput{
		RepositoryName: aws.String(name),
		ImageScanningConfiguration: &ecr.ImageScanningConfiguration{
			ScanOnPush: aws.Bool(true),
		},
		ImageTagMutability: aws.String(ecr.ImageTagMutabilityMutable),
		Tags: []*ecr.Tag{
			{
				Key:   aws.String("CreatedBy"),
				Value: aws.String("k8s-image-swapper"),
			},
		},
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case ecr.ErrCodeRepositoryAlreadyExistsException:
				// We ignore this case as it is valid.
			default:
				return err
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return err
		}
	}

	e.cache.Set(name, "", 1)

	return nil
}

func (e *ECRClient) RepositoryExists() bool {
	panic("implement me")
}

func (e *ECRClient) CopyImage() error {
	panic("implement me")
}

func (e *ECRClient) PullImage() error {
	panic("implement me")
}

func (e *ECRClient) PutImage() error {
	panic("implement me")
}

func (e *ECRClient) ImageExists(ref string) bool {
	if _, found := e.cache.Get(ref); found {
		return true
	}

	app := "skopeo"
	args := []string{
		"inspect",
		"--retry-times", "3",
		"docker://" + ref,
		"--creds", e.Credentials(),
	}

	log.Trace().Str("app", app).Strs("args", args).Msg("executing command to inspect image")
	cmd := execCommand(app, args...)

	if _, err := cmd.Output(); err != nil {
		return false
	}

	e.cache.Set(ref, "", 1)

	return true
}

func (e *ECRClient) Endpoint() string {
	return e.ecrDomain
}

// requestAuthToken requests and returns an authentication token from ECR with its expiration date
func (e *ECRClient) requestAuthToken() ([]byte, time.Time, error) {
	getAuthTokenOutput, err := e.client.GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return []byte(""), time.Time{}, err
	}

	authToken, err := base64.StdEncoding.DecodeString(*getAuthTokenOutput.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return []byte(""), time.Time{}, err
	}

	return authToken, *getAuthTokenOutput.AuthorizationData[0].ExpiresAt, nil
}

// scheduleTokenRenewal sets a scheduler to execute token renewal before the token expires
func (e *ECRClient) scheduleTokenRenewal() error {
	token, expiryAt, err := e.requestAuthToken()
	if err != nil {
		return err
	}

	renewalAt := expiryAt.Add(-2 * time.Minute)
	e.authToken = token

	log.Debug().Time("expiryAt", expiryAt).Time("renewalAt", renewalAt).Msg("auth token set, schedule next token renewal")

	j, _ := e.scheduler.Every(1).StartAt(renewalAt).Do(e.scheduleTokenRenewal)
	j.LimitRunsTo(1)

	return nil
}

func NewECRClient(region string, ecrDomain string) (*ECRClient, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	ecrClient := ecr.New(sess, &aws.Config{Region: aws.String(region)})

	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,     // number of keys to track frequency of (10M).
		MaxCost:     1 << 30, // maximum cost of cache (1GB).
		BufferItems: 64,      // number of keys per Get buffer.
	})
	if err != nil {
		panic(err)
	}

	scheduler := gocron.NewScheduler(time.UTC)
	scheduler.StartAsync()

	client := &ECRClient{
		client:    ecrClient,
		ecrDomain: ecrDomain,
		cache:     cache,
		scheduler: scheduler,
	}

	if err := client.scheduleTokenRenewal(); err != nil {
		return nil, err
	}

	return client, nil
}

func NewMockECRClient(ecrClient ecriface.ECRAPI, region string, ecrDomain string) (*ECRClient, error) {
	client := &ECRClient{
		client:    ecrClient,
		ecrDomain: ecrDomain,
		cache:     nil,
		scheduler: nil,
		authToken: []byte("mock-ecr-client-fake-auth-token"),
	}

	return client, nil
}
