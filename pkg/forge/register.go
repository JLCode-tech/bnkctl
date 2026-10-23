package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// RegisterRequest carries everything `forge register` needs from the
// caller. The caller is responsible for resolving the workspace dir and
// providing the kubeconfig — this package doesn't reach into config/.
type RegisterRequest struct {
	// ForgeRESTURL is the Forge REST base URL recorded in the link so the
	// REST-side commands (forge scan --register-targets, benchmark) find Forge
	// without flags. Optional.
	ForgeRESTURL  string
	WorkspaceName string // logical workspace label (used in project name)
	WorkspaceDir  string // filesystem dir where forge_link.json lives
	ProjectName   string // forge project name; empty → "azrbnkctl-<workspace>"
	ClusterName   string // EKS cluster name registered into forge
	Region        string // AWS region
	Kubeconfig    []byte // raw YAML for the EKS cluster

	// AWSProfile is the named AWS profile forge should use when minting
	// EKS bearer tokens for this cluster. When empty, Register fills it
	// from os.Getenv("AWS_PROFILE") so the forge backend records the
	// same identity the operator used to provision the cluster — without
	// it forge falls back to its own global env, which silently breaks
	// for clusters outside the backend's default region. Set to "-" to
	// explicitly skip sending an AWS profile.
	AWSProfile string

	// Environment is the forge project environment (e.g. "dev", "staging", "prod").
	// Empty defaults to "dev".
	Environment string

	// ProjectType is the forge project type (e.g. "cloud-azure").
	// Empty defaults to "cloud-azure".
	ProjectType string

	// CloudProvider is the cloud provider label (e.g. "aws").
	// Empty defaults to "aws".
	CloudProvider string

	// CredentialTemplateID is the forge credential template ID to attach to
	// the newly-created project. When 0, no credential is attached and forge
	// falls back to its default (typically the operator must wire it manually).
	CredentialTemplateID int

	// If true, after CreateCluster azrbnkctl calls scan_cluster +
	// bnk_health to seed forge's view and smoke-test the link.
	PostRegisterScan bool
}

// RegisterResult is what forge register returns to the CLI for display.
type RegisterResult struct {
	Link *Link
	// LinkRefreshed is true when an existing link was kept but its Forge
	// URLs were rewritten to the ones this run used.
	LinkRefreshed bool
	ScanOutput    string // raw JSON from scan_cluster (empty if skipped)
	HealthCheck   string // raw JSON from bnk_health (empty if skipped)
	ForgeURL      string
}

