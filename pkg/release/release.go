// Package release implements the manifest-driven and discovery-driven BNK
// version & capability engine.
//
// Zero cloud SDK imports are permitted in this package.
package release

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/discovery"
)

//go:embed releases.yaml
var embeddedReleasesYAML []byte

// NetworkModel constants describe the BNK dataplane CRD topology for a release train.
const (
	// NetworkModelLegacySPK is the BNK 2.3 F5SPKVlan + GatewayConfig model.
	NetworkModelLegacySPK = "legacy-spk"
	// NetworkModelGatewayInfra is the BNK 2.4+ Infra + GatewaySettings + L4Route model.
	NetworkModelGatewayInfra = "gateway-infra"
)

// Train represents a parsed BNK major.minor release train (e.g. 2.3, 2.4, 2.5).
type Train struct {
	Major int `json:"major" yaml:"major"`
	Minor int `json:"minor" yaml:"minor"`
}

// String returns "Major.Minor" (e.g. "2.4").
func (t Train) String() string {
	return fmt.Sprintf("%d.%d", t.Major, t.Minor)
}

// AtLeast reports whether t >= major.minor.
func (t Train) AtLeast(major, minor int) bool {
	if t.Major != major {
		return t.Major > major
	}
	return t.Minor >= minor
}

// Is23 reports whether the release train is 2.3.x.
func (t Train) Is23() bool {
	return t.Major == 2 && t.Minor == 3
}

// Is24OrLater reports whether the release train is 2.4 or later.
func (t Train) Is24OrLater() bool {
	return t.AtLeast(2, 4)
}

// ParseTrain extracts the Major.Minor release train from a BNK version string
// (e.g. "2.3.2-3.2598.3-0.0.464" -> {2, 3}, "2.4.0" -> {2, 4}, "v2.5.1" -> {2, 5}).
func ParseTrain(version string) Train {
	clean := strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if clean == "" {
		return Train{Major: 2, Minor: 4}
	}
	if idx := strings.IndexAny(clean, "-+"); idx >= 0 {
		clean = clean[:idx]
	}
	parts := strings.Split(clean, ".")
	if len(parts) < 2 {
		return Train{Major: 2, Minor: 4}
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return Train{Major: 2, Minor: 4}
	}
	return Train{Major: maj, Minor: min}
}

// Quirks declares release-specific or runtime-probed workarounds needed by the BNK overlay engine.
type Quirks struct {
	// EndpointSliceGetRBAC grants cne-controller-sa missing get verb on endpointslices (BNK 2.4.0 bug).
	EndpointSliceGetRBAC bool `json:"endpointSliceGetRBAC" yaml:"endpointSliceGetRBAC"`
	// EndpointSliceWeightResync forces BackendTrafficPolicy weight touch when endpointslices scale out.
	EndpointSliceWeightResync bool `json:"endpointSliceWeightResync" yaml:"endpointSliceWeightResync"`
	// RecreateCNEControllerOnNull deletes f5-cne-controller if env vars render as null before CNEInstance reconcile.
	RecreateCNEControllerOnNull bool `json:"recreateCNEControllerOnNull" yaml:"recreateCNEControllerOnNull"`
	// PodManagerHeal patches pod-manager webhook / probe configuration when needed.
	PodManagerHeal bool `json:"podManagerHeal" yaml:"podManagerHeal"`
	// DSSMInsecureOverlay patches dssm Redis sentinel TLS flags when needed.
	DSSMInsecureOverlay bool `json:"dssmInsecureOverlay" yaml:"dssmInsecureOverlay"`
	// MultusTokenWatch refreshes Multus serviceaccount token kubeconfig on rotation.
	MultusTokenWatch bool `json:"multusTokenWatch" yaml:"multusTokenWatch"`
	// CPCLKeySync synchronizes cpcl-key-secret from CWC's apiserver JWKS and reconciles f5-utils License CR.
	CPCLKeySync bool `json:"cpclKeySync" yaml:"cpclKeySync"`
}

// ReleaseProfile is the resolved capability & version profile for a BNK release.
type ReleaseProfile struct {
	Version                     string          `json:"version" yaml:"version"`
	Train                       Train           `json:"train" yaml:"train"`
	FLOChartVersion             string          `json:"floChartVersion" yaml:"floChartVersion"`
	NetworkModel                string          `json:"networkModel" yaml:"networkModel"`
	ServedGatewayAPIVersion     string          `json:"servedGatewayAPIVersion" yaml:"servedGatewayAPIVersion"`
	ServedInferenceAPIVersion   string          `json:"servedInferenceAPIVersion" yaml:"servedInferenceAPIVersion"`
	ServedF5InferenceAPIVersion string          `json:"servedF5InferenceAPIVersion,omitempty" yaml:"servedF5InferenceAPIVersion,omitempty"`
	Quirks                      Quirks          `json:"quirks" yaml:"quirks"`
	Components                  []bnk.Component `json:"components,omitempty" yaml:"components,omitempty"`
	ChartArchivePaths           []string        `json:"chartArchivePaths,omitempty" yaml:"chartArchivePaths,omitempty"`
}

type catalogFile struct {
	Releases map[string]ReleaseProfile `yaml:"releases"`
}

