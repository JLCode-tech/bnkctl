package forge

// scan.go — typed views of forge's scan_cluster and bnk_health tool output.
//
// Forge indexes a cluster server-side; the MCP tools take only cluster_id.
// awsbnkctl reads the CRD groups forge saw to tell a 2.3 cluster
// (gateway.k8s.f5net.com) from a 2.4 one (gateway.k8s.f5.com) and to spot a
// forge whose resource registry predates 2.4, so the operator knows whether the
// Fleet view can be trusted for Infra / GatewaySettings / SecPolicy / NetPolicy.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
)

// ScanResult is the part of POST /api/k8s/clusters/{id}/scan awsbnkctl reads.
// Forge returns either the typed envelope or {"success","message","scan_results"}
// wrapping it; ParseScanResult accepts both.
type ScanResult struct {
	ClusterID   int    `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	BNKInstall  struct {
		Status string `json:"status"`
		Health string `json:"health"`
		CRDs   struct {
			Total          int      `json:"total"`
			Groups         []string `json:"groups"`
			HasDataPlane   bool     `json:"has_data_plane"`
			HasFLO         bool     `json:"has_flo"`
			HasGatewayExt  bool     `json:"has_gateway_ext"`
			HasF5Inference bool     `json:"has_f5_inference"`
		} `json:"crds"`
		FLO struct {
			Version string `json:"version"`
			Pods    int    `json:"pods"`
			Running int    `json:"running"`
		} `json:"flo"`
		TMM struct {
			Pods    int `json:"pods"`
			Running int `json:"running"`
		} `json:"tmm"`
		Controller struct {
			Pods    int `json:"pods"`
			Running int `json:"running"`
		} `json:"controller"`
		CNEInstance map[string]any `json:"cne_instance"`
	} `json:"bnk_install"`
	Prerequisites   map[string]any   `json:"prerequisites"`
	Recommendations []map[string]any `json:"recommendations"`
}

// ParseScanResult decodes scan_cluster output in either envelope shape.
func ParseScanResult(text string) (ScanResult, error) {
	var wrapper struct {
		Success     *bool           `json:"success"`
		Message     string          `json:"message"`
		ScanResults json.RawMessage `json:"scan_results"`
	}
	if err := json.Unmarshal([]byte(text), &wrapper); err != nil {
		return ScanResult{}, fmt.Errorf("scan_cluster: decode: %w", err)
	}
	body := []byte(text)
	if len(wrapper.ScanResults) > 0 && string(wrapper.ScanResults) != "null" {
		body = wrapper.ScanResults
	} else if wrapper.Success != nil && !*wrapper.Success {
		return ScanResult{}, fmt.Errorf("scan_cluster: %s", wrapper.Message)
	}
	var out ScanResult
	if err := json.Unmarshal(body, &out); err != nil {
		return ScanResult{}, fmt.Errorf("scan_cluster: decode scan_results: %w", err)
	}
	return out, nil
}

// Generation classifies the cluster from the CRD groups forge indexed.
func (s ScanResult) Generation() bnkscan.Generation {
	return bnkscan.DetectGeneration(s.BNKInstall.CRDs.Groups)
}

// Indexes24 reports whether forge saw the 2.4 policy group at all. False on a
// 2.4 cluster means forge's resource registry is older than the cluster.
func (s ScanResult) Indexes24() bool {
	for _, g := range s.BNKInstall.CRDs.Groups {
		if g == "gateway.k8s.f5.com" {
			return true
		}
	}
	return false
}

// IndexesF5Inference reports whether forge indexed the BNK 2.4.1
// inference.k8s.f5.com AI Gateway CRD group.
func (s ScanResult) IndexesF5Inference() bool {
	if s.BNKInstall.CRDs.HasF5Inference {
		return true
	}
	for _, g := range s.BNKInstall.CRDs.Groups {
		if g == bnkscan.InferenceF5Group {
			return true
		}
	}
	return false
}

// ManifestVersion returns the CNEInstance manifestVersion forge parsed, if any.
func (s ScanResult) ManifestVersion() string {
	for _, k := range []string{"manifest_version", "manifestVersion", "version"} {
		if v, ok := s.BNKInstall.CNEInstance[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Summary renders one operator-facing line.
func (s ScanResult) Summary() string {
	b := s.BNKInstall
	parts := []string{
		"bnk=" + orDash(b.Status),
		"api=" + string(s.Generation()),
	}
	if v := s.ManifestVersion(); v != "" {
		parts = append(parts, "manifest="+v)
	}
	if b.FLO.Version != "" {
		parts = append(parts, "flo="+b.FLO.Version)
	}
	parts = append(parts,
		fmt.Sprintf("tmm=%d/%d", b.TMM.Running, b.TMM.Pods),
		fmt.Sprintf("controller=%d/%d", b.Controller.Running, b.Controller.Pods),
	)
	return strings.Join(parts, " ")
}

// Severity is a forge health severity ("healthy", "warning", "critical",
// "unknown"). Forge emits it as a bare string or as {"severity": ...}.
type Severity string

// UnmarshalJSON accepts both encodings.
func (s *Severity) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = Severity(str)
		return nil
	}
	var obj struct {
		Severity string `json:"severity"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	if obj.Severity != "" {
		*s = Severity(obj.Severity)
	} else {
		*s = Severity(obj.Status)
	}
	return nil
}

