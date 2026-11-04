// Package provider defines the strict contract between cloud IaaS underlays
// (AWS, Azure, GCP) and the cloud-agnostic BNK overlay engine.
//
// Zero cloud SDK imports are permitted in this package or any pkg/** package.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Canonical cloud network, interface, and IPAM defaults used across underlay
// builders and provider discovery.
const (
	CNIVersionDefault = "0.3.1"

	NetNameExternal = "external-net"
	NetNameInternal = "internal-net"
	NetRoleExternal = "external"
	NetRoleInternal = "internal"

	DefaultLinuxExtNIC    = "eth1"
	DefaultLinuxIntNIC    = "eth2"
	DefaultAWSLinuxExtNIC = "ens8"
	DefaultAWSLinuxIntNIC = "ens7"
	DefaultAWSExtPCIBDF   = "0000:28:00.0"
	DefaultAWSIntPCIBDF   = "0000:29:00.0"
	DefaultAzureBridgeExt = "br-ext"
	DefaultAzureBridgeInt = "br-int"

	TMMInterfaceDPDKExt   = "1.1"
	TMMInterfaceDPDKInt   = "1.2"
	TMMInterfaceSocketExt = "0.2"
	TMMInterfaceSocketInt = "0.3"

	DefaultFloatingIPOffset           = 10
	DefaultSelfIP1Offset              = 11
	DefaultSelfIP2Offset              = 12
	DefaultGCPSelfIP1Offset           = 10
	DefaultGCPSelfIP2Offset           = 11
	DefaultBridgeIPAMRangeStartOffset = 200
	DefaultBridgeIPAMRangeEndOffset   = 250

	DefaultAWSServiceCIDR        = "172.20.0.0/16"
	DefaultAWSPodCIDR            = "192.168.0.0/16"
	DefaultAWSExternalSubnetCIDR = "10.90.100.0/24"
	DefaultAWSStorageClass       = "gp2"

	DefaultAzureServiceCIDR        = "10.240.0.0/16"
	DefaultAzurePodCIDR            = "10.244.0.0/16"
	DefaultAzureExternalSubnetCIDR = "10.90.3.0/24"
	DefaultAzureInternalSubnetCIDR = "10.90.4.0/24"
	DefaultAzureStorageClass       = "managed-csi"

	DefaultGCPServiceCIDR        = "10.96.0.0/20"
	DefaultGCPPodCIDR            = "10.92.0.0/14"
	DefaultGCPExternalSubnetCIDR = "10.90.2.0/24"
	DefaultGCPInternalSubnetCIDR = "10.90.3.0/24"
	DefaultGCPStorageClass       = "standard-rwo"
)

// DataplaneDriver identifies how TMM binds to secondary dataplane interfaces.
type DataplaneDriver string

const (
	// DriverDPDKPCI uses DPDK PCI pass-through / host-device CNI ("1.1", "1.2") — e.g. AWS ENA.
	DriverDPDKPCI DataplaneDriver = "dpdk-pci"
	// DriverSocket uses kernel socket / macvlan / bridge CNI ("0.2", "0.3", reserving "0.1" for xeth0) — e.g. Azure.
	DriverSocket DataplaneDriver = "socket"
	// DriverVirtio uses virtio dataplane interfaces ("1.1", "1.2") — e.g. GCP.
	DriverVirtio DataplaneDriver = "virtio"
)

