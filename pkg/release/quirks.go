package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ProbeEndpointSliceGetRBAC checks via SubjectAccessReview whether
// system:serviceaccount:<ns>:cne-controller-sa already has "get" permission on
// discovery.k8s.io/endpointslices in namespace.
//
// Returns needsPatch=true when the service account lacks "get" on endpointslices.
func ProbeEndpointSliceGetRBAC(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	if client == nil {
		return false, nil
	}
	if namespace == "" {
		namespace = bnkconst.InstanceNamespace
	}
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: fmt.Sprintf("system:serviceaccount:%s:%s", namespace, bnkconst.CNEControllerServiceAccount),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "get",
				Group:     "discovery.k8s.io",
				Resource:  "endpointslices",
			},
		},
	}
	res, err := client.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return !res.Status.Allowed, nil
}

// ProbeCNEControllerNullEnv inspects the f5-cne-controller Deployment in namespace
// and reports whether any container environment variable has a literal "null" or
// unresolved value requiring a recreate after CNEInstance reconciliation.
func ProbeCNEControllerNullEnv(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	if client == nil {
		return false, nil
	}
	if namespace == "" {
		namespace = bnkconst.InstanceNamespace
	}
	dep, err := client.AppsV1().Deployments(namespace).Get(ctx, bnkconst.CNEControllerDeployment, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if strings.EqualFold(strings.TrimSpace(env.Value), "null") {
				return true, nil
			}
		}
	}
	return false, nil
}

// ProbeCPCLKeySecret checks whether cpcl-key-secret is missing or empty in namespace.
func ProbeCPCLKeySecret(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	if client == nil {
		return false, nil
	}
	if namespace == "" {
		namespace = bnkconst.UtilsNamespace
	}
	sec, err := client.CoreV1().Secrets(namespace).Get(ctx, bnkconst.CPCLKeySecretName, metav1.GetOptions{})
	if err != nil {
		return true, nil
	}
	return len(sec.Data) == 0, nil
}

// ProbeLiveQuirks refines profile.Quirks against a live Kubernetes cluster so
// upstream fixes in BNK 2.5+ automatically disable obsolete workarounds.
func ProbeLiveQuirks(ctx context.Context, client kubernetes.Interface, profile *ReleaseProfile) {
	if client == nil || profile == nil {
		return
	}
	if needsPatch, err := ProbeEndpointSliceGetRBAC(ctx, client, bnkconst.InstanceNamespace); err == nil {
		profile.Quirks.EndpointSliceGetRBAC = needsPatch
	}
	if hasNull, err := ProbeCNEControllerNullEnv(ctx, client, bnkconst.InstanceNamespace); err == nil && hasNull {
		profile.Quirks.RecreateCNEControllerOnNull = true
	}
	if needsSync, err := ProbeCPCLKeySecret(ctx, client, bnkconst.UtilsNamespace); err == nil {
		profile.Quirks.CPCLKeySync = needsSync
	}
}
