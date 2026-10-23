package aws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 implements S3API for unit tests. Captures the last input
// per method so assertions can pin the wire shape (SSE-KMS, key, etc).
type fakeS3 struct {
	putErr     error
	putOut     *s3.PutObjectOutput
	lastPut    *s3.PutObjectInput
	headObjErr error
	headObjOut *s3.HeadObjectOutput
	headBucErr error
}

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.lastPut = in
	if f.putErr != nil {
		return nil, f.putErr
	}
	return f.putOut, nil
}

func (f *fakeS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return f.headObjOut, f.headObjErr
}

func (f *fakeS3) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, f.headBucErr
}

func TestPutObject_EnforcesSSEKMS(t *testing.T) {
	etag := `"deadbeef"`
	fake := &fakeS3{putOut: &s3.PutObjectOutput{ETag: &etag}}
	c := &Clients{}
	c.SetS3ForTest(fake)

	body := bytes.NewBufferString("payload")
	got, err := c.PutObject(context.Background(), "bnk-bucket", "far-auth.tar.gz", body, "arn:aws:kms:us-east-1:111122223333:key/abc")
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got != etag {
		t.Errorf("etag mismatch: got %q, want %q", got, etag)
	}
	if fake.lastPut == nil {
		t.Fatal("no PutObject call captured")
	}
	if fake.lastPut.ServerSideEncryption != s3types.ServerSideEncryptionAwsKms {
		t.Errorf("SSE: got %q, want aws:kms (bucket policy denies anything else)", fake.lastPut.ServerSideEncryption)
	}
	if fake.lastPut.SSEKMSKeyId == nil || *fake.lastPut.SSEKMSKeyId == "" {
		t.Error("SSEKMSKeyId should be set when caller passes a CMK")
	}
}

func TestPutObject_RejectsEmptyBucketOrKey(t *testing.T) {
	c := &Clients{}
	c.SetS3ForTest(&fakeS3{})

	cases := []struct{ bucket, key string }{
		{"", "k"},
		{"b", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if _, err := c.PutObject(context.Background(), tc.bucket, tc.key, io.NopCloser(bytes.NewReader(nil)), ""); err == nil {
			t.Errorf("expected error for bucket=%q key=%q", tc.bucket, tc.key)
		}
	}
}

func TestPutObject_PropagatesSDKError(t *testing.T) {
	sentinel := errors.New("AccessDenied")
	fake := &fakeS3{putErr: sentinel}
	c := &Clients{}
	c.SetS3ForTest(fake)

	_, err := c.PutObject(context.Background(), "b", "k", bytes.NewReader(nil), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHeadObject_ProjectsFields(t *testing.T) {
	length := int64(12345)
	etag := `"abc"`
	vid := "v1"
	ct := "application/gzip"
	fake := &fakeS3{
		headObjOut: &s3.HeadObjectOutput{
			ContentLength: &length,
			ETag:          &etag,
			VersionId:     &vid,
			ContentType:   &ct,
		},
	}
	c := &Clients{}
	c.SetS3ForTest(fake)

	info, err := c.HeadObject(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.ContentLength != length || info.ETag != etag || info.VersionID != vid || info.ContentType != ct {
		t.Errorf("projection mismatch: %+v", info)
	}
}

func TestIsS3NotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"typed NotFound", &s3types.NotFound{}, true},
		{"typed NoSuchKey", &s3types.NoSuchKey{}, true},
		{"typed NoSuchBucket", &s3types.NoSuchBucket{}, true},
		{"wrapped string", errors.New("operation error S3: HeadObject, https response error StatusCode: 404, RequestID: x, NotFound: "), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsS3NotFound(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsS3AccessDenied(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"AccessDenied", errors.New("operation error S3: AccessDenied: bucket policy denies"), true},
		{"403", errors.New("https response error StatusCode: 403, RequestID: x"), true},
		{"Forbidden", errors.New("operation error S3: Forbidden: cross-account"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsS3AccessDenied(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnsureS3_NilClients(t *testing.T) {
	var c *Clients
	if _, err := c.EnsureS3(); err == nil {
		t.Fatal("expected error on nil Clients")
	}
}

func TestEnsureS3_EmptyRegion(t *testing.T) {
	c := &Clients{}
	if _, err := c.EnsureS3(); err == nil {
		t.Fatal("expected error when region is empty")
	}
}