// DataplaneTraits models the declarative underlay physics and dataplane capabilities
// so the BNK overlay engine and manifest renderer do not branch on ad-hoc cloud names.
type DataplaneTraits struct {
	Initialized                      bool
	UseVFIOHostDevice                bool   // DPDK PCI / VFIO host-device CNI + OnDelete DaemonSet strategy
	UseKernelSocketMode              bool   // Kernel socket / xeth mode + routeDomain0MTU + debug sidecar
	UseVirtioMode                    bool   // Virtio PMD + virtio_eth0_ignore + tmm-init postStart hook
	UseCalicoVXLANGateway            bool   // Route ServiceCIDR/PodCIDR via Calico VXLAN link-local 169.254.1.1
	SuppressDefaultRouteOnExternal   bool   // Omit 0.0.0.0/0 F5SPKStaticRoute on external-net (Virtio eth0/eth1 safety)
	IncludeClusterDNSInTMMResolver   bool   // Include in-cluster kube-dns ClusterIP in F5BigDnsCache forwarders
	EmitSNATEgressPool               bool   // Emit F5SPKSnatpool alongside F5BigNetopPolicy
	PreserveHostDeviceIPs            bool   // Set preserve_ip_addresses: true on CNEInstance cniNetworks
	SkipAdminLoadBalancerService     bool   // Skip creating f5-tmm-LoadBalancer service (e.g. GCP L4 ILB port limits)
	DisablePSE                       bool   // Set TMM_F5_PSE_DISABLED=true
	RenderDefaultNamespaceNADs       bool   // Also render NADs in default namespace (Azure Socket)
	UseShortInfraNetworkNames        bool   // Use "external"/"internal" instead of "external-net"/"internal-net" in Infra CR
	OmitTMMInterfacesAndSkipNICs     bool   // Omit TMM_INTERFACES and TMM_SKIP_NICS env vars (Azure Socket)
	ConservativeTMMResourceRequests  bool   // Use 1 CPU / 4Gi requests with 2 CPU / 8Gi limits when unconfigured (Azure)
	EgressPrefersExternalNetwork     bool   // Keep egressNet on external network even when internal network exists
	IncludeNodeCIDRInK8sRoutes       bool   // Include NodeCIDR in TMM_K8S_ROUTES and Infra staticRoutes (Virtio)
	IncludeDefaultInEgressMatchNames bool   // Append "default" to EgressGateway sourceSelector matchNames (Virtio)
	EmitTMMInitConfigMap             bool   // Emit tmm-init ConfigMap (tmm_init.tcl / user_conf.tcl) alongside Infra
	UseSplitMultusManifests          bool   // Apply multus/02-multus-*.yaml instead of monolithic daemonset
	DefaultPalCPUSet                 string // Default PAL_CPU_SET ("0,2" on Azure, "0-1" otherwise)
	DefaultClusterDNSIP              string // Fallback kube-dns ClusterIP when offline/dry-run
	DefaultMTU                       int    // Default dataplane MTU when not specified on intent/network
	DefaultRouteDomain0MTU           int    // Optional routeDomain0MTU override (1390 for kernel socket mode)
}

// DefaultTraitsForUnderlay returns the canonical DataplaneTraits for the given
// cloud provider name and DataplaneDriver.
func DefaultTraitsForUnderlay(cloudName string, driver DataplaneDriver) DataplaneTraits {
	if driver == "" {
		switch cloudName {
		case intent.ProviderAzure:
			driver = DriverSocket
		case intent.ProviderGCP:
			driver = DriverVirtio
		default:
			driver = DriverDPDKPCI
		}
	}

	isAzure := cloudName == intent.ProviderAzure
	palCPUSet := "0-1"
	if isAzure {
		palCPUSet = "0,2"
	}

	switch driver {
	case DriverSocket:
		return DataplaneTraits{
			Initialized:                     true,
			UseKernelSocketMode:             true,
			IncludeClusterDNSInTMMResolver:  true,
			EmitSNATEgressPool:              true,
			DisablePSE:                      true,
			RenderDefaultNamespaceNADs:      isAzure,
			UseShortInfraNetworkNames:       isAzure,
			OmitTMMInterfacesAndSkipNICs:    isAzure,
			ConservativeTMMResourceRequests: isAzure,
			EgressPrefersExternalNetwork:    isAzure,
			DefaultPalCPUSet:                palCPUSet,
			DefaultClusterDNSIP:             bnkconst.DefaultK8sClusterDNSIP,
			DefaultMTU:                      bnkconst.DefaultMTU,
			DefaultRouteDomain0MTU:          bnkconst.DefaultRouteDomain0MTU,
		}
	case DriverVirtio:
		return DataplaneTraits{
			Initialized:                      true,
			UseVirtioMode:                    true,
			UseCalicoVXLANGateway:            true,
			SuppressDefaultRouteOnExternal:   true,
			IncludeClusterDNSInTMMResolver:   false,
			EmitSNATEgressPool:               false,
			PreserveHostDeviceIPs:            true,
			SkipAdminLoadBalancerService:     true,
			EgressPrefersExternalNetwork:     true,
			IncludeNodeCIDRInK8sRoutes:       true,
			IncludeDefaultInEgressMatchNames: true,
			UseSplitMultusManifests:          true,
			DefaultPalCPUSet:                 palCPUSet,
			DefaultClusterDNSIP:              bnkconst.DefaultK8sClusterDNSIP,
			DefaultMTU:                       bnkconst.DefaultGCPMTU,
		}
	default: // DriverDPDKPCI
		dnsIP := bnkconst.DefaultAWSClusterDNSIP
		if cloudName != "" && cloudName != intent.ProviderAWS {
			dnsIP = bnkconst.DefaultK8sClusterDNSIP
		}
		return DataplaneTraits{
			Initialized:                     true,
			UseVFIOHostDevice:               true,
			IncludeClusterDNSInTMMResolver:  true,
			EmitSNATEgressPool:              true,
			RenderDefaultNamespaceNADs:      isAzure,
			UseShortInfraNetworkNames:       isAzure,
			OmitTMMInterfacesAndSkipNICs:    isAzure,
			ConservativeTMMResourceRequests: isAzure,
			EgressPrefersExternalNetwork:    isAzure,
			EmitTMMInitConfigMap:            cloudName == intent.ProviderAWS,
			DefaultPalCPUSet:                palCPUSet,
			DefaultClusterDNSIP:             dnsIP,
			DefaultMTU:                      bnkconst.DefaultMTU,
		}
	}
}

