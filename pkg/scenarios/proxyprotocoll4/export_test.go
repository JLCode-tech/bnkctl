package proxyprotocoll4

// This file exports internal constructors for use by the external _test package.
// It is compiled only during `go test`.

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

// NewScenarioForTest returns a scenario with the supplied deps injected.
// Only for use in tests.
func NewScenarioForTest(d VerifyDeps) scenarios.Scenario {
	return &scenario{vDeps: &d}
}

// WaitL4RouteConditionForTest exposes the unexported waitL4RouteCondition for
// unit tests in the external _test package.
func WaitL4RouteConditionForTest(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error {
	return waitL4RouteCondition(ctx, sctx, ns, name, condType, timeout)
}

// L4RouteGVRForTest exposes the unexported l4RouteGVR for unit tests.
var L4RouteGVRForTest = l4RouteGVR

// L4RouteGVKForTest returns the GroupVersionKind for l4RouteGVR (used to build
// fake dynamic client scheme registrations).
func L4RouteGVKForTest() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   l4RouteGVR.Group,
		Version: l4RouteGVR.Version,
		Kind:    "L4Route",
	}
}
