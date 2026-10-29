package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestForgeREST_FullLifecycle(t *testing.T) {
	projects := make(map[string]int)
	clusters := make(map[string]int)
	projectList := []map[string]any{}
	clusterList := make(map[int][]map[string]any)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/auth/login" && r.Method == http.MethodPost:
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["username"] == "admin" && req["password"] == "changeme" {
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "mock-jwt-token"})
			} else {
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
			}
		case path == "/api/projects" && r.Method == http.MethodPost:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			name := req["name"].(string)
			if _, exists := projects[name]; exists {
				http.Error(w, "project already exists", http.StatusConflict)
				return
			}
			newID := len(projects) + 1
			projects[name] = newID
			p := map[string]any{"id": newID, "name": name, "project_type": req["project_type"], "cloud_provider": req["cloud_provider"]}
			projectList = append(projectList, p)
			_ = json.NewEncoder(w).Encode(p)
		case path == "/api/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(projectList)
		case stringsHasPrefix(path, "/api/projects/") && stringsHasSuffix(path, "/k8s/clusters") && r.Method == http.MethodPost:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			name := req["name"].(string)
			if _, exists := clusters[name]; exists {
				http.Error(w, "cluster already exists", http.StatusConflict)
				return
			}
			newID := len(clusters) + 100
			clusters[name] = newID
			c := map[string]any{"id": newID, "name": name, "cloud_provider": req["cloud_provider"]}
			_ = json.NewEncoder(w).Encode(c)
		case stringsHasPrefix(path, "/api/projects/") && stringsHasSuffix(path, "/k8s/clusters") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 100, "name": "test-cluster"}})
		case stringsHasPrefix(path, "/api/projects/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	wsDir := filepath.Join(tmpDir, ".azrbnkctl", "test-cluster")
	_ = os.MkdirAll(wsDir, 0o750)

	req := forge.RegisterRequest{
		WorkspaceName: "test-cluster",
		WorkspaceDir:  wsDir,
		ProjectName:   "azrbnkctl-test-cluster",
		ClusterName:   "test-cluster",
		Region:        "eastus2",
		Kubeconfig:    []byte("test-kubeconfig"),
	}
	creds := forge.RestCreds{Username: "admin", Password: "changeme"}

	t.Run("successful register via REST", func(t *testing.T) {
		res, err := forge.RegisterREST(context.Background(), srv.URL, req, creds)
		if err != nil {
			t.Fatalf("RegisterREST: %v", err)
		}
		if res.Link == nil || res.Link.ProjectID != 1 || res.Link.ClusterID != 100 {
			t.Fatalf("unexpected link result: %+v", res.Link)
		}
		if !res.Link.IsRegistered() {
			t.Errorf("expected IsRegistered = true")
		}

		// Verify on-disk link
		diskLink, err := forge.ReadLink(wsDir)
		if err != nil {
			t.Fatalf("ReadLink: %v", err)
		}
		if diskLink.ProjectID != 1 || diskLink.ClusterID != 100 {
			t.Errorf("unexpected disk link: %+v", diskLink)
		}
	})

	t.Run("unregister via REST", func(t *testing.T) {
		link, _ := forge.ReadLink(wsDir)
		if err := forge.UnregisterREST(context.Background(), srv.URL, link, creds); err != nil {
			t.Fatalf("UnregisterREST: %v", err)
		}
	})

	t.Run("default project name and cloud labels for AWS", func(t *testing.T) {
		awsDir := filepath.Join(tmpDir, ".awsbnkctl", "aws-cluster")
		_ = os.MkdirAll(awsDir, 0o750)
		awsReq := forge.RegisterRequest{
			WorkspaceName: "aws-cluster",
			WorkspaceDir:  awsDir,
			ClusterName:   "aws-cluster",
			Region:        "ap-southeast-2",
			CloudProvider: "aws",
			Kubeconfig:    []byte("test-kubeconfig"),
		}
		res, err := forge.RegisterREST(context.Background(), srv.URL, awsReq, creds)
		if err != nil {
			t.Fatalf("RegisterREST AWS: %v", err)
		}
		if res.Link == nil || res.Link.ProjectName != "awsbnkctl-aws-cluster" {
			t.Fatalf("expected ProjectName awsbnkctl-aws-cluster, got %+v", res.Link)
		}
		if _, ok := projects["awsbnkctl-aws-cluster"]; !ok {
			t.Errorf("expected server to receive project name awsbnkctl-aws-cluster, got projects=%v", projects)
		}
	})

	_ = clusterList
}

func TestForgeREST_UnregisterRESTByName_WrappedResponse(t *testing.T) {
	deletedAgents := 0
	deletedTargets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/auth/login" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "mock-jwt-token"})
		case path == "/api/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects": []map[string]any{
					{"id": 42, "name": "azrbnkctl-poc-dual-interface"},
				},
			})
		case path == "/api/projects/42/k8s/clusters" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"clusters": []map[string]any{
					{"id": 99, "name": "poc-dual-interface", "project_id": 42},
				},
			})
		case path == "/api/benchmarks/targets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7, "cluster_id": 99},
			})
		case path == "/api/benchmarks/targets/7/proxies" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case path == "/api/benchmarks/targets/7" && r.Method == http.MethodDelete:
			deletedTargets++
			w.WriteHeader(http.StatusOK)
		case path == "/api/benchmarks/agents" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 11, "name": "awsbnkctl-jumphost-i-0d54ca729219a0c7f"},
			})
		case path == "/api/benchmarks/agents/11" && r.Method == http.MethodDelete:
			deletedAgents++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	creds := forge.RestCreds{Username: "admin", Password: "changeme"}
	if err := forge.UnregisterRESTByName(context.Background(), srv.URL, "poc-dual-interface", creds); err != nil {
		t.Fatalf("UnregisterRESTByName with wrapped JSON response failed: %v", err)
	}
	if deletedTargets != 1 || deletedAgents != 1 {
		t.Fatalf("expected benchmark target and agent to be deleted before cluster purge, got deletedTargets=%d deletedAgents=%d", deletedTargets, deletedAgents)
	}
}

func TestUnregisterMCPByName_ReturnsErrNotExistWhenNotPurged(t *testing.T) {
	err := forge.UnregisterMCPByName(context.Background(), forge.NewClient("http://127.0.0.1:1"), "agent-core-singapore", false)
	if err == nil {
		t.Fatal("expected error when purge=false, got nil")
	}
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
