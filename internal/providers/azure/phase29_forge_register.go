package azure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase29ForgeRegister registers the AKS cluster with BNK-Forge during bnkctl up.
func Phase29ForgeRegister(ctx context.Context, cl *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if cl == nil || cl.Forge == nil || !cl.Forge.Enabled {
		return nil
	}
	ensureAzureDefaults(cl)

	restURL := cl.Forge.ResolveURL()
	mcpURL := cl.Forge.MCPURL
	if mcpURL == "" {
		mcpURL = forge.DefaultMCPURL
	}

	fmt.Fprintf(os.Stderr, "[phase 29] forge-register: registering cluster %s with Forge\n", cl.Metadata.Name)

	if dryRun {
		fmt.Fprintf(os.Stderr, "[phase 29] [dry-run] would register cluster %s with Forge (%s / %s)\n",
			cl.Metadata.Name, mcpURL, restURL)
		return nil
	}

	var workspaceDir string
	if st != nil {
		workspaceDir = st.Dir()
	}
	if workspaceDir == "" {
		workspaceDir = cl.StateDir()
	}

	if existing, err := forge.ReadLink(workspaceDir); err == nil && existing.IsRegistered() {
		fmt.Fprintf(os.Stderr, "[phase 29] ✓ already registered with forge: project=%s (id=%d) cluster=%s (id=%d)\n",
			existing.ProjectName, existing.ProjectID, existing.ClusterName, existing.ClusterID)
		if st != nil {
			st.Set("FORGE_REGISTERED", "true")
		}
		return nil
	}

	var kubeconfigPath string
	if st != nil {
		kubeconfigPath = st.Get("KUBECONFIG_PATH")
		if kubeconfigPath == "" {
			kubeconfigPath = st.KubeconfigPath()
		}
	}
	kubeconfigBytes, err := os.ReadFile(kubeconfigPath) // #nosec G304 -- workspace managed path
	if err != nil {
		fmt.Fprintf(os.Stderr, "[phase 29] warning: reading kubeconfig from %s: %v\n", kubeconfigPath, err)
		return nil
	}

	projectType := cl.Forge.ProjectType
	if projectType == "" {
		projectType = "cloud-azure"
	}
	cloudProvider := cl.Forge.CloudProvider
	if cloudProvider == "" {
		cloudProvider = "azure"
	}

	req := forge.RegisterRequest{
		ForgeRESTURL:         restURL,
		WorkspaceName:        cl.Metadata.Name,
		WorkspaceDir:         workspaceDir,
		ProjectName:          cl.Forge.ResolveProjectName(cl.Metadata.Name),
		ClusterName:          cl.Metadata.Name,
		Region:               cl.Metadata.Location,
		Kubeconfig:           kubeconfigBytes,
		Environment:          cl.Forge.ResolveEnvironment(),
		ProjectType:          projectType,
		CloudProvider:        cloudProvider,
		CredentialTemplateID: cl.Forge.CredentialTemplateID,
		PostRegisterScan:     true,
	}

	username := cl.Forge.ResolveUsername()
	password, usingDefault := cl.Forge.ResolvePassword()
	if usingDefault {
		fmt.Fprintf(os.Stderr, "[phase 29] warning: using default Forge password ('changeme'); configure forge.password or set AZRBNKCTL_FORGE_PASSWORD\n")
	}
	creds := forge.RestCreds{
		Username: username,
		Password: password,
	}

	backoffs := []time.Duration{1 * time.Second, 3 * time.Second, 9 * time.Second, 27 * time.Second}
	var lastErr error

	for attempt := 0; attempt < len(backoffs); attempt++ {
		if attempt > 0 {
			time.Sleep(backoffs[attempt-1])
		}

		mcpClient := forge.NewClient(mcpURL)
		res, mcpErr := forge.Register(ctx, mcpClient, req)
		if mcpErr == nil {
			fmt.Fprintf(os.Stderr, "[phase 29] ✓ registered with forge via MCP: project=%s (id=%d) cluster=%s (id=%d)\n",
				res.Link.ProjectName, res.Link.ProjectID, res.Link.ClusterName, res.Link.ClusterID)
			if st != nil {
				st.Set("FORGE_REGISTERED", "true")
			}
			return nil
		}

		lastErr = mcpErr
		restRes, restErr := forge.RegisterREST(ctx, restURL, req, creds)
		if restErr == nil {
			fmt.Fprintf(os.Stderr, "[phase 29] ✓ registered with forge via REST: project=%s (id=%d) cluster=%s (id=%d)\n",
				restRes.Link.ProjectName, restRes.Link.ProjectID, restRes.Link.ClusterName, restRes.Link.ClusterID)
			if st != nil {
				st.Set("FORGE_REGISTERED", "true")
			}
			return nil
		}
		lastErr = fmt.Errorf("mcp: %v; rest: %w", mcpErr, restErr)
	}

	pendingLink := &forge.Link{
		ForgeURL:     restURL,
		ForgeMCPURL:  mcpURL,
		Workspace:    cl.Metadata.Name,
		ProjectName:  cl.Forge.ResolveProjectName(cl.Metadata.Name),
		ClusterName:  cl.Metadata.Name,
		RegisteredAt: time.Now().UTC(),
		Status:       "pending",
	}
	_ = forge.WriteLink(workspaceDir, pendingLink)

	fmt.Fprintf(os.Stderr, "[phase 29] ⚠️ could not register with forge: %v (marked pending; run 'bnkctl forge register' when forge is online)\n", lastErr)
	return nil
}

