package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge/mcp"
)

// DefaultMCPURL is the localhost endpoint for the forge MCP server's
// Streamable HTTP transport, matching FastMCP's default.
const DefaultMCPURL = "http://localhost:8081/mcp/"

// DefaultForgeRESTURL is the fallback REST base when forge.url is not set.
const DefaultForgeRESTURL = "http://localhost:8000"

// DefaultTimeout is the per-MCP-call timeout.
const DefaultTimeout = 30 * time.Second

// Client is the high-level forge API azrbnkctl uses. It wraps the
// low-level MCP client with typed Go signatures for each tool we touch.
type Client struct {
	mcp *mcp.Client
	url string
}

// NewClient constructs a Client targeting the given MCP endpoint.
// Pass "" to use [DefaultMCPURL], or BNKCTL_FORGE_MCP_URL / cloud-specific env vars when set.
func NewClient(endpoint string) *Client {
	if endpoint == "" {
		for _, k := range []string{
			"BNKCTL_FORGE_MCP_URL",
			"AWSBNKCTL_FORGE_MCP_URL",
			"AZRBNKCTL_FORGE_MCP_URL",
			"GKEBNKCTL_FORGE_MCP_URL",
		} {
			if v := os.Getenv(k); v != "" {
				endpoint = v
				break
			}
		}
	}
	if endpoint == "" {
		endpoint = DefaultMCPURL
	}
	return &Client{
		mcp: mcp.New(endpoint, DefaultTimeout),
		url: endpoint,
	}
}

// URL reports the MCP endpoint the client targets.
func (c *Client) URL() string { return c.url }

// SystemHealth pings forge's system_health MCP tool.
func (c *Client) SystemHealth(ctx context.Context) (string, error) {
	r, err := c.mcp.CallTool(ctx, "system_health", nil)
	if err != nil {
		return "", err
	}
	return r.Text(), nil
}

// SystemVersion returns the forge version string.
func (c *Client) SystemVersion(ctx context.Context) (string, error) {
	r, err := c.mcp.CallTool(ctx, "system_version", nil)
	if err != nil {
		return "", err
	}
	return r.Text(), nil
}

// CreateProjectRequest mirrors the create_project MCP tool's argument shape.
type CreateProjectRequest struct {
	Name                  string `json:"name"`
	Description           string `json:"description,omitempty"`
	ProjectType           string `json:"project_type,omitempty"`   // e.g. "cloud-aws", "cloud-azure", "cloud-gcp"
	CloudProvider         string `json:"cloud_provider,omitempty"` // e.g. "aws", "azure", "gcp"
	Environment           string `json:"environment,omitempty"`    // default "dev"
	Region                string `json:"region,omitempty"`
	AWSProfile            string `json:"aws_profile,omitempty"`
	AzureSubscriptionID   string `json:"azure_subscription_id,omitempty"`
	BackendType           string `json:"backend_type,omitempty"` // default "local"
	Color                 string `json:"color,omitempty"`
	Icon                  string `json:"icon,omitempty"`
	CredentialTemplateID  int    `json:"credential_template_id,omitempty"`
	TargetPlatformProfile string `json:"target_platform_profile,omitempty"`
}

