package k8s

// multusheal.go — the Multus thin plugin writes its kubeconfig from the
// service-account token it holds at pod start. Its entrypoint re-reads the
// token and rewrites the kubeconfig only inside the watch loop that upstream
// enables with --cleanup-config-on-exit (docs/how-to-use.md, "watch for
// changes of the master CNI configuration and kubeconfig"). Without the flag
// the token expires and every new pod on the node fails with
// "Multus: ... error waiting for pod: Unauthorized" until the Multus pod is
// recreated. Seen live 2026-09-14 (13-day-old cluster during bnk upgrade) and
// again on bnk-singapore-pe (TMM Pending for four days on a 2.3 cluster).
// EnsureMultusTokenWatch makes the DaemonSet carry the flag, which also rolls
// the pods and so refreshes the token; it runs from awsbnkctl up (phase 12),
// bnk upgrade (step 0) and bnk heal.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// MultusNamespace and MultusDaemonSet locate the Multus install.
	MultusNamespace = "kube-system"
	MultusDaemonSet = "kube-multus-ds"
	// MultusContainer is the thin-plugin entrypoint container.
	MultusContainer = "kube-multus"
	// MultusPodSelector selects the Multus pods.
	MultusPodSelector = "app=multus"
	// MultusWatchFlag turns on the entrypoint loop that rewrites the
	// kubeconfig whenever the projected service-account token rotates.
	MultusWatchFlag = "--cleanup-config-on-exit=true" // #nosec G101 -- CLI flag, not a credential
	// multusSkipWatchFlag disables that loop again.
	multusSkipWatchFlag = "--skip-config-watch" // #nosec G101 -- CLI flag, not a credential
	// multusRestartAnnotation marks the DaemonSet template to roll its pods.
	multusRestartAnnotation = "awsbnkctl.f5.com/restartedAt"
	multusRolloutWait       = 3 * time.Minute
)

// MultusHealResult reports what EnsureMultusTokenWatch found and did.
type MultusHealResult struct {
	// Installed is false when the Multus DaemonSet does not exist.
	Installed bool `json:"installed"`
	// WatchEnabled is true when the DaemonSet already carried the token watch.
	WatchEnabled bool `json:"watchEnabled"`
	// Unauthorized is true when a pod failed its sandbox with Multus
	// Unauthorized since the newest Multus pod started.
	Unauthorized bool `json:"unauthorized"`
	// Reason explains the action taken.
	Reason string `json:"reason,omitempty"`
	// Patched is true when the DaemonSet args were changed (which rolls it).
	Patched bool `json:"patched"`
	// Restarted is true when the DaemonSet was rolled without an args change.
	Restarted bool `json:"restarted"`
}

// MultusState is the read-only view doctor reports.
type MultusState struct {
	Installed    bool
	WatchEnabled bool
	// Unauthorized names the pod whose sandbox Multus refused since the
	// newest Multus pod started; empty when none.
	Unauthorized string
}

// MultusWatchEnabled reports whether the DaemonSet's entrypoint watches the
// service-account token: --cleanup-config-on-exit(=true) present and
// --skip-config-watch absent, with --multus-conf-file=auto (the default).
func MultusWatchEnabled(ds *appsv1.DaemonSet) bool {
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != MultusContainer {
			continue
		}
		if strings.Contains(c.Image, "thick") {
			return true
		}
		for _, cmd := range c.Command {
			if strings.Contains(cmd, "multus-daemon") {
				return true
			}
		}
		cleanup, skip, auto := false, false, true
		for _, a := range c.Args {
			switch {
			case a == "--cleanup-config-on-exit" || a == MultusWatchFlag:
				cleanup = true
			case a == "--cleanup-config-on-exit=false":
				cleanup = false
			case a == multusSkipWatchFlag || a == multusSkipWatchFlag+"=true":
				skip = true
			case strings.HasPrefix(a, "--multus-conf-file=") && a != "--multus-conf-file=auto":
				auto = false
			}
		}
		return cleanup && !skip && auto
	}
	return false
}

// InspectMultus reads the DaemonSet, its pods and the FailedCreatePodSandBox
// events. Installed is false when the DaemonSet is absent.
func InspectMultus(ctx context.Context, cs kubernetes.Interface) (MultusState, *appsv1.DaemonSet, error) {
	ds, err := cs.AppsV1().DaemonSets(MultusNamespace).Get(ctx, MultusDaemonSet, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return MultusState{}, nil, nil
	}
	if err != nil {
		return MultusState{}, nil, fmt.Errorf("get daemonset %s/%s: %w", MultusNamespace, MultusDaemonSet, err)
	}
	st := MultusState{Installed: true, WatchEnabled: MultusWatchEnabled(ds)}
	pods, err := cs.CoreV1().Pods(MultusNamespace).List(ctx, metav1.ListOptions{LabelSelector: MultusPodSelector})
	if err != nil {
		return st, ds, fmt.Errorf("list multus pods: %w", err)
	}
	// Events older than the newest Multus pod predate a restart that already
	// refreshed the token.
	var newest time.Time
	for i := range pods.Items {
		if s := pods.Items[i].Status.StartTime; s != nil && s.Time.After(newest) {
			newest = s.Time
		}
	}
	events, err := cs.CoreV1().Events("").List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedCreatePodSandBox"})
	if err != nil {
		return st, ds, nil //nolint:nilerr — events are advisory
	}
	for i := range events.Items {
		ev := &events.Items[i]
		if !MultusUnauthorizedEvent(ev) {
			continue
		}
		ts := ev.LastTimestamp.Time
		if ts.IsZero() {
			ts = ev.EventTime.Time
		}
		if !ts.IsZero() && ts.Before(newest) {
			continue
		}
		st.Unauthorized = ev.InvolvedObject.Namespace + "/" + ev.InvolvedObject.Name
		break
	}
	return st, ds, nil
}

