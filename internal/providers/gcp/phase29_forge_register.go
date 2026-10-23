package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

var forgeRegisterBackoffs = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// Phase29ForgeRegister auto-registers the provisioned GKE cluster with Forge
// when cl.Forge != nil && cl.Forge.Enabled.
//
// Soft-fail policy: If Forge is unreachable after retries, this phase logs a
// warning, writes a pending status to forge_link.json, and returns nil so the
// overall cluster provisioning is never aborted.
func Phase29ForgeRegister(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	_ = clients

	if cl.Forge == nil || !cl.Forge.Enabled {
		slog.Debug("phase 29: forge integration disabled — skipping")
		return nil
	}

	wsName := cl.Metadata.Name
	wsDir := filepath.Dir(st.KubeconfigPath())
	mcpURL := cl.Forge.MCPURL
	restURL := cl.Forge.URL
	if restURL == "" {
		restURL = forge.DefaultForgeRESTURL
	}

	slog.Info("phase 29: forge registration",
		"cluster", wsName,
		"project", cl.Metadata.Project,
		"zone", cl.Metadata.Zone,
		"mcp_url", mcpURL,
		"rest_url", restURL,
		"dry_run", dryRun,
	)

	if dryRun {
		slog.Info("phase 29 [dry-run]: would register cluster with Forge",
			"workspace", wsName,
			"project_name", cl.Forge.ProjectName,
			"environment", cl.Forge.Environment,
		)
		return nil
	}

	kubePath := st.Get("KUBECONFIG_PATH")
	if kubePath == "" {
		kubePath = st.Get("KUBECONFIG")
	}
	if kubePath == "" {
		kubePath = st.KubeconfigPath()
	}
	kubeBytes, err := os.ReadFile(kubePath) // #nosec G304 -- path is managed workspace kubeconfig
	if err != nil {
		slog.Warn("phase 29: could not read workspace kubeconfig for Forge registration (skipping)",
			"path", kubePath, "err", err)
		return nil
	}

	fc := forge.NewClient(mcpURL)
	req := forge.RegisterRequest{
		ForgeRESTURL:         restURL,
		WorkspaceName:        wsName,
		WorkspaceDir:         wsDir,
		ProjectName:          cl.Forge.ProjectName,
		ClusterName:          cl.Metadata.Name,
		Region:               cl.Region,
		Kubeconfig:           kubeBytes,
		Environment:          cl.Forge.Environment,
		ProjectType:          "cloud-gcp",
		CloudProvider:        "gcp",
		CredentialTemplateID: cl.Forge.CredentialTemplateID,
		PostRegisterScan:     true,
	}
	if req.ProjectName == "" {
		req.ProjectName = "gkebnkctl-" + wsName
	}

	var res forge.RegisterResult
	var lastErr error
	for attempt := 0; attempt <= len(forgeRegisterBackoffs); attempt++ {
		if attempt > 0 {
			backoff := forgeRegisterBackoffs[attempt-1]
			slog.Info("phase 29: retrying Forge registration",
				"attempt", attempt,
				"backoff", backoff.String(),
				"last_err", lastErr,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		res, lastErr = forge.Register(ctx, fc, req)
		if lastErr == nil {
			break
		}
	}

	if lastErr != nil {
		slog.Warn("phase 29: Forge registration failed after retries (soft-fail); run 'bnkctl forge register' once Forge is reachable",
			"err", lastErr,
		)
		pendingLink := &forge.Link{
			ForgeURL:     restURL,
			ForgeMCPURL:  fc.URL(),
			ProjectName:  req.ProjectName,
			ClusterName:  cl.Metadata.Name,
			RegisteredAt: time.Now().UTC(),
			Workspace:    wsName,
			Status:       "pending",
		}
		_ = forge.WriteLink(wsDir, pendingLink)
		return nil
	}

	st.Set("FORGE_REGISTERED", "true")
	st.Set("FORGE_PROJECT_ID", fmt.Sprintf("%d", res.Link.ProjectID))
	st.Set("FORGE_CLUSTER_ID", fmt.Sprintf("%d", res.Link.ClusterID))
	_ = st.Save()

	slog.Info("phase 29: registered cluster with Forge",
		"project_id", res.Link.ProjectID,
		"cluster_id", res.Link.ClusterID,
		"forge_url", res.ForgeURL,
	)

	return nil
}

// Phase29ForgeRegisterDown unregisters the cluster from Forge during `down`
// if a forge_link.json exists in the workspace. Soft-fails on error so IaaS
// teardown is never blocked by an unreachable Forge server.
func Phase29ForgeRegisterDown(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	_ = clients

	clearForgeKeys := func() {
		if st == nil {
			return
		}
		for _, k := range []string{"FORGE_REGISTERED", "FORGE_PROJECT_ID", "FORGE_CLUSTER_ID", "FORGE_SYNCED"} {
			st.Delete(k)
		}
		_ = st.Save()
	}

	wsDir := st.Dir()
	if wsDir == "" {
		wsDir = filepath.Dir(st.KubeconfigPath())
	}
	link, err := forge.ReadLink(wsDir)
	if err != nil {
		clearForgeKeys()
		if os.IsNotExist(err) {
			return nil
		}
		slog.Warn("down: could not read forge_link.json (continuing teardown)", "err", err)
		return nil
	}

	if dryRun {
		slog.Info("down [dry-run]: would unregister cluster from Forge",
			"cluster_id", link.ClusterID,
			"project_id", link.ProjectID,
		)
		return nil
	}

	if !link.IsRegistered() {
		_ = forge.RemoveLink(wsDir)
		clearForgeKeys()
		return nil
	}

	mcpURL := link.ForgeMCPURL
	if cl != nil && cl.Forge != nil && cl.Forge.MCPURL != "" {
		mcpURL = cl.Forge.MCPURL
	}
	fc := forge.NewClient(mcpURL)
	if err := forge.Unregister(ctx, fc, wsDir, true); err != nil {
		_ = forge.RemoveLink(wsDir)
		clearForgeKeys()
		slog.Warn("down: Forge unregister failed (removed stale local link; continuing IaaS teardown)", "err", err)
		return nil
	}
	clearForgeKeys()

	slog.Info("down: unregistered cluster from Forge", "cluster_id", link.ClusterID)
	return nil
}