// NICAttachment describes how a specific secondary NIC is attached to a dataplane node.
type NICAttachment struct {
	NetworkName  string
	NICIndex     int
	LinuxIfName  string
	PCIAddress   string
	MACAddress   string
	TMMInterface string
	Driver       DataplaneDriver
}

// NodeAttachment describes a Kubernetes worker node and its attached dataplane NICs.
type NodeAttachment struct {
	NodeName   string
	ProviderID string
	Zone       string
	NICs       []NICAttachment
}

// NetworkAttachmentSpec describes a secondary dataplane network normalized by the cloud underlay.
type NetworkAttachmentSpec struct {
	Name          string            // e.g. "external-net", "internal-net", or "external"
	Role          string            // "external", "internal", "storage", "custom"
	SubnetCIDR    string            // e.g. "10.90.100.0/24"
	GatewayIP     string            // e.g. "10.90.100.1"
	MTU           int               // e.g. 1500 or 1460
	VLANTag       int               // 0 for untagged
	SelfIPs       []string          // per-TMM replica self-IPs (with or without /prefix)
	FloatingIP    string            // optional floating self-IP
	CNIConfigJSON string            // pre-rendered CNI config JSON for NetworkAttachmentDefinition
	NodeNICMap    map[string]string // optional nodeName -> host device / MAC / PCI address
	TMMInterface  string            // "1.1", "1.2" (DPDK PCI / Virtio) or "0.2", "0.3" (kernel socket)
}

// HookFunc is a cloud-injected callback invoked by the BNK overlay engine at specific lifecycle points.
type HookFunc func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error

// UnderlayContext is the sole handoff contract from a Cloud Provider to the BNK Overlay Engine.
type UnderlayContext struct {
	CloudName      string // "aws", "azure", "gcp"
	ClusterName    string
	Region         string
	Kubeconfig     []byte
	KubeconfigPath string
	RestConfig     *rest.Config

	ServiceCIDR string // e.g. "10.100.0.0/16" or "172.16.0.0/16"
	PodCIDR     string // e.g. "10.244.0.0/16"
	NodeCIDR    string

	DataplaneDriver DataplaneDriver
	Traits          DataplaneTraits
	Networks        []NetworkAttachmentSpec
	StorageClass    string
	NodeNames       []string
	DataplaneNodes  []NodeAttachment

	// Cloud-specific service annotations (e.g. AWS NLB internal annotation, Azure internal LB, GCP internal LB)
	AdminLoadBalancerAnnotations map[string]string

	// Cloud-specific discovery / identity / post-activation hooks (nil if not needed on this cloud)
	DiscoverDataplaneResources  HookFunc // discovers worker node NICs / PCI addresses before NAD / CNEInstance rendering
	ReconcileControllerIdentity HookFunc // e.g. AWS Pod Identity Association, Azure Workload Identity, GCP Workload Identity
	PostActivationHook          HookFunc // e.g. Azure LbCtl routes & NSG, GCP VIP alias IP / route programming

	// Optional dialer for private clusters or bastion tunnels
	JumphostDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// EnsureTraits returns the UnderlayContext's DataplaneTraits, initializing them
// from CloudName and DataplaneDriver if they were not explicitly populated.
func (u *UnderlayContext) EnsureTraits() DataplaneTraits {
	if u == nil {
		return DefaultTraitsForUnderlay(intent.ProviderAWS, DriverDPDKPCI)
	}
	if !u.Traits.Initialized {
		u.Traits = DefaultTraitsForUnderlay(u.CloudName, u.DataplaneDriver)
	}
	return u.Traits
}

// Provider is the cloud IaaS underlay lifecycle interface implemented by internal/providers/{aws,azure,gcp}.
type Provider interface {
	Name() string
	ProvisionUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) (*UnderlayContext, error)
	DiscoverUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State) (*UnderlayContext, error)
	DestroyUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error
	OpenJumphostSession(ctx context.Context, cl *intent.Cluster, st *state.State) (jumphost.Runner, error)
}

