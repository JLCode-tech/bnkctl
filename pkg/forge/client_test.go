package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/forge/mcp"
)

func TestForgeClient_MCPWrappers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]

		if method == "initialize" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion,
					"serverInfo":      map[string]string{"name": "test-forge", "version": "1.0"},
				},
			})
			return
		}
		if method == "notifications/initialized" {
			w.WriteHeader(http.StatusOK)
			return
		}

		params, _ := req["params"].(map[string]any)
		toolName, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)

		var resText string
		switch toolName {
		case "system_health":
			resText = `{"status":"ok"}`
		case "system_version":
			resText = `{"version":"2.10.0"}`
		case "create_project":
			resText = `{"project_id":42,"name":"` + args["name"].(string) + `","success":true}`
		case "create_cluster":
			resText = `{"cluster_id":101,"name":"` + args["name"].(string) + `","project_id":42,"success":true}`
		case "get_cluster":
			resText = `{"id":101,"name":"test-aks"}`
		case "scan_cluster":
			resText = `{"discovered_nodes":3}`
		case "bnk_health":
			resText = `{"bnk_status":"healthy"}`
		case "delete_cluster":
			resText = `{"deleted":true}`
		case "delete_project":
			resText = `{"deleted":true}`
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error":   map[string]any{"code": -32601, "message": "unknown tool"},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"content": []map[string]string{
					{"type": "text", "text": resText},
				},
			},
		})
	}))
	defer srv.Close()

	client := forge.NewClient(srv.URL)
	ctx := context.Background()

	t.Run("SystemHealth", func(t *testing.T) {
		h, err := client.SystemHealth(ctx)
		if err != nil {
			t.Fatalf("SystemHealth: %v", err)
		}
		if h != `{"status":"ok"}` {
			t.Errorf("got %q", h)
		}
	})

	t.Run("CreateProject", func(t *testing.T) {
		resp, err := client.CreateProject(ctx, forge.CreateProjectRequest{
			Name: "test-proj",
		})
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if resp.Project.ID != 42 || resp.Project.Name != "test-proj" {
			t.Errorf("unexpected project: %+v", resp)
		}
	})

	t.Run("CreateCluster", func(t *testing.T) {
		resp, err := client.CreateCluster(ctx, forge.CreateClusterRequest{
			ProjectID: 42,
			Name:      "test-aks",
			Region:    "eastus2",
		}, []byte("dummy-kubeconfig"))
		if err != nil {
			t.Fatalf("CreateCluster: %v", err)
		}
		if resp.Cluster.ID != 101 || resp.Cluster.Name != "test-aks" {
			t.Errorf("unexpected cluster: %+v", resp)
		}
	})

	t.Run("GetCluster", func(t *testing.T) {
		res, err := client.GetCluster(ctx, 101)
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		if res != `{"id":101,"name":"test-aks"}` {
			t.Errorf("got %q", res)
		}
	})

	t.Run("ScanCluster", func(t *testing.T) {
		res, err := client.ScanCluster(ctx, 101)
		if err != nil {
			t.Fatalf("ScanCluster: %v", err)
		}
		if res != `{"discovered_nodes":3}` {
			t.Errorf("got %q", res)
		}
	})

	t.Run("BNKHealth", func(t *testing.T) {
		res, err := client.BNKHealth(ctx, 101)
		if err != nil {
			t.Fatalf("BNKHealth: %v", err)
		}
		if res != `{"bnk_status":"healthy"}` {
			t.Errorf("got %q", res)
		}
	})

	t.Run("DeleteCluster", func(t *testing.T) {
		if err := client.DeleteCluster(ctx, 101); err != nil {
			t.Fatalf("DeleteCluster: %v", err)
		}
	})

	t.Run("DeleteProject", func(t *testing.T) {
		if err := client.DeleteProject(ctx, 42, true); err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}
	})
}
