package render

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	"sigs.k8s.io/yaml"
)

// RenderUnifiedNamespaces renders the multi-doc Namespace YAML stream for all standard
// BNK namespaces plus the optional test namespace.
func RenderUnifiedNamespaces(testNS string) ([]byte, error) {
	if strings.TrimSpace(testNS) == "" {
		testNS = "default"
	}
	nsList := append([]string{}, bnkconst.DefaultNamespaces...)
	hasTestNS := testNS == "default"
	for _, existing := range nsList {
		if existing == testNS {
			hasTestNS = true
			break
		}
	}
	if !hasTestNS {
		nsList = append(nsList, testNS)
	}
	var docs []string
	for _, ns := range nsList {
		doc := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns)
		docs = append(docs, doc)
	}
	return []byte(strings.Join(docs, "---\n")), nil
}

// RenderUnifiedNADs renders the NetworkAttachmentDefinitions in f5-cne-system
// (and default when traits.RenderDefaultNamespaceNADs is true) from the normalized UnderlayContext.
func RenderUnifiedNADs(u *provider.UnderlayContext) ([]byte, error) {
	if u == nil || len(u.Networks) == 0 {
		return nil, fmt.Errorf("render NADs: underlay has no networks")
	}
	traits := u.EnsureTraits()
	var docs []string
	nadNamespaces := []string{bnkconst.InstanceNamespace}
	if traits.RenderDefaultNamespaceNADs {
		nadNamespaces = []string{bnkconst.InstanceNamespace, "default"}
	}
	for _, ns := range nadNamespaces {
		for _, netSpec := range u.Networks {
			cniJSON := strings.TrimSpace(netSpec.CNIConfigJSON)
			if cniJSON == "" {
				cniType := "macvlan"
				if traits.UseVFIOHostDevice {
					cniType = "host-device"
				}
				raw, _ := json.Marshal(map[string]any{
					"cniVersion": provider.CNIVersionDefault,
					"name":       netSpec.Name,
					"type":       cniType,
				})
				cniJSON = string(raw)
			}
			obj := map[string]any{
				"apiVersion": "k8s.cni.cncf.io/v1",
				"kind":       "NetworkAttachmentDefinition",
				"metadata": map[string]any{
					"name":      netSpec.Name,
					"namespace": ns,
				},
				"spec": map[string]any{
					"config": cniJSON,
				},
			}
			b, err := yaml.Marshal(obj)
			if err != nil {
				return nil, fmt.Errorf("marshal NAD %s (%s): %w", netSpec.Name, ns, err)
			}
			docs = append(docs, strings.TrimSpace(string(b)))
		}
	}
	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

// RenderUnifiedCNEInstance renders the canonical CNEInstance CR for AWS (DriverDPDKPCI),
// Azure (DriverSocket), and GCP (DriverVirtio), driven by UnderlayContext.EnsureTraits()
// and ReleaseProfile.
func RenderUnifiedCNEInstance(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("underlay context is nil")
	}
	traits := u.EnsureTraits()

	depSize := "Small"
	storageClass := u.StorageClass
	if storageClass == "" {
		storageClass = provider.DefaultGCPStorageClass
	}
	mtu := bnkconst.DefaultMTU
	if ext := u.ExternalNetwork(); ext != nil && ext.MTU > 0 {
		mtu = ext.MTU
	} else if traits.DefaultMTU > 0 {
		mtu = traits.DefaultMTU
	}
	tmmCPU := "2"
	tmmMem := "8Gi"
	tmmHuge := "2Gi"
	palCPUSet := traits.DefaultPalCPUSet
	if palCPUSet == "" {
		palCPUSet = "0-1"
	}
	imageReg := "repo.f5.com"
	if cl != nil && cl.Bnk != nil {
		if cl.Bnk.DeploymentSize != "" {
			depSize = normalizeDeploymentSize(cl.Bnk.DeploymentSize)
		}
		if cl.Bnk.StorageClassName != "" {
			storageClass = cl.Bnk.StorageClassName
		}
		if cl.Bnk.TmmMtuInt() > 0 {
			mtu = cl.Bnk.TmmMtuInt()
		}
		if cl.Bnk.TmmCpu != "" {
			tmmCPU = cl.Bnk.TmmCpu
		}
		if cl.Bnk.TmmMemory != "" {
			tmmMem = cl.Bnk.TmmMemory
		}
		if cl.Bnk.TmmHugepages != "" {
			tmmHuge = cl.Bnk.TmmHugepages
		}
		if cl.Bnk.PalCpuSet != "" {
			palCPUSet = cl.Bnk.PalCpuSet
		}
		if cl.Bnk.ImageRegistry != "" {
			imageReg = cl.Bnk.ImageRegistry
		}
	}
	if traits.UseVFIOHostDevice && mtu > bnkconst.DefaultMTU {
		mtu = bnkconst.DefaultMTU
	}

	var nadNames []string
	var tmmIfaces []string
	for _, n := range u.Networks {
		nadNames = append(nadNames, n.Name)
		if n.TMMInterface != "" {
			tmmIfaces = append(tmmIfaces, n.TMMInterface)
		}
	}
	if len(tmmIfaces) == 0 {
		if traits.UseVFIOHostDevice || traits.UseVirtioMode {
			tmmIfaces = []string{provider.TMMInterfaceDPDKExt}
		} else {
			tmmIfaces = []string{provider.TMMInterfaceSocketExt}
		}
	}

	// Build TMM environment variables
	tmmEnvMap := map[string]string{
		"TMM_DEFAULT_MTU":   fmt.Sprintf("%d", mtu),
		"ZEBOS_STATE":       "legacy",
		"TMM_SOCAT_DISABLE": "1",
	}
	if !traits.UseVirtioMode {
		tmmEnvMap["TMM_MTU"] = fmt.Sprintf("%d", mtu)
		if !traits.OmitTMMInterfacesAndSkipNICs {
			tmmEnvMap["TMM_INTERFACES"] = strings.Join(tmmIfaces, ",")
		}
		tmmEnvMap["PAL_CPU_SET"] = palCPUSet
		tmmEnvMap["TMM_CALICO_ROUTER"] = "default"
		var k8sRoutes []string
		if u.ServiceCIDR != "" {
			k8sRoutes = append(k8sRoutes, u.ServiceCIDR)
		}
		if u.PodCIDR != "" {
			k8sRoutes = append(k8sRoutes, u.PodCIDR)
		}
		if len(k8sRoutes) > 0 {
			tmmEnvMap[TMMK8sRoutesEnv] = strings.Join(k8sRoutes, ",")
		}
	}
	claimList := "net1"
	if len(u.Networks) > 1 {
		claimList = "net1,net2"
	}
	switch {
	case traits.UseKernelSocketMode:
		if !traits.OmitTMMInterfacesAndSkipNICs {
			tmmEnvMap["TMM_SKIP_NICS"] = "eth0"
		}
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_MAPRES_IFC_CLAIM_LIST"] = claimList
	case traits.UseVirtioMode:
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_MAPRES_IFC_CLAIM_LIST"] = claimList
		var k8sRoutes []string
		for _, cidr := range []string{u.ServiceCIDR, u.PodCIDR, u.NodeCIDR} {
			if trimmed := strings.TrimSpace(cidr); trimmed != "" {
				k8sRoutes = append(k8sRoutes, trimmed)
			}
		}
		if len(k8sRoutes) > 0 {
			tmmEnvMap["TMM_CALICO_ROUTER"] = "default"
			tmmEnvMap[TMMK8sRoutesEnv] = strings.Join(k8sRoutes, ",")
		}
		extPCI := traits.DefaultExtPCIBDF
		if extPCI == "" {
			extPCI = provider.DefaultGCPExtPCIBDF
		}
		intPCI := traits.DefaultIntPCIBDF
		if intPCI == "" {
			intPCI = provider.DefaultGCPIntPCIBDF
		}
		if len(u.DataplaneNodes) > 0 {
			for idx, nic := range u.DataplaneNodes[0].NICs {
				if (nic.NICIndex == 1 || (nic.NICIndex == 0 && idx == 0)) && nic.PCIAddress != "" {
					extPCI = nic.PCIAddress
				}
				if (nic.NICIndex == 2 || (nic.NICIndex == 0 && idx == 1)) && nic.PCIAddress != "" {
					intPCI = nic.PCIAddress
				}
			}
		}
		tmmEnvMap["PCIDEVICE_INTEL_COM_EXT"] = extPCI
		if len(u.Networks) > 1 {
			tmmEnvMap["PCIDEVICE_INTEL_COM_INT"] = intPCI
		}
	default: // traits.UseVFIOHostDevice
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_SOCAT_DISABLE"] = "1"
		extIf := provider.DefaultAWSLinuxExtNIC
		defaultExtPCI := traits.DefaultExtPCIBDF
		if defaultExtPCI == "" {
			defaultExtPCI = provider.DefaultAWSExtPCIBDF
		}
		extPCI := defaultExtPCI
		intIf := provider.DefaultAWSLinuxIntNIC
		defaultIntPCI := traits.DefaultIntPCIBDF
		if defaultIntPCI == "" {
			defaultIntPCI = provider.DefaultAWSIntPCIBDF
		}
		intPCI := defaultIntPCI
		extNetName := provider.NetNameExternal
		if ext := u.ExternalNetwork(); ext != nil && ext.Name != "" {
			extNetName = ext.Name
		}
		intNetName := provider.NetNameInternal
		if intl := u.InternalNetwork(); intl != nil && intl.Name != "" {
			intNetName = intl.Name
		}
		if len(u.DataplaneNodes) > 0 {
			for idx, nic := range u.DataplaneNodes[0].NICs {
				isExt := nic.NICIndex == 1 || nic.TMMInterface == provider.TMMInterfaceDPDKExt || nic.NetworkName == extNetName || nic.NetworkName == "external-nad" || (nic.NICIndex == 0 && nic.NetworkName == "" && idx == 0)
				isInt := nic.NICIndex == 2 || nic.TMMInterface == provider.TMMInterfaceDPDKInt || nic.NetworkName == intNetName || nic.NetworkName == "internal-nad" || (nic.NICIndex == 0 && nic.NetworkName == "" && idx == 1)
				if isExt {
					if nic.LinuxIfName != "" {
						extIf = nic.LinuxIfName
					}
					if nic.PCIAddress != "" {
						extPCI = nic.PCIAddress
					}
				}
				if isInt {
					if nic.LinuxIfName != "" {
						intIf = nic.LinuxIfName
					}
					if nic.PCIAddress != "" {
						intPCI = nic.PCIAddress
					}
				}
			}
		}
		if extPCI == defaultExtPCI {
			if ext := u.ExternalNetwork(); ext != nil && len(ext.NodeNICMap) > 0 {
				for _, pci := range ext.NodeNICMap {
					if pci != "" {
						extPCI = pci
						break
					}
				}
			}
		}
		if intPCI == defaultIntPCI {
			if intl := u.InternalNetwork(); intl != nil && len(intl.NodeNICMap) > 0 {
				for _, pci := range intl.NodeNICMap {
					if pci != "" {
						intPCI = pci
						break
					}
				}
			}
		}
		extIfUpper := strings.ToUpper(strings.ReplaceAll(extIf, "-", "_"))
		tmmEnvMap["ROBIN_VFIO_RESOURCE_1"] = extIfUpper
		tmmEnvMap["PCIDEVICE_INTEL_COM_"+extIfUpper] = extPCI
		if len(u.Networks) > 1 {
			intIfUpper := strings.ToUpper(strings.ReplaceAll(intIf, "-", "_"))
			tmmEnvMap["ROBIN_VFIO_RESOURCE_2"] = intIfUpper
			tmmEnvMap["PCIDEVICE_INTEL_COM_"+intIfUpper] = intPCI
		}
	}
	if cl != nil && cl.Bnk != nil {
		if cl.Bnk.AIGateway != nil && cl.Bnk.AIGateway.MultiNUMASpanning {
			tmmEnvMap["TMM_ENFORCE_SINGLE_NUMA_NODE"] = "false"
		}
		for k, v := range cl.Bnk.TmmEnv {
			tmmEnvMap[k] = v
		}
	}

	// Build Controller environment variables
	ctrlEnvMap := map[string]string{
		"TMM_DEFAULT_MTU": fmt.Sprintf("%d", mtu),
	}
	if prof != nil && prof.Train.Is24OrLater() {
		ctrlEnvMap["USE_GATEWAY_SETTINGS"] = "true"
		ctrlEnvMap["MAX_ACTIVE_TMM_REPLICAS"] = "32"
		if cl != nil && cl.Bnk != nil && (cl.Bnk.BGP || cl.Bnk.DynamicRouting) {
			ctrlEnvMap["ZEBOS_STATE"] = "legacy"
		}
	}
	if cl != nil && cl.Bnk != nil {
		for k, v := range cl.Bnk.ControllerEnv {
			ctrlEnvMap[k] = v
		}
	}

	tmmEnvList := mapToEnvSlice(tmmEnvMap)
	ctrlEnvList := mapToEnvSlice(ctrlEnvMap)

	cvars := CertChainVarsFromCluster(cl)
	dynRoutingEnabled := cl != nil && cl.Bnk != nil && (cl.Bnk.BGP || cl.Bnk.DynamicRouting)

	envDiscSpec := map[string]any{
		"enabled": false,
	}
	demoModeEnabled := false
	if traits.UseKernelSocketMode || traits.UseVirtioMode {
		envDiscSpec = map[string]any{
			"runAfterSuccess": true,
			"stopOnFail":      true,
		}
		demoModeEnabled = true
	}

	tmmAdv := map[string]any{
		"env": tmmEnvList,
	}
	hasExplicitTMMRes := cl != nil && cl.Bnk != nil && (cl.Bnk.TmmCpu != "" || cl.Bnk.TmmMemory != "" || cl.Bnk.TmmHugepages != "")
	if !traits.UseVirtioMode || hasExplicitTMMRes {
		reqCPU := tmmCPU
		reqMem := tmmMem
		if traits.ConservativeTMMResourceRequests && !hasExplicitTMMRes {
			reqCPU = "1"
			reqMem = "4Gi"
		}
		tmmAdv["resources"] = map[string]any{
			"limits": map[string]any{
				"cpu":           tmmCPU,
				"memory":        tmmMem,
				"hugepages-2Mi": tmmHuge,
			},
			"requests": map[string]any{
				"cpu":           reqCPU,
				"memory":        reqMem,
				"hugepages-2Mi": tmmHuge,
			},
		}
	}
	advancedSpec := map[string]any{
		"coremon": map[string]any{
			"hostPath": true,
		},
		"envDiscovery": envDiscSpec,
		"demoMode": map[string]any{
			"enabled": demoModeEnabled,
		},
		"maintenanceMode": map[string]any{
			"enabled": false,
		},
		"tmm": tmmAdv,
	}
	if len(ctrlEnvList) > 0 {
		advancedSpec["cneController"] = map[string]any{
			"env": ctrlEnvList,
		}
	}

	manifestVersion := ""
	if prof != nil {
		manifestVersion = prof.Version
	}

	cneSpec := map[string]any{
		"product": map[string]any{
			"gatewayAPI": true,
			"type":       "BNK",
		},
		"manifestVersion":  manifestVersion,
		"deploymentSize":   depSize,
		"wholeCluster":     true,
		"storageClassName": storageClass,
		"certificate": map[string]any{
			"clusterIssuer": cvars.CAIssuer,
		},
		"registry": map[string]any{
			"uri": imageReg,
			"imagePullSecrets": []map[string]any{
				{"name": bnkconst.SecretRegistry},
			},
			"imagePullPolicy": "Always",
		},
		"networkAttachments": nadNames,
		"dynamicRouting": map[string]any{
			"enabled": dynRoutingEnabled,
		},
		"firewallACL": map[string]any{
			"enabled": true,
		},
		"pseudoCNI": map[string]any{
			"enabled": true,
		},
		"coreCollection": map[string]any{
			"enabled": true,
		},
		"intelligentLB": map[string]any{
			"enabled": true,
		},
		"tmmReplicas": 1,
		"telemetry": map[string]any{
			"loggingSubsystem": map[string]any{
				"enabled": true,
			},
			"metricSubsystem": map[string]any{
				"enabled": true,
			},
		},
		"advanced": advancedSpec,
	}
	if prof != nil && prof.Train.Is24OrLater() {
		cneSpec["eppNamespaces"] = []string{"All"}
	}
	aiGwEnabled := strings.HasPrefix(manifestVersion, "2.4.1") || strings.HasPrefix(manifestVersion, "2.4.2") || strings.HasPrefix(manifestVersion, "2.5")
	aiMetrics := aiGwEnabled
	consoleURL := ""
	if cl != nil && cl.Bnk != nil && cl.Bnk.AIGateway != nil {
		aiGwEnabled = cl.Bnk.AIGateway.Enabled
		aiMetrics = cl.Bnk.AIGateway.AIMetrics || cl.Bnk.AIGateway.Enabled
		consoleURL = strings.TrimSpace(cl.Bnk.AIGateway.ConsoleURL)
	}
	if aiGwEnabled {
		if consoleURL == "" {
			consoleURL = "http://f5-ai-console-webui." + bnkconst.NamespaceAIGateway + ":8080/v1/services/default"
		}
		cneSpec["aiGateway"] = map[string]any{
			"enabled":    true,
			"aiMetrics":  aiMetrics,
			"consoleURL": consoleURL,
		}
	}

	cneName := "bnk"
	if cl != nil && cl.EffectiveName() != "" {
		cneName = cl.EffectiveName() + "-bnk"
	}

	obj := map[string]any{
		"apiVersion": "k8s.f5.com/v1",
		"kind":       "CNEInstance",
		"metadata": map[string]any{
			"name":      cneName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": cneSpec,
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal CNEInstance: %w", err)
	}
	return b, nil
}

// RenderUnifiedLicenseCR renders the License CR across f5-cne-system, f5-cne-core,
// and f5-utils so CWC reconciles regardless of which namespace it watches.
func RenderUnifiedLicenseCR(cl *intent.Cluster, jwtToken string, teemCertURL string) ([]byte, error) {
	if teemCertURL == "" {
		teemCertURL = bnkconst.DefaultTEEMCertURL
	}
	var docs []string
	for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.CoreNamespace, bnkconst.UtilsNamespace} {
		obj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "License",
			"metadata": map[string]any{
				"name":      bnkconst.CRLicenseName,
				"namespace": ns,
			},
			"spec": map[string]any{
				"operationMode": "connected",
				"jwt":           strings.TrimSpace(jwtToken),
				"teemCertUrl":   teemCertURL,
			},
		}
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal License CR (%s): %w", ns, err)
		}
		docs = append(docs, strings.TrimSpace(string(b)))
	}
	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

