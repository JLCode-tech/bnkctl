package azure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jwt"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func resolveRelativeToSource(sourcePath, p string) string {
	if p == "" || filepath.IsAbs(p) || sourcePath == "" {
		return p
	}
	candidate := filepath.Join(filepath.Dir(sourcePath), p)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return p
}

func ensureAzureDefaults(cl *intent.Cluster) {
	if cl == nil {
		return
	}
	if cl.Metadata.Location == "" && cl.Metadata.Region != "" {
		cl.Metadata.Location = cl.Metadata.Region
	}
	if cl.Metadata.Region == "" && cl.Metadata.Location != "" {
		cl.Metadata.Region = cl.Metadata.Location
	}
	name := cl.Metadata.Name
	if cl.Derived.ResourceGroupName == "" {
		if cl.Metadata.ResourceGroup != "" {
			cl.Derived.ResourceGroupName = cl.Metadata.ResourceGroup
		} else if name != "" {
			cl.Derived.ResourceGroupName = name + "-rg"
		}
	}
	if cl.Metadata.ResourceGroup == "" && cl.Derived.ResourceGroupName != "" {
		cl.Metadata.ResourceGroup = cl.Derived.ResourceGroupName
	}
	if cl.Derived.VNetName == "" && name != "" {
		cl.Derived.VNetName = name + "-vnet"
	}
	if cl.Derived.AKSClusterName == "" && name != "" {
		cl.Derived.AKSClusterName = name
	}
	if cl.Derived.NSGName == "" && name != "" {
		cl.Derived.NSGName = name + "-nsg"
	}
	if cl.Derived.SystemSubnetName == "" && name != "" {
		cl.Derived.SystemSubnetName = name + "-system"
	}
	if cl.Derived.PublicSubnetName == "" && name != "" {
		cl.Derived.PublicSubnetName = name + "-public"
	}
	if cl.Derived.ExternalSubnetName == "" && name != "" {
		cl.Derived.ExternalSubnetName = name + "-external"
	}
	if cl.Derived.InternalSubnetName == "" && name != "" {
		cl.Derived.InternalSubnetName = name + "-internal"
	}
	if cl.Derived.NatGatewayName == "" && name != "" {
		cl.Derived.NatGatewayName = name + "-nat"
	}
	if cl.Derived.NatGatewayPublicIPName == "" && name != "" {
		cl.Derived.NatGatewayPublicIPName = name + "-nat-pip"
	}
	if cl.Derived.PublicIPName == "" && name != "" {
		cl.Derived.PublicIPName = name + "-jb-pip"
	}
	if cl.Network.VNetCIDR == "" && cl.Network.VPCCidr != "" {
		cl.Network.VNetCIDR = cl.Network.VPCCidr
	}
	if cl.Network.Subnets.External == "" && cl.Network.DataPath != nil && cl.Network.DataPath.External.CIDR != "" {
		cl.Network.Subnets.External = cl.Network.DataPath.External.CIDR
	}
	if cl.Network.Subnets.Internal == "" && cl.Network.DataPath != nil && cl.Network.DataPath.Internal.CIDR != "" {
		cl.Network.Subnets.Internal = cl.Network.DataPath.Internal.CIDR
	}
	if cl.ClusterSpec == nil {
		cl.ClusterSpec = &intent.ClusterSpec{}
	}
	if cl.Bnk == nil {
		cl.Bnk = &intent.BnkSpec{}
	}
	if cl.Derived.JWTFullPath == "" && cl.Bnk.EffectiveJWTPath() != "" {
		cl.Derived.JWTFullPath = resolveRelativeToSource(cl.SourcePath, cl.Bnk.EffectiveJWTPath())
	}
	if cl.Derived.FARArchiveFullPath == "" && cl.Bnk.FARArchive != "" {
		cl.Derived.FARArchiveFullPath = resolveRelativeToSource(cl.SourcePath, cl.Bnk.FARArchive)
	}
}

