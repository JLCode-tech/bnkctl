package gcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"golang.org/x/oauth2/google"
	"gopkg.in/yaml.v3"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
)

// BackupSuffix is the extension appended to the original ~/.kube/config during sync.
const BackupSuffix = ".gkebnkctl-bak"

// Kubeconfig represents a standard Kubernetes kubeconfig structure.
type Kubeconfig struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	CurrentContext string         `yaml:"current-context"`
	Clusters       []NamedCluster `yaml:"clusters"`
	Contexts       []NamedContext `yaml:"contexts"`
	Users          []NamedUser    `yaml:"users"`
	Preferences    map[string]any `yaml:"preferences,omitempty"`
}

// NamedCluster is a named cluster entry in kubeconfig.
type NamedCluster struct {
	Name    string         `yaml:"name"`
	Cluster ClusterDetails `yaml:"cluster"`
}

// ClusterDetails holds server and CA data for a cluster entry.
type ClusterDetails struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

// NamedContext is a named context entry in kubeconfig.
type NamedContext struct {
	Name    string         `yaml:"name"`
	Context ContextDetails `yaml:"context"`
}

// ContextDetails binds a cluster name and user name.
type ContextDetails struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

// NamedUser is a named user entry in kubeconfig.
type NamedUser struct {
	Name string      `yaml:"name"`
	User UserDetails `yaml:"user"`
}

// UserDetails holds exec plugin or bearer token configuration for a user entry.
type UserDetails struct {
	Token string      `yaml:"token,omitempty"`
	Exec  *ExecConfig `yaml:"exec,omitempty"`
}

// ExecConfig holds client.authentication.k8s.io exec plugin settings.
type ExecConfig struct {
	APIVersion         string   `yaml:"apiVersion"`
	Command            string   `yaml:"command"`
	Args               []string `yaml:"args,omitempty"`
	InteractiveMode    string   `yaml:"interactiveMode,omitempty"`
	ProvideClusterInfo bool     `yaml:"provideClusterInfo,omitempty"`
}

// GenerateKubeconfig builds a standard GKE kubeconfig YAML byte slice.
func GenerateKubeconfig(clusterName, project, zone, endpoint, caData string) ([]byte, error) {
	return GenerateKubeconfigWithToken(clusterName, project, zone, endpoint, caData, "")
}

// GenerateKubeconfigWithToken builds a GKE kubeconfig YAML byte slice using a bearer token
// when provided (for portable Forge/client-go usage without gke-gcloud-auth-plugin),
// falling back to gke-gcloud-auth-plugin exec auth when token is empty.
func GenerateKubeconfigWithToken(clusterName, project, zone, endpoint, caData, token string) ([]byte, error) {
	contextName := fmt.Sprintf("gke_%s_%s_%s", project, zone, clusterName)
	server := endpoint
	if server != "" && !strings.HasPrefix(server, "http") {
		server = "https://" + server
	}

	userDetails := UserDetails{}
	if strings.TrimSpace(token) != "" {
		userDetails.Token = strings.TrimSpace(token)
	} else {
		userDetails.Exec = &ExecConfig{
			APIVersion:         "client.authentication.k8s.io/v1beta1",
			Command:            "gke-gcloud-auth-plugin",
			InteractiveMode:    "IfAvailable",
			ProvideClusterInfo: true,
		}
	}

	cfg := Kubeconfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: contextName,
		Clusters: []NamedCluster{
			{
				Name: contextName,
				Cluster: ClusterDetails{
					Server:                   server,
					CertificateAuthorityData: caData,
				},
			},
		},
		Contexts: []NamedContext{
			{
				Name: contextName,
				Context: ContextDetails{
					Cluster: contextName,
					User:    contextName,
				},
			},
		},
		Users: []NamedUser{
			{
				Name: contextName,
				User: userDetails,
			},
		},
	}

	return yaml.Marshal(&cfg)
}

// FetchAndGenerateKubeconfig queries GKE for the cluster descriptor and generates its Kubeconfig.
func FetchAndGenerateKubeconfig(ctx context.Context, clients *Clients, cl *intent.Cluster) ([]byte, error) {
	if clients == nil || clients.GKE == nil {
		var err error
		clients, err = NewClients(ctx, cl.Metadata.Project)
		if err != nil {
			return nil, fmt.Errorf("init GCP clients: %w", err)
		}
	}

	clusterName := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		cl.Metadata.Project, cl.Metadata.Zone, cl.Metadata.Name)

	resp, err := clients.GKE.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil {
		return nil, fmt.Errorf("fetch cluster %s: %w", cl.Metadata.Name, err)
	}

	caCert := resp.GetMasterAuth().GetClusterCaCertificate()
	endpoint := resp.GetEndpoint()

	var bearerToken string
	if ts, tErr := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform"); tErr == nil && ts != nil {
		if tok, tokErr := ts.Token(); tokErr == nil && tok != nil {
			bearerToken = tok.AccessToken
		}
	}

	return GenerateKubeconfigWithToken(cl.Metadata.Name, cl.Metadata.Project, cl.Metadata.Zone, endpoint, caCert, bearerToken)
}

// WriteKubeconfigFile writes the kubeconfig bytes to the target path.
func WriteKubeconfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return os.WriteFile(path, data, 0o600)
}

// GlobalKubeconfigPath returns the path to ~/.kube/config.
func GlobalKubeconfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// SyncGlobalKubeconfig safely writes data to ~/.kube/config, backing up any existing file.
func SyncGlobalKubeconfig(data []byte) (string, error) {
	globalPath, err := GlobalKubeconfigPath()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}

	backupPath := globalPath + BackupSuffix

	if _, err := os.Stat(globalPath); err == nil {
		if _, err := os.Stat(backupPath); os.IsNotExist(err) {
			orig, err := os.ReadFile(globalPath) // #nosec G304 -- path is standard user home kubeconfig
			if err != nil {
				return "", fmt.Errorf("read existing kubeconfig: %w", err)
			}
			if err := os.WriteFile(backupPath, orig, 0o600); err != nil { // #nosec G304 G703 -- backup path in user home
				return "", fmt.Errorf("write backup %s: %w", backupPath, err)
			}
		}
	}

	if err := WriteKubeconfigFile(globalPath, data); err != nil {
		return "", fmt.Errorf("write global kubeconfig: %w", err)
	}

	return backupPath, nil
}

// RestoreGlobalKubeconfig restores ~/.kube/config from ~/.kube/config.gkebnkctl-bak if it exists.
func RestoreGlobalKubeconfig() (bool, error) {
	globalPath, err := GlobalKubeconfigPath()
	if err != nil {
		return false, fmt.Errorf("get home dir: %w", err)
	}

	backupPath := globalPath + BackupSuffix
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return false, nil
	}

	backupData, err := os.ReadFile(backupPath) // #nosec G304 -- path is standard user home kubeconfig backup
	if err != nil {
		return false, fmt.Errorf("read backup: %w", err)
	}

	if err := os.WriteFile(globalPath, backupData, 0o600); err != nil { // #nosec G304 G703 -- global kubeconfig path in user home
		return false, fmt.Errorf("restore kubeconfig: %w", err)
	}

	_ = os.Remove(backupPath)
	return true, nil
}