// RenderUnifiedGatewayClass renders the cluster-specific GatewayClass plus the
// canonical f5-gateway-class CR.
func RenderUnifiedGatewayClass(cl *intent.Cluster) ([]byte, error) {
	tmpl, err := manifests.FS.ReadFile("host-device/gatewayclass.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read host-device/gatewayclass.yaml.tmpl: %w", err)
	}
	primary, err := RenderGatewayClass(tmpl, cl)
	if err != nil {
		return nil, err
	}
	canonical := fmt.Sprintf("apiVersion: gateway.networking.k8s.io/v1\nkind: GatewayClass\nmetadata:\n  name: %s\nspec:\n  controllerName: f5.com/f5-cne-system-f5-cne-controller\n", bnkconst.CanonicalGatewayClassName)
	if cl != nil && cl.EffectiveName()+"-gatewayclass" != bnkconst.CanonicalGatewayClassName {
		return []byte(strings.TrimSpace(string(primary)) + "\n---\n" + canonical), nil
	}
	return primary, nil
}

// RenderUnifiedNetworkAndEgress renders both BNK 2.4+ Gateway-Infra CRs (Infra +
// GatewaySettings + EgressGateway + optional tmm-init ConfigMap + F5BigDnsCache)
// and BNK 2.3 Legacy SPK CRs (F5SPKVlan + F5SPKStaticRoute + F5SPKSnatpool + F5SPKEgress)
// from the normalized UnderlayContext and ReleaseProfile.
func RenderUnifiedNetworkAndEgress(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, []byte, error) {
	if u == nil {
		return nil, nil, fmt.Errorf("render network CRs: underlay is nil")
	}
	traits := u.EnsureTraits()
	ext := u.ExternalNetwork()
	if ext == nil {
		return nil, nil, fmt.Errorf("render network CRs: external network is nil")
	}
	intNet := u.InternalNetwork()
	mtu := ext.MTU
	if mtu <= 0 {
		if traits.DefaultMTU > 0 {
			mtu = traits.DefaultMTU
		} else {
			mtu = bnkconst.DefaultMTU
		}
	}
	if traits.UseVFIOHostDevice && mtu > bnkconst.DefaultMTU {
		mtu = bnkconst.DefaultMTU
	}

	extStart, extEnd, extPrefLen := ipRangeFromSelfIPs(ext.SelfIPs, ext.SubnetCIDR)
	snatStart, _ := snatRangeFromSubnet(ext.SubnetCIDR, extStart, extEnd)

	if prof != nil && prof.NetworkModel == release.NetworkModelLegacySPK {
		var docs []string
		extVLAN := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKVlan",
			"metadata": map[string]any{
				"name":      provider.NetRoleExternal,
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"name":      provider.NetRoleExternal,
				"interface": ext.TMMInterface,
				"mtu":       mtu,
				"selfips":   stripMasks(ext.SelfIPs),
				"prefixlen": extPrefLen,
			},
		}
		b, err := yaml.Marshal(extVLAN)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(b)))

		if intNet != nil {
			intVLAN := map[string]any{
				"apiVersion": "k8s.f5net.com/v1",
				"kind":       "F5SPKVlan",
				"metadata": map[string]any{
					"name":      provider.NetRoleInternal,
					"namespace": bnkconst.InstanceNamespace,
				},
				"spec": map[string]any{
					"name":      provider.NetRoleInternal,
					"interface": intNet.TMMInterface,
					"mtu":       mtu,
					"selfips":   stripMasks(intNet.SelfIPs),
					"prefixlen": prefixLenFromCIDR(intNet.SubnetCIDR, 24),
				},
			}
			ib, err := yaml.Marshal(intVLAN)
			if err != nil {
				return nil, nil, err
			}
			docs = append(docs, strings.TrimSpace(string(ib)))
		}

		defRoute := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKStaticRoute",
			"metadata": map[string]any{
				"name":      "default-route",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"destination": "0.0.0.0",
				"prefixlen":   0,
				"gateway":     ext.GatewayIP,
				"vlanName":    provider.NetRoleExternal,
			},
		}
		rb, err := yaml.Marshal(defRoute)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(rb)))

		snatObj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKSnatpool",
			"metadata": map[string]any{
				"name":      GatewaySettingsSnatPool,
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"name": GatewaySettingsSnatPool,
				"addressList": [][]string{
					{snatStart},
				},
			},
		}
		sb, err := yaml.Marshal(snatObj)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(sb)))

		egressObj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKEgress",
			"metadata": map[string]any{
				"name":      "egress",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"dualStackEnabled": false,
				"snatpoolName":     GatewaySettingsSnatPool,
			},
		}
		eb, err := yaml.Marshal(egressObj)
		if err != nil {
			return nil, nil, err
		}
		return []byte(strings.Join(docs, "\n---\n") + "\n"), eb, nil
	}

	// BNK 2.4+ / 2.5+ Gateway-Infra network model (gateway.k8s.f5.com/v1 or v1alpha1)
	gwAPIVer := "gateway.k8s.f5.com/v1"
	if prof != nil && prof.ServedGatewayAPIVersion != "" {
		gwAPIVer = prof.ServedGatewayAPIVersion
	}

	az := ""
	if cl != nil && cl.Bnk != nil && provider.ShouldIncludeIPAMAvailabilityZone(cl.Bnk.ControllerEnv) {
		if cl.Metadata.Zone != "" {
			az = cl.Metadata.Zone
		} else if len(cl.Network.AZs) > 0 {
			az = cl.Network.AZs[0]
		}
	}

	listenerStart, listenerEnd := extStart, extEnd
	if len(ext.SelfIPs) > 0 && strings.Contains(ext.SelfIPs[0], "/") {
		if ip, ipNet, err := net.ParseCIDR(ext.SelfIPs[0]); err == nil && ipNet != nil {
			ones, bits := ipNet.Mask.Size()
			if bits == 32 && ones >= 28 {
				if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
					hostCount := byte(1 << (32 - ones))
					extStart = net.IPv4(v4[0], v4[1], v4[2], v4[3]).String()
					extEnd = net.IPv4(v4[0], v4[1], v4[2], v4[3]+2).String()
					listenerStart = net.IPv4(v4[0], v4[1], v4[2], v4[3]+3).String()
					listenerEnd = net.IPv4(v4[0], v4[1], v4[2], v4[3]+hostCount-2).String()
				}
			}
		}
	}
	extPool := map[string]any{
		"rangeStart": extStart,
		"rangeEnd":   extEnd,
	}
	if az != "" {
		extPool["availabilityZone"] = az
	}
	if listenerStart == extStart && listenerEnd == extEnd {
		if ip, ipNet, err := net.ParseCIDR(ext.SubnetCIDR); err == nil && ipNet != nil {
			if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
				listenerStart = net.IPv4(v4[0], v4[1], v4[2], 100).String()
				listenerEnd = net.IPv4(v4[0], v4[1], v4[2], 199).String()
			}
		}
	}
	listenerPool := map[string]any{
		"rangeStart": listenerStart,
		"rangeEnd":   listenerEnd,
	}
	if az != "" {
		listenerPool["availabilityZone"] = az
	}
	ipams := []map[string]any{
		{
			"name": InfraExtIPAM,
			"ipPools": []map[string]any{
				extPool,
			},
		},
		{
			"name": "listener-pool",
			"ipPools": []map[string]any{
				listenerPool,
			},
		},
	}

	extNetName := InfraExtNetwork
	intNetName := InfraIntNetwork
	if traits.UseShortInfraNetworkNames {
		extNetName = provider.NetRoleExternal
		intNetName = provider.NetRoleInternal
	}

	netAttachments := []map[string]any{
		{
			"name": "ext-attach",
			"networkAttachmentDefinitions": []map[string]any{
				{"name": ext.Name},
			},
		},
	}

	networks := []map[string]any{
		{
			"name": extNetName,
			"type": "vlan",
			"vlan": map[string]any{
				"tag": 0,
				"mtu": mtu,
				"ipamRefs": []map[string]any{
					{"name": InfraExtIPAM},
				},
				"networkAttachmentRef": map[string]any{
					"name": "ext-attach",
				},
			},
		},
	}

	if intNet != nil {
		intStart, intEnd, _ := ipRangeFromSelfIPs(intNet.SelfIPs, intNet.SubnetCIDR)
		if len(intNet.SelfIPs) > 0 && strings.Contains(intNet.SelfIPs[0], "/") {
			if ip, ipNet, err := net.ParseCIDR(intNet.SelfIPs[0]); err == nil && ipNet != nil {
				ones, bits := ipNet.Mask.Size()
				if bits == 32 && ones >= 28 {
					if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
						intStart = net.IPv4(v4[0], v4[1], v4[2], v4[3]).String()
						intEnd = net.IPv4(v4[0], v4[1], v4[2], v4[3]+2).String()
					}
				}
			}
		}
		intPool := map[string]any{
			"rangeStart": intStart,
			"rangeEnd":   intEnd,
		}
		if az != "" {
			intPool["availabilityZone"] = az
		}
		ipams = append(ipams, map[string]any{
			"name": InfraIntIPAM,
			"ipPools": []map[string]any{
				intPool,
			},
		})
		netAttachments = append(netAttachments, map[string]any{
			"name": "int-attach",
			"networkAttachmentDefinitions": []map[string]any{
				{"name": intNet.Name},
			},
		})
		networks = append(networks, map[string]any{
			"name": intNetName,
			"type": "vlan",
			"vlan": map[string]any{
				"tag": 0,
				"mtu": mtu,
				"ipamRefs": []map[string]any{
					{"name": InfraIntIPAM},
				},
				"networkAttachmentRef": map[string]any{
					"name": "int-attach",
				},
			},
		})
	}

	staticRoutes := []map[string]any{}
	if ext.GatewayIP != "" {
		if traits.UseVirtioMode {
			staticRoutes = append(staticRoutes, map[string]any{
				"name":         "route-ext-gw",
				"destinations": []string{ext.GatewayIP + "/32"},
				"networkRef": map[string]any{
					"name": extNetName,
				},
			})
		}
		staticRoutes = append(staticRoutes, map[string]any{
			"name":         InfraRouteDefault,
			"destinations": []string{"0.0.0.0/0"},
			"nextHop":      ext.GatewayIP,
		})
	}
	clusterEastWestGW := ext.GatewayIP
	if intNet != nil && intNet.GatewayIP != "" {
		clusterEastWestGW = intNet.GatewayIP
	} else if traits.UseCalicoVXLANGateway {
		clusterEastWestGW = bnkconst.CalicoVXLANLinkLocalGW
	}
	if u.ServiceCIDR != "" && clusterEastWestGW != "" {
		staticRoutes = append(staticRoutes, map[string]any{
			"name":         "service-cidr",
			"destinations": []string{u.ServiceCIDR},
			"nextHop":      clusterEastWestGW,
		})
	}
	var podCIDRs []string
	if cl != nil && len(cl.Network.Subnets.PrivateList) > 0 {
		for _, sub := range cl.Network.Subnets.PrivateList {
			if trimmed := strings.TrimSpace(sub.CIDR); trimmed != "" {
				podCIDRs = append(podCIDRs, trimmed)
			}
		}
	}
	if len(podCIDRs) == 0 && u.PodCIDR != "" {
		for _, part := range strings.Split(u.PodCIDR, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				podCIDRs = append(podCIDRs, trimmed)
			}
		}
	}
	if len(podCIDRs) > 0 && clusterEastWestGW != "" {
		staticRoutes = append(staticRoutes, map[string]any{
			"name":         "pod-cidr",
			"destinations": podCIDRs,
			"nextHop":      clusterEastWestGW,
		})
	}
	if traits.IncludeNodeCIDRInK8sRoutes && intNet == nil && strings.TrimSpace(u.NodeCIDR) != "" {
		staticRoutes = append(staticRoutes, map[string]any{
			"name":         "mgmt-nodes",
			"destinations": []string{strings.TrimSpace(u.NodeCIDR)},
			"nextHop":      clusterEastWestGW,
		})
	}

	egressNet := extNetName
	if intNet != nil && !traits.EgressPrefersExternalNetwork {
		egressNet = intNetName
	}

	egressSubnet := ""
	for _, part := range strings.Split(u.PodCIDR, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			egressSubnet = trimmed
			break
		}
	}
	if egressSubnet == "" {
		if traits.DefaultPodCIDR != "" {
			egressSubnet = traits.DefaultPodCIDR
		} else {
			egressSubnet = provider.DefaultAWSPodCIDR
		}
	}

	infraSpec := map[string]any{
		"ipams":              ipams,
		"networkAttachments": netAttachments,
		"networks":           networks,
		"egressDefaults": map[string]any{
			"networkRef": map[string]any{
				"name": egressNet,
			},
			"port":   bnkconst.DefaultVXLANPort,
			"subnet": egressSubnet,
		},
	}
	if len(staticRoutes) > 0 {
		infraSpec["staticRoutes"] = staticRoutes
	}

	infraObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "Infra",
		"metadata": map[string]any{
			"name":      InfraName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": infraSpec,
	}
	infraBytes, err := yaml.Marshal(infraObj)
	if err != nil {
		return nil, nil, err
	}

	gwSettingsSpec := map[string]any{
		"sourceNATPools": []map[string]any{
			{
				"name": GatewaySettingsSnatPool,
				"ipamRefs": []map[string]any{
					{"name": InfraExtIPAM},
				},
			},
		},
		"egressConfigs": []map[string]any{
			{
				"name": GatewaySettingsEgressCfg,
				"networkRef": map[string]any{
					"name": egressNet,
				},
				"sourceNATConfig": map[string]any{
					"type": "Automap",
				},
			},
		},
		"ingressConfig": map[string]any{
			"defaultListenerNetwork": map[string]any{
				"ipamRefs": []map[string]any{
					{"name": "listener-pool"},
				},
				"networkRefs": []map[string]any{
					{"name": extNetName},
				},
				"sourceNATConfig": map[string]any{
					"type": "Automap",
				},
			},
		},
	}

	gwSettingsObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "GatewaySettings",
		"metadata": map[string]any{
			"name":      GatewaySettingsName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": gwSettingsSpec,
	}
	gwsBytes, err := yaml.Marshal(gwSettingsObj)
	if err != nil {
		return nil, nil, err
	}

	testNS := effectiveTestNamespaceForRender(cl)
	matchNames := []string{testNS}
	if traits.IncludeDefaultInEgressMatchNames && testNS != "default" {
		matchNames = append(matchNames, "default")
	}
	gwClassName := "bnk-gatewayclass"
	if cl != nil && cl.EffectiveName() != "" {
		gwClassName = cl.EffectiveName() + "-gatewayclass"
	}
	egressObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "EgressGateway",
		"metadata": map[string]any{
			"name":      EgressGatewayName,
			"namespace": testNS,
		},
		"spec": map[string]any{
			"gatewayClassName": gwClassName,
			"infrastructure": map[string]any{
				"parametersRef": map[string]any{
					"name":        GatewaySettingsName,
					"sectionName": GatewaySettingsEgressCfg,
				},
			},
			"sourceSelector": map[string]any{
				"selectionMode": "NamespaceSelector",
				"namespaces": map[string]any{
					"matchNames": matchNames,
				},
			},
		},
	}
	egressBytes, err := yaml.Marshal(egressObj)
	if err != nil {
		return nil, nil, err
	}
	if testNS != bnkconst.InstanceNamespace {
		testGWSObj := map[string]any{
			"apiVersion": gwAPIVer,
			"kind":       "GatewaySettings",
			"metadata": map[string]any{
				"name":      GatewaySettingsName,
				"namespace": testNS,
			},
			"spec": map[string]any{
				"egressConfigs": []map[string]any{
					{
						"name": GatewaySettingsEgressCfg,
						"networkRef": map[string]any{
							"name": egressNet,
						},
						"sourceNATConfig": map[string]any{
							"type": "Automap",
						},
					},
				},
			},
		}
		if testGWSBytes, mErr := yaml.Marshal(testGWSObj); mErr == nil {
			egressBytes = []byte(strings.TrimSpace(string(testGWSBytes)) + "\n---\n" + strings.TrimSpace(string(egressBytes)) + "\n")
		}
	}

	netCombined := strings.TrimSpace(string(infraBytes)) + "\n---\n" + strings.TrimSpace(string(gwsBytes)) + "\n"
	if traits.EmitTMMInitConfigMap {
		tmmInitTcl := `set_from_env  POD_IP  POD_IP  [info hostname]

    profile tcp nagledisable {
      default_name tcp
      nagle disable
    }

    profile tcp dia-tcp {
      default_name tcp
    }

    profile sctp dia-sctp {
      default_name sctp
    }

    profile http http_socket {
      default_name http
    }
    
    pool pool1 {

    }
    
    snatpool overlay_pool {

    }

    file_reload {
      file_name /opt/lib/tmm/user_conf.tcl
      interval 1
    }

    profile genericmsg gm_proto_for_gtp {
      app-service none
      disable-parser yes
      message-terminator none
      no-response yes
      description none
      max-egress-buffer 32768
      max-message-size 32768
    }

    profile messagerouter gm_router_for_gtp {
      name "gm_router_for_gtp"
      app-service none
      irule-scope-message no
    }

	bigdb tm.tcpsegmentationoffload disable

	bigdb tm.tcpwoodsidefasterrecovery disable

	bigdb tm.tcplargereceiveoffload disable

    pool blobd_pool {
      member 169.254.0.253 9222
    }

    profile tcp blobd_tcp {
      default_name tcp
    }

    # Virtual for blobd
    virtual $POD_IP 9222 {
      name blobd_virtual
      profile blobd_tcp
      vlan enable all
      snat_automap
      pool blobd_pool
      auto_lasthop off
    }
`
		tmmInitObj := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      bnkconst.ConfigMapTMMInit,
				"namespace": bnkconst.InstanceNamespace,
			},
			"data": map[string]string{
				"tmm_init.tcl":  tmmInitTcl,
				"user_conf.tcl": "bigdb tm.tcpsegmentationoffload disable\nbigdb tm.tcplargereceiveoffload disable\npool pool1 {\n}\n",
			},
		}
		if cmBytes, cmErr := yaml.Marshal(tmmInitObj); cmErr == nil {
			netCombined += "---\n" + strings.TrimSpace(string(cmBytes)) + "\n"
		}
	}

	manifestVersion := ""
	if prof != nil {
		manifestVersion = prof.Version
	}
	aiGwDNS := strings.HasPrefix(manifestVersion, "2.4.1") || strings.HasPrefix(manifestVersion, "2.4.2") || strings.HasPrefix(manifestVersion, "2.5")
	if cl != nil && cl.Bnk != nil && cl.Bnk.AIGateway != nil {
		aiGwDNS = cl.Bnk.AIGateway.Enabled || cl.Bnk.AIGateway.DNSResolver
	}
	if aiGwDNS {
		dnsObj := BuildAIGWDNSResolverObject(cl, u, bnkconst.InstanceNamespace)
		if dnsBytes, mErr := yaml.Marshal(dnsObj); mErr == nil {
			netCombined += "---\n" + strings.TrimSpace(string(dnsBytes)) + "\n"
		}
	}
	return []byte(netCombined), egressBytes, nil
}

