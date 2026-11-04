package provider_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/bnkengine"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
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
					expectedPodSubnet := "subnet: " + strings.TrimSpace(strings.Split(u.PodCIDR, ",")[0])
					if !strings.Contains(string(rendered.NetworkCRsYAML), expectedPodSubnet) {
						t.Errorf("expected %s Infra egressDefaults to contain %q, got:\n%s", v.ver, expectedPodSubnet, string(rendered.NetworkCRsYAML))
					}
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

// TestDataplaneTraitsContract verifies that EnsureTraits populates the canonical
// underlay capability traits for AWS (DriverDPDKPCI), Azure (DriverSocket), and GCP (DriverVirtio).
func TestDataplaneTraitsContract(t *testing.T) {
	awsU := &provider.UnderlayContext{
		CloudName:       intent.ProviderAWS,
		DataplaneDriver: provider.DriverDPDKPCI,
	}
	awsTraits := awsU.EnsureTraits()
	if !awsTraits.UseVFIOHostDevice || !awsTraits.EmitSNATEgressPool || !awsTraits.IncludeClusterDNSInTMMResolver || awsTraits.DefaultClusterDNSIP != bnkconst.DefaultAWSClusterDNSIP || awsTraits.DefaultPodCIDR != provider.DefaultAWSPodCIDR || awsTraits.DefaultExtPCIBDF != provider.DefaultAWSExtPCIBDF || awsTraits.DefaultIntPCIBDF != provider.DefaultAWSIntPCIBDF {
		t.Fatalf("unexpected AWS traits: %+v", awsTraits)
	}

	azrU := &provider.UnderlayContext{
		CloudName:       intent.ProviderAzure,
		DataplaneDriver: provider.DriverSocket,
	}
	azrTraits := azrU.EnsureTraits()
	if !azrTraits.UseKernelSocketMode || !azrTraits.RenderDefaultNamespaceNADs || !azrTraits.UseShortInfraNetworkNames || !azrTraits.OmitTMMInterfacesAndSkipNICs || azrTraits.DefaultClusterDNSIP != bnkconst.DefaultAzureClusterDNSIP || azrTraits.DefaultRouteDomain0MTU != 1390 || azrTraits.DefaultPalCPUSet != "0,2" || azrTraits.DefaultPodCIDR != provider.DefaultAzurePodCIDR {
		t.Fatalf("unexpected Azure traits: %+v", azrTraits)
	}

	gcpU := &provider.UnderlayContext{
		CloudName:       intent.ProviderGCP,
		DataplaneDriver: provider.DriverVirtio,
	}
	gcpTraits := gcpU.EnsureTraits()
	if !gcpTraits.UseVirtioMode || !gcpTraits.UseCalicoVXLANGateway || !gcpTraits.SuppressDefaultRouteOnExternal || !gcpTraits.PreserveHostDeviceIPs || !gcpTraits.IncludeNodeCIDRInK8sRoutes || gcpTraits.DefaultMTU != 1460 || gcpTraits.DefaultPodCIDR != provider.DefaultGCPPodCIDR || gcpTraits.DefaultExtPCIBDF != provider.DefaultGCPExtPCIBDF || gcpTraits.DefaultIntPCIBDF != provider.DefaultGCPIntPCIBDF {
		t.Fatalf("unexpected GCP traits: %+v", gcpTraits)
	}
}

