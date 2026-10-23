package gcp

import (
	"context"
	"testing"

	"k8s.io/client-go/rest"
)

// TestAttachK8sFromConfig proves the typed/dynamic/RESTMapper trio is built
// from a rest.Config without requiring a live API server — client-go client
// construction only validates the Host string, it doesn't dial.
func TestAttachK8sFromConfig(t *testing.T) {
	c := &Clients{}
	cfg := &rest.Config{Host: "https://127.0.0.1:1"}
	if err := attachK8sFromConfig(c, cfg); err != nil {
		t.Fatalf("attachK8sFromConfig: unexpected error: %v", err)
	}
	if c.K8s == nil {
		t.Error("K8s client is nil after attachK8sFromConfig")
	}
	if c.Dynamic == nil {
		t.Error("Dynamic client is nil after attachK8sFromConfig")
	}
	if c.RESTMapper == nil {
		t.Error("RESTMapper is nil after attachK8sFromConfig")
	}
	if c.RestConfig != cfg {
		t.Error("RestConfig is not the cfg passed to attachK8sFromConfig")
	}
}

func TestNewClients_EmptyProject(t *testing.T) {
	//lint:ignore SA1012 project=="" is checked before ctx is ever touched
	if _, err := NewClients(nil, ""); err == nil {
		t.Fatal("expected error for empty project, got nil")
	}
}

func TestClients_AttachK8s_NilGKEClientErrors(t *testing.T) {
	c := &Clients{}
	err := c.AttachK8s(context.Background(), "bnk-hostdev", "us-central1-a")
	if err == nil {
		t.Fatal("expected AttachK8s to error when Clients.GKE is nil, got nil")
	}
}

// Note: AttachK8s's success path additionally calls BuildRESTConfig, which
// requires Application Default Credentials (google.DefaultTokenSource) —
// that path is exercised live, not unit-tested here, matching the ADC
// boundary documented in docs/SPEC-go-tracer.md §5/§9.
// attachK8sFromConfig (the ADC-free half) is covered by
// TestAttachK8sFromConfig above; the nil-GKE guard is covered by
// TestClients_AttachK8s_NilGKEClientErrors.
