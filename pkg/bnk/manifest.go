// Package bnk provides OCI release manifest pulling, local archive probing,
// and Bill-of-Materials (BOM) inspection for F5 BIG-IP Next for Kubernetes (BNK).
package bnk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
)

const (
	FARRegistryHost      = "repo.f5.com"
	ReleaseManifestRepo  = "oci://repo.f5.com/release"
	ReleaseManifestChart = "f5-bigip-k8s-manifest"
	// DefaultManifestVersion is the default F5 BNK release manifest version.
	DefaultManifestVersion = "2.4.0"
)

// Component represents a single component chart/image entry inside a BNK release manifest.
type Component struct {
	Name         string `yaml:"name" json:"name"`
	FullName     string `yaml:"full-name" json:"fullName"`
	ChartVersion string `yaml:"chart-version" json:"chartVersion"`
}

// ReleaseManifest represents the parsed BOM from F5's release manifest chart.
type ReleaseManifest struct {
	Version         string            `json:"version"`
	FLOVersion      string            `json:"floVersion"`
	HelmRepo        string            `json:"helmRepo,omitempty"`
	DockerRepo      string            `json:"dockerRepo,omitempty"`
	Components      []Component       `json:"components"`
	HelmCharts      map[string]string `json:"helmCharts,omitempty"`
	DockerImgs      map[string]string `json:"dockerImages,omitempty"`
	RawManifestYAML string            `json:"rawManifestYaml,omitempty"`
	ChartNotes      string            `json:"chartNotes,omitempty"`
}

func (m *ReleaseManifest) Chart(name string) string { return m.HelmCharts[name] }
func (m *ReleaseManifest) Image(name string) string { return m.DockerImgs[name] }
func (m *ReleaseManifest) RawYAML() []byte          { return []byte(m.RawManifestYAML) }

// SinkSummary prints a concise summary of the manifest contents.
func (m *ReleaseManifest) SinkSummary(w io.Writer) {
	fmt.Fprintf(w, "Release-manifest:  %s\n", m.Version)
	fmt.Fprintf(w, "  helm repo:       %s\n", m.HelmRepo)
	fmt.Fprintf(w, "  docker repo:     %s\n", m.DockerRepo)
	fmt.Fprintf(w, "  helm charts:     %d\n", len(m.HelmCharts))
	fmt.Fprintf(w, "  docker images:   %d\n", len(m.DockerImgs))
	for _, name := range []string{
		"charts/f5-lifecycle-operator",
		"f5-lifecycle-operator",
		"utils/f5-cert-gen",
		"charts/cwc",
		"charts/f5-cert-manager",
	} {
		if v := m.Chart(name); v != "" {
			fmt.Fprintf(w, "    %-35s %s\n", name, v)
		}
	}
}

// PrintFullBOM dumps every chart and image in the manifest in sorted order.
func PrintFullBOM(w io.Writer, m *ReleaseManifest) {
	if m == nil {
		return
	}
	for _, sec := range []struct {
		title string
		items map[string]string
	}{
		{"helm charts", m.HelmCharts},
		{"docker images", m.DockerImgs},
	} {
		names := make([]string, 0, len(sec.items))
		for n := range sec.items {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "\n  %s (%d):\n", sec.title, len(names))
		for _, n := range names {
			fmt.Fprintf(w, "    %-45s %s\n", n, sec.items[n])
		}
	}
}

// Release records the paired F5 Lifecycle Operator chart version for a known BNK manifest version.
type Release struct {
	Version  string
	FLOChart string
}

// KnownReleases lists BNK release manifests and their paired FLO chart versions.
var KnownReleases = []Release{
	{Version: "2.3.0-3.2598.3-0.0.170", FLOChart: "v2.21.13-0.0.28"},
	{Version: "2.3.0-3.2598.3-0.0.171", FLOChart: "v2.21.13-0.0.28"},
	{Version: "2.3.1-3.2598.3-0.0.304", FLOChart: "v2.21.13-0.0.53"},
	{Version: "2.3.1-3.2598.3-0.0.312", FLOChart: "v2.21.13-0.0.53"},
	{Version: "2.3.2-3.2598.3-0.0.392", FLOChart: "v2.21.13-0.0.58"},
	{Version: "2.3.2-3.2598.3-0.0.464", FLOChart: "v2.21.13-0.0.58"},
	{Version: "2.3.3-3.2598.3-0.0.509", FLOChart: "v2.21.13-0.0.64"},
	{Version: "2.4.0", FLOChart: "v2.30.0-0.5.2"},
	{Version: "2.4.1", FLOChart: "v2.30.1-0.0.8"},
	{Version: "2.4.1-3.2850.1-0.0.9", FLOChart: "v2.30.1-0.0.8"},
	{Version: "2.5.0", FLOChart: "v2.40.0-0.1.0"},
}

