package forge_test

// proxy_test.go — unit tests for DiscoverProxies / ListProxyDeployments /
// ResolveProxyDeploymentID using the BenchmarkHTTPDoFn transport seam.
//
// All tests use the same mock-transport seam (BenchmarkHTTPDoFn) so no second
// HTTP client or test seam is introduced. Tests cover:
//   - discover success (discovered_count > 0)
//   - list + resolve by type (id returned correctly)
//   - resolve-miss returns 0
//   - forge error case is non-fatal (returns err, caller logs)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

// ---------------------------------------------------------------------------
// proxyServer — minimal httptest handler for proxy discovery + list paths.
// ---------------------------------------------------------------------------

type proxyServer struct {
	// deployments to return from GET /proxies. nil = empty array.
	deployments []forge.ProxyDeployment
	// discoveredCount to echo back from POST /discover-proxies.
	discoveredCount int
	// discoverErr causes a non-2xx from POST /discover-proxies when non-zero.
	discoverErrStatus int
	// listErrStatus causes a non-2xx from GET /proxies when non-zero.
	listErrStatus int
}

func (s *proxyServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "proxy-test-token"})

	case r.Method == http.MethodPost && r.URL.Path == "/api/benchmarks/targets/42/discover-proxies":
		if s.discoverErrStatus != 0 {
			w.WriteHeader(s.discoverErrStatus)
			_, _ = w.Write([]byte(`{"detail":"discovery error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		resp := map[string]any{
			"target_id":        42,
			"target_name":      "test-target",
			"cluster_id":       1,
			"discovered_count": s.discoveredCount,
			"total_scanned":    5,
			"results":          []any{},
		}
		_ = json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodGet && r.URL.Path == "/api/benchmarks/targets/42/proxies":
		if s.listErrStatus != 0 {
			w.WriteHeader(s.listErrStatus)
			_, _ = w.Write([]byte(`{"detail":"list error"}`))
			return
		}
		deps := s.deployments
		if deps == nil {
			deps = []forge.ProxyDeployment{}
		}
		_ = json.NewEncoder(w).Encode(deps)

	default:
		http.NotFound(w, r)
	}
}

func newProxyTestServer(t *testing.T, srv *proxyServer) (string, func()) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	return ts.URL, func() {
		*forge.BenchmarkHTTPDoFn = origDo
		ts.Close()
	}
}

// ---------------------------------------------------------------------------
// DiscoverProxies
// ---------------------------------------------------------------------------

func TestDiscoverProxies_Success(t *testing.T) {
	srv := &proxyServer{discoveredCount: 2}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	result, err := forge.DiscoverProxies(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err != nil {
		t.Fatalf("DiscoverProxies: %v", err)
	}
	if result.TargetID != 42 {
		t.Errorf("TargetID = %d, want 42", result.TargetID)
	}
	if result.DiscoveredCount != 2 {
		t.Errorf("DiscoveredCount = %d, want 2", result.DiscoveredCount)
	}
}

func TestDiscoverProxies_ForgeError_ReturnsErr(t *testing.T) {
	// Forge returns 500 — caller should log and continue (non-fatal).
	srv := &proxyServer{discoverErrStatus: http.StatusInternalServerError}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	_, err := forge.DiscoverProxies(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err == nil {
		t.Error("expected error for forge 500, got nil")
	}
}

func TestDiscoverProxies_NoRestURL_ReturnsErr(t *testing.T) {
	_, err := forge.DiscoverProxies(context.Background(), forge.ProxyDiscoverOptions{TargetID: 42})
	if err == nil {
		t.Error("expected error for empty RestURL, got nil")
	}
}

func TestDiscoverProxies_NoTargetID_ReturnsErr(t *testing.T) {
	_, err := forge.DiscoverProxies(context.Background(), forge.ProxyDiscoverOptions{RestURL: "http://localhost:8000"})
	if err == nil {
		t.Error("expected error for zero TargetID, got nil")
	}
}

// ---------------------------------------------------------------------------
// ListProxyDeployments
// ---------------------------------------------------------------------------

func TestListProxyDeployments_ReturnsAll(t *testing.T) {
	srv := &proxyServer{
		deployments: []forge.ProxyDeployment{
			{ID: 10, TargetID: 42, ProxyType: "envoy", Status: "discovered"},
			{ID: 11, TargetID: 42, ProxyType: "haproxy", Status: "discovered"},
			{ID: 12, TargetID: 42, ProxyType: "f5-bnk", Status: "ready"},
		},
	}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	list, err := forge.ListProxyDeployments(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err != nil {
		t.Fatalf("ListProxyDeployments: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	if list[0].ProxyType != "envoy" {
		t.Errorf("list[0].ProxyType = %q, want %q", list[0].ProxyType, "envoy")
	}
}

func TestListProxyDeployments_EmptyReturnsEmptySlice(t *testing.T) {
	// Offline scenario: forge returns an empty array.
	srv := &proxyServer{deployments: nil}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	list, err := forge.ListProxyDeployments(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err != nil {
		t.Fatalf("ListProxyDeployments: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d entries", len(list))
	}
}

func TestListProxyDeployments_ForgeError_ReturnsErr(t *testing.T) {
	srv := &proxyServer{listErrStatus: http.StatusUnauthorized}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	_, err := forge.ListProxyDeployments(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err == nil {
		t.Error("expected error for forge 401, got nil")
	}
}

// ---------------------------------------------------------------------------
// ResolveProxyDeploymentID — pure helper, no network
// ---------------------------------------------------------------------------

func TestResolveProxyDeploymentID_MatchByType(t *testing.T) {
	proxies := []forge.ProxyDeployment{
		{ID: 10, ProxyType: "envoy"},
		{ID: 11, ProxyType: "haproxy"},
		{ID: 12, ProxyType: "f5-bnk"},
	}

	cases := []struct {
		proxyType string
		wantID    int
	}{
		{"envoy", 10},
		{"haproxy", 11},
		{"f5-bnk", 12},
	}
	for _, tc := range cases {
		got := forge.ResolveProxyDeploymentID(proxies, tc.proxyType)
		if got != tc.wantID {
			t.Errorf("ResolveProxyDeploymentID(%q) = %d, want %d", tc.proxyType, got, tc.wantID)
		}
	}
}

func TestResolveProxyDeploymentID_MissReturnsZero(t *testing.T) {
	proxies := []forge.ProxyDeployment{
		{ID: 10, ProxyType: "envoy"},
	}
	got := forge.ResolveProxyDeploymentID(proxies, "nodeport")
	if got != 0 {
		t.Errorf("ResolveProxyDeploymentID(nodeport on envoy-only list) = %d, want 0", got)
	}
}

func TestResolveProxyDeploymentID_EmptyListReturnsZero(t *testing.T) {
	got := forge.ResolveProxyDeploymentID(nil, "f5-bnk")
	if got != 0 {
		t.Errorf("ResolveProxyDeploymentID(nil, f5-bnk) = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// IsValidProxyType — canonical type validation
// ---------------------------------------------------------------------------

func TestIsValidProxyType_KnownTypes(t *testing.T) {
	for _, pt := range []string{"envoy", "nginx", "haproxy", "f5-bnk", "nodeport"} {
		if !forge.IsValidProxyType(pt) {
			t.Errorf("IsValidProxyType(%q) = false, want true", pt)
		}
	}
}

func TestIsValidProxyType_Typo_ReturnsFalse(t *testing.T) {
	for _, bad := range []string{"envooy", "Envoy", "ENVOY", "f5bnk", "", "unknown"} {
		if forge.IsValidProxyType(bad) {
			t.Errorf("IsValidProxyType(%q) = true, want false", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end: discover → list → resolve
// ---------------------------------------------------------------------------

func TestProxyShootout_DiscoverThenListResolve(t *testing.T) {
	// Simulates the offline path: discover returns 0 found, list returns empty.
	srv := &proxyServer{discoveredCount: 0, deployments: nil}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	opts := forge.ProxyDiscoverOptions{RestURL: url, TargetID: 42}

	_, discErr := forge.DiscoverProxies(context.Background(), opts)
	if discErr != nil {
		t.Fatalf("DiscoverProxies: %v", discErr)
	}

	list, listErr := forge.ListProxyDeployments(context.Background(), opts)
	if listErr != nil {
		t.Fatalf("ListProxyDeployments: %v", listErr)
	}

	// Offline: all types resolve to 0 (unlinked).
	for _, pType := range []string{"envoy", "haproxy", "f5-bnk", "nodeport"} {
		id := forge.ResolveProxyDeploymentID(list, pType)
		if id != 0 {
			t.Errorf("offline: ResolveProxyDeploymentID(%q) = %d, want 0", pType, id)
		}
	}
}

func TestProxyShootout_DiscoverThenListResolve_WithData(t *testing.T) {
	// Simulates a live cluster with three discovered proxies.
	srv := &proxyServer{
		discoveredCount: 3,
		deployments: []forge.ProxyDeployment{
			{ID: 7, TargetID: 42, ProxyType: "envoy", Status: "discovered"},
			{ID: 8, TargetID: 42, ProxyType: "haproxy", Status: "discovered"},
			{ID: 9, TargetID: 42, ProxyType: "f5-bnk", Status: "ready"},
		},
	}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	opts := forge.ProxyDiscoverOptions{RestURL: url, TargetID: 42}

	discResult, discErr := forge.DiscoverProxies(context.Background(), opts)
	if discErr != nil {
		t.Fatalf("DiscoverProxies: %v", discErr)
	}
	if discResult.DiscoveredCount != 3 {
		t.Errorf("DiscoveredCount = %d, want 3", discResult.DiscoveredCount)
	}

	list, listErr := forge.ListProxyDeployments(context.Background(), opts)
	if listErr != nil {
		t.Fatalf("ListProxyDeployments: %v", listErr)
	}

	if id := forge.ResolveProxyDeploymentID(list, "envoy"); id != 7 {
		t.Errorf("envoy id = %d, want 7", id)
	}
	if id := forge.ResolveProxyDeploymentID(list, "haproxy"); id != 8 {
		t.Errorf("haproxy id = %d, want 8", id)
	}
	if id := forge.ResolveProxyDeploymentID(list, "f5-bnk"); id != 9 {
		t.Errorf("f5-bnk id = %d, want 9", id)
	}
	// nodeport not in list → 0
	if id := forge.ResolveProxyDeploymentID(list, "nodeport"); id != 0 {
		t.Errorf("nodeport id = %d, want 0 (not discovered)", id)
	}
}

// ---------------------------------------------------------------------------
// ProxyDeployment decode: proxy_url + external_url fields
// ---------------------------------------------------------------------------

func TestProxyDeployment_Decode_ProxyURLAndExternalURL(t *testing.T) {
	// Verify that GET /proxies JSON with proxy_url + external_url decodes correctly.
	srv := &proxyServer{
		deployments: []forge.ProxyDeployment{
			{
				ID: 7, TargetID: 42, ProxyType: "envoy", Status: "discovered",
				ProxyURL:    "http://envoy-svc.perf-proxies.svc.cluster.local:10080",
				ExternalURL: "10.0.1.55:31234",
			},
			{
				ID: 8, TargetID: 42, ProxyType: "f5-bnk", Status: "ready",
				ProxyURL:    "",
				ExternalURL: "",
			},
		},
	}
	url, cleanup := newProxyTestServer(t, srv)
	defer cleanup()

	list, err := forge.ListProxyDeployments(context.Background(), forge.ProxyDiscoverOptions{
		RestURL:  url,
		TargetID: 42,
	})
	if err != nil {
		t.Fatalf("ListProxyDeployments: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 deployments, got %d", len(list))
	}

	envoy := list[0]
	if envoy.ProxyURL != "http://envoy-svc.perf-proxies.svc.cluster.local:10080" {
		t.Errorf("envoy ProxyURL = %q, want in-cluster DNS", envoy.ProxyURL)
	}
	if envoy.ExternalURL != "10.0.1.55:31234" {
		t.Errorf("envoy ExternalURL = %q, want 10.0.1.55:31234", envoy.ExternalURL)
	}

	f5bnk := list[1]
	if f5bnk.ExternalURL != "" {
		t.Errorf("f5-bnk ExternalURL = %q, want empty (appliance, no Service)", f5bnk.ExternalURL)
	}
}

func TestProxyDeployment_Decode_NullFields(t *testing.T) {
	// Verify that JSON null for proxy_url / external_url decodes to "" (not error).
	// The test server encodes the struct via json.NewEncoder; to test null we use
	// a raw JSON payload instead.
	raw := `[{"id":5,"target_id":42,"proxy_type":"nginx","status":"discovered","proxy_url":null,"external_url":null}]`

	var list []forge.ProxyDeployment
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&list); err != nil {
		t.Fatalf("Decode with null fields: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].ProxyURL != "" {
		t.Errorf("ProxyURL = %q, want empty string for JSON null", list[0].ProxyURL)
	}
	if list[0].ExternalURL != "" {
		t.Errorf("ExternalURL = %q, want empty string for JSON null", list[0].ExternalURL)
	}
}

func TestProxyDeployment_Decode_AbsentFields(t *testing.T) {
	// Verify that a JSON payload without proxy_url / external_url keys decodes to "".
	raw := `[{"id":6,"target_id":42,"proxy_type":"haproxy","status":"discovered"}]`

	var list []forge.ProxyDeployment
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&list); err != nil {
		t.Fatalf("Decode with absent fields: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].ProxyURL != "" {
		t.Errorf("ProxyURL = %q, want empty string for absent key", list[0].ProxyURL)
	}
	if list[0].ExternalURL != "" {
		t.Errorf("ExternalURL = %q, want empty string for absent key", list[0].ExternalURL)
	}
}

// ---------------------------------------------------------------------------
// FindProxyDeployment — pure helper
// ---------------------------------------------------------------------------

func TestFindProxyDeployment_MatchByType(t *testing.T) {
	proxies := []forge.ProxyDeployment{
		{ID: 10, ProxyType: "envoy", ExternalURL: "10.0.1.55:31234"},
		{ID: 11, ProxyType: "haproxy", ExternalURL: "10.0.1.55:31235"},
		{ID: 12, ProxyType: "f5-bnk"},
	}

	dep := forge.FindProxyDeployment(proxies, "envoy")
	if dep == nil {
		t.Fatal("FindProxyDeployment(envoy) = nil, want non-nil")
	}
	if dep.ID != 10 || dep.ExternalURL != "10.0.1.55:31234" {
		t.Errorf("FindProxyDeployment(envoy) = %+v, want id=10 ExternalURL=10.0.1.55:31234", dep)
	}
}

func TestFindProxyDeployment_MissReturnsNil(t *testing.T) {
	proxies := []forge.ProxyDeployment{
		{ID: 10, ProxyType: "envoy"},
	}
	dep := forge.FindProxyDeployment(proxies, "nginx")
	if dep != nil {
		t.Errorf("FindProxyDeployment(nginx) = %+v, want nil", dep)
	}
}

func TestFindProxyDeployment_NilSliceReturnsNil(t *testing.T) {
	dep := forge.FindProxyDeployment(nil, "envoy")
	if dep != nil {
		t.Errorf("FindProxyDeployment(nil, envoy) = %+v, want nil", dep)
	}
}
