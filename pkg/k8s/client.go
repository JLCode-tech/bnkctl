// Package k8s provides Kubernetes client construction, dynamic Server-Side Apply,
// in-process kubectl replacements (get, describe, apply, delete, logs, exec, port-forward),
// readiness polling, and BNK cluster inspection/healing.
package k8s

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// InClusterKubeconfigSentinel is the magic value for kubeconfigPath that
// triggers rest.InClusterConfig() lookup. Used when bnkctl runs inside
// a cluster and gets its credentials from the projected service account.
const InClusterKubeconfigSentinel = "in-cluster"

// Client wraps a Kubernetes clientset and the REST config used to build it.
type Client struct {
	config    *rest.Config
	clientset *kubernetes.Clientset
}

// NewFromKubeconfigBytes builds a Client from raw kubeconfig YAML.
func NewFromKubeconfigBytes(b []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(b)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return &Client{config: cfg, clientset: cs}, nil
}

// NewFromKubeconfigFile builds a Client from a kubeconfig file on disk.
func NewFromKubeconfigFile(path string) (*Client, error) {
	if path == "" {
		return nil, errors.New("kubeconfig path is empty")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", path, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return &Client{config: cfg, clientset: cs}, nil
}

// NewFromDefault builds a Client by walking the default kubeconfig lookup chain.
func NewFromDefault() (*Client, error) {
	path := DefaultKubeconfigPath()
	if path == "" {
		return nil, errors.New("no kubeconfig found: set $KUBECONFIG or configure cluster credentials")
	}
	return NewFromKubeconfigFile(path)
}

// DefaultKubeconfigPath returns the first existing path in $KUBECONFIG
// (colon-separated), falling back to artifacts/kubeconfig, .bnkctl/*/kubeconfig,
// .awsbnkctl/*/kubeconfig, .azrbnkctl/*/kubeconfig, .gkebnkctl/*/kubeconfig, or ~/.kube/config.
func DefaultKubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		for _, p := range filepath.SplitList(v) {
			if _, err := os.Stat(p); err == nil { // #nosec G703
				return p
			}
		}
	}
	if _, err := os.Stat("artifacts/kubeconfig"); err == nil {
		return "artifacts/kubeconfig"
	}
	for _, pat := range []string{
		".bnkctl/*/kubeconfig",
		".awsbnkctl/*/kubeconfig",
		".azrbnkctl/*/kubeconfig",
		".gkebnkctl/*/kubeconfig",
	} {
		if matches, err := filepath.Glob(pat); err == nil && len(matches) > 0 {
			return matches[0]
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	def := filepath.Join(home, ".kube", "config")
	if _, err := os.Stat(def); err == nil {
		return def
	}
	return ""
}

// Clientset returns the underlying client-go clientset.
func (c *Client) Clientset() *kubernetes.Clientset { return c.clientset }

// RESTConfig returns the rest.Config used to construct the clientset.
func (c *Client) RESTConfig() *rest.Config { return c.config }

// BuildRESTConfig builds a rest.Config from a kubeconfig path or in-cluster sentinel.
func BuildRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == InClusterKubeconfigSentinel {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return cfg, nil
	}
	if kubeconfigPath == "" {
		kubeconfigPath = DefaultKubeconfigPath()
	}
	if kubeconfigPath == "" {
		return nil, errors.New("no kubeconfig found: set $KUBECONFIG or configure cluster credentials")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", kubeconfigPath, err)
	}
	return cfg, nil
}

// BuildRESTConfigFromKubeconfig builds a rest.Config from a local kubeconfig file + context name.
func BuildRESTConfigFromKubeconfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		kubeconfigPath = DefaultKubeconfigPath()
	}
	if kubeconfigPath == "" {
		return nil, errors.New("no kubeconfig found: set $KUBECONFIG or point bnk.farSecret.sourceContext at an existing ~/.kube/config entry")
	}
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	loader.ExplicitPath = kubeconfigPath
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s (context %q): %w", kubeconfigPath, contextName, err)
	}
	return cfg, nil
}

// NewClientsetFromConfig creates a typed kubernetes.Interface from rest.Config.
func NewClientsetFromConfig(cfg *rest.Config) (kubernetes.Interface, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return cs, nil
}

// NewDynamicFromConfig creates a dynamic.Interface from rest.Config.
func NewDynamicFromConfig(cfg *rest.Config) (dynamic.Interface, error) {
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return dc, nil
}

// BuildClientset returns a typed client for core + apps + batch + etc.
func BuildClientset(kubeconfigPath string) (kubernetes.Interface, error) {
	cfg, err := BuildRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	return NewClientsetFromConfig(cfg)
}

// BuildDynamicClient returns a dynamic.Interface for unstructured access.
func BuildDynamicClient(kubeconfigPath string) (dynamic.Interface, error) {
	cfg, err := BuildRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	return NewDynamicFromConfig(cfg)
}