// CreateProjectResponse captures the fields bnkctl reads.
type CreateProjectResponse struct {
	Project struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON handles flat and nested envelopes from FastMCP.
func (r *CreateProjectResponse) UnmarshalJSON(b []byte) error {
	type alias CreateProjectResponse
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = CreateProjectResponse(a)

	var flat struct {
		ProjectID int    `json:"project_id"`
		ID        int    `json:"id"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(b, &flat); err == nil {
		if flat.ProjectID > 0 {
			r.Project.ID = flat.ProjectID
		} else if flat.ID > 0 {
			r.Project.ID = flat.ID
		}
		if r.Project.Name == "" && flat.Name != "" {
			r.Project.Name = flat.Name
		}
	}
	return nil
}

// CreateProject creates a project in forge.
func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (CreateProjectResponse, error) {
	args := map[string]any{
		"name": req.Name,
	}
	addStr := func(k, v string) {
		if v != "" {
			args[k] = v
		}
	}
	addStr("description", req.Description)
	addStr("project_type", req.ProjectType)
	addStr("cloud_provider", req.CloudProvider)
	addStr("environment", req.Environment)
	addStr("region", req.Region)
	addStr("aws_profile", req.AWSProfile)
	addStr("azure_subscription_id", req.AzureSubscriptionID)
	addStr("backend_type", req.BackendType)
	addStr("color", req.Color)
	addStr("icon", req.Icon)
	addStr("target_platform_profile", req.TargetPlatformProfile)
	if req.CredentialTemplateID > 0 {
		args["credential_template_id"] = req.CredentialTemplateID
	}

	res, err := c.mcp.CallTool(ctx, "create_project", args)
	if err != nil {
		return CreateProjectResponse{}, err
	}

	var out CreateProjectResponse
	if err := res.UnmarshalToolJSON(&out); err != nil {
		return CreateProjectResponse{}, fmt.Errorf("decode create_project: %w (raw: %s)", err, res.Text())
	}
	var errCheck struct {
		Ok     *bool  `json:"ok"`
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(res.Text()), &errCheck)
	if (errCheck.Ok != nil && !*errCheck.Ok) || (errCheck.Error != nil && out.Project.ID == 0) || out.Project.ID == 0 {
		return CreateProjectResponse{}, fmt.Errorf("create_project failed: %s", res.Text())
	}
	return out, nil
}

// ListProjectsResponse captures projects returned by the list_projects MCP tool.
type ListProjectsResponse struct {
	Projects []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"projects"`
}

// UnmarshalJSON handles both {"projects": [...]} envelopes and bare [...] arrays.
func (r *ListProjectsResponse) UnmarshalJSON(b []byte) error {
	type projectItem struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	var env struct {
		Projects []projectItem `json:"projects"`
	}
	if err := json.Unmarshal(b, &env); err == nil && env.Projects != nil {
		r.Projects = make([]struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}, len(env.Projects))
		for i, p := range env.Projects {
			r.Projects[i].ID = p.ID
			r.Projects[i].Name = p.Name
		}
		return nil
	}
	var arr []projectItem
	if err := json.Unmarshal(b, &arr); err == nil {
		r.Projects = make([]struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}, len(arr))
		for i, p := range arr {
			r.Projects[i].ID = p.ID
			r.Projects[i].Name = p.Name
		}
		return nil
	}
	return fmt.Errorf("decode list_projects: %s", string(b))
}

// ListProjects queries forge's list_projects MCP tool.
func (c *Client) ListProjects(ctx context.Context) (ListProjectsResponse, error) {
	r, err := c.mcp.CallTool(ctx, "list_projects", map[string]any{})
	if err != nil {
		return ListProjectsResponse{}, err
	}
	var out ListProjectsResponse
	if err := r.UnmarshalToolJSON(&out); err != nil {
		return ListProjectsResponse{}, err
	}
	return out, nil
}

// FindProjectByName queries list_projects MCP tool and matches by name (case-insensitive fallback).
func (c *Client) FindProjectByName(ctx context.Context, name string) (CreateProjectResponse, error) {
	data, err := c.ListProjects(ctx)
	if err != nil {
		return CreateProjectResponse{}, err
	}
	for _, p := range data.Projects {
		if p.Name == name || strings.EqualFold(p.Name, name) {
			var resp CreateProjectResponse
			resp.Success = true
			resp.Project.ID = p.ID
			resp.Project.Name = p.Name
			return resp, nil
		}
	}
	return CreateProjectResponse{}, fmt.Errorf("project %q not found", name)
}

// DeleteProject deletes a project in forge.
func (c *Client) DeleteProject(ctx context.Context, projectID int, force bool) error {
	_, err := c.mcp.CallTool(ctx, "delete_project", map[string]any{
		"project_id": projectID,
		"force":      force,
		"confirm":    true,
	})
	return err
}

// CreateClusterRequest mirrors the create_cluster tool arguments.
type CreateClusterRequest struct {
	ProjectID        int    `json:"project_id"`
	Name             string `json:"name"`
	Kubeconfig       string `json:"kubeconfig,omitempty"`
	CloudProvider    string `json:"cloud_provider"`
	Region           string `json:"region"`
	Context          string `json:"context,omitempty"`
	DefaultNamespace string `json:"default_namespace,omitempty"`
}

// CreateClusterResponse is what create_cluster returns.
type CreateClusterResponse struct {
	Cluster struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ProjectID int    `json:"project_id"`
	} `json:"cluster"`
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON handles flat and nested envelopes.
func (r *CreateClusterResponse) UnmarshalJSON(b []byte) error {
	type alias CreateClusterResponse
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = CreateClusterResponse(a)

	var flat struct {
		ClusterID int    `json:"cluster_id"`
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ProjectID int    `json:"project_id"`
	}
	if err := json.Unmarshal(b, &flat); err == nil {
		if flat.ClusterID > 0 {
			r.Cluster.ID = flat.ClusterID
		} else if flat.ID > 0 {
			r.Cluster.ID = flat.ID
		}
		if r.Cluster.Name == "" && flat.Name != "" {
			r.Cluster.Name = flat.Name
		}
		if flat.ProjectID > 0 {
			r.Cluster.ProjectID = flat.ProjectID
		}
	}
	return nil
}

