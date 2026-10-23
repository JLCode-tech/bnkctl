package provider_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkengine"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
)

// TestZeroCloudSDKImportsInPkg walks every Go source file under pkg/ and
// verifies that no file imports AWS, Azure, or GCP cloud SDKs.
func TestZeroCloudSDKImportsInPkg(t *testing.T) {
	forbiddenPrefixes := []string{
		"github.com/aws/",
		"github.com/Azure/azure-sdk-for-go/",
		"cloud.google.com/go/",
		"google.golang.org/api/",
	}

	pkgRoot := filepath.Clean("..")
	fset := token.NewFileSet()
	var scanned int

	err := filepath.WalkDir(pkgRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range forbiddenPrefixes {
				if strings.HasPrefix(impPath, prefix) {
					t.Errorf("forbidden cloud SDK import %q found in %s", impPath, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking pkg/ directory: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("expected to scan >0 Go files in %s", pkgRoot)
	}
	t.Logf("Verified zero cloud SDK imports across %d Go files in pkg/**", scanned)
}

// TestZeroKubectlExecCalls verifies that no production Go file in cmd/, internal/, or pkg/
// shells out to `kubectl` via exec.Command or exec.CommandContext.
func TestZeroKubectlExecCalls(t *testing.T) {
	roots := []string{
		filepath.Clean("../../cmd"),
		filepath.Clean("../../internal"),
		filepath.Clean(".."),
	}
	var scanned int
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			src := string(raw)
			if strings.Contains(src, `exec.Command("kubectl"`) || (strings.Contains(src, `"kubectl"`) && strings.Contains(src, "exec.Command")) {
				t.Errorf("forbidden kubectl subprocess exec found in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("expected to scan >0 production Go files")
	}
	t.Logf("Verified zero kubectl exec calls across %d production Go files", scanned)
}

// TestProviderOverlayContractMatrix validates the 3x3 (aws, azure, gcp) x (2.3, 2.4, simulated 2.5)
// contract between provider.UnderlayContext and bnkengine.Engine, including pass-through overrides.
func TestProviderOverlayContractMatrix(t *testing.T) {
	ctx := context.Background()

	clouds := []struct {
		name             string
		expectedDriver   provider.DataplaneDriver
		expectedExtIface string
		expectedIntIface string
		cl               *intent.Cluster
	}{
		{
			name:             intent.ProviderAWS,
			expectedDriver:   provider.DriverDPDKPCI,
			expectedExtIface: "1.1",
			expectedIntIface: "1.2",
			cl: &intent.Cluster{
				APIVersion: intent.APIVersion,
				Kind:       intent.Kind,
				Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
				Metadata:   intent.Metadata{Name: "aws-matrix", Region: "ap-southeast-2"},
				Pattern:    intent.PatternDualInterface,
				Network: intent.Network{
					VPCCidr: "10.90.0.0/16",
					DataPath: &intent.DataPathSpec{
						External: intent.SubnetSpec{CIDR: "10.90.100.0/24"},
						Internal: intent.SubnetSpec{CIDR: "10.90.101.0/24"},
					},
				},
			},
		},
		{
			name:             intent.ProviderAzure,
			expectedDriver:   provider.DriverSocket,
			expectedExtIface: "0.2",
			expectedIntIface: "0.3",
			cl: &intent.Cluster{
				APIVersion: intent.APIVersion,
				Kind:       intent.Kind,
				Cloud:      &intent.CloudSpec{Provider: intent.ProviderAzure},
				Metadata:   intent.Metadata{Name: "azr-matrix", Location: "eastus2", ResourceGroup: "rg-matrix"},
				Pattern:    intent.PatternDualInterface,
				Network: intent.Network{
					VNetCIDR: "10.90.0.0/16",
					Subnets: intent.SubnetsSpec{
						External: "10.90.3.0/24",
						Internal: "10.90.4.0/24",
					},
				},
			},
		},
		{
			name:             intent.ProviderGCP,
			expectedDriver:   provider.DriverVirtio,
			expectedExtIface: "1.1",
			expectedIntIface: "1.2",
			cl: &intent.Cluster{
				APIVersion: intent.APIVersion,
				Kind:       intent.Kind,
				Cloud:      &intent.CloudSpec{Provider: intent.ProviderGCP},
				Metadata:   intent.Metadata{Name: "gcp-matrix", Region: "us-central1", Project: "p-matrix", Zone: "us-central1-a"},
				Pattern:    intent.PatternDualInterface,
				Network: intent.Network{
					Main:     intent.MainNetworkSpec{CIDR: "10.90.1.0/24", PodsCIDR: "10.92.0.0/14", SvcsCIDR: "10.96.0.0/20"},
					External: intent.DataplaneSpec{CIDR: "10.90.2.0/24"},
					Internal: intent.DataplaneSpec{CIDR: "10.90.3.0/24"},
				},
			},
		},
	}

	versions := []struct {
		ver         string
		expectedFam string
		expectedMod string
	}{
		{
			ver:         "2.3.2-3.2598.3-0.0.464",
			expectedFam: "2.3",
			expectedMod: release.NetworkModelLegacySPK,
		},
		{
			ver:         "2.4.0",
			expectedFam: "2.4",
			expectedMod: release.NetworkModelGatewayInfra,
		},
		{
			ver:         "2.5.1-rc1", // forward-compatible uncatalogued 2.5 release resolved dynamically
			expectedFam: "2.5",
			expectedMod: release.NetworkModelGatewayInfra,
		},
	}

	for _, c := range clouds {
		for _, v := range versions {
			t.Run(c.name+"_"+v.ver, func(t *testing.T) {
				clCopy := *c.cl
				clCopy.Bnk = &intent.BnkSpec{
					ManifestVersion: v.ver,
					ControllerEnv:   map[string]string{"FEATURE_X_ENABLED": "true"},
					TmmEnv:          map[string]string{"TMM_DEBUG_LEVEL": "2"},
					FLOValues:       map[string]any{"customFlag": true},
					ExtraManifests: []string{
						"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: custom-extra\n  namespace: f5-bnk\n",
					},
				}

				u, err := provider.BuildUnderlayFromIntent(&clCopy)
				if err != nil {
					t.Fatalf("BuildUnderlayFromIntent: %v", err)
				}
				if u.DataplaneDriver != c.expectedDriver {
					t.Fatalf("DataplaneDriver = %q, want %q", u.DataplaneDriver, c.expectedDriver)
				}
				if len(u.Networks) != 2 {
					t.Fatalf("expected 2 dataplane networks, got %d", len(u.Networks))
				}
				if u.Networks[0].TMMInterface != c.expectedExtIface {
					t.Fatalf("external TMMInterface = %q, want %q", u.Networks[0].TMMInterface, c.expectedExtIface)
				}
				if u.Networks[1].TMMInterface != c.expectedIntIface {
					t.Fatalf("internal TMMInterface = %q, want %q", u.Networks[1].TMMInterface, c.expectedIntIface)
				}

				prof, err := release.Resolve(ctx, v.ver, "", nil)
				if err != nil {
					t.Fatalf("release.Resolve: %v", err)
				}
				if prof.Train.String() != v.expectedFam {
					t.Fatalf("prof.Train = %q, want %q", prof.Train.String(), v.expectedFam)
				}
				if prof.NetworkModel != v.expectedMod {
					t.Fatalf("prof.NetworkModel = %q, want %q", prof.NetworkModel, v.expectedMod)
				}

				eng, err := bnkengine.New(&clCopy, nil, u, prof)
				if err != nil {
					t.Fatalf("bnkengine.New: %v", err)
				}

				rendered, err := bnkengine.RenderAll(&clCopy, u, prof, "contract-test-jwt")
				if err != nil {
					t.Fatalf("RenderAll: %v", err)
				}
				all := string(rendered.AllManifests())
				cneYAML := string(rendered.CNEInstanceYAML)
				if !strings.Contains(cneYAML, "FEATURE_X_ENABLED") {
					t.Errorf("expected CNEInstanceYAML to contain ControllerEnv pass-through FEATURE_X_ENABLED")
				}
				if !strings.Contains(cneYAML, "TMM_DEBUG_LEVEL") {
					t.Errorf("expected CNEInstanceYAML to contain TmmEnv pass-through TMM_DEBUG_LEVEL")
				}
				if c.name == intent.ProviderAzure {
					if !strings.Contains(cneYAML, "TMM_GENERIC_SOCKET_DRIVER") {
						t.Errorf("expected Azure CNEInstanceYAML to contain TMM_GENERIC_SOCKET_DRIVER=true, got:\n%s", cneYAML)
					}
				}
				if !strings.Contains(string(rendered.FLOValuesYAML), "customFlag") {
					t.Errorf("expected FLOValuesYAML to contain FLOValues pass-through customFlag")
				}
				if !strings.Contains(all, "name: custom-extra") {
					t.Errorf("expected AllManifests to contain ExtraManifests custom-extra")
				}
				if v.expectedMod == release.NetworkModelLegacySPK {
					if !strings.Contains(all, "kind: F5SPKVlan") {
						t.Errorf("expected 2.3 LegacySPK manifests to contain F5SPKVlan")
					}
				} else {
					gwYAML := string(rendered.NetworkCRsYAML) + "\n" + string(rendered.EgressYAML)
					for _, required := range []string{
						"kind: Infra",
						"kind: GatewaySettings",
						"kind: EgressGateway",
						"ipams:",
						"networkAttachments:",
						"type: vlan",
						"staticRoutes:",
						"sourceNATPools:",
						"gatewayClassName:",
						"parametersRef:",
					} {
						if !strings.Contains(gwYAML, required) {
							t.Errorf("expected %s GatewayInfra manifests to contain %q", v.ver, required)
						}
					}
					if v.expectedFam == "2.4" && !strings.Contains(gwYAML, "apiVersion: gateway.k8s.f5.com/v1alpha1") {
						t.Errorf("expected 2.4 GatewayInfra manifests to use apiVersion gateway.k8s.f5.com/v1alpha1")
					}
					if v.expectedFam == "2.5" && !strings.Contains(gwYAML, "apiVersion: gateway.k8s.f5.com/v1\n") {
						t.Errorf("expected 2.5 GatewayInfra manifests to use promoted apiVersion gateway.k8s.f5.com/v1")
					}
				}

				if _, err := eng.Install(ctx, bnkengine.Options{DryRun: true}); err != nil {
					t.Fatalf("Install dry-run: %v", err)
				}
				if _, err := eng.Upgrade(ctx, bnkengine.Options{DryRun: true, TargetVersion: "2.5.0"}); err != nil {
					t.Fatalf("Upgrade dry-run: %v", err)
				}
				if _, err := eng.Migrate(ctx, bnkengine.Options{DryRun: true, TargetVersion: "2.5.0", MigrateInPlace: true}); err != nil {
					t.Fatalf("Migrate dry-run: %v", err)
				}
				if err := eng.Uninstall(ctx, bnkengine.Options{DryRun: true}); err != nil {
					t.Fatalf("Uninstall dry-run: %v", err)
				}
			})
		}
	}
}