// ExternalNetwork returns the primary external NetworkAttachmentSpec, or the first network if none is marked external.
func (u *UnderlayContext) ExternalNetwork() *NetworkAttachmentSpec {
	if u == nil {
		return nil
	}
	for i := range u.Networks {
		if u.Networks[i].Role == NetRoleExternal || u.Networks[i].Name == NetRoleExternal || u.Networks[i].Name == NetNameExternal {
			return &u.Networks[i]
		}
	}
	if len(u.Networks) > 0 {
		return &u.Networks[0]
	}
	return nil
}

// InternalNetwork returns the secondary internal NetworkAttachmentSpec if present.
func (u *UnderlayContext) InternalNetwork() *NetworkAttachmentSpec {
	if u == nil {
		return nil
	}
	for i := range u.Networks {
		if u.Networks[i].Role == NetRoleInternal || u.Networks[i].Name == NetRoleInternal || u.Networks[i].Name == NetNameInternal {
			return &u.Networks[i]
		}
	}
	if len(u.Networks) > 1 {
		return &u.Networks[1]
	}
	return nil
}

// BuildUnderlayFromIntent constructs a deterministic UnderlayContext from the cluster intent.
// Cloud providers use this as the baseline for dry-run / offline rendering and enrich it with
// live cloud state (kubeconfig, discovered CIDRs, node NIC maps, and cloud hooks) when online.
func BuildUnderlayFromIntent(cl *intent.Cluster) (*UnderlayContext, error) {
	if cl == nil {
		return nil, fmt.Errorf("cluster intent is nil")
	}
	cloud := cl.EffectiveProvider()
	if cloud == "" {
		cloud = intent.ProviderAWS
	}

	u := &UnderlayContext{
		CloudName:                    cloud,
		ClusterName:                  cl.EffectiveName(),
		Region:                       cl.EffectiveRegion(),
		AdminLoadBalancerAnnotations: make(map[string]string),
	}

	switch cloud {
	case intent.ProviderAWS:
		u.DataplaneDriver = DriverDPDKPCI
		u.Traits = DefaultTraitsForUnderlay(cloud, u.DataplaneDriver)
		u.ServiceCIDR = DefaultAWSServiceCIDR
		u.PodCIDR = DefaultAWSPodCIDR
		u.NodeCIDR = cl.Network.VPCCidr
		u.StorageClass = DefaultAWSStorageClass
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/aws-load-balancer-type"] = "nlb"
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/aws-load-balancer-internal"] = "true"

		mtu := u.Traits.DefaultMTU
		if cl.Bnk != nil && cl.Bnk.TmmMtuInt() > 0 {
			mtu = cl.Bnk.TmmMtuInt()
		}

		extCIDR := DefaultAWSExternalSubnetCIDR
		intCIDR := ""
		if cl.Network.DataPath != nil {
			if cl.Network.DataPath.External.CIDR != "" {
				extCIDR = cl.Network.DataPath.External.CIDR
			}
			if cl.Network.DataPath.Internal.CIDR != "" {
				intCIDR = cl.Network.DataPath.Internal.CIDR
			}
		}
		extGW := netcalc.GatewayIP(extCIDR)
		extSelf1 := netcalc.IPAtOffset(extCIDR, DefaultSelfIP1Offset)
		extSelf2 := netcalc.IPAtOffset(extCIDR, DefaultSelfIP2Offset)
		extFloat := netcalc.IPAtOffset(extCIDR, DefaultFloatingIPOffset)
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": CNIVersionDefault,
			"name":       NetNameExternal,
			"type":       "host-device",
			"device":     DefaultLinuxExtNIC,
		})
		u.Networks = append(u.Networks, NetworkAttachmentSpec{
			Name:          NetNameExternal,
			Role:          NetRoleExternal,
			SubnetCIDR:    extCIDR,
			GatewayIP:     extGW,
			MTU:           mtu,
			SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
			FloatingIP:    extFloat + "/24",
			CNIConfigJSON: string(extCNI),
			TMMInterface:  TMMInterfaceDPDKExt,
		})
		if intCIDR != "" {
			intGW := netcalc.GatewayIP(intCIDR)
			intSelf1 := netcalc.IPAtOffset(intCIDR, DefaultSelfIP1Offset)
			intSelf2 := netcalc.IPAtOffset(intCIDR, DefaultSelfIP2Offset)
			intFloat := netcalc.IPAtOffset(intCIDR, DefaultFloatingIPOffset)
			intCNI, _ := json.Marshal(map[string]any{
				"cniVersion": CNIVersionDefault,
				"name":       NetNameInternal,
				"type":       "host-device",
				"device":     DefaultLinuxIntNIC,
			})
			u.Networks = append(u.Networks, NetworkAttachmentSpec{
				Name:          NetNameInternal,
				Role:          NetRoleInternal,
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				FloatingIP:    intFloat + "/24",
				CNIConfigJSON: string(intCNI),
				TMMInterface:  TMMInterfaceDPDKInt,
			})
		}

	case intent.ProviderAzure:
		u.DataplaneDriver = DriverSocket
		u.Traits = DefaultTraitsForUnderlay(cloud, u.DataplaneDriver)
		u.ServiceCIDR = DefaultAzureServiceCIDR
		u.PodCIDR = DefaultAzurePodCIDR
		u.NodeCIDR = cl.Network.VNetCIDR
		u.StorageClass = DefaultAzureStorageClass
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/azure-load-balancer-internal"] = "true"

		mtu := u.Traits.DefaultMTU
		if cl.Bnk != nil && cl.Bnk.TmmMtuInt() > 0 {
			mtu = cl.Bnk.TmmMtuInt()
		}

		extCIDR := cl.Network.Subnets.External
		if extCIDR == "" {
			extCIDR = DefaultAzureExternalSubnetCIDR
		}
		intCIDR := cl.Network.Subnets.Internal
		if intCIDR == "" && cl.HasInternalInterface() {
			intCIDR = DefaultAzureInternalSubnetCIDR
		}
		extGW := netcalc.GatewayIP(extCIDR)
		extSelf1 := netcalc.IPAtOffset(extCIDR, DefaultSelfIP1Offset)
		extSelf2 := netcalc.IPAtOffset(extCIDR, DefaultSelfIP2Offset)
		extFloat := netcalc.IPAtOffset(extCIDR, DefaultFloatingIPOffset)
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": CNIVersionDefault,
			"name":       NetNameExternal,
			"type":       "bridge",
			"bridge":     DefaultAzureBridgeExt,
			"isGateway":  true,
			"ipMasq":     false,
			"ipam": map[string]any{
				"type":       "host-local",
				"subnet":     extCIDR,
				"rangeStart": netcalc.IPAtOffset(extCIDR, DefaultBridgeIPAMRangeStartOffset),
				"rangeEnd":   netcalc.IPAtOffset(extCIDR, DefaultBridgeIPAMRangeEndOffset),
			},
		})
		u.Networks = append(u.Networks, NetworkAttachmentSpec{
			Name:          NetNameExternal,
			Role:          NetRoleExternal,
			SubnetCIDR:    extCIDR,
			GatewayIP:     extGW,
			MTU:           mtu,
			SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
			FloatingIP:    extFloat + "/24",
			CNIConfigJSON: string(extCNI),
			TMMInterface:  TMMInterfaceSocketExt,
		})
		if intCIDR != "" {
			intGW := netcalc.GatewayIP(intCIDR)
			intSelf1 := netcalc.IPAtOffset(intCIDR, DefaultSelfIP1Offset)
			intSelf2 := netcalc.IPAtOffset(intCIDR, DefaultSelfIP2Offset)
			intFloat := netcalc.IPAtOffset(intCIDR, DefaultFloatingIPOffset)
			intCNI, _ := json.Marshal(map[string]any{
				"cniVersion": CNIVersionDefault,
				"name":       NetNameInternal,
				"type":       "bridge",
				"bridge":     DefaultAzureBridgeInt,
				"isGateway":  true,
				"ipMasq":     false,
				"ipam": map[string]any{
					"type":       "host-local",
					"subnet":     intCIDR,
					"rangeStart": netcalc.IPAtOffset(intCIDR, DefaultBridgeIPAMRangeStartOffset),
					"rangeEnd":   netcalc.IPAtOffset(intCIDR, DefaultBridgeIPAMRangeEndOffset),
				},
			})
			u.Networks = append(u.Networks, NetworkAttachmentSpec{
				Name:          NetNameInternal,
				Role:          NetRoleInternal,
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				FloatingIP:    intFloat + "/24",
				CNIConfigJSON: string(intCNI),
				TMMInterface:  TMMInterfaceSocketInt,
			})
		}

	case intent.ProviderGCP:
		u.DataplaneDriver = DriverVirtio
		u.Traits = DefaultTraitsForUnderlay(cloud, u.DataplaneDriver)
		u.ServiceCIDR = cl.Network.Main.SvcsCIDR
		if u.ServiceCIDR == "" {
			u.ServiceCIDR = DefaultGCPServiceCIDR
		}
		u.PodCIDR = cl.Network.Main.PodsCIDR
		if u.PodCIDR == "" {
			u.PodCIDR = DefaultGCPPodCIDR
		}
		u.NodeCIDR = cl.Network.Main.CIDR
		u.StorageClass = DefaultGCPStorageClass
		u.AdminLoadBalancerAnnotations["networking.gke.io/load-balancer-type"] = "Internal"

		mtu := u.Traits.DefaultMTU
		if cl.Bnk != nil && cl.Bnk.TmmMtuInt() > 0 {
			mtu = cl.Bnk.TmmMtuInt()
		}

		extCIDR := cl.Network.External.CIDR
		if extCIDR == "" {
			extCIDR = DefaultGCPExternalSubnetCIDR
		}
		intCIDR := cl.Network.Internal.CIDR
		if intCIDR == "" {
			intCIDR = DefaultGCPInternalSubnetCIDR
		}
		extGW := netcalc.GatewayIP(extCIDR)
		intGW := netcalc.GatewayIP(intCIDR)
		extSelf1 := netcalc.IPAtOffset(extCIDR, DefaultGCPSelfIP1Offset)
		extSelf2 := netcalc.IPAtOffset(extCIDR, DefaultGCPSelfIP2Offset)
		intSelf1 := netcalc.IPAtOffset(intCIDR, DefaultGCPSelfIP1Offset)
		intSelf2 := netcalc.IPAtOffset(intCIDR, DefaultGCPSelfIP2Offset)

		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": CNIVersionDefault,
			"name":       NetNameExternal,
			"type":       "host-device",
			"device":     DefaultLinuxExtNIC,
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": extSelf1 + "/28"},
				},
			},
		})
		intCNI, _ := json.Marshal(map[string]any{
			"cniVersion": CNIVersionDefault,
			"name":       NetNameInternal,
			"type":       "host-device",
			"device":     DefaultLinuxIntNIC,
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": intSelf1 + "/28"},
				},
			},
		})
		u.Networks = []NetworkAttachmentSpec{
			{
				Name:          NetNameExternal,
				Role:          NetRoleExternal,
				SubnetCIDR:    extCIDR,
				GatewayIP:     extGW,
				MTU:           mtu,
				SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
				CNIConfigJSON: string(extCNI),
				TMMInterface:  TMMInterfaceDPDKExt,
			},
			{
				Name:          NetNameInternal,
				Role:          NetRoleInternal,
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				CNIConfigJSON: string(intCNI),
				TMMInterface:  TMMInterfaceDPDKInt,
			},
		}
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q", cloud)
	}

	return u, nil
}