// FLOChartFor returns the FLO chart version paired with manifestVersion.
func FLOChartFor(manifestVersion string) (chart string, ok bool) {
	v := strings.TrimPrefix(strings.TrimSpace(manifestVersion), "v")
	for _, r := range KnownReleases {
		if strings.TrimPrefix(r.Version, "v") == v {
			return r.FLOChart, true
		}
	}
	return "", false
}

// DefaultFLOChart is the FLO chart paired with DefaultManifestVersion.
func DefaultFLOChart() string {
	chart, _ := FLOChartFor(DefaultManifestVersion)
	return chart
}

// IsKnownRelease reports whether manifestVersion is a known release.
func IsKnownRelease(manifestVersion string) bool {
	_, ok := FLOChartFor(manifestVersion)
	return ok
}

// ExtractFARAuth extracts the username and password for repo.f5.com from a FAR archive.
// Supports both .tar.gz / .tgz archives containing cne_pull_64.json or raw base64 JSON.
func ExtractFARAuth(farPath string) (username, password string, err error) {
	data, err := os.ReadFile(farPath) // #nosec G304 -- operator-supplied path via cluster.yaml or CLI
	if err != nil {
		return "", "", fmt.Errorf("read FAR file %s: %w", farPath, err)
	}

	if isGzipTar(data) {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err == nil {
			defer gz.Close()
			tr := tar.NewReader(gz)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				if hdr.Typeflag == tar.TypeReg {
					content, err := io.ReadAll(tr)
					if err == nil && len(content) > 0 {
						data = content
						break
					}
				}
			}
		}
	}

	contentStr := strings.TrimSpace(string(data))
	decoded, err := base64.StdEncoding.DecodeString(contentStr)
	if err == nil {
		var sa map[string]any
		if json.Unmarshal(decoded, &sa) == nil && sa["type"] == "service_account" {
			return "_json_key", string(decoded), nil
		}
	}

	var sa map[string]any
	if json.Unmarshal(data, &sa) == nil && sa["type"] == "service_account" {
		return "_json_key", string(data), nil
	}

	return "_json_key_base64", contentStr, nil
}

func isGzipTar(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

// PullReleaseManifest logs into repo.f5.com and pulls the release manifest chart into cacheDir.
func PullReleaseManifest(_ context.Context, username, password, manifestVersion, cacheDir string) (*ReleaseManifest, error) {
	if manifestVersion == "" {
		manifestVersion = DefaultManifestVersion
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir cache %s: %w", cacheDir, err)
	}
	absCache, err := filepath.Abs(cacheDir)
	if err != nil {
		return nil, err
	}

	regClient, err := registry.NewClient(registry.ClientOptBasicAuth(username, password))
	if err != nil {
		return nil, fmt.Errorf("create helm registry client: %w", err)
	}

	extractedDir := filepath.Join(absCache, ReleaseManifestChart+"-"+manifestVersion)
	_ = os.RemoveAll(extractedDir)
	_ = os.RemoveAll(filepath.Join(absCache, ReleaseManifestChart))

	cfg := &action.Configuration{RegistryClient: regClient}
	pull := action.NewPullWithOpts(action.WithConfig(cfg))
	pull.Settings = cli.New()
	pull.Version = manifestVersion
	pull.DestDir = absCache
	pull.Untar = true
	pull.UntarDir = absCache
	if _, err := pull.Run(ReleaseManifestRepo + "/" + ReleaseManifestChart); err != nil {
		return nil, fmt.Errorf("pull release-manifest %s from %s: %w", manifestVersion, ReleaseManifestRepo, err)
	}
	if err := os.Rename(filepath.Join(absCache, ReleaseManifestChart), extractedDir); err != nil {
		return nil, fmt.Errorf("rename extracted chart: %w", err)
	}

	manifestPath, err := findManifestFile(extractedDir, manifestVersion)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(manifestPath) // #nosec G304 -- reading extracted manifest yaml
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}

	m, err := ParseReleaseManifest(body)
	if err != nil {
		return nil, err
	}

	_ = os.WriteFile(filepath.Join(absCache, "manifest.yaml"), body, 0o600) // #nosec G306,G703 -- cache write
	return m, nil
}

