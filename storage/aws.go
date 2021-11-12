package storage

import (
	"errors"
	URL "net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type s3Backend struct {
    s3Session *session.Session
}

func NewS3Backend() *s3Backend {
    endpoint := os.Getenv("AWS_ENDPOINT")
    return &s3Backend{
        s3Session: session.Must(session.NewSessionWithOptions(session.Options{
            Config: aws.Config{
                Endpoint: &endpoint,
            },
            SharedConfigState: session.SharedConfigEnable,
        }))}
}

func (backend *s3Backend) Filesizes(originalURL string) (uint64, uint64, error) {
    s3Client := s3.New(backend.s3Session)
    url, err := URL.Parse(originalURL)
	if err != nil {
		return 0, 0, err
	}

	path := strings.SplitN(url.Path, "/", 3)
	bucket := path[1]
    keyOriginal := path[2]
    keyLow := strings.Replace(keyOriginal, "_original", "_low", -1)

    originalResult, err := s3Client.HeadObject(&s3.HeadObjectInput{
        Bucket: &bucket,
        Key: &keyOriginal,
    })
    if err != nil {
        return 0, 0, err
    }
    originalLength := *originalResult.ContentLength
    if originalLength < 0 {
        return 0, 0, errors.New("content length < 0 for original asset")
    }

    lowResult, err := s3Client.HeadObject(&s3.HeadObjectInput{
        Bucket: &bucket,
        Key: &keyLow,
    })
    if err != nil {
        return 0, 0, err
    }
    lowLength := *lowResult.ContentLength
    if lowLength < 0 {
        return 0, 0, errors.New("content length < 0 for low asset")
    }

    return uint64(originalLength), uint64(lowLength), nil
}

func (backend *s3Backend) Delete(remotepaths []string) error {
    s3Client := s3.New(backend.s3Session)
    s3Objects := map[string]*[]*s3.ObjectIdentifier{}

    for _, remotepath := range remotepaths {
        url, err := URL.Parse(remotepath)
        if err != nil {
            return err
        }
        path := strings.SplitN(url.Path, "/", 3)
	    bucket := path[1]
        key := path[2]

        _, ok := s3Objects[bucket]
		if !ok {
			s3Objects[bucket] = &[]*s3.ObjectIdentifier{}
        }
        *s3Objects[bucket] = append(*s3Objects[bucket], &s3.ObjectIdentifier {
            Key: &key,
        })
    }

    for bucket, objects := range s3Objects {
        input := &s3.DeleteObjectsInput {
            Bucket: &bucket,
            Delete: &s3.Delete{
                Objects: *objects,
                Quiet: aws.Bool(true),
            },
        }
        _, err := s3Client.DeleteObjects(input)
        if err != nil {
            return err
        }
    }

    return nil
}
