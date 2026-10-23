package mcpsession

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func baseSpec() Spec {
	return Spec{
		Name:      "mcp-session",
		Namespace: "default",
		Gateway:   "bnk-agentcore-demo-gateway",
		Listeners: []string{"http", "https"},
		IRules:    []string{"mcp-rate-limit-irule"},
	}
}

func TestValidate(t *testing.T) {
	if err := baseSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]func(*Spec){
		"name is required":            func(s *Spec) { s.Name = "" },
		"gateway is required":         func(s *Spec) { s.Gateway = "" },
		"namespace is required":       func(s *Spec) { s.Namespace = "" },
		"must be a lowercase DNS":     func(s *Spec) { s.Name = "Mcp_Session" },
		"type \"SRC_ADDR\" must be":   func(s *Spec) { s.Type = "SRC_ADDR" },
		"timeout must be at least":    func(s *Spec) { s.Timeout = -5 },
		"listener \"Bad Name\" must":  func(s *Spec) { s.Listeners = []string{"Bad Name"} },
		"at most 8 extensionRefs":     func(s *Spec) { s.IRules = []string{"a", "b", "c", "d", "e", "f", "g", "h"} },
		"secret name \"Bad\" must be": func(s *Spec) { s.SecretName = "Bad" },
	}
	for want, mutate := range cases {
		s := baseSpec()
		mutate(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

func TestObjects_MCPPerListener(t *testing.T) {
	s := baseSpec()
	s.Passphrase = "demo-passphrase"
	objs := s.Objects()
	kinds := kindsOf(objs)
	if strings.Join(kinds, ",") != "Secret,F5BigPersistenceProfile,NetPolicy,NetPolicy" {
		t.Fatalf("kinds = %v", kinds)
	}

	secret := objs[0]
	if secret.GetName() != "mcp-session-passphrase" || secret.GetNamespace() != "default" {
		t.Errorf("secret = %s/%s", secret.GetNamespace(), secret.GetName())
	}
	if v, _, _ := unstructured.NestedString(secret.Object, "stringData", "passphrase"); v != "demo-passphrase" {
		t.Errorf("stringData.passphrase = %q", v)
	}

	profile := objs[1]
	if profile.GetAPIVersion() != "k8s.f5net.com/v1" {
		t.Errorf("profile apiVersion = %s", profile.GetAPIVersion())
	}
	if v, _, _ := unstructured.NestedString(profile.Object, "spec", "persistenceType"); v != TypeMCP {
		t.Errorf("persistenceType = %s", v)
	}
	if v, _, _ := unstructured.NestedInt64(profile.Object, "spec", "timeout"); v != DefaultTimeout {
		t.Errorf("timeout = %d", v)
	}
	if v, _, _ := unstructured.NestedString(profile.Object, "spec", "mcpEncryptionPassphrase", "secretRef", "name"); v != "mcp-session-passphrase" {
		t.Errorf("secretRef.name = %q", v)
	}
	if _, found, _ := unstructured.NestedString(profile.Object, "spec", "mcpEncryptionPassphrase", "secretRef", "passphraseField"); found {
		t.Error("default passphraseField should be omitted")
	}
	if _, found, _ := unstructured.NestedMap(profile.Object, "spec", "a2aEncryptionPassphrase"); found {
		t.Error("MCP profile must not carry a2aEncryptionPassphrase")
	}

	for i, want := range []struct{ name, section string }{{"mcp-session-http", "http"}, {"mcp-session-https", "https"}} {
		np := objs[2+i]
		if np.GetAPIVersion() != "gateway.k8s.f5.com/v1alpha1" || np.GetName() != want.name {
			t.Errorf("netpolicy %d = %s %s", i, np.GetAPIVersion(), np.GetName())
		}
		targets, _, _ := unstructured.NestedSlice(np.Object, "spec", "targetRefs")
		if len(targets) != 1 {
			t.Fatalf("targetRefs = %v", targets)
		}
		tr := targets[0].(map[string]any)
		if tr["kind"] != "Gateway" || tr["name"] != "bnk-agentcore-demo-gateway" || tr["sectionName"] != want.section {
			t.Errorf("targetRef = %v", tr)
		}
		refs, _, _ := unstructured.NestedSlice(np.Object, "spec", "extensionRefs")
		if len(refs) != 2 {
			t.Fatalf("extensionRefs = %v", refs)
		}
		if refs[0].(map[string]any)["kind"] != "F5BigCneIrule" || refs[1].(map[string]any)["kind"] != "F5BigPersistenceProfile" || refs[1].(map[string]any)["name"] != "mcp-session" {
			t.Errorf("extensionRefs = %v", refs)
		}
	}
}

func TestObjects_A2AWholeGatewayCustomField(t *testing.T) {
	s := Spec{Name: "a2a-session", Namespace: "agents", Gateway: "gw", Type: TypeA2A, Timeout: 900,
		SecretName: "shared-secret", PassphraseField: "key", NetPolicyName: "a2a-pin"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	objs := s.Objects()
	if kinds := kindsOf(objs); strings.Join(kinds, ",") != "F5BigPersistenceProfile,NetPolicy" {
		t.Fatalf("kinds = %v (no Secret without a passphrase)", kinds)
	}
	profile := objs[0]
	if v, _, _ := unstructured.NestedString(profile.Object, "spec", "a2aEncryptionPassphrase", "secretRef", "name"); v != "shared-secret" {
		t.Errorf("a2a secretRef.name = %q", v)
	}
	if v, _, _ := unstructured.NestedString(profile.Object, "spec", "a2aEncryptionPassphrase", "secretRef", "passphraseField"); v != "key" {
		t.Errorf("passphraseField = %q", v)
	}
	if v, _, _ := unstructured.NestedInt64(profile.Object, "spec", "timeout"); v != 900 {
		t.Errorf("timeout = %d", v)
	}
	np := objs[1]
	if np.GetName() != "a2a-pin" {
		t.Errorf("netpolicy name = %s", np.GetName())
	}
	targets, _, _ := unstructured.NestedSlice(np.Object, "spec", "targetRefs")
	if _, has := targets[0].(map[string]any)["sectionName"]; has {
		t.Error("whole-Gateway attachment must not set sectionName")
	}
}

func TestWriteYAMLAndApply(t *testing.T) {
	s := baseSpec()
	objs := s.Objects()
	var sb strings.Builder
	if err := WriteYAML(&sb, objs); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Count(out, "\n---\n") != 2 {
		t.Errorf("want 2 separators for 3 documents, got:\n%s", out)
	}
	if !strings.Contains(out, "persistenceType: MODEL_CONTEXT_PROTOCOL") || !strings.Contains(out, "kind: NetPolicy") {
		t.Errorf("yaml:\n%s", out)
	}

	applied := &recordingApplier{}
	var log strings.Builder
	if err := Apply(context.Background(), applied, nil, s, objs, 0, &log); err != nil {
		t.Fatal(err)
	}
	if len(applied.kinds) != 3 || !strings.Contains(log.String(), "applied NetPolicy default/mcp-session-https") {
		t.Errorf("applied %v, log:\n%s", applied.kinds, log.String())
	}
	if got := Summary(objs); len(got) != 3 || got[0] != "F5BigPersistenceProfile default/mcp-session" {
		t.Errorf("Summary = %v", got)
	}
}

type recordingApplier struct{ kinds []string }

func (r *recordingApplier) Apply(_ context.Context, obj *unstructured.Unstructured) error {
	r.kinds = append(r.kinds, obj.GetKind())
	return nil
}

func kindsOf(objs []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetKind())
	}
	return out
}
