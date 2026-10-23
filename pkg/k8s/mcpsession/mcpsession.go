// Package mcpsession renders the BNK 2.4 objects that pin MCP (and A2A)
// sessions to one backend: an F5BigPersistenceProfile with
// persistenceType MODEL_CONTEXT_PROTOCOL, the Secret holding its encryption
// passphrase, and the NetPolicy that attaches the profile to a Gateway or to
// one of its listeners.
//
// The CRD rules this package enforces before the API server does:
//
//   - mcpEncryptionPassphrase.secretRef is required for MODEL_CONTEXT_PROTOCOL,
//     a2aEncryptionPassphrase.secretRef for AGENT2AGENT, and the Secret must
//     live in the profile's namespace;
//   - a NetPolicy may carry at most one F5BigPersistenceProfile extensionRef
//     and exactly one targetRef, so one listener needs one NetPolicy;
//   - the NetPolicy, the profile and the Gateway share a namespace.
package mcpsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/migrate"
)

// Persistence types the profile supports for agent traffic.
const (
	TypeMCP = bnkscan.PersistenceTypeMCP
	TypeA2A = bnkscan.PersistenceTypeA2A

	// DefaultTimeout is the CRD default for persistence entries, in seconds.
	DefaultTimeout int64 = 180
	// DefaultPassphraseField is the Secret key the CRD reads by default.
	DefaultPassphraseField = "passphrase"

	// FieldManager owns the server-side-applied fields.
	FieldManager = "awsbnkctl-mcp-session"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Spec is the operator's declaration.
type Spec struct {
	// Name of the F5BigPersistenceProfile; also the base name of the NetPolicy
	// when NetPolicyName is empty.
	Name      string
	Namespace string
	// Type is TypeMCP (default) or TypeA2A.
	Type string
	// Timeout in seconds for persistence entries (DEFAULT hash only).
	Timeout int64

	// Gateway is the Gateway to attach to, in Namespace.
	Gateway string
	// Listeners are the Gateway listener names (sectionName) to attach to.
	// Empty attaches one NetPolicy to the whole Gateway.
	Listeners []string
	// IRules are F5BigCneIrule names to keep on the same NetPolicy, so a
	// listener that already carries a governance iRule is not attached twice.
	IRules []string
	// NetPolicyName overrides the NetPolicy base name.
	NetPolicyName string

	// SecretName is the Secret holding the passphrase; PassphraseField is the
	// key inside it. Passphrase, when set, renders the Secret too.
	SecretName      string
	PassphraseField string
	Passphrase      string
}

func (s Spec) withDefaults() Spec {
	if s.Type == "" {
		s.Type = TypeMCP
	}
	if s.Timeout == 0 {
		s.Timeout = DefaultTimeout
	}
	if s.SecretName == "" && s.Name != "" {
		s.SecretName = s.Name + "-passphrase"
	}
	if s.PassphraseField == "" {
		s.PassphraseField = DefaultPassphraseField
	}
	if s.NetPolicyName == "" {
		s.NetPolicyName = s.Name
	}
	return s
}

// Validate checks the spec against the CRD rules.
func (s Spec) Validate() error {
	s = s.withDefaults()
	var errs []string
	for _, f := range []struct{ field, value string }{
		{"name", s.Name}, {"gateway", s.Gateway}, {"secret name", s.SecretName}, {"net-policy name", s.NetPolicyName},
	} {
		switch {
		case f.value == "":
			errs = append(errs, f.field+" is required")
		case !dnsLabel.MatchString(f.value):
			errs = append(errs, fmt.Sprintf("%s %q must be a lowercase DNS label", f.field, f.value))
		}
	}
	if s.Namespace == "" {
		errs = append(errs, "namespace is required")
	}
	if s.Type != TypeMCP && s.Type != TypeA2A {
		errs = append(errs, fmt.Sprintf("type %q must be %s or %s", s.Type, TypeMCP, TypeA2A))
	}
	if s.Timeout < 1 {
		errs = append(errs, "timeout must be at least 1 second")
	}
	for _, l := range s.Listeners {
		if !dnsLabel.MatchString(l) {
			errs = append(errs, fmt.Sprintf("listener %q must be a lowercase DNS label", l))
		}
	}
	for _, r := range s.IRules {
		if !dnsLabel.MatchString(r) {
			errs = append(errs, fmt.Sprintf("irule %q must be a lowercase DNS label", r))
		}
	}
	if len(s.IRules) > 7 {
		errs = append(errs, "a NetPolicy holds at most 8 extensionRefs: 7 iRules plus the profile")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Objects renders the Secret (when Passphrase is set), the profile and the
// NetPolicies, in apply order. Call Validate first.
func (s Spec) Objects() []*unstructured.Unstructured {
	s = s.withDefaults()
	var out []*unstructured.Unstructured

	if s.Passphrase != "" {
		out = append(out, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": s.SecretName, "namespace": s.Namespace},
			"type":       "Opaque",
			"stringData": map[string]any{s.PassphraseField: s.Passphrase},
		}})
	}

	// CRD spec field holding the secretRef; the value is a field name, not a credential.
	refField := "mcpEncryptionPassphrase"
	if s.Type == TypeA2A {
		refField = "a2aEncryptionPassphrase"
	}
	secretRef := map[string]any{"name": s.SecretName}
	if s.PassphraseField != DefaultPassphraseField {
		secretRef["passphraseField"] = s.PassphraseField
	}
	out = append(out, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": bnkscan.PersistenceProfileGVR.Group + "/" + bnkscan.PersistenceProfileGVR.Version,
		"kind":       "F5BigPersistenceProfile",
		"metadata":   map[string]any{"name": s.Name, "namespace": s.Namespace},
		"spec": map[string]any{
			"persistenceType": s.Type,
			"timeout":         s.Timeout,
			refField:          map[string]any{"secretRef": secretRef},
		},
	}})

	listeners := s.Listeners
	if len(listeners) == 0 {
		listeners = []string{""}
	}
	for _, l := range listeners {
		name := s.NetPolicyName
		target := map[string]any{
			"group": bnkscan.GatewayAPIGroup,
			"kind":  "Gateway",
			"name":  s.Gateway,
		}
		if l != "" {
			name = s.NetPolicyName + "-" + l
			target["sectionName"] = l
		}
		var refs []any
		for _, r := range s.IRules {
			refs = append(refs, map[string]any{"group": bnkscan.DataPlaneGroup, "kind": "F5BigCneIrule", "name": r})
		}
		refs = append(refs, map[string]any{"group": bnkscan.DataPlaneGroup, "kind": "F5BigPersistenceProfile", "name": s.Name})
		out = append(out, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": bnkscan.GatewayF5Group + "/" + bnkscan.GatewayF5Version,
			"kind":       "NetPolicy",
			"metadata":   map[string]any{"name": name, "namespace": s.Namespace},
			"spec": map[string]any{
				"targetRefs":    []any{target},
				"extensionRefs": refs,
			},
		}})
	}
	return out
}

