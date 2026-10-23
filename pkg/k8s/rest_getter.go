package k8s

import (
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// newRESTClientGetter builds a cli-runtime RESTClientGetter wired to the
// awsbnkctl kubeconfig discovery rules. We delegate to cli-runtime's
// own ConfigFlags and just plumb our resolved path + namespace through.
//
// kubeconfigPath: empty → workspace default via DefaultKubeconfigPath().
// "in-cluster" sentinel is *not* supported by ConfigFlags directly —
// callers needing in-cluster mode use BuildClientset/BuildDynamicClient
// and bypass cli-runtime. Get/Apply/Describe always run against an
// explicit kubeconfig in v1.
//
// namespace is the value of -n; "" means "fall back to whatever the
// current-context says (or 'default')".
func newRESTClientGetter(kubeconfigPath, namespace string) genericclioptions.RESTClientGetter {
	cf := genericclioptions.NewConfigFlags(true)
	if kubeconfigPath == "" {
		kubeconfigPath = DefaultKubeconfigPath()
	}
	if kubeconfigPath != "" && kubeconfigPath != InClusterKubeconfigSentinel {
		cf.KubeConfig = &kubeconfigPath
	}
	if namespace != "" {
		cf.Namespace = &namespace
	}
	return cf
}
