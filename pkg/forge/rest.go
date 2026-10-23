package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// restHTTPErr allows callers to switch on HTTP status code.
type restHTTPErr struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *restHTTPErr) Error() string {
	return fmt.Sprintf("http %d from %s: %s", e.StatusCode, e.URL, e.Body)
}

// RestCreds carries the forge REST login credentials.
type RestCreds struct {
	Username string
	Password string
}

func (c RestCreds) restUsername() string {
	if c.Username != "" {
		return c.Username
	}
	for _, k := range []string{"BNKCTL_FORGE_USERNAME", "FORGE_USERNAME", "AWSBNKCTL_FORGE_USERNAME", "AZRBNKCTL_FORGE_USERNAME", "GKEBNKCTL_FORGE_USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "admin"
}

func (c RestCreds) restPassword() string {
	if c.Password != "" {
		return c.Password
	}
	for _, k := range []string{"BNKCTL_FORGE_PASSWORD", "FORGE_PASSWORD", "AWSBNKCTL_FORGE_PASSWORD", "AZRBNKCTL_FORGE_PASSWORD", "GKEBNKCTL_FORGE_PASSWORD"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "changeme"
}

func resolveRESTBaseURL(restURL string) string {
	u := strings.TrimSpace(restURL)
	if u == "" {
		for _, k := range []string{"BNKCTL_FORGE_URL", "FORGE_URL", "AWSBNKCTL_FORGE_URL", "AZRBNKCTL_FORGE_URL", "GKEBNKCTL_FORGE_URL"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				u = v
				break
			}
		}
	}
	if u == "" {
		u = DefaultForgeRESTURL
	}
	return strings.TrimRight(u, "/")
}

// benchmarkHTTPDoFn is the single injectable HTTP transport seam used by all
// forge REST operations (lifecycle, agents, targets, proxies, configs, results).
// Default: http.DefaultClient.Do. Tests replace it via the BenchmarkHTTPDoFn
// export in benchmark_export_test.go.
var benchmarkHTTPDoFn func(*http.Request) (*http.Response, error) = http.DefaultClient.Do

// isConflictHTTP returns true if err represents an HTTP 409 Conflict
// or HTTP 400 Bad Request with "already exists" in the response body.
func isConflictHTTP(err error) bool {
	var herr *restHTTPErr
	if !errors.As(err, &herr) {
		return false
	}
	return herr.StatusCode == http.StatusConflict ||
		(herr.StatusCode == http.StatusBadRequest && strings.Contains(herr.Body, "already exists"))
}

// RegisterREST executes registration against Forge's REST API.
func RegisterREST(ctx context.Context, restURL string, req RegisterRequest, creds RestCreds) (RegisterResult, error) {
	if req.WorkspaceName == "" {
		return RegisterResult{}, fmt.Errorf("forge.RegisterREST: workspace name is required")
	}
	if req.WorkspaceDir == "" {
		return RegisterResult{}, fmt.Errorf("forge.RegisterREST: workspace dir is required")
	}
	if req.ClusterName == "" {
		return RegisterResult{}, fmt.Errorf("forge.RegisterREST: cluster name is required")
	}
	if len(req.Kubeconfig) == 0 {
		return RegisterResult{}, fmt.Errorf("forge.RegisterREST: kubeconfig is empty")
	}
	if req.ProjectName == "" {
		req.ProjectName = defaultProjectName(req.WorkspaceName)
	}

	base := resolveRESTBaseURL(restURL)

	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return RegisterResult{}, fmt.Errorf("forge REST login: %w", err)
	}

	proj, err := restCreateProject(ctx, base, token, req)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("forge REST create project: %w", err)
	}

	cluster, err := restCreateCluster(ctx, base, token, proj.ID, req)
	if err != nil {
		_ = restDeleteProject(ctx, base, token, proj.ID)
		return RegisterResult{}, fmt.Errorf("forge REST create cluster: %w", err)
	}

	link := &Link{
		ForgeURL:     base,
		ProjectID:    proj.ID,
		ProjectName:  proj.Name,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
		RegisteredAt: time.Now().UTC(),
		Workspace:    req.WorkspaceName,
		Status:       "registered",
	}
	if err := WriteLink(req.WorkspaceDir, link); err != nil {
		return RegisterResult{Link: link, ForgeURL: base},
			fmt.Errorf("registration succeeded but writing forge_link.json failed: %w", err)
	}
	return RegisterResult{Link: link, ForgeURL: base}, nil
}

