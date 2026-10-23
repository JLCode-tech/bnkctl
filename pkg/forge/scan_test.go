package forge

import (
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
)

// Shape of backend/services/scanner/bnk_install.py on a 2.4 cluster, inside
// the ClusterScanResponse wrapper the REST route returns.
const scan24Wrapped = `{"success": true, "message": "scan complete", "scan_results": {
  "cluster_id": 23, "cluster_name": "bnk-agentcore-demo",
  "cluster_info": {"version": "v1.35.1-eks"},
  "prerequisites": {"cert_manager": {"installed": true}},
  "bnk_install": {
    "status": "installed", "health": "healthy",
    "namespaces": {"f5_cne_system": true, "f5_bnk": false, "f5_utils": true},
    "crds": {"total": 61, "groups": ["gateway.k8s.f5.com", "k8s.f5.com", "k8s.f5net.com"],
             "has_data_plane": true, "has_flo": true, "has_gateway_ext": false},
    "flo": {"version": "v2.30.0-0.5.2", "pods": 1, "running": 1, "helm_release": {"name": "f5-lifecycle-operator"}},
    "tmm": {"pods": 1, "running": 1, "containers": {}},
    "controller": {"pods": 1, "running": 1},
    "analyzer": {"pods": 0, "running": 0},
    "crd_installer": {"completed": true, "pods": 1},
    "cne_instance": {"name": "bnk-agentcore-demo", "manifest_version": "2.4.0"},
    "vlans": []
  },
  "recommendations": [], "enabled_prerequisites": [], "scan_metadata": {"duration_ms": 1200}
}}`

// The same envelope unwrapped (ClusterScanEnvelope), on a 2.3 cluster.
const scan23Bare = `{"cluster_id": 7, "cluster_name": "lab-23",
  "cluster_info": {}, "prerequisites": {},
  "bnk_install": {"status": "installed", "crds": {"groups": ["gateway.k8s.f5net.com", "k8s.f5.com", "k8s.f5net.com"], "has_gateway_ext": true},
                  "flo": {"version": "v2.21.13-0.0.64"}, "tmm": {"pods": 2, "running": 1}, "controller": {"pods": 1, "running": 1}},
  "recommendations": [], "scan_metadata": {}}`

func TestParseScanResult_WrappedAnd24(t *testing.T) {
	res, err := ParseScanResult(scan24Wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if res.ClusterID != 23 || res.ClusterName != "bnk-agentcore-demo" {
		t.Errorf("cluster = %d %q", res.ClusterID, res.ClusterName)
	}
	if res.Generation() != bnkscan.Gen24 || !res.Indexes24() {
		t.Errorf("generation = %s indexes24=%v", res.Generation(), res.Indexes24())
	}
	if res.ManifestVersion() != "2.4.0" {
		t.Errorf("manifest = %q", res.ManifestVersion())
	}
	want := "bnk=installed api=2.4 manifest=2.4.0 flo=v2.30.0-0.5.2 tmm=1/1 controller=1/1"
	if got := res.Summary(); got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

func TestParseScanResult_BareAnd23(t *testing.T) {
	res, err := ParseScanResult(scan23Bare)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generation() != bnkscan.Gen23 || res.Indexes24() {
		t.Errorf("generation = %s indexes24=%v", res.Generation(), res.Indexes24())
	}
	if got := res.Summary(); !strings.Contains(got, "api=2.3") || !strings.Contains(got, "tmm=1/2") {
		t.Errorf("Summary = %q", got)
	}
}

func TestParseScanResult_FailureEnvelope(t *testing.T) {
	_, err := ParseScanResult(`{"success": false, "message": "cluster unreachable", "scan_results": null}`)
	if err == nil || !strings.Contains(err.Error(), "cluster unreachable") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseScanResult("not json"); err == nil {
		t.Error("expected decode error")
	}
}

func TestParseHealthResult_SeverityShapes(t *testing.T) {
	// analyze_health emits nested sections with a severity each; overall is a
	// bare string. Accept both encodings for every field.
	text := `{"cluster_id": 23, "overall": "warning",
	  "platform": {"severity": "healthy", "controller": {}},
	  "dataPlane": {"severity": "warning", "cneInstance": {}},
	  "networking": {"severity": "healthy"}, "security": {"severity": "unknown"}, "ai": {"severity": "healthy"},
	  "counts": {"gateways": 2, "listeners": 3, "httpRoutes": 4, "tmm_pods": 1, "tmm_running": 1, "tmm_containers": "3/3"}}`
	h, err := ParseHealthResult(text)
	if err != nil {
		t.Fatal(err)
	}
	if h.Overall != "warning" || h.Platform != "healthy" || h.DataPlane != "warning" || h.Security != "unknown" {
		t.Errorf("health = %+v", h)
	}
	if h.Count("gateways") != 2 || h.Count("missing") != 0 {
		t.Errorf("counts = %v", h.Counts)
	}
	want := "overall=warning platform=healthy dataPlane=warning networking=healthy security=unknown gateways=2 httpRoutes=4 tmm=1/1"
	if got := h.Summary(); got != want {
		t.Errorf("Summary = %q\nwant      %q", got, want)
	}

	h2, err := ParseHealthResult(`{"overall": {"status": "critical"}, "platform": "critical"}`)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Overall != "critical" || h2.Summary() != "overall=critical platform=critical dataPlane=-" {
		t.Errorf("h2 = %+v summary=%q", h2, h2.Summary())
	}
}
