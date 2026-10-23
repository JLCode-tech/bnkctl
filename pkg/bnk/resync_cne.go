package bnk

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var cneInstanceGVR = schema.GroupVersionResource{
	Group:    "k8s.f5.com",
	Version:  "v1",
	Resource: "cneinstances",
}

// ResyncCNEInstance nudges the F5 cne-controller into re-evaluating the
// CNEInstance's Available condition by patching a harmless annotation.
//
// The cne-controller watch triggers on any metadata change; the RFC3339Nano
// timestamp guarantees each call is a real diff, not a no-op. Annotation edits
// carry no semantic risk (unlike spec changes).
//
// Live-validated pattern: same root cause as project_pool_member_sync_root_cause —
// controller only reconciles on resource change events, not on external state
// transitions (pods becoming Ready).
func ResyncCNEInstance(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{"awsbnkctl.io/resync-trigger":%q}}}`,
		time.Now().UTC().Format(time.RFC3339Nano),
	))
	_, err := dyn.Resource(cneInstanceGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "awsbnkctl"},
	)
	if err != nil {
		return fmt.Errorf("ResyncCNEInstance %s/%s: %w", namespace, name, err)
	}
	fmt.Fprintf(Stderr, "[resync-cne] annotation patch applied to CNEInstance %s/%s\n", namespace, name)
	return nil
}
