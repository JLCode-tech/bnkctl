package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeSTS implements STSAPI for unit tests using the explicit-interface
// mock pattern.
type fakeSTS struct {
	out *sts.GetCallerIdentityOutput
	err error
}

func (f *fakeSTS) GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return f.out, f.err
}

func TestCallerIdentity_ProjectsFields(t *testing.T) {
	account := "111122223333"
	arn := "arn:aws:iam::111122223333:user/awsbnkctl-test"
	userID := "AIDA1234567890EXAMPLE"
	c := &Clients{
		STS: &fakeSTS{
			out: &sts.GetCallerIdentityOutput{
				Account: &account,
				Arn:     &arn,
				UserId:  &userID,
			},
		},
	}
	got, err := c.CallerIdentity(context.Background())
	if err != nil {
		t.Fatalf("CallerIdentity: %v", err)
	}
	if got.Account != account || got.ARN != arn || got.UserID != userID {
		t.Fatalf("projection mismatch: got %+v", got)
	}
}

func TestCallerIdentity_PropagatesSDKError(t *testing.T) {
	sentinel := errors.New("sts denied")
	c := &Clients{STS: &fakeSTS{err: sentinel}}
	_, err := c.CallerIdentity(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestCallerIdentity_NilClient(t *testing.T) {
	var c *Clients
	_, err := c.CallerIdentity(context.Background())
	if err == nil {
		t.Fatal("expected error on nil Clients")
	}
}

func TestAwsStringOrEmpty(t *testing.T) {
	if aws_string_or_empty(nil) != "" {
		t.Fatal("nil should map to empty string")
	}
	v := "hello"
	if got := aws_string_or_empty(&v); got != "hello" {
		t.Fatalf("expected hello, got %q", got)
	}
}