// BuildAIGWDNSResolverObject constructs the canonical unstructured map for the
// F5BigDnsCache/aigw-dns-resolver CR, driven by UnderlayContext.EnsureTraits().
func BuildAIGWDNSResolverObject(cl *intent.Cluster, u *provider.UnderlayContext, targetNS string, explicitDNSIPs ...string) map[string]any {
	if strings.TrimSpace(targetNS) == "" {
		targetNS = bnkconst.InstanceNamespace
	}
	var nameServers []any
	for _, ip := range explicitDNSIPs {
		if trimmed := strings.TrimSpace(ip); trimmed != "" {
			nameServers = append(nameServers, map[string]any{
				"ipAddress": trimmed,
				"port":      int64(bnkconst.DefaultDNSPort),
			})
		}
	}
	netResolver := map[string]any{}
	if len(nameServers) == 0 {
		netResolver["useIpv4"] = true
		netResolver["useIpv6"] = false
		var traits provider.DataplaneTraits
		if u == nil && cl != nil {
			if derived, err := provider.BuildUnderlayFromIntent(cl); err == nil && derived != nil {
				u = derived
			}
		}
		if u != nil {
			traits = u.EnsureTraits()
		} else {
			traits = provider.DefaultTraitsForUnderlay("", provider.DriverDPDKPCI)
		}
		if traits.IncludeClusterDNSInTMMResolver {
			dnsIP := ""
			if u != nil && u.ServiceCIDR != "" {
				if ip, ipNet, err := net.ParseCIDR(u.ServiceCIDR); err == nil && ipNet != nil {
					if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
						dnsIP = net.IPv4(v4[0], v4[1], v4[2], 10).String()
					}
				}
			}
			if dnsIP == "" {
				if traits.DefaultClusterDNSIP != "" {
					dnsIP = traits.DefaultClusterDNSIP
				} else {
					dnsIP = bnkconst.DefaultK8sClusterDNSIP
				}
			}
			nameServers = append(nameServers, map[string]any{
				"ipAddress": dnsIP,
				"port":      int64(bnkconst.DefaultDNSPort),
			})
		}
		for _, fwd := range bnkconst.DefaultPublicDNSForwarders {
			nameServers = append(nameServers, map[string]any{
				"ipAddress": fwd,
				"port":      int64(bnkconst.DefaultDNSPort),
			})
		}
	}
	netResolver["forwardZones"] = []any{
		map[string]any{
			"forwardZone": ".",
			"nameServers": nameServers,
		},
	}

	return map[string]any{
		"apiVersion": "k8s.f5net.com/v1",
		"kind":       "F5BigDnsCache",
		"metadata": map[string]any{
			"name":      bnkconst.CRAIGWDNSResolverName,
			"namespace": targetNS,
		},
		"spec": map[string]any{
			"cacheType":   "net-resolver",
			"netResolver": netResolver,
		},
	}
}