// MultusUnauthorizedEvent reports whether a FailedCreatePodSandBox event
// blames Multus for an expired kubeconfig token.
func MultusUnauthorizedEvent(ev *corev1.Event) bool {
	return ev.Reason == "FailedCreatePodSandBox" && strings.Contains(ev.Message, "multus") && strings.Contains(ev.Message, "Unauthorized")
}

// EnableMultusTokenWatch appends MultusWatchFlag to the entrypoint
// container and waits for the rollout, which also gives every pod a fresh
// token. No-op when the flag is already present.
func EnableMultusTokenWatch(ctx context.Context, cs kubernetes.Interface, ds *appsv1.DaemonSet) (bool, error) {
	if MultusWatchEnabled(ds) {
		return false, nil
	}
	var args []string
	found := false
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == MultusContainer {
			found = true
			for _, a := range c.Args {
				if a == "--cleanup-config-on-exit" || strings.HasPrefix(a, "--cleanup-config-on-exit=") || a == multusSkipWatchFlag || a == multusSkipWatchFlag+"=true" {
					continue
				}
				args = append(args, a)
			}
		}
	}
	if !found {
		return false, fmt.Errorf("daemonset %s/%s has no container %q", MultusNamespace, MultusDaemonSet, MultusContainer)
	}
	args = append(args, MultusWatchFlag)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"args":[%s]}]}}}}`, MultusContainer, strings.Join(quoted, ","))
	if _, err := cs.AppsV1().DaemonSets(MultusNamespace).Patch(ctx, MultusDaemonSet, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: "awsbnkctl-multus"}); err != nil {
		return false, fmt.Errorf("patch multus DaemonSet args: %w", err)
	}
	return true, WaitForDaemonSetRollout(ctx, cs, MultusNamespace, MultusDaemonSet, multusRolloutWait)
}

// RestartMultus rolls the Multus DaemonSet and waits for it to be ready.
// Running pods keep their networking; only new pod creation needs Multus.
func RestartMultus(ctx context.Context, cs kubernetes.Interface, now time.Time) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, multusRestartAnnotation, now.UTC().Format(time.RFC3339))
	if _, err := cs.AppsV1().DaemonSets(MultusNamespace).Patch(ctx, MultusDaemonSet, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restart multus DaemonSet: %w", err)
	}
	return WaitForDaemonSetRollout(ctx, cs, MultusNamespace, MultusDaemonSet, multusRolloutWait)
}

// EnsureMultusTokenWatch is the reusable heal: it turns on the token watch
// when the DaemonSet lacks it (the rollout refreshes the token as a side
// effect) and, when the watch is already on but a pod still hit Multus
// Unauthorized since the newest Multus pod started, rolls the DaemonSet.
// dryRun reports without writing. log may be nil.
func EnsureMultusTokenWatch(ctx context.Context, cs kubernetes.Interface, dryRun bool, log io.Writer) (MultusHealResult, error) {
	if log == nil {
		log = io.Discard
	}
	st, ds, err := InspectMultus(ctx, cs)
	res := MultusHealResult{Installed: st.Installed, WatchEnabled: st.WatchEnabled, Unauthorized: st.Unauthorized != ""}
	if err != nil || !st.Installed {
		return res, err
	}
	switch {
	case !st.WatchEnabled:
		res.Reason = fmt.Sprintf("DaemonSet %s/%s writes its kubeconfig once at pod start (no %s); adding the flag so the entrypoint rewrites it on every token rotation", MultusNamespace, MultusDaemonSet, MultusWatchFlag)
		if st.Unauthorized != "" {
			res.Reason = fmt.Sprintf("pod %s failed its network sandbox with Multus Unauthorized (expired kubeconfig token); %s", st.Unauthorized, res.Reason)
		}
		fmt.Fprintf(log, "[multus] %s\n", res.Reason)
		if dryRun {
			return res, nil
		}
		patched, err := EnableMultusTokenWatch(ctx, cs, ds)
		if err != nil {
			return res, err
		}
		res.Patched = patched
		fmt.Fprintln(log, "[multus] DaemonSet rolled with the token watch and ready")
	case st.Unauthorized != "":
		res.Reason = fmt.Sprintf("pod %s failed its network sandbox with Multus Unauthorized although the token watch is on; rolling DaemonSet %s/%s", st.Unauthorized, MultusNamespace, MultusDaemonSet)
		fmt.Fprintf(log, "[multus] %s\n", res.Reason)
		if dryRun {
			return res, nil
		}
		if err := RestartMultus(ctx, cs, time.Now()); err != nil {
			return res, err
		}
		res.Restarted = true
		fmt.Fprintln(log, "[multus] DaemonSet rolled and ready")
	default:
		res.Reason = "token watch on, no Unauthorized sandbox events"
	}
	return res, nil
}