// TestZeroCloudBranchingAndInlinePCILiteralsInRenderAndEngine ensures that
// pkg/k8s/render and pkg/bnkengine do not branch directly on cloud names
// (intent.ProviderAWS/ProviderAzure/ProviderGCP or u.CloudName) and do not
// hardcode PCI BDF literals instead of using DataplaneTraits or provider constants.
func TestZeroCloudBranchingAndInlinePCILiteralsInRenderAndEngine(t *testing.T) {
	roots := []string{
		filepath.Clean("../k8s/render"),
		filepath.Clean("../bnkengine"),
	}
	forbiddenTokens := []string{
		"intent.ProviderAWS",
		"intent.ProviderAzure",
		"intent.ProviderGCP",
		"EqualFold(u.CloudName",
		"u.CloudName ==",
		"u.CloudName !=",
		"switch u.CloudName",
		`["PLATFORM_TYPE"] ==`,
		`"0000:00:05.0"`,
		`"0000:00:06.0"`,
		`"0000:00:07.0"`,
		`"0000:00:08.0"`,
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
			for _, tok := range forbiddenTokens {
				if strings.Contains(src, tok) {
					t.Errorf("forbidden cloud-branching or inline PCI literal %s found in %s", tok, path)
				}
			}
			if strings.Contains(filepath.ToSlash(path), "k8s/render") && strings.Contains(src, "u.CloudName") {
				t.Errorf("forbidden u.CloudName reference found in render package %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("expected to scan >0 production files in pkg/k8s/render and pkg/bnkengine")
	}
}

// TestInfraEgressDefaultsSubnetFollowsPodCIDRAndTraits verifies that
// RenderUnifiedNetworkAndEgress populates Infra.spec.egressDefaults.subnet from
// u.PodCIDR (or traits.DefaultPodCIDR / provider.DefaultAWSPodCIDR fallback).
func TestInfraEgressDefaultsSubnetFollowsPodCIDRAndTraits(t *testing.T) {
	ctx := context.Background()
	prof, err := release.Resolve(ctx, "2.4.1", "", nil)
	if err != nil {
		t.Fatalf("release.Resolve: %v", err)
	}

	cases := []struct {
		name           string
		cloud          string
		driver         provider.DataplaneDriver
		podCIDR        string
		expectedSubnet string
	}{
		{
			name:           "aws_default_pod_cidr",
			cloud:          intent.ProviderAWS,
			driver:         provider.DriverDPDKPCI,
			podCIDR:        provider.DefaultAWSPodCIDR,
			expectedSubnet: provider.DefaultAWSPodCIDR,
		},
		{
			name:           "azure_pod_cidr",
			cloud:          intent.ProviderAzure,
			driver:         provider.DriverSocket,
			podCIDR:        provider.DefaultAzurePodCIDR,
			expectedSubnet: provider.DefaultAzurePodCIDR,
		},
		{
			name:           "gcp_pod_cidr",
			cloud:          intent.ProviderGCP,
			driver:         provider.DriverVirtio,
			podCIDR:        provider.DefaultGCPPodCIDR,
			expectedSubnet: provider.DefaultGCPPodCIDR,
		},
		{
			name:           "custom_comma_separated_pod_cidr",
			cloud:          intent.ProviderAWS,
			driver:         provider.DriverDPDKPCI,
			podCIDR:        "172.28.0.0/16, 172.29.0.0/16",
			expectedSubnet: "172.28.0.0/16",
		},
		{
			name:           "empty_pod_cidr_falls_back_to_traits",
			cloud:          intent.ProviderAzure,
			driver:         provider.DriverSocket,
			podCIDR:        "",
			expectedSubnet: provider.DefaultAzurePodCIDR,
		},
		{
			name:           "empty_pod_cidr_and_unknown_cloud_falls_back_to_aws_default",
			cloud:          "",
			driver:         provider.DriverDPDKPCI,
			podCIDR:        "",
			expectedSubnet: provider.DefaultAWSPodCIDR,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &provider.UnderlayContext{
				ClusterName:     "egress-subnet-test",
				CloudName:       tc.cloud,
				DataplaneDriver: tc.driver,
				PodCIDR:         tc.podCIDR,
				Networks: []provider.NetworkAttachmentSpec{
					{
						Name:         provider.NetNameExternal,
						Role:         provider.NetRoleExternal,
						SubnetCIDR:   "10.90.100.0/24",
						GatewayIP:    "10.90.100.1",
						SelfIPs:      []string{"10.90.100.10/24"},
						TMMInterface: "1.1",
					},
				},
			}
			cl := &intent.Cluster{
				Metadata: intent.Metadata{Name: "egress-subnet-test"},
			}
			netYAML, _, err := render.RenderUnifiedNetworkAndEgress(cl, u, prof)
			if err != nil {
				t.Fatalf("RenderUnifiedNetworkAndEgress: %v", err)
			}
			wantLine := "subnet: " + tc.expectedSubnet
			if !strings.Contains(string(netYAML), wantLine) {
				t.Errorf("expected Infra YAML to contain %q, got:\n%s", wantLine, string(netYAML))
			}
		})
	}
}

// TestBuildAIGWDNSResolverUsesDataplaneTraits verifies that BuildAIGWDNSResolverObject
// selects DefaultClusterDNSIP from DataplaneTraits without checking u.CloudName directly.
func TestBuildAIGWDNSResolverUsesDataplaneTraits(t *testing.T) {
	extractFirstDNSIP := func(t *testing.T, obj map[string]any) string {
		t.Helper()
		spec, _ := obj["spec"].(map[string]any)
		nr, _ := spec["netResolver"].(map[string]any)
		fzs, _ := nr["forwardZones"].([]any)
		if len(fzs) == 0 {
			t.Fatalf("missing forwardZones in %+v", obj)
		}
		fz, _ := fzs[0].(map[string]any)
		ns, _ := fz["nameServers"].([]any)
		if len(ns) == 0 {
			t.Fatalf("missing nameServers in %+v", obj)
		}
		first, _ := ns[0].(map[string]any)
		ip, _ := first["ipAddress"].(string)
		return ip
	}

	// 1. AWS traits with empty ServiceCIDR -> DefaultAWSClusterDNSIP (172.20.0.10)
	awsU := &provider.UnderlayContext{
		CloudName:       intent.ProviderAWS,
		DataplaneDriver: provider.DriverDPDKPCI,
	}
	if got := extractFirstDNSIP(t, render.BuildAIGWDNSResolverObject(nil, awsU, "")); got != bnkconst.DefaultAWSClusterDNSIP {
		t.Errorf("AWS DNS IP = %q, want %q", got, bnkconst.DefaultAWSClusterDNSIP)
	}

	// 2. Azure traits with empty ServiceCIDR -> DefaultAzureClusterDNSIP (10.240.0.10)
	azrU := &provider.UnderlayContext{
		CloudName:       intent.ProviderAzure,
		DataplaneDriver: provider.DriverSocket,
	}
	if got := extractFirstDNSIP(t, render.BuildAIGWDNSResolverObject(nil, azrU, "")); got != bnkconst.DefaultAzureClusterDNSIP {
		t.Errorf("Azure DNS IP = %q, want %q", got, bnkconst.DefaultAzureClusterDNSIP)
	}

	// 3. GCP traits (DriverVirtio) omit cluster DNS forwarder and start with public forwarder
	gcpU := &provider.UnderlayContext{
		CloudName:       intent.ProviderGCP,
		DataplaneDriver: provider.DriverVirtio,
	}
	if got := extractFirstDNSIP(t, render.BuildAIGWDNSResolverObject(nil, gcpU, "")); got != bnkconst.DefaultPublicDNSForwarders[0] {
		t.Errorf("GCP first DNS IP = %q, want %q", got, bnkconst.DefaultPublicDNSForwarders[0])
	}

	// 4. Custom traits with custom DefaultClusterDNSIP and empty CloudName
	customU := &provider.UnderlayContext{
		DataplaneDriver: provider.DriverDPDKPCI,
		Traits: provider.DataplaneTraits{
			Initialized:                    true,
			IncludeClusterDNSInTMMResolver: true,
			DefaultClusterDNSIP:            "10.250.0.10",
		},
	}
	if got := extractFirstDNSIP(t, render.BuildAIGWDNSResolverObject(nil, customU, "")); got != "10.250.0.10" {
		t.Errorf("custom traits DNS IP = %q, want 10.250.0.10", got)
	}

	// 5. Empty DefaultClusterDNSIP falls back to bnkconst.DefaultK8sClusterDNSIP
	fallbackU := &provider.UnderlayContext{
		DataplaneDriver: provider.DriverDPDKPCI,
		Traits: provider.DataplaneTraits{
			Initialized:                    true,
			IncludeClusterDNSInTMMResolver: true,
			DefaultClusterDNSIP:            "",
		},
	}
	if got := extractFirstDNSIP(t, render.BuildAIGWDNSResolverObject(nil, fallbackU, "")); got != bnkconst.DefaultK8sClusterDNSIP {
		t.Errorf("fallback DNS IP = %q, want %q", got, bnkconst.DefaultK8sClusterDNSIP)
	}
}

// TestSubnetPrefixSuffixAndCustomMasksInBuildUnderlay verifies that SubnetPrefixSuffix
// and BuildUnderlayFromIntent preserve custom subnet masks (/20, /22, /26, /28) across
// AWS, Azure, and GCP instead of hardcoding /24.
func TestSubnetPrefixSuffixAndCustomMasksInBuildUnderlay(t *testing.T) {
	if got := provider.SubnetPrefixSuffix("10.90.16.0/20", "/24"); got != "/20" {
		t.Fatalf("SubnetPrefixSuffix(/20) = %q, want /20", got)
	}
	if got := provider.SubnetPrefixSuffix("10.90.16.10", "/24"); got != "/24" {
		t.Fatalf("SubnetPrefixSuffix(no slash) = %q, want /24", got)
	}

	// AWS with /22 external and /26 internal
	awsCl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata: intent.Metadata{Name: "aws-custom-mask", Region: "us-west-2"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.90.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.90.4.0/22"},
				Internal: intent.SubnetSpec{CIDR: "10.90.8.0/26"},
			},
		},
	}
	awsU, err := provider.BuildUnderlayFromIntent(awsCl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent(aws): %v", err)
	}
	if got := awsU.ExternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/22") {
		t.Errorf("AWS external SelfIP = %q, want /22 suffix", got)
	}
	if got := awsU.ExternalNetwork().FloatingIP; !strings.HasSuffix(got, "/22") {
		t.Errorf("AWS external FloatingIP = %q, want /22 suffix", got)
	}
	if got := awsU.InternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/26") {
		t.Errorf("AWS internal SelfIP = %q, want /26 suffix", got)
	}
	if got := awsU.InternalNetwork().FloatingIP; !strings.HasSuffix(got, "/26") {
		t.Errorf("AWS internal FloatingIP = %q, want /26 suffix", got)
	}

	// Azure with /20 external and /28 internal
	azrCl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderAzure},
		Metadata: intent.Metadata{Name: "azr-custom-mask", Location: "eastus2", ResourceGroup: "rg"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			VNetCIDR: "10.90.0.0/16",
			Subnets: intent.SubnetsSpec{
				External: "10.90.16.0/20",
				Internal: "10.90.32.0/28",
			},
		},
	}
	azrU, err := provider.BuildUnderlayFromIntent(azrCl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent(azure): %v", err)
	}
	if got := azrU.ExternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/20") {
		t.Errorf("Azure external SelfIP = %q, want /20 suffix", got)
	}
	if got := azrU.InternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/28") {
		t.Errorf("Azure internal SelfIP = %q, want /28 suffix", got)
	}

	// GCP with /22 external and /28 internal
	gcpCl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderGCP},
		Metadata: intent.Metadata{Name: "gcp-custom-mask", Region: "us-central1", Project: "p", Zone: "us-central1-a"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			Main:     intent.MainNetworkSpec{CIDR: "10.90.1.0/24"},
			External: intent.DataplaneSpec{CIDR: "10.90.4.0/22"},
			Internal: intent.DataplaneSpec{CIDR: "10.90.8.0/28"},
		},
	}
	gcpU, err := provider.BuildUnderlayFromIntent(gcpCl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent(gcp): %v", err)
	}
	if got := gcpU.ExternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/22") {
		t.Errorf("GCP external SelfIP = %q, want /22 suffix", got)
	}
	if got := gcpU.InternalNetwork().SelfIPs[0]; !strings.HasSuffix(got, "/28") {
		t.Errorf("GCP internal SelfIP = %q, want /28 suffix", got)
	}
}

