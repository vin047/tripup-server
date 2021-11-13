package storage

import (
	"errors"
	URL "net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
)

type s3Client struct {
    s3Session *session.Session
}

func NewS3Client(idToken string) (*s3Client, error) {
    endpoint := os.Getenv("AWS_ENDPOINT")
    s3PathStyle := endpoint != ""
    stsSession := session.Must(session.NewSessionWithOptions(session.Options{
        Config: aws.Config{
            Endpoint: aws.String(endpoint),
        },
        SharedConfigState: session.SharedConfigEnable,
    }))

    stsService := sts.New(stsSession)
    input := &sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          aws.String("arn:aws:iam::123456789012:role/FederatedWebIdentityRole"),
		RoleSessionName:  aws.String("app1"),
		WebIdentityToken: aws.String(idToken),
	}
    result, err := stsService.AssumeRoleWithWebIdentity(input)
    if err != nil {
        return nil, err
    }

    stsCredentials := credentials.NewStaticCredentials(*result.Credentials.AccessKeyId, *result.Credentials.SecretAccessKey, *result.Credentials.SessionToken)
    s3Client := s3Client{
        s3Session: session.Must(session.NewSessionWithOptions(session.Options{
            Config: aws.Config{
                Credentials: stsCredentials,
                Endpoint: aws.String(endpoint),
                S3ForcePathStyle: aws.Bool(s3PathStyle),
            },
            SharedConfigState: session.SharedConfigEnable,
        })),
    }
    return &s3Client, nil
}

func (client *s3Client) Filesizes(originalURL string) (uint64, uint64, error) {
    s3Service := s3.New(client.s3Session)
    url, err := URL.Parse(originalURL)
	if err != nil {
		return 0, 0, err
	}

	path := strings.SplitN(url.Path, "/", 3)
	bucket := path[1]
    keyOriginal := path[2]
    keyLow := strings.Replace(keyOriginal, "_original", "_low", -1)

    originalResult, err := s3Service.HeadObject(&s3.HeadObjectInput{
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

    lowResult, err := s3Service.HeadObject(&s3.HeadObjectInput{
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

func (client *s3Client) Delete(remotepaths []string) error {
    s3Service := s3.New(client.s3Session)
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
        _, err := s3Service.DeleteObjects(input)
        if err != nil {
            return err
        }
    }

    return nil
}
