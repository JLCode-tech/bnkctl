package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestForgeCleanup_DeleteArtifacts(t *testing.T) {
	targets := []map[string]any{
		{"id": 1, "name": "target-1", "cluster_id": 55},
		{"id": 2, "name": "target-2", "cluster_id": 99},
	}
	proxies := []map[string]any{
		{"id": 10, "target_id": 1, "proxy_type": "f5-bnk"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		case path == "/api/benchmarks/targets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(targets)
		case path == "/api/benchmarks/targets/1/proxies" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(proxies)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	creds := forge.RestCreds{Username: "admin", Password: "changeme"}

	err := forge.DeleteClusterBenchmarkArtifacts(ctx, srv.URL, creds, 55)
	if err != nil {
		t.Fatalf("DeleteClusterBenchmarkArtifacts: %v", err)
	}
}