// Register executes the azrbnkctl→forge handoff:
//
//  1. Reuse an existing link if one exists (idempotent — re-running
//     `forge register` should not duplicate projects/clusters).
//  2. Otherwise: create_project → create_cluster.
//  3. Optionally: scan_cluster + bnk_health smoke check.
//  4. Write forge_link.json so `forge status` / `forge unregister` work.
//
// If the workspace already has a link, Register keeps its project and cluster
// after a get_cluster sanity check and rewrites the link when the Forge URLs
// in use differ from the recorded ones (Forge moved, or the link was written
// from another host).
func Register(ctx context.Context, c *Client, req RegisterRequest) (RegisterResult, error) {
	if req.WorkspaceName == "" {
		return RegisterResult{}, errors.New("forge.Register: workspace name is required")
	}
	if req.WorkspaceDir == "" {
		return RegisterResult{}, errors.New("forge.Register: workspace dir is required")
	}
	if req.ClusterName == "" {
		return RegisterResult{}, errors.New("forge.Register: cluster name is required")
	}
	if len(req.Kubeconfig) == 0 {
		return RegisterResult{}, errors.New("forge.Register: kubeconfig is empty")
	}

	if req.ProjectName == "" {
		req.ProjectName = defaultProjectName(req.WorkspaceName)
	}
	if req.AWSProfile == "" {
		req.AWSProfile = os.Getenv("AWS_PROFILE")
	}

	// Idempotency: existing link wins — but only when fully registered.
	// A "pending" link means the forge handoff failed previously; treat it
	// the same as no link so we fall through to the create_project →
	// create_cluster path and complete the registration.
	if existing, err := ReadLink(req.WorkspaceDir); err == nil {
		if existing.IsRegistered() {
			if _, gerr := c.GetCluster(ctx, existing.ClusterID); gerr != nil {
				return RegisterResult{Link: existing, ForgeURL: c.URL()},
					fmt.Errorf("workspace already linked to forge cluster_id=%d but get_cluster failed: %w",
						existing.ClusterID, gerr)
			}
			out := RegisterResult{Link: existing, ForgeURL: c.URL()}
			if refreshLinkURLs(existing, c.URL(), req.ForgeRESTURL) {
				if err := WriteLink(req.WorkspaceDir, existing); err != nil {
					return out, fmt.Errorf("refresh link URLs: %w", err)
				}
				out.LinkRefreshed = true
			}
			return out, nil
		}
		// Link exists but is not registered (e.g. Status=="pending") — fall through.
	} else if !errors.Is(err, os.ErrNotExist) {
		return RegisterResult{}, fmt.Errorf("read existing link: %w", err)
	}

	// 1) resolve or create project
	env := req.Environment
	if env == "" {
		env = "dev"
	}
	cloudProv := req.CloudProvider
	if cloudProv == "" {
		cloudProv = "aws"
	}
	projType := req.ProjectType
	if projType == "" {
		projType = "cloud-" + cloudProv
	}

	var projectID int
	var projectName string
	createdProject := false

	proj, err := c.CreateProject(ctx, CreateProjectRequest{
		Name:                 req.ProjectName,
		ProjectType:          projType,
		CloudProvider:        cloudProv,
		Region:               req.Region,
		AWSProfile:           sendableAWSProfile(req.AWSProfile),
		Environment:          env,
		Description:          fmt.Sprintf("Created by bnkctl for workspace %q", req.WorkspaceName),
		CredentialTemplateID: req.CredentialTemplateID,
	})
	if err != nil {
		// If project already exists (conflict), find and reuse it.
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "already exists") || strings.Contains(errLower, "conflict") || strings.Contains(errLower, "409") {
			if plist, lerr := c.ListProjects(ctx); lerr == nil {
				for _, p := range plist.Projects {
					if strings.EqualFold(p.Name, req.ProjectName) {
						projectID = p.ID
						projectName = p.Name
						err = nil
						break
					}
				}
			}
		}
		if err != nil {
			return RegisterResult{}, fmt.Errorf("create_project: %w", err)
		}
	} else {
		projectID = proj.Project.ID
		projectName = proj.Project.Name
		createdProject = true
	}

	// 2) create cluster (or reuse/recreate if already exists in project)
	cluster, err := c.CreateCluster(ctx, CreateClusterRequest{
		ProjectID:     projectID,
		Name:          req.ClusterName,
		CloudProvider: cloudProv,
		Region:        req.Region,
	}, req.Kubeconfig)
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "already exists") || strings.Contains(errLower, "conflict") || strings.Contains(errLower, "409") {
			if clist, lerr := c.ListClusters(ctx, projectID); lerr == nil {
				for _, existing := range clist.Clusters {
					if strings.EqualFold(existing.Name, req.ClusterName) {
						// Delete stale cluster entry and recreate so Forge has the latest kubeconfig/CA
						_ = c.DeleteCluster(ctx, existing.ID)
						if recreated, rerr := c.CreateCluster(ctx, CreateClusterRequest{
							ProjectID:     projectID,
							Name:          req.ClusterName,
							CloudProvider: cloudProv,
							Region:        req.Region,
						}, req.Kubeconfig); rerr == nil {
							cluster = recreated
							err = nil
						} else {
							cluster.Cluster.ID = existing.ID
							cluster.Cluster.Name = existing.Name
							cluster.Cluster.ProjectID = projectID
							err = nil
						}
						break
					}
				}
			}
		}
		if err != nil {
			// Cluster create failed — try to roll back the project only if we created it.
			// Re-running shouldn't delete pre-existing shared projects.
			if createdProject {
				_ = c.DeleteProject(ctx, projectID, true)
			}
			return RegisterResult{}, fmt.Errorf("create_cluster: %w", err)
		}
	}

	link := &Link{
		ForgeMCPURL:  c.URL(),
		ForgeURL:     req.ForgeRESTURL,
		ProjectID:    projectID,
		ProjectName:  projectName,
		ClusterID:    cluster.Cluster.ID,
		ClusterName:  cluster.Cluster.Name,
		RegisteredAt: time.Now().UTC(),
		Workspace:    req.WorkspaceName,
	}
	if err := WriteLink(req.WorkspaceDir, link); err != nil {
		return RegisterResult{Link: link, ForgeURL: c.URL()},
			fmt.Errorf("registration succeeded but writing forge_link.json failed: %w", err)
	}

	out := RegisterResult{Link: link, ForgeURL: c.URL()}

	// 3) optional post-register scan + smoke check
	if req.PostRegisterScan {
		if s, err := c.ScanCluster(ctx, cluster.Cluster.ID); err != nil {
			out.ScanOutput = "<scan_cluster failed: " + err.Error() + ">"
		} else {
			out.ScanOutput = s
		}
		if h, err := c.BNKHealth(ctx, cluster.Cluster.ID); err != nil {
			out.HealthCheck = "<bnk_health failed: " + err.Error() + ">"
		} else {
			out.HealthCheck = h
		}
	}

	return out, nil
}

// sendableAWSProfile returns the profile string actually transmitted to
// forge. Callers use "-" to opt out of sending a profile (forge will then
// fall back to its backend-global env); any other empty / dash variant
// is treated as "no profile set" and returns "".
func sendableAWSProfile(profile string) string {
	if profile == "-" || profile == "" {
		return ""
	}
	return profile
}

