package aws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// ForgeRegisterFunc is the seam tests replace so Phase 09 doesn't make real HTTP calls.
var ForgeRegisterFunc = func(ctx context.Context, url string, req forge.RegisterRequest) (forge.RegisterResult, error) {
	c := forge.NewClient(url)
	return forge.Register(ctx, c, req)
}

// ForgeUnregisterFunc is the seam for Phase09ForgeRegisterDown.
var ForgeUnregisterFunc = func(ctx context.Context, url, workspaceDir string, purge bool) error {
	c := forge.NewClient(url)
	return forge.Unregister(ctx, c, workspaceDir, purge)
}

// Phase09ForgeRegister hands the active EKS control plane off to F5 Forge immediately after Phase 08.
func Phase09ForgeRegister(ctx context.Context, cl *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 09 forge register: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 09 forge register: state is nil")
	}
	if cl.Forge == nil || !cl.Forge.Enabled {
		return nil
	}
	if dryRun {
		return nil
	}

	wsDir := filepath.Dir(st.Path())
	wsName := filepath.Base(wsDir)
	url := cl.Forge.ResolveURL()
	if cl.Forge.MCPURL != "" {
		url = cl.Forge.MCPURL
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}
	endpoint := st.Get("EKS_CLUSTER_ENDPOINT")
	caData := st.Get("EKS_CLUSTER_CA")
	if endpoint == "" || caData == "" {
		return writeForgePending(wsDir, wsName, url, clusterName,
			fmt.Errorf("EKS_CLUSTER_ENDPOINT / EKS_CLUSTER_CA missing from state"))
	}

	raw, err := RenderKubeconfig(KubeconfigTemplateData{
		ClusterName: clusterName,
		Endpoint:    endpoint,
		CAData:      caData,
		Region:      cl.Metadata.Region,
	})
	if err != nil {
		return writeForgePending(wsDir, wsName, url, clusterName, err)
	}

	k8sVersion := ""
	if cl.ClusterSpec != nil {
		k8sVersion = cl.ClusterSpec.KubernetesVersion
	}
	res, err := ForgeRegisterFunc(ctx, url, forge.RegisterRequest{
		ForgeRESTURL:         cl.Forge.ResolveURL(),
		WorkspaceName:        wsName,
		WorkspaceDir:         wsDir,
		ProjectName:          cl.Forge.ProjectName,
		ClusterName:          clusterName,
		Region:               cl.Metadata.Region,
		Kubeconfig:           raw,
		AWSProfile:           cl.Metadata.Profile,
		Environment:          cl.Forge.ResolveEnvironment(),
		ProjectType:          cl.Forge.ResolveProjectType(),
		CloudProvider:        cl.Forge.ResolveCloudProvider(),
		CredentialTemplateID: cl.Forge.CredentialTemplateID,
		PostRegisterScan:     false,
	})
	_ = k8sVersion
	if err != nil {
		return writeForgePending(wsDir, wsName, url, clusterName, err)
	}

	if res.Link != nil {
		st.Set("FORGE_PROJECT_ID", strconv.Itoa(res.Link.ProjectID))
		st.Set("FORGE_CLUSTER_ID", strconv.Itoa(res.Link.ClusterID))
		st.Set("FORGE_MCP_URL", res.Link.ForgeMCPURL)
		return st.Save()
	}
	return nil
}

func writeForgePending(wsDir, wsName, url, clusterName string, cause error) error {
	fmt.Printf("warning: phase 09 forge register failed (recording pending link): %v\n", cause)
	link := &forge.Link{
		ForgeMCPURL:  url,
		ClusterName:  clusterName,
		RegisteredAt: time.Now().UTC(),
		Workspace:    wsName,
		Status:       "pending",
	}
	_ = forge.WriteLink(wsDir, link)
	return nil
}

