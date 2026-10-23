package render

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
)

// DeepMergeMaps recursively merges src into dst and returns the merged map.
func DeepMergeMaps(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = make(map[string]any)
	}
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, exists := dst[k].(map[string]any); exists {
				dst[k] = DeepMergeMaps(dstMap, srcMap)
				continue
			}
			copied := make(map[string]any, len(srcMap))
			dst[k] = DeepMergeMaps(copied, srcMap)
			continue
		}
		dst[k] = v
	}
	return dst
}

// RenderUnifiedFLOValues renders the FLO Helm values YAML and deep-merges any
// pass-through cl.Bnk.FLOValues overrides.
func RenderUnifiedFLOValues(cl *intent.Cluster, underlay *provider.UnderlayContext, jwt string) ([]byte, error) {
	tmpl, err := manifests.FS.ReadFile("shared/flo-values.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("render unified flo values: read template: %w", err)
	}
	baseBytes, err := RenderFLOValues(tmpl, cl, jwt)
	if err != nil {
		return nil, err
	}
	var vals map[string]any
	if err := yaml.Unmarshal(baseBytes, &vals); err != nil {
		return nil, fmt.Errorf("render unified flo values: unmarshal base: %w", err)
	}
	if underlay != nil {
		switch underlay.CloudName {
		case intent.ProviderAzure:
			vals["containerPlatform"] = "Azure"
		case intent.ProviderGCP:
			vals["containerPlatform"] = "GCP"
		default:
			vals["containerPlatform"] = "AWS"
		}
	}
	if cl != nil && cl.Bnk != nil && len(cl.Bnk.FLOValues) > 0 {
		vals = DeepMergeMaps(vals, cl.Bnk.FLOValues)
	}
	return yaml.Marshal(vals)
}

