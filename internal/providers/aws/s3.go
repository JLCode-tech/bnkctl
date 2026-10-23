package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3API is the subset of s3.Client surface awsbnkctl exercises. The
// init wizard uses PutObject to upload the FAR archive + JWT to the
// supply-chain bucket; doctor uses HeadBucket to probe s3:PutObject
// permission against a fresh workspace.
//
// Tests inject a fake; production code constructs the real client via
// s3.NewFromConfig in EnsureS3.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

// EnsureS3 constructs a real s3.Client off the resolved aws.Config and
// caches it on the Clients struct. S3 is not pre-built in NewClients
// because not every awsbnkctl invocation touches S3 — the init wizard
// does, doctor's S3 probe does, every other verb doesn't.
//
// Idempotent: returns the already-cached client on subsequent calls.
func (c *Clients) EnsureS3() (S3API, error) {
	if c == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if c.s3 != nil {
		return c.s3, nil
	}
	if c.AWSConfig.Region == "" && c.Region == "" {
		return nil, errors.New("aws.Clients region is empty; cannot construct S3 client")
	}
	c.s3 = s3.NewFromConfig(c.AWSConfig)
	return c.s3, nil
}

// PutObject uploads body to bucket/key with SSE-KMS enforced (the
// bucket policy DenyUnencryptedUploads statement rejects any PutObject
// without aws:kms server-side encryption). If
// kmsKeyID is empty the bucket's default CMK is used.
//
// Returns the ETag from the SDK response so callers can pin
// upload integrity for downstream verification.
func (c *Clients) PutObject(ctx context.Context, bucket, key string, body io.Reader, kmsKeyID string) (string, error) {
	if bucket == "" || key == "" {
		return "", errors.New("bucket and key must be non-empty")
	}
	cli, err := c.EnsureS3()
	if err != nil {
		return "", err
	}
	in := &s3.PutObjectInput{
		Bucket:               &bucket,
		Key:                  &key,
		Body:                 body,
		ServerSideEncryption: s3types.ServerSideEncryptionAwsKms,
	}
	if kmsKeyID != "" {
		in.SSEKMSKeyId = &kmsKeyID
	}
	out, err := cli.PutObject(ctx, in)
	if err != nil {
		return "", fmt.Errorf("s3:PutObject s3://%s/%s: %w", bucket, key, err)
	}
	return aws_string_or_empty(out.ETag), nil
}

// HeadObject probes whether bucket/key exists + the caller has
// s3:GetObject permission. Returns ObjectInfo on success, an
// IsNotFound-discriminable error when the object is missing, and a
// wrapped error otherwise.
//
// Used by the init wizard to detect "FAR archive already uploaded"
// scenarios (avoids re-uploading a multi-GB tarball on re-runs).
type ObjectInfo struct {
	ContentLength int64
	ETag          string
	VersionID     string
	ContentType   string
}

// HeadObject calls s3:HeadObject.
func (c *Clients) HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	if bucket == "" || key == "" {
		return nil, errors.New("bucket and key must be non-empty")
	}
	cli, err := c.EnsureS3()
	if err != nil {
		return nil, err
	}
	out, err := cli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("s3:HeadObject s3://%s/%s: %w", bucket, key, err)
	}
	info := &ObjectInfo{
		ETag:        aws_string_or_empty(out.ETag),
		VersionID:   aws_string_or_empty(out.VersionId),
		ContentType: aws_string_or_empty(out.ContentType),
	}
	if out.ContentLength != nil {
		info.ContentLength = *out.ContentLength
	}
	return info, nil
}

// HeadBucket probes bucket reachability + the caller's
// s3:HeadBucket / s3:ListBucket permission. The doctor uses this as
// the "PutObject permission feasibility" probe: HeadBucket
// against the workspace's expected supply-chain bucket name returns
// either OK (bucket exists, perm OK), NotFound (bucket doesn't exist
// yet — that's fine pre-`awsbnkctl up`), or AccessDenied (bucket
// exists but the cred can't reach it — actionable IAM gap).
func (c *Clients) HeadBucket(ctx context.Context, bucket string) error {
	if bucket == "" {
		return errors.New("bucket must be non-empty")
	}
	cli, err := c.EnsureS3()
	if err != nil {
		return err
	}
	_, err = cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
	if err != nil {
		return fmt.Errorf("s3:HeadBucket %s: %w", bucket, err)
	}
	return nil
}

// IsS3NotFound returns true when err is the S3 "NoSuchKey" /
// "NoSuchBucket" / 404 condition. Used by the doctor probe to
// distinguish "bucket not created yet" (acceptable) from "AccessDenied"
// (actionable).
func IsS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	// HeadBucket / HeadObject sometimes surface NotFound through the
	// operation error wrap without a typed AS match; fall back to a
	// substring check.
	s := err.Error()
	return strings.Contains(s, "NotFound") || strings.Contains(s, "NoSuchKey") || strings.Contains(s, "NoSuchBucket") || strings.Contains(s, "status code: 404")
}

// IsS3AccessDenied returns true when err is the S3 AccessDenied / 403
// condition. Distinct from NotFound — drives a different doctor row
// remediation message.
func IsS3AccessDenied(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "AccessDenied") ||
		strings.Contains(s, "StatusCode: 403") ||
		strings.Contains(s, "status code: 403") ||
		strings.Contains(s, "Forbidden")
}