// Phase29ForgeRegisterDown unregisters the cluster from BNK-Forge during teardown.
func Phase29ForgeRegisterDown(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if cl == nil {
		return nil
	}
	var workspaceDir string
	if st != nil {
		workspaceDir = st.Dir()
	}
	if workspaceDir == "" {
		workspaceDir = cl.StateDir()
	}
	link, linkErr := forge.ReadLink(workspaceDir)
	hasLink := linkErr == nil && link != nil
	forgeEnabled := cl.Forge != nil && cl.Forge.Enabled
	forgeRegisteredState := st != nil && st.Get("FORGE_REGISTERED") == "true"

	if !hasLink && !forgeEnabled && !forgeRegisteredState {
		return nil
	}

	restURL := cl.Forge.ResolveURL()
	mcpURL := forge.DefaultMCPURL
	if cl.Forge != nil && cl.Forge.MCPURL != "" {
		mcpURL = cl.Forge.MCPURL
	}
	username := cl.Forge.ResolveUsername()
	password, _ := cl.Forge.ResolvePassword()
	creds := forge.RestCreds{Username: username, Password: password}

	fmt.Fprintf(os.Stderr, "[down] forge: unregistering cluster %s\n", cl.Metadata.Name)

	if dryRun {
		fmt.Fprintf(os.Stderr, "[down] [dry-run] would unregister cluster %s from Forge (%s)\n", cl.Metadata.Name, restURL)
		return nil
	}

	if hasLink {
		if !link.IsRegistered() {
			_ = forge.RemoveLink(workspaceDir)
			if st != nil {
				st.Delete("FORGE_REGISTERED")
				_ = st.Save()
			}
			return nil
		}

		if link.ClusterID > 0 {
			_ = forge.DeleteAllClusterBenchmarkArtifacts(ctx, restURL, creds, link.ClusterID)
		}

		mcpClient := forge.NewClient(mcpURL)
		unregErr := forge.Unregister(ctx, mcpClient, workspaceDir, true)
		if unregErr != nil {
			if restErr := forge.UnregisterREST(ctx, restURL, link, creds); restErr == nil {
				_ = forge.RemoveLink(workspaceDir)
				unregErr = nil
			} else {
				unregErr = restErr
			}
		}
		if unregErr != nil {
			_ = forge.RemoveLink(workspaceDir)
			if st != nil {
				st.Delete("FORGE_REGISTERED")
				_ = st.Save()
			}
			fmt.Fprintf(os.Stderr, "[down] warning: forge unregister failed (removed stale local link; run 'bnkctl forge unregister --purge' when Forge is reachable): %v\n", unregErr)
			return nil
		}
		if st != nil {
			st.Delete("FORGE_REGISTERED")
			_ = st.Save()
		}
		fmt.Fprintf(os.Stderr, "[down] ✓ cluster %s unregistered from forge\n", cl.Metadata.Name)
		return nil
	}

	if unregErr := forge.UnregisterRESTByName(ctx, restURL, cl.Metadata.Name, creds); unregErr != nil {
		if !errors.Is(unregErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "[down] forge unregister by name: %v\n", unregErr)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[down] ✓ cluster %s purged from forge by name\n", cl.Metadata.Name)
	}
	if st != nil {
		st.Delete("FORGE_REGISTERED")
		_ = st.Save()
	}

	return nil
}
