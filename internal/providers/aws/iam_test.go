package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// fakeIAM implements IAMAPI for unit tests.
type fakeIAM struct {
	oidcOut *iam.GetOpenIDConnectProviderOutput
	oidcErr error
	roleOut *iam.GetRoleOutput
	roleErr error
}

func (f *fakeIAM) GetOpenIDConnectProvider(ctx context.Context, in *iam.GetOpenIDConnectProviderInput, opts ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error) {
	return f.oidcOut, f.oidcErr
}

func (f *fakeIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return f.roleOut, f.roleErr
}

func TestGetOIDCProvider_ProjectsFields(t *testing.T) {
	url := "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
	key, val := "Env", "demo"
	fake := &fakeIAM{
		oidcOut: &iam.GetOpenIDConnectProviderOutput{
			Url:          &url,
			ClientIDList: []string{"sts.amazonaws.com"},
			Tags: []iamtypes.Tag{
				{Key: &key, Value: &val},
			},
		},
	}
	c := &Clients{}
	c.SetIAMForTest(fake)

	info, err := c.GetOIDCProvider(context.Background(), "arn:aws:iam::111122223333:oidc-provider/"+url)
	if err != nil {
		t.Fatalf("GetOIDCProvider: %v", err)
	}
	if info.URL != url {
		t.Errorf("URL: got %q want %q", info.URL, url)
	}
	if len(info.ClientIDs) != 1 || info.ClientIDs[0] != "sts.amazonaws.com" {
		t.Errorf("ClientIDs: %+v", info.ClientIDs)
	}
	if info.Tags["Env"] != "demo" {
		t.Errorf("Tags: %+v", info.Tags)
	}
}

func TestGetOIDCProvider_RejectsEmptyARN(t *testing.T) {
	c := &Clients{}
	c.SetIAMForTest(&fakeIAM{})
	if _, err := c.GetOIDCProvider(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty ARN")
	}
}

func TestHasIRSARole_NotFoundIsNilNil(t *testing.T) {
	fake := &fakeIAM{roleErr: &iamtypes.NoSuchEntityException{}}
	c := &Clients{}
	c.SetIAMForTest(fake)

	info, err := c.HasIRSARole(context.Background(), "missing-role")
	if err != nil {
		t.Fatalf("HasIRSARole: unexpected error %v", err)
	}
	if info != nil {
		t.Fatalf("HasIRSARole: expected nil info for missing role, got %+v", info)
	}
}

func TestHasIRSARole_ProjectsFields(t *testing.T) {
	name := "awsbnkctl-demo-flo-supply-chain-reader"
	arn := "arn:aws:iam::111122223333:role/" + name
	path := "/"
	fake := &fakeIAM{
		roleOut: &iam.GetRoleOutput{
			Role: &iamtypes.Role{
				RoleName: &name,
				Arn:      &arn,
				Path:     &path,
			},
		},
	}
	c := &Clients{}
	c.SetIAMForTest(fake)

	info, err := c.HasIRSARole(context.Background(), name)
	if err != nil {
		t.Fatalf("HasIRSARole: %v", err)
	}
	if info == nil {
		t.Fatal("expected info, got nil")
	}
	if info.RoleName != name || info.ARN != arn || info.Path != path {
		t.Errorf("projection mismatch: %+v", info)
	}
}

func TestHasIRSARole_PropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("AccessDenied: iam:GetRole")
	fake := &fakeIAM{roleErr: sentinel}
	c := &Clients{}
	c.SetIAMForTest(fake)
	if _, err := c.HasIRSARole(context.Background(), "r"); err == nil {
		t.Fatal("expected error")
	} else if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestIsIAMNoSuchEntity(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"typed", &iamtypes.NoSuchEntityException{}, true},
		{"wrapped string", errors.New("operation error IAM: GetRole, https response error: NoSuchEntity"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIAMNoSuchEntity(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIRSARoleNameForCluster(t *testing.T) {
	got := IRSARoleNameForCluster("demo")
	want := "awsbnkctl-demo-flo-supply-chain-reader"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnsureIAM_NilClients(t *testing.T) {
	var c *Clients
	if _, err := c.EnsureIAM(); err == nil {
		t.Fatal("expected error on nil Clients")
	}
}

func TestEnsureIAM_EmptyRegion(t *testing.T) {
	c := &Clients{}
	if _, err := c.EnsureIAM(); err == nil {
		t.Fatal("expected error when region is empty")
	}
}