// Unregister tears down the forge-side registration for a workspace.
// It always removes the local link file on success.
//
//	purge=false → delete cluster only (default; preserves the project for
//	  any other clusters or modules the operator may have added).
//	purge=true  → delete cluster AND project (force=true).
//
// Returns os.ErrNotExist if the workspace has never been registered.
func Unregister(ctx context.Context, c *Client, workspaceDir string, purge bool) error {
	link, err := ReadLink(workspaceDir)
	if err != nil {
		return err
	}
	return UnregisterLink(ctx, c, link, workspaceDir, purge)
}

// UnregisterLink tears down the forge-side registration using an already-resolved
// Link (e.g. synthesized from state.env when forge_link.json was lost).
func UnregisterLink(ctx context.Context, c *Client, link *Link, workspaceDir string, purge bool) error {
	if link == nil {
		return os.ErrNotExist
	}
	if link.ClusterID > 0 {
		if err := c.DeleteCluster(ctx, link.ClusterID); err != nil {
			// 404 is fine — the cluster was already gone forge-side.
			if !is404(err) {
				return fmt.Errorf("delete_cluster: %w", err)
			}
		}
	}
	if purge && link.ProjectID > 0 {
		if err := c.DeleteProject(ctx, link.ProjectID, true); err != nil && !is404(err) {
			return fmt.Errorf("delete_project: %w", err)
		}
	}
	if workspaceDir != "" {
		if err := RemoveLink(workspaceDir); err != nil {
			return fmt.Errorf("remove forge_link.json: %w", err)
		}
	}
	return nil
}

// UnregisterMCPByName discovers and deletes a Forge project (and its clusters) by cluster name
// via MCP when no local forge_link.json or FORGE_PROJECT_ID is present.
// Returns os.ErrNotExist when no matching project exists on the MCP server.
func UnregisterMCPByName(ctx context.Context, c *Client, clusterName string, purge bool) error {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return os.ErrNotExist
	}
	plist, err := c.ListProjects(ctx)
	if err != nil {
		return err
	}
	candidates := []string{
		clusterName,
		"awsbnkctl-" + clusterName,
		"bnkctl-" + clusterName,
		"azrbnkctl-" + clusterName,
		"gkebnkctl-" + clusterName,
	}
	matchedAny := false
	for _, p := range plist.Projects {
		if p.ID <= 0 {
			continue
		}
		matched := false
		for _, cand := range candidates {
			if strings.EqualFold(p.Name, cand) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		matchedAny = true
		_ = purge
		if dErr := c.DeleteProject(ctx, p.ID, true); dErr != nil && !is404(dErr) {
			return fmt.Errorf("delete_project: %w", dErr)
		}
	}
	if !matchedAny {
		return os.ErrNotExist
	}
	return nil
}

// StatusResult is what `forge status` returns.
type StatusResult struct {
	Link          *Link
	Reachable     bool
	ForgeVersion  string
	ClusterDetail string // raw get_cluster JSON
	Err           error
}

// Status returns the current state of the workspace's forge link.
// Returns os.ErrNotExist if there is no link.
func Status(ctx context.Context, c *Client, workspaceDir string) (StatusResult, error) {
	link, err := ReadLink(workspaceDir)
	if err != nil {
		return StatusResult{}, err
	}
	out := StatusResult{Link: link}

	if v, err := c.SystemVersion(ctx); err == nil {
		out.Reachable = true
		out.ForgeVersion = v
	} else {
		out.Err = fmt.Errorf("forge unreachable at %s: %w", c.URL(), err)
		return out, nil
	}
	if d, err := c.GetCluster(ctx, link.ClusterID); err == nil {
		out.ClusterDetail = d
	} else {
		out.Err = fmt.Errorf("get_cluster(%d): %w", link.ClusterID, err)
	}
	return out, nil
}

// Is404 is a heuristic for the not-found case — MCP tool errors are
// flattened to strings, so we sniff for the conventional markers.
func Is404(err error) bool {
	if err == nil {
		return false
	}
	var he *restHTTPErr
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusNotFound
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not_found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "http 404") ||
		strings.Contains(s, "\"status_code\": 404")
}

func is404(err error) bool {
	return Is404(err)
}

// refreshLinkURLs points l at the Forge endpoints in use and reports whether
// anything changed. An empty restURL leaves the recorded REST URL alone.
func refreshLinkURLs(l *Link, mcpURL, restURL string) bool {
	changed := false
	if mcpURL != "" && l.ForgeMCPURL != mcpURL {
		l.ForgeMCPURL = mcpURL
		changed = true
	}
	if restURL != "" && l.ForgeURL != restURL {
		l.ForgeURL = restURL
		changed = true
	}
	return changed
}