// RenderUnifiedNADs renders NetworkAttachmentDefinitions from the UnderlayContext.
func RenderUnifiedNADs(underlay *provider.UnderlayContext, namespace string) ([]byte, error) {
	if underlay == nil {
		return nil, fmt.Errorf("render unified NADs: underlay is nil")
	}
	if namespace == "" {
		namespace = bnkconst.InstanceNamespace
	}
	var docs []string
	for _, netSpec := range underlay.Networks {
		cfg := netSpec.CNIConfigJSON
		if cfg == "" {
			cfg = fmt.Sprintf(`{"cniVersion":"0.3.1","name":%q,"type":"host-device","device":"eth1"}`, netSpec.Name)
		}
		doc := fmt.Sprintf(`apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: '%s'`, netSpec.Name, namespace, cfg)
		docs = append(docs, doc)
	}
	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

// RenderUnifiedCNEInstance renders a cloud-agnostic CNEInstance CR from the
// cluster intent, UnderlayContext, and ReleaseProfile, applying any
// cl.Bnk.ControllerEnv and cl.Bnk.TmmEnv pass-through overrides.
func RenderUnifiedCNEInstance(cl *intent.Cluster, underlay *provider.UnderlayContext, profile *release.ReleaseProfile) ([]byte, error) {
	if cl == nil || cl.Bnk == nil {
		return nil, fmt.Errorf("render unified CNEInstance: cl.Bnk is nil")
	}
	if underlay == nil {
		return nil, fmt.Errorf("render unified CNEInstance: underlay is nil")
	}
	if profile == nil {
		return nil, fmt.Errorf("render unified CNEInstance: release profile is nil")
	}

	ctrlEnv := []map[string]any{
		{"name": "CLOUD_ENV", "value": "true"},
	}
	switch underlay.CloudName {
	case intent.ProviderAWS:
		ctrlEnv = append(ctrlEnv,
			map[string]any{"name": "CLOUD_PROVIDER", "value": "aws"},
			map[string]any{"name": "PLATFORM_TYPE", "value": "AWS"},
		)
	case intent.ProviderAzure:
		ctrlEnv = append(ctrlEnv,
			map[string]any{"name": "CLOUD_PROVIDER", "value": "azure"},
			map[string]any{"name": "PLATFORM_TYPE", "value": "Azure"},
		)
	case intent.ProviderGCP:
		ctrlEnv = append(ctrlEnv,
			map[string]any{"name": "CLOUD_PROVIDER", "value": "gcp"},
			map[string]any{"name": "PLATFORM_TYPE", "value": "GCP"},
		)
	}
	if profile.Train.Is24OrLater() || profile.NetworkModel == release.NetworkModelGatewayInfra {
		ctrlEnv = append(ctrlEnv,
			map[string]any{"name": "USE_GATEWAY_SETTINGS", "value": "true"},
			map[string]any{"name": "MAX_ACTIVE_TMM_REPLICAS", "value": "32"},
		)
	}
	ctrlEnv = applyEnvOverrides(ctrlEnv, cl.Bnk.ControllerEnv)

	mtu := cl.Bnk.TmmMtu.Int()
	if mtu <= 0 {
		mtu = 1500
	}
	palCpu := cl.Bnk.PalCpuSet
	if palCpu == "" {
		palCpu = "0-3"
	}
	tmmEnv := []map[string]any{
		{"name": "PAL_CPU_SET", "value": palCpu},
		{"name": "DEFAULT_ROUTE_INTERFACE_MTU", "value": fmt.Sprintf("%d", mtu)},
	}
	if underlay.ServiceCIDR != "" {
		tmmEnv = append(tmmEnv, map[string]any{"name": TMMK8sRoutesEnv, "value": underlay.ServiceCIDR})
	}

	var nadNames []string
	for _, n := range underlay.Networks {
		if n.Name != "" {
			nadNames = append(nadNames, n.Name)
		}
	}

	switch underlay.DataplaneDriver {
	case provider.DriverSocket:
		claims := make([]string, 0, len(underlay.Networks))
		for i := range underlay.Networks {
			claims = append(claims, fmt.Sprintf("net%d", i+1))
		}
		if len(claims) == 0 {
			claims = []string{"net1"}
		}
		tmmEnv = append(tmmEnv,
			map[string]any{"name": "TMM_GENERIC_SOCKET_DRIVER", "value": "true"},
			map[string]any{"name": "TMM_MAPRES_ADDL_VETHS_ON_DP", "value": "TRUE"},
			map[string]any{"name": "TMM_MAPRES_IFC_CLAIM_LIST", "value": strings.Join(claims, ",")},
		)
	case provider.DriverVirtio:
		tmmEnv = append(tmmEnv,
			map[string]any{"name": "TMM_GENERIC_SOCKET_DRIVER", "value": "true"},
			map[string]any{"name": "TMM_MAPRES_ADDL_VETHS_ON_DP", "value": "TRUE"},
		)
		for i, n := range underlay.Networks {
			suffix := "EXT"
			if i > 0 || n.Role == "internal" {
				suffix = "INT"
			}
			pci := "0000:00:05.0"
			if i > 0 {
				pci = "0000:00:06.0"
			}
			for _, v := range n.NodeNICMap {
				pci = v
				break
			}
			tmmEnv = append(tmmEnv, map[string]any{
				"name":  "PCIDEVICE_INTEL_COM_" + suffix,
				"value": pci,
			})
		}
	default: // DriverDPDKPCI
		for i, n := range underlay.Networks {
			ifName := "ENS8"
			pci := "0000:00:08.0"
			if i > 0 || n.Role == "internal" {
				ifName = "ENS7"
				pci = "0000:00:07.0"
			}
			for _, v := range n.NodeNICMap {
				pci = v
				break
			}
			resEnv := fmt.Sprintf("ROBIN_VFIO_RESOURCE_%d", i+1)
			pciEnv := "PCIDEVICE_INTEL_COM_" + ifName
			tmmEnv = append(tmmEnv,
				map[string]any{"name": resEnv, "value": "intel.com/" + strings.ToLower(ifName)},
				map[string]any{"name": pciEnv, "value": pci},
			)
		}
	}
	if cl.Bnk.AIGateway != nil && cl.Bnk.AIGateway.MultiNUMASpanning {
		tmmEnv = append(tmmEnv, map[string]any{"name": "TMM_ENFORCE_SINGLE_NUMA_NODE", "value": "false"})
	}
	tmmEnv = applyEnvOverrides(tmmEnv, cl.Bnk.TmmEnv)

	spec := map[string]any{
		"type":               "BNK",
		"manifestVersion":    profile.Version,
		"deploymentSize":     normalizeDeploymentSize(cl.Bnk.DeploymentSize),
		"wholeCluster":       true,
		"gatewayAPI":         true,
		"networkAttachments": nadNames,
		"advanced": map[string]any{
			"cneController": map[string]any{
				"env": ctrlEnv,
			},
			"tmm": map[string]any{
				"env": tmmEnv,
			},
		},
	}
	if profile.Train.Is24OrLater() {
		spec["eppNamespaces"] = []string{"All"}
	}
	aiGwEnabled := isAIGatewayManifestVersion(profile.Version)
	aiMetrics := aiGwEnabled
	consoleURL := ""
	if cl.Bnk.AIGateway != nil {
		if cl.Bnk.AIGateway.Enabled {
			aiGwEnabled = true
		}
		aiMetrics = cl.Bnk.AIGateway.AIMetrics || aiGwEnabled
		consoleURL = strings.TrimSpace(cl.Bnk.AIGateway.ConsoleURL)
	}
	if aiGwEnabled {
		aiGwSpec := map[string]any{
			"enabled": true,
		}
		if consoleURL != "" {
			aiGwSpec["consoleURL"] = consoleURL
		} else if !isAIGatewayManifestVersion(profile.Version) {
			aiGwSpec["consoleURL"] = "http://f5-ai-console-webui.aigateway:8080/v1/services/default"
		}
		if aiMetrics {
			aiGwSpec["aiMetrics"] = true
		}
		spec["aiGateway"] = aiGwSpec
		spec["telemetry"] = map[string]any{
			"loggingSubsystem": map[string]any{
				"enabled": true,
			},
		}
	}

	obj := map[string]any{
		"apiVersion": "k8s.f5.com/v1",
		"kind":       "CNEInstance",
		"metadata": map[string]any{
			"name":      cl.Metadata.Name + "-bnk",
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": spec,
	}
	return yaml.Marshal(obj)
}

func applyEnvOverrides(base []map[string]any, overrides map[string]string) []map[string]any {
	if len(overrides) == 0 {
		return base
	}
	idxByName := make(map[string]int, len(base))
	for i, entry := range base {
		if name, ok := entry["name"].(string); ok {
			idxByName[name] = i
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if idx, exists := idxByName[k]; exists {
			base[idx]["value"] = overrides[k]
		} else {
			base = append(base, map[string]any{
				"name":  k,
				"value": overrides[k],
			})
		}
	}
	return base
}

// RenderUnifiedDataplane renders the dataplane network manifests for the given
// UnderlayContext and ReleaseProfile:
//   - NetworkModelGatewayInfra (2.4+): Infra (gateway.k8s.f5.com/<ServedGatewayAPIVersion>), GatewayClass
//   - NetworkModelLegacySPK (2.3): F5SPKVlan, F5SPKStaticRoute, GatewayConfig, GatewayClass
func RenderUnifiedDataplane(cl *intent.Cluster, underlay *provider.UnderlayContext, profile *release.ReleaseProfile) ([]byte, error) {
	if cl == nil {
		return nil, fmt.Errorf("render unified dataplane: cluster is nil")
	}
	if underlay == nil || len(underlay.Networks) == 0 {
		return nil, fmt.Errorf("render unified dataplane: underlay must define at least one network")
	}
	if profile == nil {
		return nil, fmt.Errorf("render unified dataplane: release profile is nil")
	}

	gwcName := cl.Metadata.Name + "-gatewayclass"
	if profile.NetworkModel == release.NetworkModelLegacySPK {
		return renderUnifiedLegacySPK(cl, underlay, gwcName)
	}
	return renderUnifiedGatewayInfra(cl, underlay, profile, gwcName)
}

func stripPrefix(ipWithOptionalPrefix string) string {
	if idx := strings.IndexByte(ipWithOptionalPrefix, '/'); idx >= 0 {
		return ipWithOptionalPrefix[:idx]
	}
	return ipWithOptionalPrefix
}

func renderUnifiedLegacySPK(cl *intent.Cluster, underlay *provider.UnderlayContext, gwcName string) ([]byte, error) {
	mtu := 1500
	if cl.Bnk != nil && cl.Bnk.TmmMtu.Int() > 0 {
		mtu = cl.Bnk.TmmMtu.Int()
	}
	var docs []string
	for i, netSpec := range underlay.Networks {
		prefixLen := 24
		if p, err := netip.ParsePrefix(netSpec.SubnetCIDR); err == nil {
			prefixLen = p.Bits()
		}
		isInternal := i > 0 || netSpec.Role == "internal"
		selfIP := "10.0.10.11"
		if len(netSpec.SelfIPs) > 0 {
			selfIP = stripPrefix(netSpec.SelfIPs[0])
		}
		vlanName := netSpec.Role
		if vlanName == "" {
			vlanName = netSpec.Name
		}
		vlanDoc := fmt.Sprintf(`apiVersion: k8s.f5net.com/v1
kind: F5SPKVlan
metadata:
  name: %s
  namespace: %s
spec:
  interfaces:
  - %q
  internal: %t
  mtu: %d
  prefixlen_v4: %d
  selfip_v4s:
  - %s
  tag: 0`, vlanName, bnkconst.InstanceNamespace, netSpec.TMMInterface, isInternal, mtu, prefixLen, selfIP)
		docs = append(docs, vlanDoc)

		if netSpec.GatewayIP != "" {
			rtDoc := fmt.Sprintf(`apiVersion: k8s.f5net.com/v1
kind: F5SPKStaticRoute
metadata:
  name: route-%s-gw
  namespace: %s
spec:
  destination: %s
  prefixLen: 32
  type: interface
  interface: %s`, vlanName, bnkconst.InstanceNamespace, netSpec.GatewayIP, vlanName)
			docs = append(docs, rtDoc)
		}
	}

	defaultNet := underlay.Networks[0].Role
	if defaultNet == "" {
		defaultNet = underlay.Networks[0].Name
	}
	gwcDoc := fmt.Sprintf(`apiVersion: k8s.f5.com/v1alpha1
kind: GatewayConfig
metadata:
  name: %s-gatewayconfig
  namespace: %s
spec:
  defaultNetwork: %s
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: %s
spec:
  controllerName: f5.com/f5-cne-system-f5-cne-controller
  parametersRef:
    group: k8s.f5.com
    kind: GatewayConfig
    name: %s-gatewayconfig
    namespace: %s`, cl.Metadata.Name, bnkconst.InstanceNamespace, defaultNet, gwcName, cl.Metadata.Name, bnkconst.InstanceNamespace)
	docs = append(docs, gwcDoc)

	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

func renderUnifiedGatewayInfra(cl *intent.Cluster, underlay *provider.UnderlayContext, profile *release.ReleaseProfile, gwcName string) ([]byte, error) {
	gwVer := profile.ServedGatewayAPIVersion
	if gwVer == "" {
		gwVer = "v1alpha1"
	}
	mtu := 1500
	if cl.Bnk != nil && cl.Bnk.TmmMtu.Int() > 0 {
		mtu = cl.Bnk.TmmMtu.Int()
	}

	var pools []map[string]any
	var attachments []map[string]any
	var networks []map[string]any

	for _, netSpec := range underlay.Networks {
		netName := netSpec.Role
		if netName == "" {
			netName = netSpec.Name
		}
		poolName := netName + "-selfip"
		rangeStart := "10.0.10.11"
		rangeEnd := "10.0.10.12"
		if len(netSpec.SelfIPs) > 0 {
			rangeStart = stripPrefix(netSpec.SelfIPs[0])
			rangeEnd = stripPrefix(netSpec.SelfIPs[len(netSpec.SelfIPs)-1])
		}
		cidr := netSpec.SubnetCIDR
		if cidr == "" {
			cidr = rangeStart + "/24"
		}
		pools = append(pools, map[string]any{
			"name": poolName,
			"cidrs": []map[string]any{
				{
					"cidr":       cidr,
					"rangeStart": rangeStart,
					"rangeEnd":   rangeEnd,
				},
			},
		})
		attachName := netName + "-attach"
		attachments = append(attachments, map[string]any{
			"name": attachName,
			"networkAttachmentDefinitionRef": map[string]any{
				"name": netSpec.Name,
			},
		})
		networks = append(networks, map[string]any{
			"name": netName,
			"type": "vlan",
			"mtu":  mtu,
			"selfIpPools": []map[string]any{
				{"name": poolName},
			},
			"networkAttachments": []map[string]any{
				{"name": attachName},
			},
		})
	}

	extNet := underlay.Networks[0]
	if extNet.SubnetCIDR != "" {
		vipStart := netcalc.IPAtOffset(extNet.SubnetCIDR, infraListenerFirst)
		vipEnd := netcalc.IPAtOffset(extNet.SubnetCIDR, infraListenerLast)
		if vipStart != "" && vipEnd != "" {
			pools = append(pools, map[string]any{
				"name": InfraListenerPool,
				"cidrs": []map[string]any{
					{
						"cidr":       extNet.SubnetCIDR,
						"rangeStart": vipStart,
						"rangeEnd":   vipEnd,
					},
				},
			})
		}
	}

	tunnelNet := underlay.Networks[0].Role
	if tunnelNet == "" {
		tunnelNet = underlay.Networks[0].Name
	}
	if len(underlay.Networks) > 1 {
		tunnelNet = underlay.Networks[1].Role
		if tunnelNet == "" {
			tunnelNet = underlay.Networks[1].Name
		}
	}

	infraObj := map[string]any{
		"apiVersion": "gateway.k8s.f5.com/" + gwVer,
		"kind":       "Infra",
		"metadata": map[string]any{
			"name":      InfraName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": map[string]any{
			"ipam": map[string]any{
				"pools": pools,
			},
			"networkAttachments": attachments,
			"networks":           networks,
			"egressDefaults": map[string]any{
				"type":   "vxlan",
				"subnet": InfraEgressSubnet,
				"port":   InfraEgressPort,
				"networkRef": map[string]any{
					"name": tunnelNet,
				},
			},
		},
	}

	infraYAML, err := yaml.Marshal(infraObj)
	if err != nil {
		return nil, fmt.Errorf("render unified infra: %w", err)
	}

	gwcObj := map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GatewayClass",
		"metadata": map[string]any{
			"name": gwcName,
		},
		"spec": map[string]any{
			"controllerName": "f5.com/f5-cne-system-f5-cne-controller",
			"parametersRef": map[string]any{
				"group":     "gateway.k8s.f5.com",
				"kind":      "Infra",
				"name":      InfraName,
				"namespace": bnkconst.InstanceNamespace,
			},
		},
	}
	gwcYAML, err := yaml.Marshal(gwcObj)
	if err != nil {
		return nil, fmt.Errorf("render unified gatewayclass: %w", err)
	}

	return []byte(string(infraYAML) + "---\n" + string(gwcYAML)), nil
}