// Phase09ForgeRegisterDown unregisters the workspace from Forge on teardown.
func Phase09ForgeRegisterDown(ctx context.Context, cl *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 09 forge register down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 09 forge register down: state is nil")
	}
	if dryRun {
		return nil
	}

	wsDir := st.Dir()
	if wsDir == "" {
		wsDir = filepath.Dir(st.Path())
	}
	if wsDir == "" || wsDir == "." {
		wsDir = cl.StateDir()
	}
	forgeSpec := cl.Forge
	if forgeSpec == nil {
		forgeSpec = &intent.ForgeSpec{}
	}
	restURL := forgeSpec.ResolveURL()
	mcpURL := st.Get("FORGE_MCP_URL")
	if mcpURL == "" && forgeSpec.MCPURL != "" {
		mcpURL = forgeSpec.MCPURL
	}
	username := forgeSpec.ResolveUsername()
	password, _ := forgeSpec.ResolvePassword()
	creds := forge.RestCreds{Username: username, Password: password}
	for _, k := range []string{"BNKCTL_FORGE_MCP_URL", "FORGE_MCP_URL", "AWSBNKCTL_FORGE_MCP_URL"} {
		if v := os.Getenv(k); v != "" {
			mcpURL = v
			break
		}
	}

	purge := true
	if cl.Forge != nil && cl.Forge.ProjectName != "" {
		purge = false
	}

	link, err := forge.ReadLink(wsDir)
	if err != nil || !link.IsRegistered() {
		projID, _ := strconv.Atoi(st.Get("FORGE_PROJECT_ID"))
		clusID, _ := strconv.Atoi(st.Get("FORGE_CLUSTER_ID"))
		if projID > 0 || clusID > 0 {
			link = &forge.Link{
				ProjectID:   projID,
				ClusterID:   clusID,
				ClusterName: cl.Metadata.Name,
				Status:      "registered",
			}
		} else {
			_ = forge.RemoveLink(wsDir)
			if cl.Metadata.Name != "" && ((cl.Forge != nil && cl.Forge.Enabled) || st.Get("FORGE_REGISTERED") == "true" || err == nil) {
				url := mcpURL
				if url == "" {
					url = forge.DefaultMCPURL
				}
				_ = forge.UnregisterMCPByName(ctx, forge.NewClient(url), cl.Metadata.Name, purge)
				_ = forge.UnregisterRESTByName(ctx, restURL, cl.Metadata.Name, creds)
			}
			clearForgeStateKeys(st)
			return st.Save()
		}
	}

	if restURL == "" && link != nil && link.ForgeURL != "" {
		restURL = link.ForgeURL
	}

	if link.ClusterID > 0 {
		_ = forge.DeleteAllClusterBenchmarkArtifacts(ctx, restURL, creds, link.ClusterID)
	}

	url := mcpURL
	if url == "" {
		url = link.ForgeMCPURL
	}
	if url == "" {
		url = forge.DefaultMCPURL
	}

	uerr := ForgeUnregisterFunc(ctx, url, wsDir, purge)
	if errors.Is(uerr, os.ErrNotExist) && link != nil {
		uerr = forge.UnregisterLink(ctx, forge.NewClient(url), link, wsDir, purge)
	}
	if uerr != nil && url != forge.DefaultMCPURL && mcpURL == "" {
		if fallbackErr := ForgeUnregisterFunc(ctx, forge.DefaultMCPURL, wsDir, purge); fallbackErr == nil {
			uerr = nil
		}
	}
	if uerr != nil {
		var mcpNameErr error
		if cl.Metadata.Name != "" {
			mcpNameErr = forge.UnregisterMCPByName(ctx, forge.NewClient(url), cl.Metadata.Name, purge)
		}
		if restErr := forge.UnregisterREST(ctx, restURL, link, creds); restErr == nil {
			_ = forge.RemoveLink(wsDir)
			uerr = nil
		} else if cl.Metadata.Name != "" {
			if nameErr := forge.UnregisterRESTByName(ctx, restURL, cl.Metadata.Name, creds); nameErr == nil {
				_ = forge.RemoveLink(wsDir)
				uerr = nil
			} else if mcpNameErr == nil {
				_ = forge.RemoveLink(wsDir)
				uerr = nil
			} else {
				fmt.Printf("warning: phase 09 forge unregister failed (continuing teardown): mcp=%v, rest=%v\n", uerr, restErr)
			}
		} else if mcpNameErr == nil {
			_ = forge.RemoveLink(wsDir)
			uerr = nil
		} else {
			fmt.Printf("warning: phase 09 forge unregister failed (continuing teardown): mcp=%v, rest=%v\n", uerr, restErr)
		}
	}

	_ = forge.RemoveLink(wsDir)
	clearForgeStateKeys(st)
	return st.Save()
}

func clearForgeStateKeys(st *state.State) {
	for _, k := range []string{
		"FORGE_PROJECT_ID",
		"FORGE_CLUSTER_ID",
		"FORGE_MCP_URL",
		"FORGE_REGISTERED",
		"FORGE_STATUS",
		"FORGE_LINK_PATH",
		"FORGE_REGISTERED_AT",
		"FORGE_SYNCED",
	} {
		st.Delete(k)
	}
}