// CreateCluster registers a Kubernetes cluster with forge.
func (c *Client) CreateCluster(ctx context.Context, req CreateClusterRequest, kubeconfig []byte) (CreateClusterResponse, error) {
	if req.ProjectID == 0 {
		return CreateClusterResponse{}, fmt.Errorf("CreateCluster: project_id is required")
	}
	if req.Name == "" {
		return CreateClusterResponse{}, fmt.Errorf("CreateCluster: cluster name is required")
	}
	if len(kubeconfig) == 0 {
		return CreateClusterResponse{}, fmt.Errorf("CreateCluster: kubeconfig is empty")
	}
	args := map[string]any{
		"project_id": req.ProjectID,
		"name":       req.Name,
		"kubeconfig": base64.StdEncoding.EncodeToString(kubeconfig),
	}
	if req.CloudProvider != "" {
		args["cloud_provider"] = req.CloudProvider
	}
	if req.Region != "" {
		args["region"] = req.Region
	}
	if req.Context != "" {
		args["context"] = req.Context
	}
	if req.DefaultNamespace != "" {
		args["default_namespace"] = req.DefaultNamespace
	}

	res, err := c.mcp.CallTool(ctx, "create_cluster", args)
	if err != nil {
		return CreateClusterResponse{}, err
	}

	var out CreateClusterResponse
	if err := res.UnmarshalToolJSON(&out); err != nil {
		return CreateClusterResponse{}, fmt.Errorf("decode create_cluster: %w (raw: %s)", err, res.Text())
	}
	var errCheck struct {
		Ok     *bool  `json:"ok"`
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(res.Text()), &errCheck)
	if (errCheck.Ok != nil && !*errCheck.Ok) || (errCheck.Error != nil && out.Cluster.ID == 0) || out.Cluster.ID == 0 {
		return CreateClusterResponse{}, fmt.Errorf("create_cluster failed: %s", res.Text())
	}
	return out, nil
}

// GetCluster reads back cluster details from forge.
func (c *Client) GetCluster(ctx context.Context, clusterID int) (string, error) {
	res, err := c.mcp.CallTool(ctx, "get_cluster", map[string]any{
		"cluster_id": clusterID,
	})
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// ScanCluster triggers an active in-cluster scan forge-side.
func (c *Client) ScanCluster(ctx context.Context, clusterID int) (string, error) {
	res, err := c.mcp.CallTool(ctx, "scan_cluster", map[string]any{
		"cluster_id": clusterID,
	})
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// BNKHealth calls forge's bnk_health MCP tool.
func (c *Client) BNKHealth(ctx context.Context, clusterID int) (string, error) {
	res, err := c.mcp.CallTool(ctx, "bnk_health", map[string]any{
		"cluster_id": clusterID,
	})
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// ListClustersResponse captures clusters returned by the list_clusters MCP tool.
type ListClustersResponse struct {
	Clusters []struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ProjectID int    `json:"project_id"`
	} `json:"clusters"`
}

// UnmarshalJSON handles both {"clusters": [...]} envelopes and bare [...] arrays.
func (r *ListClustersResponse) UnmarshalJSON(b []byte) error {
	type clusterItem struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ProjectID int    `json:"project_id"`
	}
	var env struct {
		Clusters []clusterItem `json:"clusters"`
	}
	if err := json.Unmarshal(b, &env); err == nil && env.Clusters != nil {
		r.Clusters = make([]struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			ProjectID int    `json:"project_id"`
		}, len(env.Clusters))
		for i, cl := range env.Clusters {
			r.Clusters[i].ID = cl.ID
			r.Clusters[i].Name = cl.Name
			r.Clusters[i].ProjectID = cl.ProjectID
		}
		return nil
	}
	var arr []clusterItem
	if err := json.Unmarshal(b, &arr); err == nil {
		r.Clusters = make([]struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			ProjectID int    `json:"project_id"`
		}, len(arr))
		for i, cl := range arr {
			r.Clusters[i].ID = cl.ID
			r.Clusters[i].Name = cl.Name
			r.Clusters[i].ProjectID = cl.ProjectID
		}
		return nil
	}
	return fmt.Errorf("decode list_clusters: %s", string(b))
}

// ListClusters queries forge's list_clusters MCP tool for a project.
func (c *Client) ListClusters(ctx context.Context, projectID int) (ListClustersResponse, error) {
	args := map[string]any{}
	if projectID > 0 {
		args["project_id"] = projectID
	}
	r, err := c.mcp.CallTool(ctx, "list_clusters", args)
	if err != nil {
		return ListClustersResponse{}, err
	}
	var out ListClustersResponse
	if err := r.UnmarshalToolJSON(&out); err != nil {
		return ListClustersResponse{}, err
	}
	return out, nil
}

// DeleteCluster unregisters a cluster from forge.
func (c *Client) DeleteCluster(ctx context.Context, clusterID int) error {
	_, err := c.mcp.CallTool(ctx, "delete_cluster", map[string]any{
		"cluster_id": clusterID,
		"confirm":    true,
	})
	return err
}