// TestPCIBDFConstantsAndCNEInstanceRendering verifies that RenderUnifiedCNEInstance
// uses DataplaneTraits.DefaultExtPCIBDF / DefaultIntPCIBDF (reconciled with
// provider.DefaultAWSExtPCIBDF/DefaultAWSIntPCIBDF and DefaultGCPExtPCIBDF/DefaultGCPIntPCIBDF).
func TestPCIBDFConstantsAndCNEInstanceRendering(t *testing.T) {
	ctx := context.Background()
	prof, err := release.Resolve(ctx, "2.4.1", "", nil)
	if err != nil {
		t.Fatalf("release.Resolve: %v", err)
	}

	awsCl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata: intent.Metadata{Name: "aws-pci-test", Region: "us-west-2"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.90.100.0/24"},
				Internal: intent.SubnetSpec{CIDR: "10.90.101.0/24"},
			},
		},
	}
	awsU, err := provider.BuildUnderlayFromIntent(awsCl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent(aws): %v", err)
	}
	awsYAML, err := render.RenderUnifiedCNEInstance(awsCl, awsU, prof)
	if err != nil {
		t.Fatalf("RenderUnifiedCNEInstance(aws): %v", err)
	}
	if !strings.Contains(string(awsYAML), provider.DefaultAWSExtPCIBDF) || !strings.Contains(string(awsYAML), provider.DefaultAWSIntPCIBDF) {
		t.Errorf("expected AWS CNEInstance YAML to contain %s and %s, got:\n%s", provider.DefaultAWSExtPCIBDF, provider.DefaultAWSIntPCIBDF, string(awsYAML))
	}

	gcpCl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderGCP},
		Metadata: intent.Metadata{Name: "gcp-pci-test", Region: "us-central1", Project: "p", Zone: "us-central1-a"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			Main:     intent.MainNetworkSpec{CIDR: "10.90.1.0/24"},
			External: intent.DataplaneSpec{CIDR: "10.90.2.0/24"},
			Internal: intent.DataplaneSpec{CIDR: "10.90.3.0/24"},
		},
	}
	gcpU, err := provider.BuildUnderlayFromIntent(gcpCl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent(gcp): %v", err)
	}
	gcpYAML, err := render.RenderUnifiedCNEInstance(gcpCl, gcpU, prof)
	if err != nil {
		t.Fatalf("RenderUnifiedCNEInstance(gcp): %v", err)
	}
	if !strings.Contains(string(gcpYAML), provider.DefaultGCPExtPCIBDF) || !strings.Contains(string(gcpYAML), provider.DefaultGCPIntPCIBDF) {
		t.Errorf("expected GCP CNEInstance YAML to contain %s and %s, got:\n%s", provider.DefaultGCPExtPCIBDF, provider.DefaultGCPIntPCIBDF, string(gcpYAML))
	}
}