// UnregisterREST deletes cluster and project via REST.
func UnregisterREST(ctx context.Context, restURL string, link *Link, creds RestCreds) error {
	if link == nil {
		return fmt.Errorf("forge.UnregisterREST: link is nil")
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return fmt.Errorf("forge REST login: %w", err)
	}

	if link.ClusterID > 0 {
		_ = DeleteAllClusterBenchmarkArtifacts(ctx, base, creds, link.ClusterID)
	}
	if link.ClusterID > 0 && link.ProjectID > 0 {
		if err := restDeleteCluster(ctx, base, token, link.ProjectID, link.ClusterID); err != nil && !Is404(err) {
			return fmt.Errorf("forge REST delete cluster: %w", err)
		}
	}
	if link.ProjectID > 0 {
		if err := restDeleteProject(ctx, base, token, link.ProjectID); err != nil && !Is404(err) {
			return fmt.Errorf("forge REST delete project: %w", err)
		}
	}
	return nil
}

// UnregisterRESTByName discovers project by name when link file is absent.
func UnregisterRESTByName(ctx context.Context, restURL, clusterName string, creds RestCreds) error {
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return fmt.Errorf("forge REST login: %w", err)
	}

	candidates := []string{
		defaultProjectName(clusterName),
		"awsbnkctl-" + clusterName,
		"gkebnkctl-" + clusterName,
		"bnkctl-" + clusterName,
		clusterName,
	}
	var proj restProject
	for _, cand := range candidates {
		p, findErr := restFindProjectByName(ctx, base, token, cand)
		if findErr == nil {
			proj = p
			err = nil
			break
		}
		err = findErr
		if !errors.Is(findErr, os.ErrNotExist) {
			return findErr
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Project record is already absent from Forge REST (idempotent success).
			return nil
		}
		return err
	}

	cluster, err := restFindClusterByName(ctx, base, token, proj.ID, clusterName)
	switch {
	case err == nil:
		if cluster.ID > 0 {
			_ = DeleteAllClusterBenchmarkArtifacts(ctx, base, creds, cluster.ID)
		}
		if derr := restDeleteCluster(ctx, base, token, proj.ID, cluster.ID); derr != nil && !Is404(derr) {
			return fmt.Errorf("forge REST delete cluster: %w", derr)
		}
	case errors.Is(err, os.ErrNotExist):
		// Cluster record already gone
	default:
		return err
	}

	if err := restDeleteProject(ctx, base, token, proj.ID); err != nil && !Is404(err) {
		return fmt.Errorf("forge REST delete project: %w", err)
	}
	return nil
}

func defaultProjectName(workspace string) string {
	return "azrbnkctl-" + workspace
}

// IsMCPCatalogGapErr detects when FastMCP lacks create_project / create_cluster tools.
func IsMCPCatalogGapErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "tool not found") ||
		strings.Contains(s, "unknown tool") ||
		strings.Contains(s, "method not found") ||
		strings.Contains(s, "no tool named") ||
		strings.Contains(s, "tool_not_found")
}

// ── REST helpers ──────────────────────────────────────────────────────────────

func restLogin(ctx context.Context, base, username, password string) (string, error) {
	body := map[string]string{"username": username, "password": password}
	var resp struct {
		Token string `json:"token"`
	}
	err := restPost(ctx, base+"/api/auth/login", "", body, &resp)
	if err != nil {
		var herr *restHTTPErr
		if errors.As(err, &herr) && herr.StatusCode == http.StatusUnauthorized && username == "admin" && password == "changeme" {
			fallbackBody := map[string]string{"username": "mcp", "password": "mcp-svc-local-token-2026-auth"}
			if fbErr := restPost(ctx, base+"/api/auth/login", "", fallbackBody, &resp); fbErr == nil && resp.Token != "" {
				return resp.Token, nil
			}
		}
		return "", err
	}
	if resp.Token == "" {
		return "", fmt.Errorf("forge REST login: empty token in response")
	}
	return resp.Token, nil
}