func effectiveTestNamespaceForRender(cl *intent.Cluster) string {
	if cl == nil {
		return "default"
	}
	if cl.Egress.TestNamespace != "" {
		return cl.Egress.TestNamespace
	}
	if cl.Egress.Namespace != "" {
		return cl.Egress.Namespace
	}
	return "default"
}

func ipRangeFromSelfIPs(selfIPs []string, subnetCIDR string) (string, string, int) {
	prefLen := prefixLenFromCIDR(subnetCIDR, 24)
	clean := stripMasks(selfIPs)
	if len(clean) > 0 {
		if parts := strings.SplitN(clean[0], "-", 2); len(parts) == 2 {
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), prefLen
		}
		return clean[0], clean[len(clean)-1], prefLen
	}
	ip, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err == nil && ipNet != nil {
		v4 := ip.Mask(ipNet.Mask).To4()
		if v4 != nil {
			start := net.IPv4(v4[0], v4[1], v4[2], 10).String()
			end := net.IPv4(v4[0], v4[1], v4[2], 20).String()
			return start, end, prefLen
		}
	}
	return "10.0.1.10", "10.0.1.20", prefLen
}

func snatRangeFromSubnet(subnetCIDR, fallbackStart, fallbackEnd string) (string, string) {
	ip, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err == nil && ipNet != nil {
		v4 := ip.Mask(ipNet.Mask).To4()
		if v4 != nil {
			start := net.IPv4(v4[0], v4[1], v4[2], 200).String()
			end := net.IPv4(v4[0], v4[1], v4[2], 210).String()
			return start, end
		}
	}
	if fallbackStart != "" && fallbackEnd != "" {
		return fallbackStart, fallbackEnd
	}
	return "10.0.1.200", "10.0.1.210"
}

func mapToEnvSlice(m map[string]string) []map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{
			"name":  k,
			"value": m[k],
		})
	}
	return out
}

// MergeYAMLMap unmarshals baseYAML, deep-merges overrides on top, and marshals back to YAML.
func MergeYAMLMap(baseYAML []byte, overrides map[string]any) ([]byte, error) {
	var base map[string]any
	if err := yaml.Unmarshal(baseYAML, &base); err != nil {
		return nil, err
	}
	if base == nil {
		base = make(map[string]any)
	}
	DeepMergeMaps(base, overrides)
	return yaml.Marshal(base)
}

// DeepMergeMaps recursively merges src into dst.
func DeepMergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				DeepMergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

func stripMasks(ips []string) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if idx := strings.IndexByte(ip, '/'); idx >= 0 {
			out = append(out, ip[:idx])
		} else {
			out = append(out, ip)
		}
	}
	return out
}

func prefixLenFromCIDR(cidr string, fallback int) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil || ipNet == nil {
		return fallback
	}
	ones, _ := ipNet.Mask.Size()
	if ones <= 0 {
		return fallback
	}
	return ones
}