// WriteYAML prints the objects as a multi-document stream.
func WriteYAML(w io.Writer, objs []*unstructured.Unstructured) error {
	for i, o := range objs {
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		b, err := yaml.Marshal(o.Object)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Apply server-side-applies the objects in order and, when wait is positive,
// waits for the profile to report Programmed=True.
func Apply(ctx context.Context, applier migrate.Applier, dyn dynamic.Interface, spec Spec, objs []*unstructured.Unstructured, wait time.Duration, log io.Writer) error {
	spec = spec.withDefaults()
	for _, o := range objs {
		if err := applier.Apply(ctx, o); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", o.GetKind(), o.GetNamespace(), o.GetName(), err)
		}
		fmt.Fprintf(log, "[mcp-session] applied %s %s/%s\n", o.GetKind(), o.GetNamespace(), o.GetName())
	}
	if wait <= 0 || dyn == nil {
		return nil
	}
	fmt.Fprintf(log, "[mcp-session] waiting for F5BigPersistenceProfile %s/%s Programmed=True (up to %s)\n", spec.Namespace, spec.Name, wait)
	if err := migrate.WaitConditionTrue(ctx, dyn, bnkscan.PersistenceProfileGVR, spec.Namespace, spec.Name, "Programmed", wait, 3*time.Second); err != nil {
		return err
	}
	fmt.Fprintf(log, "[mcp-session] F5BigPersistenceProfile %s/%s Programmed=True\n", spec.Namespace, spec.Name)
	return nil
}

// Summary returns one line per rendered object for the report.
func Summary(objs []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, fmt.Sprintf("%s %s/%s", o.GetKind(), o.GetNamespace(), o.GetName()))
	}
	sort.Strings(out)
	return out
}