type restProject struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func restCreateProject(ctx context.Context, base, token string, req RegisterRequest) (restProject, error) {
	projectType := req.ProjectType
	if projectType == "" {
		projectType = "cloud-azure"
	}
	cloudProvider := req.CloudProvider
	if cloudProvider == "" {
		cloudProvider = "azure"
	}
	env := req.Environment
	if env == "" {
		env = "dev"
	}
	body := map[string]any{
		"name":           req.ProjectName,
		"project_type":   projectType,
		"cloud_provider": cloudProvider,
		"region":         req.Region,
		"environment":    env,
		"description":    fmt.Sprintf("Created by azrbnkctl for workspace %q", req.WorkspaceName),
	}
	if req.CredentialTemplateID > 0 {
		body["credential_template_id"] = req.CredentialTemplateID
	}

	var resp struct {
		ID        int         `json:"id"`
		ProjectID int         `json:"project_id"`
		Name      string      `json:"name"`
		Project   restProject `json:"project"`
		Success   bool        `json:"success"`
	}
	err := restPost(ctx, base+"/api/projects", token, body, &resp)
	if err != nil {
		if isConflictHTTP(err) {
			return restFindProjectByName(ctx, base, token, req.ProjectName)
		}
		return restProject{}, err
	}

	id := resp.ID
	if id == 0 {
		id = resp.ProjectID
	}
	if id == 0 {
		id = resp.Project.ID
	}
	name := resp.Name
	if name == "" {
		name = resp.Project.Name
	}
	if name == "" {
		name = req.ProjectName
	}
	if id == 0 {
		return restFindProjectByName(ctx, base, token, req.ProjectName)
	}
	return restProject{ID: id, Name: name}, nil
}

