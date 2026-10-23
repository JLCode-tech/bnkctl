package corefiles

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "core-file-collection"
	scnTitle     = "Core file collection (how-to #4) — CNEInstance.spec.coreCollection.enabled + CoreMond"
	scnNamespace = bnkconst.InstanceNamespace
)

var (
	cneInstanceGVR = schema.GroupVersionResource{
		Group:    "k8s.f5.com",
		Version:  "v1",
		Resource: "cneinstances",
	}
	coreMondGVR = schema.GroupVersionResource{
		Group:    "k8s.f5.com",
		Version:  "v1",
		Resource: "coremonds",
	}
)

func init() { scenarios.Register(&scenario{}) }

type VerifyDeps struct {
	CheckCoreMondFn   func(ctx context.Context, sctx *scenarios.Context) (crOK bool, dsOK bool, condOK bool, details string)
	CheckTMMVolumesFn func(ctx context.Context, sctx *scenarios.Context) (bool, string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		CheckCoreMondFn:   checkCoreMond,
		CheckTMMVolumesFn: checkTMMVolumes,
	}
}

type scenario struct {
	vDeps *VerifyDeps
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's core-dump collection infrastructure: enables spec.coreCollection.enabled
on CNEInstance, reconciling CoreMond DaemonSet and crash host mounts on TMM pods.
`)
}

func (s *scenario) Namespace(ctx *scenarios.Context) string {
	return scnNamespace
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	var paths []string
	err := fs.WalkDir(manifestFS, "manifests", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		body, rerr := manifestFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		base := p[len("manifests/"):]
		outPath, werr := scenarios.WriteManifest(ctx.WorkspaceDir, scnName, base, string(body))
		if werr != nil {
			return werr
		}
		paths = append(paths, outPath)
		return nil
	})
	return paths, err
}

func (s *scenario) Apply(ctx *scenarios.Context) error {
	if ctx.Dynamic == nil {
		return nil
	}
	ns, name := cneInstanceRef(ctx)
	obj, err := ctx.Dynamic.Resource(cneInstanceGVR).Namespace(ns).Get(ctx.Ctx, name, metav1.GetOptions{})
	if err == nil {
		enabled, found, _ := unstructured.NestedBool(obj.Object, "spec", "coreCollection", "enabled")
		if found && enabled {
			// Already enabled at rollout — no patch needed, no DaemonSet restart.
			return nil
		}
	}
	patch := []byte(`{"spec":{"coreCollection":{"enabled":true},"advanced":{"coremon":{"hostPath":true}}}}`)
	_, err = ctx.Dynamic.Resource(cneInstanceGVR).Namespace(ns).Patch(
		ctx.Ctx, name, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		if scenarios.IsNotFound(err) {
			return fmt.Errorf("patch CNEInstance %s/%s for coreCollection: not found — "+
				"awsbnkctl names it <cluster>-bnk in %s; pass --config for the cluster that owns it: %w",
				ns, name, bnkconst.InstanceNamespace, err)
		}
		return fmt.Errorf("patch CNEInstance %s/%s for coreCollection: %w", ns, name, err)
	}
	return nil
}

// cneInstanceRef returns the namespace and name of the CNEInstance Phase 22
// applies: bnkconst.InstanceNamespace / <cluster>-bnk (render.InstanceNameCR).
// Options "cne-namespace" / "cne-instance" override both for clusters that
// were not built by awsbnkctl.
func cneInstanceRef(ctx *scenarios.Context) (ns, name string) {
	ns = bnkconst.InstanceNamespace
	if ctx.Cluster != nil && ctx.Cluster.Metadata.Name != "" {
		name = ctx.Cluster.Metadata.Name + "-bnk"
	}
	if v := ctx.Options["cne-namespace"]; v != "" {
		ns = v
	}
	if v := ctx.Options["cne-instance"]; v != "" {
		name = v
	}
	return ns, name
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	deps := realVerifyDeps()
	if s.vDeps != nil {
		deps = *s.vDeps
	}

	var assertions []scenarios.Assertion

	crOK, dsOK, condOK, details := deps.CheckCoreMondFn(ctx.Ctx, ctx)
	assertions = append(assertions, scenarios.Assertion{
		Description: "CoreMond CR auto-created by FLO",
		OK:          crOK,
		Got:         details,
	})
	assertions = append(assertions, scenarios.Assertion{
		Description: "CoreMond DaemonSet scheduled",
		OK:          dsOK,
		Got:         details,
	})
	assertions = append(assertions, scenarios.Assertion{
		Description: "CNEInstance CoreMonAvailable=True status condition",
		OK:          condOK,
		Got:         details,
	})

	volOK, volDetails := deps.CheckTMMVolumesFn(ctx.Ctx, ctx)
	assertions = append(assertions, scenarios.Assertion{
		Description: "TMM DaemonSet template includes crash / core dump volume mounts",
		OK:          volOK,
		Got:         volDetails,
	})

	res := scenarios.Result{
		Assertions: assertions,
	}
	return scenarios.FinalizeResult(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	// Revert patch is preserved as no-op to protect subsequent test runs from controller churn
	return nil
}

// Reconcile budget after the CNEInstance patch: FLO has to create the CoreMond
// CR, roll its DaemonSet out, and re-render the TMM DaemonSet with the crash
// mounts. Package vars so tests can shrink them.
var (
	reconcileTimeout = 5 * time.Minute
	pollInterval     = 10 * time.Second
)

// coreMondLabel selects the DaemonSet FLO renders for the CoreMond CR.
const coreMondLabel = "app=f5-coremond"

// checkCoreMond is the real CheckCoreMondFn. It polls until all three facts
// hold or the budget runs out, and returns each one honestly — the previous
// implementation gathered the same facts and then returned true regardless.
//
//   - crOK:   a CoreMond CR exists (CNEInstance namespace first, then f5-cne-core).
//   - dsOK:   the app=f5-coremond DaemonSet in that namespace has every desired
//     pod Ready (and desires at least one).
//   - condOK: the CNEInstance Phase 22 applied carries a CoreMon* condition with
//     status True; on builds that expose no such condition, spec.coreCollection
//     .enabled=true on the live object is accepted and the detail says so.
func checkCoreMond(ctx context.Context, sctx *scenarios.Context) (crOK, dsOK, condOK bool, details string) {
	if sctx.Dynamic == nil || sctx.Clientset == nil {
		return false, false, false, "no Kubernetes clients — nothing verified"
	}
	_, details = scenarios.PollMarkers(ctx, reconcileTimeout, pollInterval, func() (bool, string) {
		var crNS, crDetail, dsDetail, condDetail string
		crOK, crNS, crDetail = findCoreMond(ctx, sctx)
		dsOK, dsDetail = coreMondDaemonSetReady(ctx, sctx, crNS)
		condOK, condDetail = cneCoreCollectionCondition(ctx, sctx)
		return crOK && dsOK && condOK, strings.Join([]string{crDetail, dsDetail, condDetail}, "; ")
	})
	return crOK, dsOK, condOK, details
}

// findCoreMond looks for a CoreMond CR where FLO puts it.
func findCoreMond(ctx context.Context, sctx *scenarios.Context) (bool, string, string) {
	for _, ns := range []string{bnkconst.InstanceNamespace, "f5-cne-core"} {
		list, err := sctx.Dynamic.Resource(coreMondGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil || len(list.Items) == 0 {
			continue
		}
		return true, ns, fmt.Sprintf("CoreMond %s/%s present", ns, list.Items[0].GetName())
	}
	return false, "", "no CoreMond CR in " + bnkconst.InstanceNamespace + " or f5-cne-core"
}

// coreMondDaemonSetReady requires the CoreMond DaemonSet to exist and to have
// every desired pod Ready. An empty ns (CR not found yet) is a failure.
func coreMondDaemonSetReady(ctx context.Context, sctx *scenarios.Context, ns string) (bool, string) {
	if ns == "" {
		return false, "CoreMond DaemonSet: namespace unknown (CR not found)"
	}
	dsList, err := sctx.Clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{LabelSelector: coreMondLabel})
	if err != nil {
		return false, "CoreMond DaemonSet list: " + err.Error()
	}
	if len(dsList.Items) == 0 {
		return false, "no DaemonSet with " + coreMondLabel + " in " + ns
	}
	ds := dsList.Items[0]
	desired, ready := ds.Status.DesiredNumberScheduled, ds.Status.NumberReady
	ok := desired > 0 && ready == desired
	return ok, fmt.Sprintf("DaemonSet %s/%s ready %d/%d", ns, ds.Name, ready, desired)
}

// cneCoreCollectionCondition reads the CNEInstance this scenario patched.
func cneCoreCollectionCondition(ctx context.Context, sctx *scenarios.Context) (bool, string) {
	ns, name := cneInstanceRef(sctx)
	cne, err := sctx.Dynamic.Resource(cneInstanceGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Sprintf("CNEInstance %s/%s: %v", ns, name, err)
	}
	conditions, _, _ := scenarios.NestedSlice(cne.Object, "status", "conditions")
	sawCoreMon := false
	for _, raw := range conditions {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := c["type"].(string)
		if !strings.Contains(strings.ToLower(t), "coremon") {
			continue
		}
		sawCoreMon = true
		if st, _ := c["status"].(string); st == "True" {
			return true, fmt.Sprintf("CNEInstance %s/%s condition %s=True", ns, name, t)
		}
	}
	if sawCoreMon {
		return false, fmt.Sprintf("CNEInstance %s/%s has a CoreMon condition but it is not True", ns, name)
	}
	enabled, _, _ := unstructured.NestedBool(cne.Object, "spec", "coreCollection", "enabled")
	if enabled {
		return true, fmt.Sprintf("CNEInstance %s/%s exposes no CoreMon condition on this build; spec.coreCollection.enabled=true", ns, name)
	}
	return false, fmt.Sprintf("CNEInstance %s/%s: no CoreMon condition and spec.coreCollection.enabled is not true", ns, name)
}

// checkTMMVolumes is the real CheckTMMVolumesFn: the f5-tmm DaemonSet in the
// CNEInstance namespace must carry a core/crash volume. Polled, because FLO
// re-renders the DaemonSet after the patch.
func checkTMMVolumes(ctx context.Context, sctx *scenarios.Context) (bool, string) {
	if sctx.Clientset == nil {
		return false, "no Kubernetes client — nothing verified"
	}
	ns := bnkconst.InstanceNamespace
	if v := sctx.Options["cne-namespace"]; v != "" {
		ns = v
	}
	return scenarios.PollMarkers(ctx, reconcileTimeout, pollInterval, func() (bool, string) {
		ds, err := sctx.Clientset.AppsV1().DaemonSets(ns).Get(ctx, "f5-tmm", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("DaemonSet %s/f5-tmm: %v", ns, err)
		}
		for _, v := range ds.Spec.Template.Spec.Volumes {
			name := strings.ToLower(v.Name)
			hostPath := ""
			if v.HostPath != nil {
				hostPath = strings.ToLower(v.HostPath.Path)
			}
			if strings.Contains(name, "core") || strings.Contains(name, "crash") ||
				strings.Contains(hostPath, "core") || strings.Contains(hostPath, "crash") {
				return true, fmt.Sprintf("DaemonSet %s/f5-tmm volume %q (hostPath %q)", ns, v.Name, hostPath)
			}
		}
		return false, fmt.Sprintf("DaemonSet %s/f5-tmm has no core/crash volume (%d volumes)", ns, len(ds.Spec.Template.Spec.Volumes))
	})
}