// HealthResult is the part of GET /f5bnk/health awsbnkctl reads.
type HealthResult struct {
	ClusterID  int            `json:"cluster_id"`
	Overall    Severity       `json:"overall"`
	Platform   Severity       `json:"platform"`
	DataPlane  Severity       `json:"dataPlane"`
	Networking Severity       `json:"networking"`
	Security   Severity       `json:"security"`
	AI         Severity       `json:"ai"`
	Counts     map[string]any `json:"counts"`
}

// ParseHealthResult decodes bnk_health output.
func ParseHealthResult(text string) (HealthResult, error) {
	var out HealthResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return HealthResult{}, fmt.Errorf("bnk_health: decode: %w", err)
	}
	return out, nil
}

// Count returns an integer from Counts, 0 when absent.
func (h HealthResult) Count(key string) int {
	switch v := h.Counts[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// Summary renders one operator-facing line.
func (h HealthResult) Summary() string {
	parts := []string{
		"overall=" + orDash(string(h.Overall)),
		"platform=" + orDash(string(h.Platform)),
		"dataPlane=" + orDash(string(h.DataPlane)),
	}
	if h.Networking != "" {
		parts = append(parts, "networking="+string(h.Networking))
	}
	if h.Security != "" {
		parts = append(parts, "security="+string(h.Security))
	}
	if len(h.Counts) > 0 {
		parts = append(parts, fmt.Sprintf("gateways=%d httpRoutes=%d tmm=%d/%d",
			h.Count("gateways"), h.Count("httpRoutes"), h.Count("tmm_running"), h.Count("tmm_pods")))
	}
	return strings.Join(parts, " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ScanClusterTyped runs scan_cluster and decodes the result. The raw text is
// returned too so callers can print forge's full payload on request.
func (c *Client) ScanClusterTyped(ctx context.Context, clusterID int) (ScanResult, string, error) {
	raw, err := c.ScanCluster(ctx, clusterID)
	if err != nil {
		return ScanResult{}, "", err
	}
	res, err := ParseScanResult(raw)
	return res, raw, err
}

// BNKHealthTyped runs bnk_health and decodes the result.
func (c *Client) BNKHealthTyped(ctx context.Context, clusterID int) (HealthResult, string, error) {
	raw, err := c.BNKHealth(ctx, clusterID)
	if err != nil {
		return HealthResult{}, "", err
	}
	res, err := ParseHealthResult(raw)
	return res, raw, err
}
