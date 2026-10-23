package aisemanticcache_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aisemanticcache"
)

func readFileHelper(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 — test helper, path from WriteManifest
}

func makeCluster() *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{
			Name:   "test-cluster",
			Region: "ap-southeast-2",
		},
		Network: intent.Network{
			VPCCidr: "10.0.0.0/16",
			AZs:     []string{"ap-southeast-2a"},
			Subnets: intent.Subnets{
				PublicList:  []intent.SubnetSpec{{CIDR: "10.0.1.0/24", AZ: "ap-southeast-2a"}},
				PrivateList: []intent.SubnetSpec{{CIDR: "10.0.2.0/24", AZ: "ap-southeast-2a"}},
			},
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.0.10.0/24", AZ: "ap-southeast-2a"},
				Internal: intent.SubnetSpec{CIDR: "10.0.20.0/24", AZ: "ap-southeast-2a"},
			},
		},
		Pattern: "host-device",
	}
}

func TestRegistered(t *testing.T) {
	s := scenarios.Find("ai-semantic-cache")
	if s == nil {
		t.Fatal("ai-semantic-cache not registered — init() not called?")
	}
	if s.Rating() != scenarios.Amber {
		t.Errorf("rating = %q, want amber", s.Rating())
	}
	if len(s.Dependencies()) != 0 {
		t.Errorf("dependencies = %v, want empty", s.Dependencies())
	}
}

func TestManifestsRendered(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	s := scenarios.Find("ai-semantic-cache")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	if len(paths) != 5 {
		t.Errorf("expected 5 manifest paths, got %d: %v", len(paths), paths)
	}

	for _, p := range paths {
		if p == "" {
			t.Error("empty path in manifest list")
			continue
		}
		rawContent, readErr := readFileHelper(p)
		if readErr != nil {
			t.Fatalf("reading %s: %v", p, readErr)
		}
		content := string(rawContent)
		// No leftover template directives.
		if strings.Contains(content, "{{") || strings.Contains(content, "}}") {
			t.Errorf("manifest %s still contains template directives:\n%s", p, content)
		}
		// The Gateway manifest must carry the exact AI annotation key.
		if strings.HasSuffix(p, "04-gateway.yaml") {
			if !strings.Contains(content, "k8s.f5.com/ai:") {
				t.Errorf("04-gateway.yaml missing k8s.f5.com/ai annotation key:\n%s", content)
			}
			if !strings.Contains(content, "infrastructure:") {
				t.Errorf("04-gateway.yaml missing spec.infrastructure block:\n%s", content)
			}
		}
		// The HTTPRoute manifest must carry the SSE annotation key.
		if strings.HasSuffix(p, "05-httproute.yaml") {
			if !strings.Contains(content, "k8s.f5.com/sse-enabled:") {
				t.Errorf("05-httproute.yaml missing k8s.f5.com/sse-enabled annotation key:\n%s", content)
			}
		}
	}
}