var (
	catalogOnce sync.Once
	catalogMap  map[string]ReleaseProfile
	catalogErr  error
)

func loadCatalog() (map[string]ReleaseProfile, error) {
	catalogOnce.Do(func() {
		var cf catalogFile
		if err := yaml.Unmarshal(embeddedReleasesYAML, &cf); err != nil {
			catalogErr = fmt.Errorf("parse embedded releases.yaml: %w", err)
			return
		}
		catalogMap = cf.Releases
	})
	return catalogMap, catalogErr
}

// IsCatalogued reports whether version is explicitly present in the embedded releases.yaml catalog.
func IsCatalogued(version string) bool {
	cat, err := loadCatalog()
	if err != nil {
		return false
	}
	_, ok := cat[strings.TrimSpace(version)]
	return ok
}

// Resolve builds the ReleaseProfile for the requested BNK version.
//
// Resolution order:
//  1. Embedded releases.yaml catalog match (or train-based default for unknown 2.3.x / 2.4.x / 2.5+ versions).
//  2. Dynamic OCI or local archive manifest inspection (if saKeyJSON or local archive path is provided).
//  3. Live K8s API discovery for served CRD APIVersions (if disc is non-nil).
func Resolve(ctx context.Context, version, saKeyOrArchive string, disc discovery.DiscoveryInterface) (*ReleaseProfile, error) {
	if strings.TrimSpace(version) == "" {
		version = bnk.DefaultManifestVersion
	}
	cat, err := loadCatalog()
	if err != nil {
		return nil, err
	}

	train := ParseTrain(version)
	var prof ReleaseProfile
	if exact, ok := cat[version]; ok {
		prof = exact
	} else {
		prof = defaultProfileForTrain(version, train, cat)
	}
	prof.Version = version
	prof.Train = train

	if saKeyOrArchive != "" {
		if strings.HasSuffix(saKeyOrArchive, ".tar.gz") || strings.HasSuffix(saKeyOrArchive, ".tgz") {
			if m, err := bnk.ProbeArchive(saKeyOrArchive); err == nil && m != nil {
				if m.FLOVersion != "" {
					prof.FLOChartVersion = m.FLOVersion
				}
				prof.Components = m.Components
			}
		} else if ctx != nil {
			if m, err := bnk.PullAndInspect(ctx, version, saKeyOrArchive); err == nil && m != nil {
				if m.FLOVersion != "" {
					prof.FLOChartVersion = m.FLOVersion
				}
				prof.Components = m.Components
			}
		}
	}

	if disc != nil {
		DiscoverServedAPIVersions(disc, &prof)
	}

	return &prof, nil
}

func defaultProfileForTrain(version string, train Train, cat map[string]ReleaseProfile) ReleaseProfile {
	if train.Is23() {
		if base, ok := cat["2.3.2-3.2598.3-0.0.464"]; ok {
			return base
		}
		return ReleaseProfile{
			Version:                   version,
			Train:                     train,
			FLOChartVersion:           "v2.21.13-0.0.58",
			NetworkModel:              NetworkModelLegacySPK,
			ServedGatewayAPIVersion:   "gateway.k8s.f5.com/v1alpha1",
			ServedInferenceAPIVersion: "inference.networking.x-k8s.io/v1alpha2",
			Quirks: Quirks{
				PodManagerHeal:      true,
				DSSMInsecureOverlay: true,
				MultusTokenWatch:    true,
				CPCLKeySync:         true,
			},
		}
	}
	if train.Major == 2 && train.Minor == 4 {
		clean := strings.TrimPrefix(strings.TrimSpace(version), "v")
		if strings.HasPrefix(clean, "2.4.1") {
			if base, ok := cat["2.4.1"]; ok {
				return base
			}
		}
		if base, ok := cat["2.4.0"]; ok {
			return base
		}
	}
	if train.AtLeast(2, 5) {
		if base, ok := cat["2.5.0"]; ok {
			return base
		}
		return ReleaseProfile{
			Version:                   version,
			Train:                     train,
			FLOChartVersion:           "v2.40.0-0.1.0",
			NetworkModel:              NetworkModelGatewayInfra,
			ServedGatewayAPIVersion:   "gateway.k8s.f5.com/v1",
			ServedInferenceAPIVersion: "inference.networking.k8s.io/v1",
			Quirks: Quirks{
				MultusTokenWatch: true,
				CPCLKeySync:      true,
			},
		}
	}
	return ReleaseProfile{
		Version:                   version,
		Train:                     train,
		FLOChartVersion:           "v2.30.0-0.5.2",
		NetworkModel:              NetworkModelGatewayInfra,
		ServedGatewayAPIVersion:   "gateway.k8s.f5.com/v1alpha1",
		ServedInferenceAPIVersion: "inference.networking.k8s.io/v1",
		Quirks: Quirks{
			EndpointSliceGetRBAC:        true,
			EndpointSliceWeightResync:   true,
			RecreateCNEControllerOnNull: true,
			PodManagerHeal:              true,
			DSSMInsecureOverlay:         true,
			MultusTokenWatch:            true,
			CPCLKeySync:                 true,
		},
	}
}