// Phase00Preflight validates credentials, JWT expiry, and file existence before provisioning.
func Phase00Preflight(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase00: cluster intent is nil")
	}
	ensureAzureDefaults(cl)

	subID, err := ResolveSubscriptionID(cl.Metadata.SubscriptionID)
	if err != nil {
		if clients != nil && clients.SubscriptionID != "" {
			subID = clients.SubscriptionID
		} else if dryRun {
			subID = "00000000-0000-0000-0000-000000000000"
		} else {
			return fmt.Errorf("phase00: %w", err)
		}
	}
	cl.Metadata.SubscriptionID = subID

	jwtPath := cl.Derived.JWTFullPath
	if jwtPath == "" && cl.Bnk != nil {
		jwtPath = resolveRelativeToSource(cl.SourcePath, cl.Bnk.EffectiveJWTPath())
	}
	if jwtPath != "" {
		var rawJWT string
		if data, readErr := os.ReadFile(jwtPath); readErr == nil {
			rawJWT = strings.TrimSpace(string(data))
		} else if strings.Count(jwtPath, ".") == 2 && !strings.ContainsAny(jwtPath, "/\\") {
			rawJWT = jwtPath
		} else if !dryRun {
			return fmt.Errorf("phase00: reading JWT %s: %w", jwtPath, readErr)
		}
		if rawJWT != "" {
			if _, err := jwt.CheckPreflight(rawJWT, os.Stderr); err != nil {
				return fmt.Errorf("phase00: JWT validation failed: %w", err)
			}
		}
	}

	if dryRun {
		fmt.Printf("[phase 00] [dry-run] preflight OK: subscription=%s rg=%s location=%s\n",
			cl.Metadata.SubscriptionID, cl.Derived.ResourceGroupName, cl.Metadata.Location)
		fmt.Printf("[phase 10-14] [dry-run] would ensure Resource Group %q, NSG %q, VNet %q (%s), NAT Gateway %q, and Public IPs\n",
			cl.Derived.ResourceGroupName, cl.Derived.NSGName, cl.Derived.VNetName, cl.Network.VNetCIDR, cl.Derived.NatGatewayName)
		if cl.ClusterSpec != nil {
			fmt.Printf("[phase 21-25] [dry-run] would ensure AKS cluster %q (k8s=%s, system=%s x%d, tmm=%s x%d) and discover Accelerated Networking NICs\n",
				cl.Derived.AKSClusterName, cl.ClusterSpec.KubernetesVersion,
				cl.ClusterSpec.SystemNodePool.VMSize, cl.ClusterSpec.SystemNodePool.Count,
				cl.ClusterSpec.TMMNodePool.VMSize, cl.ClusterSpec.TMMNodePool.Count)
		}
	}

	if !dryRun {
		farPath := cl.Derived.FARArchiveFullPath
		if farPath == "" && cl.Bnk != nil {
			farPath = resolveRelativeToSource(cl.SourcePath, cl.Bnk.FARArchive)
		}
		if farPath != "" {
			if _, err := os.Stat(farPath); err != nil {
				return fmt.Errorf("phase00: FAR archive %s not found: %w", farPath, err)
			}
		}

		if !ShouldSkipAuth() && clients == nil {
			if _, err := NewCredential(); err != nil {
				return fmt.Errorf("phase00: Azure credential failure: %w", err)
			}
		}
	}

	if st != nil {
		st.Set("SUBSCRIPTION_ID", subID)
		st.Set("RESOURCE_GROUP", cl.Derived.ResourceGroupName)
		st.Set("AZURE_RESOURCE_GROUP", cl.Derived.ResourceGroupName)
		st.Set("LOCATION", cl.Metadata.Location)
		st.Set("AZURE_LOCATION", cl.Metadata.Location)
		st.Set("CLUSTER_NAME", cl.Derived.AKSClusterName)
		st.Set("PATTERN", cl.Pattern)
		if !dryRun {
			return st.Save()
		}
	}

	return nil
}
