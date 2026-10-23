package forge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

// ---------------------------------------------------------------------------
// RegisterJumphostAccessMethod — mock HTTP transport via BenchmarkHTTPDoFn
// ---------------------------------------------------------------------------

// accessMethodServer is a minimal httptest server for the SSH credential path.
type accessMethodServer struct {
	// capturedPosts tracks (path, body) for each POST call in order.
	capturedPosts []capturedReq
	// capturedPuts tracks PUT calls.
	capturedPuts []capturedReq
	// capturedGets tracks GET calls.
	capturedGets []string
	// postStatus controls the status code returned for POST /api/ssh-credentials.
	postStatus int
	// existingCredentials is the list returned by GET /api/ssh-credentials.
	existingCredentials []map[string]any
}

type capturedReq struct {
	path string
	body map[string]any
}

func (s *accessMethodServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "am-token"})

	case r.Method == http.MethodPost && r.URL.Path == forge.SSHCredentialEndpoint:
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.capturedPosts = append(s.capturedPosts, capturedReq{path: r.URL.Path, body: body})

		status := s.postStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   99,
				"name": body["name"],
				"host": body["host"],
			})
		}

	case r.Method == http.MethodGet && r.URL.Path == forge.SSHCredentialEndpoint:
		s.capturedGets = append(s.capturedGets, r.URL.Path)
		creds := s.existingCredentials
		if creds == nil {
			creds = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(creds)

	case r.Method == http.MethodPut:
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.capturedPuts = append(s.capturedPuts, capturedReq{path: r.URL.Path, body: body})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   77,
			"name": "azrbnkctl-jumphost-i-abc",
			"host": body["host"],
		})

	default:
		http.NotFound(w, r)
	}
}

// TestRegisterJumphostAccessMethod_PostsToCorrectPath asserts the POST goes to
// /api/ssh-credentials with auth_type=key and required fields.
func TestRegisterJumphostAccessMethod_PostsToCorrectPath(t *testing.T) {
	srv := &accessMethodServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterJumphostAccessMethod(context.Background(), forge.AccessMethodOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-jumphost-i-abc",
		Host:       "i-abc123",
		Region:     "ap-southeast-2",
		InstanceID: "i-abc123",
	})
	if err != nil {
		t.Fatalf("RegisterJumphostAccessMethod: %v", err)
	}
	if resp.ID != 99 {
		t.Errorf("response ID = %d, want 99", resp.ID)
	}
	if len(srv.capturedPosts) != 1 {
		t.Fatalf("expected 1 POST, got %d", len(srv.capturedPosts))
	}
	post := srv.capturedPosts[0]
	if post.path != forge.SSHCredentialEndpoint {
		t.Errorf("POST path = %q, want %q", post.path, forge.SSHCredentialEndpoint)
	}

	// Verify required body fields.
	checks := map[string]any{
		"name":      "azrbnkctl-jumphost-i-abc",
		"host":      "i-abc123",
		"username":  "azureuser",
		"auth_type": "key",
	}
	for k, want := range checks {
		if got := post.body[k]; got != want {
			t.Errorf("body[%q] = %v, want %v", k, got, want)
		}
	}
	// port must be 22
	if port, ok := post.body["port"].(float64); !ok || int(port) != 22 {
		t.Errorf("body[port] = %v, want 22", post.body["port"])
	}
	// private_key must NOT be present (EICE — no static key)
	if _, present := post.body["private_key"]; present {
		t.Error("body must not contain private_key for EICE jumphost")
	}
}

// TestRegisterJumphostAccessMethod_AuthLoginCalledFirst asserts that
// /api/auth/login is called before the credential POST and that the
// Authorization header is set.
func TestRegisterJumphostAccessMethod_AuthLoginCalledFirst(t *testing.T) {
	var calls []string
	var capturedAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls = append(calls, r.Method+":"+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-xyz"})
		case r.Method == http.MethodPost && r.URL.Path == forge.SSHCredentialEndpoint:
			capturedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "x", "host": "h"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	_, err := forge.RegisterJumphostAccessMethod(context.Background(), forge.AccessMethodOptions{
		RestURL: ts.URL, Name: "x", Host: "h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) < 2 || calls[0] != "POST:/api/auth/login" {
		t.Errorf("expected first call to be login, got %v", calls)
	}
	if capturedAuth != "Bearer tok-xyz" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Bearer tok-xyz")
	}
}

// TestRegisterJumphostAccessMethod_409UpsertPath asserts that on 409 the
// function GETs the list, finds the record by name, and PUTs the update.
func TestRegisterJumphostAccessMethod_409UpsertPath(t *testing.T) {
	srv := &accessMethodServer{
		postStatus: http.StatusConflict,
		existingCredentials: []map[string]any{
			{"id": float64(77), "name": "azrbnkctl-jumphost-i-abc", "host": "old-host"},
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterJumphostAccessMethod(context.Background(), forge.AccessMethodOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-jumphost-i-abc",
		Host:       "i-abc123",
		Region:     "ap-southeast-2",
		InstanceID: "i-abc123",
	})
	if err != nil {
		t.Fatalf("upsert path: %v", err)
	}
	// Should have GETted the list.
	if len(srv.capturedGets) == 0 {
		t.Error("expected GET /api/ssh-credentials for upsert list lookup")
	}
	// Should have PUT the update.
	if len(srv.capturedPuts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(srv.capturedPuts))
	}
	if resp.ID != 77 {
		t.Errorf("upsert response ID = %d, want 77", resp.ID)
	}
}

// TestRegisterJumphostAccessMethod_MissingRestURL returns an error.
func TestRegisterJumphostAccessMethod_MissingRestURL(t *testing.T) {
	_, err := forge.RegisterJumphostAccessMethod(context.Background(), forge.AccessMethodOptions{
		Name: "x", Host: "h",
	})
	if err == nil {
		t.Error("expected error for empty RestURL, got nil")
	}
}

// TestRegisterJumphostAccessMethod_DescriptionContainsEICENote verifies that
// the description field in the posted body mentions EICE.
func TestRegisterJumphostAccessMethod_DescriptionContainsEICENote(t *testing.T) {
	srv := &accessMethodServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	_, err := forge.RegisterJumphostAccessMethod(context.Background(), forge.AccessMethodOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-jumphost-i-test",
		Host:       "i-test",
		Region:     "us-east-1",
		InstanceID: "i-test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(srv.capturedPosts) == 0 {
		t.Fatal("no POST captured")
	}
	desc, _ := srv.capturedPosts[0].body["description"].(string)
	if desc == "" {
		t.Fatal("description field missing in POST body")
	}
	for _, want := range []string{"azrbnkctl-managed", "Region: us-east-1", "VM: i-test", "informational"} {
		found := false
		for i := 0; i+len(want) <= len(desc); i++ {
			if desc[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("description %q missing expected substring %q", desc, want)
		}
	}
}