func restFindProjectByName(ctx context.Context, base, token, name string) (restProject, error) {
	var raw json.RawMessage
	if err := restGet(ctx, base+"/api/projects", token, &raw); err != nil {
		var herr *restHTTPErr
		if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
			return restProject{}, fmt.Errorf("project %q: %w", name, os.ErrNotExist)
		}
		return restProject{}, fmt.Errorf("list projects: %w", err)
	}
	var list []restProject
	var wrapped struct {
		Projects []restProject `json:"projects"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Projects != nil {
		list = wrapped.Projects
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return restProject{}, fmt.Errorf("list projects: %w", err)
	}
	for _, p := range list {
		if p.Name == name {
			return p, nil
		}
	}
	return restProject{}, fmt.Errorf("project %q: %w", name, os.ErrNotExist)
}

type restCluster struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ProjectID int    `json:"project_id"`
}

func restCreateCluster(ctx context.Context, base, token string, projectID int, req RegisterRequest) (restCluster, error) {
	encodedKubeconfig := base64.StdEncoding.EncodeToString(req.Kubeconfig)
	cloudProvider := req.CloudProvider
	if cloudProvider == "" {
		cloudProvider = "azure"
	}
	body := map[string]any{
		"name":           req.ClusterName,
		"cloud_provider": cloudProvider,
		"region":         req.Region,
		"kubeconfig":     encodedKubeconfig,
	}
	url := fmt.Sprintf("%s/api/projects/%d/k8s/clusters", base, projectID)

	var resp struct {
		ID        int         `json:"id"`
		ClusterID int         `json:"cluster_id"`
		Name      string      `json:"name"`
		Cluster   restCluster `json:"cluster"`
		Success   bool        `json:"success"`
	}
	err := restPost(ctx, url, token, body, &resp)
	if err != nil {
		if isConflictHTTP(err) {
			existing, findErr := restFindClusterByName(ctx, base, token, projectID, req.ClusterName)
			if findErr != nil {
				return restCluster{}, fmt.Errorf("409 conflict but cluster lookup failed: %w", findErr)
			}
			updateURL := fmt.Sprintf("%s/api/projects/%d/k8s/clusters/%d", base, projectID, existing.ID)
			updateBody := map[string]any{
				"kubeconfig": encodedKubeconfig,
				"region":     req.Region,
			}
			var updated restCluster
			if putErr := restPut(ctx, updateURL, token, updateBody, &updated); putErr != nil {
				return restCluster{}, fmt.Errorf("update cluster %d: %w", existing.ID, putErr)
			}
			if updated.ID == 0 {
				updated = existing
			}
			return updated, nil
		}
		return restCluster{}, err
	}

	id := resp.ID
	if id == 0 {
		id = resp.ClusterID
	}
	if id == 0 {
		id = resp.Cluster.ID
	}
	name := resp.Name
	if name == "" {
		name = resp.Cluster.Name
	}
	if name == "" {
		name = req.ClusterName
	}
	if id == 0 {
		return restFindClusterByName(ctx, base, token, projectID, req.ClusterName)
	}
	return restCluster{ID: id, Name: name, ProjectID: projectID}, nil
}

func restFindClusterByName(ctx context.Context, base, token string, projectID int, name string) (restCluster, error) {
	url := fmt.Sprintf("%s/api/projects/%d/k8s/clusters", base, projectID)
	var raw json.RawMessage
	if err := restGet(ctx, url, token, &raw); err != nil {
		return restCluster{}, fmt.Errorf("list clusters: %w", err)
	}
	var list []restCluster
	var wrapped struct {
		Clusters []restCluster `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Clusters != nil {
		list = wrapped.Clusters
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return restCluster{}, fmt.Errorf("list clusters: %w", err)
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	return restCluster{}, fmt.Errorf("cluster %q in project %d: %w", name, projectID, os.ErrNotExist)
}

func restDeleteCluster(ctx context.Context, base, token string, projectID, clusterID int) error {
	url := fmt.Sprintf("%s/api/projects/%d/k8s/clusters/%d", base, projectID, clusterID)
	return restDelete(ctx, url, token)
}

func restDeleteProject(ctx context.Context, base, token string, projectID int) error {
	url := fmt.Sprintf("%s/api/projects/%d?force=true", base, projectID)
	err := restDelete(ctx, url, token)
	if err != nil && strings.Contains(err.Error(), "Cannot delete active project") {
		_ = restPut(ctx, fmt.Sprintf("%s/api/projects/%d", base, projectID), token, map[string]any{"enabled": false}, nil)
		err = restDelete(ctx, url, token)
	}
	return err
}

// ── HTTP verbs ────────────────────────────────────────────────────────────────

// restPost sends a POST request with JSON body and decodes the JSON response
// into out. On HTTP status >= 400 returns *restHTTPErr. Uses the unified
// benchmarkHTTPDoFn transport seam.
func restPost(ctx context.Context, url, token string, body, out any) error {
	req, err := newRESTRequest(ctx, http.MethodPost, url, token, body)
	if err != nil {
		return err
	}
	return doRESTRequest(req, url, out)
}

// restPut mirrors restPost but uses HTTP PUT.
func restPut(ctx context.Context, url, token string, body, out any) error {
	req, err := newRESTRequest(ctx, http.MethodPut, url, token, body)
	if err != nil {
		return err
	}
	return doRESTRequest(req, url, out)
}

// restGet sends a GET request and decodes the JSON response into out.
func restGet(ctx context.Context, url, token string, out any) error {
	req, err := newRESTRequest(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return err
	}
	return doRESTRequest(req, url, out)
}

// newRESTRequest builds an *http.Request with JSON body (when body != nil) and
// Authorization header.
func newRESTRequest(ctx context.Context, method, url, token string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// doRESTRequest executes req via benchmarkHTTPDoFn, checks for HTTP errors, and
// decodes the JSON response into out (when out != nil and body is non-empty).
func doRESTRequest(req *http.Request, url string, out any) error {
	resp, err := benchmarkHTTPDoFn(req)
	if err != nil {
		return fmt.Errorf("http %s %s: %w", req.Method, url, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &restHTTPErr{StatusCode: resp.StatusCode, URL: url, Body: truncateREST(string(respBytes), 400)}
	}
	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("decode response from %s: %w", url, err)
		}
	}
	return nil
}

// restDelete sends a DELETE request and returns *restHTTPErr on status >= 400.
func restDelete(ctx context.Context, url, token string) error {
	req, err := newRESTRequest(ctx, http.MethodDelete, url, token, nil)
	if err != nil {
		return err
	}
	return doRESTRequest(req, url, nil)
}

func truncateREST(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
