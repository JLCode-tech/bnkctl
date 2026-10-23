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

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
		if u.Networks[i].Role == "external" || u.Networks[i].Name == "external" || u.Networks[i].Name == "external-net" {
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
		if u.Networks[i].Role == "internal" || u.Networks[i].Name == "internal" || u.Networks[i].Name == "internal-net" {
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

	mtu := 1500
	if cl.Bnk != nil && cl.Bnk.TmmMtuInt() > 0 {
		mtu = cl.Bnk.TmmMtuInt()
	} else if cloud == intent.ProviderGCP {
		mtu = 1460
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
		u.ServiceCIDR = "172.20.0.0/16"
		u.PodCIDR = "192.168.0.0/16"
		u.NodeCIDR = cl.Network.VPCCidr
		u.StorageClass = "gp2"
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/aws-load-balancer-type"] = "nlb"
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/aws-load-balancer-internal"] = "true"

		extCIDR := "10.90.100.0/24"
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
		extSelf1 := netcalc.IPAtOffset(extCIDR, 11)
		extSelf2 := netcalc.IPAtOffset(extCIDR, 12)
		extFloat := netcalc.IPAtOffset(extCIDR, 10)
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "external-net",
			"type":       "host-device",
			"device":     "eth1",
		})
		u.Networks = append(u.Networks, NetworkAttachmentSpec{
			Name:          "external-net",
			Role:          "external",
			SubnetCIDR:    extCIDR,
			GatewayIP:     extGW,
			MTU:           mtu,
			SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
			FloatingIP:    extFloat + "/24",
			CNIConfigJSON: string(extCNI),
			TMMInterface:  "1.1",
		})
		if intCIDR != "" {
			intGW := netcalc.GatewayIP(intCIDR)
			intSelf1 := netcalc.IPAtOffset(intCIDR, 11)
			intSelf2 := netcalc.IPAtOffset(intCIDR, 12)
			intFloat := netcalc.IPAtOffset(intCIDR, 10)
			intCNI, _ := json.Marshal(map[string]any{
				"cniVersion": "0.3.1",
				"name":       "internal-net",
				"type":       "host-device",
				"device":     "eth2",
			})
			u.Networks = append(u.Networks, NetworkAttachmentSpec{
				Name:          "internal-net",
				Role:          "internal",
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				FloatingIP:    intFloat + "/24",
				CNIConfigJSON: string(intCNI),
				TMMInterface:  "1.2",
			})
		}

	case intent.ProviderAzure:
		u.DataplaneDriver = DriverSocket
		u.ServiceCIDR = "10.0.0.0/16"
		u.PodCIDR = "10.244.0.0/16"
		u.NodeCIDR = cl.Network.VNetCIDR
		u.StorageClass = "managed-csi"
		u.AdminLoadBalancerAnnotations["service.beta.kubernetes.io/azure-load-balancer-internal"] = "true"

		extCIDR := cl.Network.Subnets.External
		if extCIDR == "" {
			extCIDR = "10.90.3.0/24"
		}
		intCIDR := cl.Network.Subnets.Internal
		if intCIDR == "" && cl.HasInternalInterface() {
			intCIDR = "10.90.4.0/24"
		}
		extGW := netcalc.GatewayIP(extCIDR)
		extSelf1 := netcalc.IPAtOffset(extCIDR, 11)
		extSelf2 := netcalc.IPAtOffset(extCIDR, 12)
		extFloat := netcalc.IPAtOffset(extCIDR, 10)
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "external-net",
			"type":       "dummy",
			"ipam": map[string]any{
				"type":   "host-local",
				"subnet": extCIDR,
			},
		})
		u.Networks = append(u.Networks, NetworkAttachmentSpec{
			Name:          "external-net",
			Role:          "external",
			SubnetCIDR:    extCIDR,
			GatewayIP:     extGW,
			MTU:           mtu,
			SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
			FloatingIP:    extFloat + "/24",
			CNIConfigJSON: string(extCNI),
			TMMInterface:  "0.2",
		})
		if intCIDR != "" {
			intGW := netcalc.GatewayIP(intCIDR)
			intSelf1 := netcalc.IPAtOffset(intCIDR, 11)
			intSelf2 := netcalc.IPAtOffset(intCIDR, 12)
			intFloat := netcalc.IPAtOffset(intCIDR, 10)
			intCNI, _ := json.Marshal(map[string]any{
				"cniVersion": "0.3.1",
				"name":       "internal-net",
				"type":       "dummy",
				"ipam": map[string]any{
					"type":   "host-local",
					"subnet": intCIDR,
				},
			})
			u.Networks = append(u.Networks, NetworkAttachmentSpec{
				Name:          "internal-net",
				Role:          "internal",
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				FloatingIP:    intFloat + "/24",
				CNIConfigJSON: string(intCNI),
				TMMInterface:  "0.3",
			})
		}

	case intent.ProviderGCP:
		u.DataplaneDriver = DriverVirtio
		u.ServiceCIDR = cl.Network.Main.SvcsCIDR
		if u.ServiceCIDR == "" {
			u.ServiceCIDR = "10.96.0.0/20"
		}
		u.PodCIDR = cl.Network.Main.PodsCIDR
		if u.PodCIDR == "" {
			u.PodCIDR = "10.92.0.0/14"
		}
		u.NodeCIDR = cl.Network.Main.CIDR
		u.StorageClass = "standard-rwo"
		u.AdminLoadBalancerAnnotations["networking.gke.io/load-balancer-type"] = "Internal"

		extCIDR := cl.Network.External.CIDR
		if extCIDR == "" {
			extCIDR = "10.90.2.0/24"
		}
		intCIDR := cl.Network.Internal.CIDR
		if intCIDR == "" {
			intCIDR = "10.90.3.0/24"
		}
		extGW := netcalc.GatewayIP(extCIDR)
		intGW := netcalc.GatewayIP(intCIDR)
		extSelf1 := netcalc.IPAtOffset(extCIDR, 10)
		extSelf2 := netcalc.IPAtOffset(extCIDR, 11)
		intSelf1 := netcalc.IPAtOffset(intCIDR, 10)
		intSelf2 := netcalc.IPAtOffset(intCIDR, 11)

		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "external-net",
			"type":       "host-device",
			"device":     "eth1",
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": extSelf1 + "/28"},
				},
			},
		})
		intCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "internal-net",
			"type":       "host-device",
			"device":     "eth2",
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": intSelf1 + "/28"},
				},
			},
		})
		u.Networks = []NetworkAttachmentSpec{
			{
				Name:          "external-net",
				Role:          "external",
				SubnetCIDR:    extCIDR,
				GatewayIP:     extGW,
				MTU:           mtu,
				SelfIPs:       []string{extSelf1 + "/24", extSelf2 + "/24"},
				CNIConfigJSON: string(extCNI),
				TMMInterface:  "1.1",
			},
			{
				Name:          "internal-net",
				Role:          "internal",
				SubnetCIDR:    intCIDR,
				GatewayIP:     intGW,
				MTU:           mtu,
				SelfIPs:       []string{intSelf1 + "/24", intSelf2 + "/24"},
				CNIConfigJSON: string(intCNI),
				TMMInterface:  "1.2",
			},
		}
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q", cloud)
	}

	return u, nil
}
