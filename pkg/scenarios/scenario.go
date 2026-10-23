// Package scenarios is the test-case framework for the awsbnkctl
// scenarios subcommand. Each scenario maps to one F5 how-to from
// clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ and
// exercises a slice of BNK functionality end-to-end against the
// running cluster — applying manifests, asserting reconciled state,
// and cleaning up.
//
// Rating is a stable hint to operators (and `scenarios list`) about
// whether the scenario can actually run in the awsbnkctl EKS / SR-IOV
// host-device shape:
//
//	Green  — fully testable here
//	Amber  — partially testable; some assertions are skipped or
//	         relaxed because the cluster shape can't reproduce them
//	Red    — not testable; requires BGP peers, DPUs, etc.
//	         Listed for discoverability, never executed.
package scenarios

import (
	"context"
	"io"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Rating qualifies how much of the underlying F5 how-to we can
// actually exercise in the awsbnkctl EKS + host-device cluster.
type Rating string

const (
	Green Rating = "green"
	Amber Rating = "amber"
	Red   Rating = "red"
)

// Assertion is one check inside Verify. Rich enough that the report
// reader can see exactly what passed / failed without running the
// scenario again. Failed assertions don't short-circuit Verify by
// themselves — the scenario decides when to bail.
type Assertion struct {
	Description string `json:"description"`
	OK          bool   `json:"ok"`
	Got         string `json:"got,omitempty"`
}

// Result is what each scenario returns from Apply+Verify. Status mirrors
// the e2e phase report vocabulary so the rollup CLI can use one renderer.
type Result struct {
	Status     string      `json:"status"` // ok | failed | skipped | dry-run
	Summary    string      `json:"summary"`
	Details    string      `json:"details,omitempty"`
	DataPath   bool        `json:"data_path,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Manifest   string      `json:"manifest_path,omitempty"`
	EnvDiagram string      `json:"env_diagram,omitempty"`
}

// AllPassed returns true when every assertion in r is OK. Empty
// assertion list returns true — Summary alone suffices for trivial
// scenarios.
func (r Result) AllPassed() bool {
	for _, a := range r.Assertions {
		if !a.OK {
			return false
		}
	}
	return true
}

// Context is the small bundle every scenario needs at runtime.
// Uses typed clients; no kubectl shell-out wrapper.
type Context struct {
	Ctx context.Context
	// Cluster is the parsed cluster.yaml intent.
	Cluster *intent.Cluster
	// State is the loaded state.env key/value store.
	State *state.State
	// Clientset provides typed access to core Kubernetes resources.
	Clientset kubernetes.Interface
	// Dynamic provides unstructured access to CRDs and Gateway API resources.
	Dynamic dynamic.Interface
	// RESTConfig is the raw REST config. Scenarios needing exec/port-forward
	// can derive SPDY clients from this.
	RESTConfig *rest.Config
	// KubeconfigPath is passed to internal/k8s.ApplyOptions so SSA uses
	// the right kubeconfig without re-parsing RESTConfig.
	KubeconfigPath string
	// WorkspaceDir is the absolute path of the cluster workspace directory
	// (e.g. .awsbnkctl/<cluster>/). Parent of state/, artifacts/, reports/.
	WorkspaceDir string
	// Out is where progress lines are streamed.
	Out io.Writer
	// DryRun: render manifests but apply nothing.
	DryRun bool
	// Verbose: surface per-assertion lines + Result.Details to Out.
	// JSON report always carries them regardless.
	Verbose bool
	// ReportStamp, if non-empty, forces every scenario in this run
	// to share the same reports/<stamp>/ directory — used by `--all`
	// so the aggregate summary lives next to all per-scenario JSONs.
	// Empty means each scenario picks its own timestamp.
	ReportStamp string
	// Options carries scenario-specific overrides passed from the CLI
	// (e.g. "vip", "iterations", "timeout").
	Options map[string]string
}

// Scenario is the interface every test case implements. Methods are
// invoked in this order: Manifests() once for artifact persistence,
// then Apply, then Verify; Cleanup is invoked by `scenarios clean`.
type Scenario interface {
	// Name is the kebab-case identifier used on the CLI.
	Name() string
	// Title is the human-readable F5 how-to title this maps to.
	Title() string
	// Rating tells `scenarios list` whether to surface it as runnable.
	Rating() Rating
	// Description is one paragraph explaining what's tested + what isn't.
	Description() string
	// Dependencies lists other scenario names this one logically
	// relies on. `scenarios run --all` topo-sorts by these so deps run
	// before their dependents. A single-name `scenarios run` does NOT
	// auto-chain — Verify surfaces "dep not running" as an assertion so
	// the operator decides whether to start it.
	Dependencies() []string

	// Manifests renders all manifest files into <WorkspaceDir>/artifacts/
	// scenarios/<Name>/ and returns the on-disk paths. Pure render —
	// no kube I/O. Always safe to call (dry-run or not).
	Manifests(*Context) ([]string, error)

	// Apply pushes the rendered manifests into the cluster. Called
	// AFTER Manifests; Apply must use internal/k8s.ApplyOptions so that
	// Gateway / HTTPRoute / GatewaySettings CRDs are resolved via live
	// RESTMapper and not a static GVR map.
	Apply(*Context) error

	// Verify asserts the expected post-Apply state. Idempotent and
	// repeatable: callers may invoke it again after a wait.
	Verify(*Context) Result

	// Cleanup undoes Apply. Idempotent — a missing namespace / object
	// is not an error.
	Cleanup(*Context) error

	// Namespace returns the namespace the scenario's resources live in
	// (honoring any ctx.Options["namespace"] override). Used by the env diagram.
	Namespace(ctx *Context) string
}

// Registry holds every scenario this binary knows about. Scenarios
// register themselves at init() time via Register(s).
var registry []Scenario

// Register adds s to the global scenario list. Safe to call from
// package init functions. Duplicate Name() panics — that's a build-
// time programmer bug, not something an operator can trigger.
func Register(s Scenario) {
	for _, existing := range registry {
		if existing.Name() == s.Name() {
			panic("scenarios: duplicate registration: " + s.Name())
		}
	}
	registry = append(registry, s)
}

// All returns the registered scenarios in registration order.
func All() []Scenario { return append([]Scenario(nil), registry...) }

// Find returns the scenario with the given name, or nil.
func Find(name string) Scenario {
	for _, s := range registry {
		if s.Name() == name {
			return s
		}
	}
	return nil
}
