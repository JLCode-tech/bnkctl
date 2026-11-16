package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeconfigOptions controls kubeconfig inlining/flattening and endpoint rewriting
// prior to uploading a cluster kubeconfig to Forge.
type KubeconfigOptions struct {
	// BaseDir is the directory used to resolve relative file paths in
	// certificate-authority, client-certificate, client-key, and tokenFile.
	BaseDir string

	// ServerURL replaces the cluster server URL when non-empty (--server-url).
	ServerURL string

	// ContainerizedForge rewrites 127.0.0.1 / localhost / ::1 server
	// endpoints to host.docker.internal so containerized Forge backends can reach
	// local kind, OpenShift CRC, or SSH-tunneled API servers on the host.
	ContainerizedForge bool
}

// InlineKubeconfig converts local file references (certificate-authority,
// client-certificate, client-key, tokenFile) into embedded base64 *-data fields.
func InlineKubeconfig(raw []byte, baseDir string) ([]byte, error) {
	out, _, err := PrepareKubeconfig(raw, KubeconfigOptions{BaseDir: baseDir})
	return out, err
}

// PrepareKubeconfig flattens external file references into embedded base64 *-data
// fields and applies optional server endpoint overrides or loopback-to-Docker rewrites.
func PrepareKubeconfig(raw []byte, opts KubeconfigOptions) ([]byte, []string, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("forge.PrepareKubeconfig: kubeconfig is empty")
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc == nil {
		return raw, nil, nil
	}
	if doc["clusters"] == nil && doc["users"] == nil {
		return raw, nil, nil
	}

	var warnings []string
	mutated := false

	if clusters, ok := doc["clusters"].([]any); ok {
		for _, item := range clusters {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			clusterMap, ok := entry["cluster"].(map[string]any)
			if !ok {
				continue
			}

			inlined, err := inlineFileField(clusterMap, "certificate-authority", "certificate-authority-data", opts.BaseDir, true)
			if err != nil {
				return nil, warnings, err
			}
			if inlined {
				mutated = true
			}

			srvStr, _ := clusterMap["server"].(string)
			srvStr = strings.TrimSpace(srvStr)
			if override := strings.TrimSpace(opts.ServerURL); override != "" {
				if srvStr != override {
					clusterMap["server"] = override
					mutated = true
				}
			} else if opts.ContainerizedForge && srvStr != "" {
				if rewritten, origHost, changed := rewriteLoopbackServerURL(srvStr, "host.docker.internal"); changed {
					clusterMap["server"] = rewritten
					if tlsName, _ := clusterMap["tls-server-name"].(string); strings.TrimSpace(tlsName) == "" {
						if insecure, _ := clusterMap["insecure-skip-tls-verify"].(bool); !insecure && origHost != "" {
							clusterMap["tls-server-name"] = origHost
						}
					}
					warnings = append(warnings, fmt.Sprintf(
						"rewrote loopback kubeconfig server %s -> %s for Forge container reachability",
						srvStr, rewritten,
					))
					mutated = true
				}
			}
		}
	}

	if users, ok := doc["users"].([]any); ok {
		for _, item := range users {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			userMap, ok := entry["user"].(map[string]any)
			if !ok {
				continue
			}

			inlinedCert, err := inlineFileField(userMap, "client-certificate", "client-certificate-data", opts.BaseDir, true)
			if err != nil {
				return nil, warnings, err
			}
			if inlinedCert {
				mutated = true
			}

			inlinedKey, err := inlineFileField(userMap, "client-key", "client-key-data", opts.BaseDir, true)
			if err != nil {
				return nil, warnings, err
			}
			if inlinedKey {
				mutated = true
			}

			inlinedTok, err := inlineFileField(userMap, "tokenFile", "token", opts.BaseDir, false)
			if err != nil {
				return nil, warnings, err
			}
			if inlinedTok {
				mutated = true
			}
		}
	}

	if !mutated {
		return raw, warnings, nil
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, warnings, fmt.Errorf("forge.PrepareKubeconfig: marshal flattened kubeconfig: %w", err)
	}
	return out, warnings, nil
}

func inlineFileField(m map[string]any, pathKey, dataKey, baseDir string, base64Encode bool) (bool, error) {
	rawPath, ok := m[pathKey].(string)
	if !ok {
		return false, nil
	}
	trimmedPath := strings.TrimSpace(rawPath)
	if trimmedPath == "" {
		delete(m, pathKey)
		return true, nil
	}

	resolved := resolveKubeconfigAssetPath(trimmedPath, baseDir)
	data, err := os.ReadFile(resolved) // #nosec G304 -- path from operator kubeconfig
	if err != nil {
		if existing, ok := m[dataKey].(string); ok && strings.TrimSpace(existing) != "" {
			delete(m, pathKey)
			return true, nil
		}
		return false, fmt.Errorf("forge.PrepareKubeconfig: read %s %q: %w", pathKey, resolved, err)
	}

	if base64Encode {
		m[dataKey] = base64.StdEncoding.EncodeToString(data)
	} else {
		m[dataKey] = strings.TrimSpace(string(data))
	}
	delete(m, pathKey)
	return true, nil
}

func resolveKubeconfigAssetPath(p, baseDir string) string {
	if filepath.IsAbs(p) || strings.TrimSpace(baseDir) == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

func rewriteLoopbackServerURL(rawURL, replacementHost string) (rewritten string, origHost string, changed bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return rawURL, "", false
	}
	host := u.Hostname()
	if !isLoopbackHost(host) {
		return rawURL, "", false
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(replacementHost, port)
	} else {
		u.Host = replacementHost
	}
	return u.String(), host, true
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return true
	}
	return false
}

// IsContainerizedForgeURL reports whether the target Forge URL likely runs inside a
// local container (localhost / 127.0.0.1 / host.docker.internal) where loopback
// kubeconfig server addresses would resolve to the Forge container itself rather
// than the host machine.
func IsContainerizedForgeURL(forgeURL string) bool {
	if strings.TrimSpace(forgeURL) == "" {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(forgeURL))
	if err != nil {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "host.docker.internal"
}

// PingServerVersionFunc is the testable seam for pre-flight Kubernetes API reachability checks.
var PingServerVersionFunc = PingKubeconfigServer

// PingKubeconfigServer performs a pre-flight Kubernetes API `/version` ping using
// the provided kubeconfig YAML bytes and returns the server gitVersion string.
func PingKubeconfigServer(ctx context.Context, raw []byte, timeout time.Duration) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("kubeconfig is empty")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	cfg.Timeout = timeout
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("build kubernetes client: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := cs.Discovery().RESTClient().Get().AbsPath("/version").Do(pingCtx).Raw()
	if err != nil {
		return "", err
	}
	var info struct {
		GitVersion string `json:"gitVersion"`
	}
	if jerr := json.Unmarshal(body, &info); jerr == nil && info.GitVersion != "" {
		return info.GitVersion, nil
	}
	return strings.TrimSpace(string(body)), nil
}