func findManifestFile(dir, manifestVersion string) (string, error) {
	exact := filepath.Join(dir, fmt.Sprintf("bigip-k8s-manifest-%s.yaml", manifestVersion))
	if _, err := os.Stat(exact); err == nil {
		return exact, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "bigip-k8s-manifest-*.yaml"))
	if err != nil {
		return "", fmt.Errorf("list manifests in %s: %w", dir, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no bigip-k8s-manifest-*.yaml in %s (requested %s)", dir, manifestVersion)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%d release manifests in %s, none named for %s: %v", len(matches), dir, manifestVersion, matches)
	}
}

// PullAndInspect pulls oci://repo.f5.com/release/f5-bigip-k8s-manifest:<version>
// via the Helm v3 OCI client and extracts the paired FLO chart version and BOM.
func PullAndInspect(_ context.Context, version, saKeyJSON string) (*ReleaseManifest, error) {
	settings := cli.New()
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), "default", "memory", func(string, ...interface{}) {}); err != nil {
		return nil, fmt.Errorf("init helm config: %w", err)
	}

	regClient, err := registry.NewClient(
		registry.ClientOptDebug(false),
		registry.ClientOptEnableCache(true),
		registry.ClientOptWriter(io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("create helm registry client: %w", err)
	}

	if strings.TrimSpace(saKeyJSON) != "" {
		if err := regClient.Login("repo.f5.com",
			registry.LoginOptBasicAuth("_json_key", saKeyJSON),
			registry.LoginOptInsecure(false),
		); err != nil {
			return nil, fmt.Errorf("login to repo.f5.com: %w", err)
		}
	}
	actionConfig.RegistryClient = regClient

	tmpDir, err := os.MkdirTemp("", "bnk-manifest-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	pullClient := action.NewPullWithOpts(action.WithConfig(actionConfig))
	pullClient.Settings = settings
	pullClient.DestDir = tmpDir
	pullClient.Version = strings.TrimPrefix(version, "v")

	chartRef := "oci://repo.f5.com/release/f5-bigip-k8s-manifest"
	out, err := pullClient.Run(chartRef)
	if err != nil {
		return nil, fmt.Errorf("pulling %s:%s: %w (out: %s)", chartRef, pullClient.Version, err, out)
	}

	matches, err := filepath.Glob(filepath.Join(tmpDir, "*.tgz"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("locating pulled manifest archive in %s", tmpDir)
	}

	rm, err := parseTarGz(matches[0])
	if err != nil {
		return nil, err
	}
	if rm.Version == "" {
		rm.Version = pullClient.Version
	}
	if rm.FLOVersion == "" {
		if flo, ok := FLOChartFor(rm.Version); ok {
			rm.FLOVersion = flo
		}
	}
	return rm, nil
}

// ParseReleaseManifest parses raw YAML bytes (either chart-versions or releases[0] format)
// into a structured ReleaseManifest.
func ParseReleaseManifest(body []byte) (*ReleaseManifest, error) {
	// First check if it's the chart-versions map format from f5-bigip-k8s-manifest
	var parsed struct {
		ChartVersions map[string]Component `yaml:"chart-versions"`
	}
	if err := yaml.Unmarshal(body, &parsed); err == nil && len(parsed.ChartVersions) > 0 {
		rm := &ReleaseManifest{
			HelmRepo:        "oci://repo.f5.com/charts",
			DockerRepo:      "repo.f5.com/images",
			RawManifestYAML: string(body),
			HelmCharts:      make(map[string]string, len(parsed.ChartVersions)),
			DockerImgs:      make(map[string]string),
		}
		keys := make([]string, 0, len(parsed.ChartVersions))
		for k := range parsed.ChartVersions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			comp := parsed.ChartVersions[k]
			if comp.Name == "" {
				comp.Name = k
			}
			rm.Components = append(rm.Components, comp)
			rm.HelmCharts[comp.Name] = comp.ChartVersion
			if comp.Name == "f5-lifecycle-operator" || strings.Contains(comp.FullName, "f5-lifecycle-operator") {
				rm.FLOVersion = comp.ChartVersion
			}
		}
		return rm, nil
	}

	// Next check the releases[] BOM format
	var raw struct {
		F5HelmRepo   string `yaml:"f5_helm_repo"`
		F5DockerRepo string `yaml:"f5_docker_repo"`
		Releases     []struct {
			Version    string `yaml:"version"`
			HelmCharts []struct {
				Name    string `yaml:"name"`
				Version string `yaml:"version"`
			} `yaml:"helm_charts"`
			DockerImages []struct {
				Name    string `yaml:"name"`
				Version string `yaml:"version"`
			} `yaml:"docker_images"`
		} `yaml:"releases"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse release-manifest yaml: %w", err)
	}
	if len(raw.Releases) == 0 {
		return nil, fmt.Errorf("release-manifest has no releases[] or chart-versions entries")
	}
	rel := raw.Releases[0]
	if rel.Version == "" {
		return nil, fmt.Errorf("release-manifest releases[0].version is empty")
	}
	m := &ReleaseManifest{
		HelmRepo:        raw.F5HelmRepo,
		DockerRepo:      raw.F5DockerRepo,
		Version:         rel.Version,
		HelmCharts:      make(map[string]string, len(rel.HelmCharts)),
		DockerImgs:      make(map[string]string, len(rel.DockerImages)),
		RawManifestYAML: string(body),
	}
	for _, c := range rel.HelmCharts {
		if c.Name != "" {
			m.HelmCharts[c.Name] = c.Version
			m.Components = append(m.Components, Component{
				Name:         c.Name,
				FullName:     c.Name,
				ChartVersion: c.Version,
			})
			if c.Name == "f5-lifecycle-operator" || c.Name == "charts/f5-lifecycle-operator" {
				m.FLOVersion = c.Version
			}
		}
	}
	for _, i := range rel.DockerImages {
		if i.Name != "" {
			m.DockerImgs[i.Name] = i.Version
		}
	}
	if m.FLOVersion == "" {
		if flo, ok := FLOChartFor(m.Version); ok {
			m.FLOVersion = flo
		}
	}
	return m, nil
}

func parseTarGz(archivePath string) (*ReleaseManifest, error) {
	// #nosec G304 -- archivePath is operator-supplied local FAR archive path
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	rm := &ReleaseManifest{
		HelmCharts: make(map[string]string),
		DockerImgs: make(map[string]string),
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasSuffix(hdr.Name, "release-manifest.yaml") || strings.HasSuffix(hdr.Name, "manifest.yaml") || (strings.Contains(filepath.Base(hdr.Name), "bigip-k8s-manifest-") && strings.HasSuffix(hdr.Name, ".yaml")):
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			parsed, err := ParseReleaseManifest(b)
			if err == nil {
				parsed.ChartNotes = rm.ChartNotes
				if parsed.FLOVersion == "" && rm.FLOVersion != "" {
					parsed.FLOVersion = rm.FLOVersion
				}
				rm = parsed
			} else {
				rm.RawManifestYAML = string(b)
			}
		case strings.HasSuffix(hdr.Name, "NOTES.txt"):
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			rm.ChartNotes = string(b)
			for _, line := range strings.Split(rm.ChartNotes, "\n") {
				if idx := strings.Index(line, "--version"); idx != -1 {
					fields := strings.Fields(line[idx:])
					if len(fields) >= 2 {
						rm.FLOVersion = strings.TrimSpace(fields[1])
					}
				}
			}
		}
	}

	if rm.RawManifestYAML == "" && len(rm.Components) == 0 && len(rm.HelmCharts) == 0 {
		return nil, fmt.Errorf("manifest.yaml not found inside archive %s", archivePath)
	}
	return rm, nil
}

// ProbeArchive extracts and parses manifest.yaml or release-manifest.yaml from a tarball or directory.
func ProbeArchive(path string) (*ReleaseManifest, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read path %s: %w", path, err)
	}
	if fi.IsDir() {
		for _, candidate := range []string{"manifest.yaml", "release-manifest.yaml"} {
			manifestFile := filepath.Join(path, candidate)
			// #nosec G304 -- path is operator-supplied local directory
			if data, err := os.ReadFile(manifestFile); err == nil {
				return ParseReleaseManifest(data)
			}
		}
		return nil, fmt.Errorf("manifest.yaml not found in %s", path)
	}
	return parseTarGz(path)
}

// GetEmbeddedManifest returns a built-in baseline manifest for offline inspection.
func GetEmbeddedManifest() *ReleaseManifest {
	flo, _ := FLOChartFor(DefaultManifestVersion)
	return &ReleaseManifest{
		HelmRepo:   "oci://repo.f5.com/charts",
		DockerRepo: "repo.f5.com/images",
		Version:    DefaultManifestVersion,
		FLOVersion: flo,
		HelmCharts: map[string]string{
			"f5-bigip-k8s-manifest": DefaultManifestVersion,
			"f5-lifecycle-operator": flo,
			"f5-spk-ingress":        "0.0.392",
			"f5-tmm-controller":     "0.0.392",
			"cert-gen":              "0.0.120",
		},
		DockerImgs: map[string]string{
			"f5-cne-controller": DefaultManifestVersion,
			"f5-tmm":            DefaultManifestVersion,
			"f5-cert-gen":       "0.0.120",
		},
		Components: []Component{
			{Name: "f5-bigip-k8s-manifest", FullName: "release/f5-bigip-k8s-manifest", ChartVersion: DefaultManifestVersion},
			{Name: "f5-lifecycle-operator", FullName: "charts/f5-lifecycle-operator", ChartVersion: flo},
			{Name: "f5-cne-controller", FullName: "charts/f5-cne-controller", ChartVersion: DefaultManifestVersion},
			{Name: "f5-tmm", FullName: "charts/f5-tmm", ChartVersion: DefaultManifestVersion},
		},
	}
}

// Probe inspects a BNK release manifest via local archive, live OCI pull (when saKeyJSON != ""),
// or embedded catalog fallback.
func Probe(ctx context.Context, version, archivePath string) (*ReleaseManifest, error) {
	if archivePath != "" {
		return ProbeArchive(archivePath)
	}
	m := GetEmbeddedManifest()
	if version != "" && version != m.Version {
		m.Version = version
		m.HelmCharts["f5-bigip-k8s-manifest"] = version
		if flo, ok := FLOChartFor(version); ok {
			m.FLOVersion = flo
			m.HelmCharts["f5-lifecycle-operator"] = flo
		}
	}
	return m, nil
}

// PrintSummary outputs a concise summary of the manifest.
func (m *ReleaseManifest) PrintSummary(w io.Writer) {
	fmt.Fprintf(w, "  F5 Helm Repo:   %s\n", m.HelmRepo)
	fmt.Fprintf(w, "  F5 Docker Repo: %s\n", m.DockerRepo)
	fmt.Fprintf(w, "  BOM Version:    %s\n", m.Version)
	if m.FLOVersion != "" {
		fmt.Fprintf(w, "  FLO Chart:      %s\n", m.FLOVersion)
	}
	fmt.Fprintf(w, "  Helm Charts:    %d\n", len(m.HelmCharts))
	fmt.Fprintf(w, "  Docker Images:  %d\n\n", len(m.DockerImgs))

	fmt.Fprintln(w, "Key Components:")
	keyCharts := []string{"f5-bigip-k8s-manifest", "f5-lifecycle-operator", "f5-spk-ingress", "f5-tmm-controller", "cert-gen"}
	for _, kc := range keyCharts {
		if v, ok := m.HelmCharts[kc]; ok {
			fmt.Fprintf(w, "  • %-25s %s\n", kc, v)
		}
	}
}

// PrintFullBOM outputs every helm chart and container image in the manifest.
func (m *ReleaseManifest) PrintFullBOM(w io.Writer) {
	fmt.Fprintln(w, "\nAll Helm Charts:")
	chartKeys := make([]string, 0, len(m.HelmCharts))
	for k := range m.HelmCharts {
		chartKeys = append(chartKeys, k)
	}
	sort.Strings(chartKeys)
	for _, k := range chartKeys {
		fmt.Fprintf(w, "  %-32s %s\n", k, m.HelmCharts[k])
	}

	fmt.Fprintln(w, "\nAll Container Images:")
	imgKeys := make([]string, 0, len(m.DockerImgs))
	for k := range m.DockerImgs {
		imgKeys = append(imgKeys, k)
	}
	sort.Strings(imgKeys)
	for _, k := range imgKeys {
		fmt.Fprintf(w, "  %-32s %s\n", k, m.DockerImgs[k])
	}
}
