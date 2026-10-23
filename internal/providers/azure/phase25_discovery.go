package azure

import (
	"context"
	"fmt"
	"os"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase25Discovery discovers or resolves TMM node PCI addresses, Gateway IPs, and Self IPs.
func Phase25Discovery(ctx context.Context, cl *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	fmt.Fprintln(os.Stderr, "[phase 25] discovery: resolving network dataplane parameters")

	extPCI := "4567:00:02.0"
	intPCI := ""
	if cl.Pattern == intent.PatternDualInterface {
		intPCI = "4567:00:03.0"
	}

	if st != nil {
		st.Set("TMM_EXT_PCI", extPCI)
		if intPCI != "" {
			st.Set("TMM_INT_PCI", intPCI)
		}
	}

	var extGw, extSelfIP string
	extSubnet := cl.Network.Subnets.External
	if extSubnet != "" {
		var err error
		extGw, err = netcalc.Nth(extSubnet, 1)
		if err != nil {
			return fmt.Errorf("phase25: calculating external gateway: %w", err)
		}
		extSelfIP, err = netcalc.Nth(extSubnet, 10)
		if err != nil {
			return fmt.Errorf("phase25: calculating external Self IP: %w", err)
		}
		if st != nil {
			st.Set("AZURE_EXT_GW_IP", extGw)
			st.Set("AZURE_EXT_SELF_IP", extSelfIP)
		}
	}

	if cl.Pattern == intent.PatternDualInterface && cl.Network.Subnets.Internal != "" {
		intSubnet := cl.Network.Subnets.Internal
		intGw, err := netcalc.Nth(intSubnet, 1)
		if err != nil {
			return fmt.Errorf("phase25: calculating internal gateway: %w", err)
		}
		intSelfIP, err := netcalc.Nth(intSubnet, 10)
		if err != nil {
			return fmt.Errorf("phase25: calculating internal Self IP: %w", err)
		}
		if st != nil {
			st.Set("AZURE_INT_GW_IP", intGw)
			st.Set("AZURE_INT_SELF_IP", intSelfIP)
		}
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "[phase 25] [dry-run] discovered extPCI=%s intPCI=%s extGw=%s\n", extPCI, intPCI, extGw)
		return nil
	}

	if st != nil {
		if err := st.Save(); err != nil {
			return fmt.Errorf("phase25: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 25] discovery complete (extPCI=%s extGw=%s extSelfIP=%s)\n", extPCI, extGw, extSelfIP)
	return nil
}
