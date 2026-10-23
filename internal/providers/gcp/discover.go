package gcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
)

// ifaceDumpScript prints "<ifname> <lower-mac> <pci_bdf>" per NIC inside a
// hostNetwork pod. On GCP virtio-net (and gVNIC on virtio fallback),
// /sys/class/net/<iface>/device resolves to the virtio bus child directory
// (.../0000:00:05.0/virtio1), so basename is "virtio1" rather than the parent
// PCI BDF. Step up one directory when the leaf doesn't match DDDD:BB:DD.F.
const ifaceDumpScript = `for f in /sys/class/net/*/address; do
  iface=$(basename "$(dirname "$f")")
  [ "$iface" = "lo" ] && continue
  dev=$(readlink -f "/sys/class/net/$iface/device" 2>/dev/null || true)
  [ -z "$dev" ] && continue
  mac=$(tr '[:upper:]' '[:lower:]' < "$f")
  leaf=$(basename "$dev")
  case "$leaf" in
    *:*:*.*) pci=$leaf ;;
    *)       pci=$(basename "$(dirname "$dev")") ;;
  esac
  echo "$iface $mac $pci"
done`

// matchesVPC reports whether a Compute Engine resource's Network URL
// (which the REST API returns as a full URL like
// "https://www.googleapis.com/compute/v1/projects/<p>/global/networks/<n>"
// or a relative selfLink "projects/<p>/global/networks/<n>") refers to
// vpcSelfLink ("projects/<p>/global/networks/<n>").
func matchesVPC(networkURL, vpcSelfLink string) bool {
	if networkURL == "" || vpcSelfLink == "" {
		return false
	}
	return networkURL == vpcSelfLink || strings.HasSuffix(networkURL, "/"+vpcSelfLink)
}

// DataplaneDiscoveryResult holds the existing resources found attached to a
// shared dataplane VPC.
type DataplaneDiscoveryResult struct {
	Subnetworks []*computepb.Subnetwork
	Firewalls   []*computepb.Firewall
	Routers     []*computepb.Router
}

// DiscoverDataplaneResources lists existing subnetworks (in region), firewall
// rules (project-wide), and Cloud Routers (in region) that belong to the given
// VPC (`projects/<project>/global/networks/<vpc>`).
func DiscoverDataplaneResources(ctx context.Context, c *Clients, project, region, vpc string) (DataplaneDiscoveryResult, error) {
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", project, vpc)
	var out DataplaneDiscoveryResult

	if c == nil {
		return out, nil
	}

	if c.Subnetworks != nil {
		allSubs, err := c.Subnetworks.ListAll(ctx, project, region)
		if err != nil {
			return out, fmt.Errorf("list subnetworks in %s/%s: %w", project, region, err)
		}
		for _, s := range allSubs {
			if matchesVPC(s.GetNetwork(), vpcSelfLink) {
				out.Subnetworks = append(out.Subnetworks, s)
			}
		}
	}

	if c.Firewalls != nil {
		allFWs, err := c.Firewalls.ListAll(ctx, project)
		if err != nil {
			return out, fmt.Errorf("list firewalls in %s: %w", project, err)
		}
		for _, f := range allFWs {
			if matchesVPC(f.GetNetwork(), vpcSelfLink) {
				out.Firewalls = append(out.Firewalls, f)
			}
		}
	}

	if c.Routers != nil {
		allRouters, err := c.Routers.ListAll(ctx, project, region)
		if err != nil {
			return out, fmt.Errorf("list routers in %s/%s: %w", project, region, err)
		}
		for _, r := range allRouters {
			if matchesVPC(r.GetNetwork(), vpcSelfLink) {
				out.Routers = append(out.Routers, r)
			}
		}
	}

	return out, nil
}

// nicForVPC finds the NetworkInterface attached to the VPC whose self-link
// ends with "/networks/" + vpcName.
func nicForVPC(inst *computepb.Instance, vpcName string) (*computepb.NetworkInterface, error) {
	if inst == nil {
		return nil, fmt.Errorf("instance is nil")
	}
	suffix := "/networks/" + vpcName
	for _, nic := range inst.GetNetworkInterfaces() {
		if nic.GetNetwork() == vpcName || strings.HasSuffix(nic.GetNetwork(), suffix) {
			return nic, nil
		}
	}
	return nil, fmt.Errorf("instance %q has no NIC on network %q", inst.GetName(), vpcName)
}

// fieldForMAC parses ifaceDumpScript output ("<ifname> <mac> <pci_bdf>" per
// line) and returns field 0 (ifname) or field 2 (pci_bdf) for the line whose
// MAC matches targetMAC (case-insensitive).
func fieldForMAC(dump, targetMAC string, fieldIdx int) string {
	want := strings.ToLower(strings.TrimSpace(targetMAC))
	for _, line := range strings.Split(dump, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if strings.ToLower(fields[1]) == want && fieldIdx >= 0 && fieldIdx < len(fields) {
			return fields[fieldIdx]
		}
	}
	return ""
}

// stripCIDR returns the IP portion of a CIDR string ("10.10.2.4/32" -> "10.10.2.4")
// or the input unchanged if it has no "/".
func stripCIDR(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// lastOctetPlusOne returns the first usable host IP in cidr (network address + 1),
// which on GCP is the subnet gateway IP.
func lastOctetPlusOne(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	v4 := ipNet.IP.To4()
	if v4 == nil {
		v4 = ip.To4()
	}
	if v4 == nil {
		return "", fmt.Errorf("expected IPv4 CIDR, got %q", cidr)
	}
	gw := make(net.IP, len(v4))
	copy(gw, v4)
	gw[3]++
	return gw.String(), nil
}

func isNotFound(err error) bool {
	return hasHTTPStatus(err, http.StatusNotFound)
}

func isForbidden(err error) bool {
	return hasHTTPStatus(err, http.StatusForbidden)
}

func hasHTTPStatus(err error, code int) bool {
	if err == nil {
		return false
	}
	var gErr *googleapi.Error
	if errors.As(err, &gErr) && gErr.Code == code {
		return true
	}
	var aErr *apierror.APIError
	if errors.As(err, &aErr) && aErr.HTTPCode() == code {
		return true
	}
	return strings.Contains(err.Error(), fmt.Sprintf("code %d", code))
}

// waitOp waits on a compute.Operation when non-nil. Unit-test fakes return
// (nil, nil), so a nil op is treated as already complete.
func waitOp(ctx context.Context, op *compute.Operation, desc string) error {
	if op == nil {
		return nil
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("%s: wait operation: %w", desc, err)
	}
	return nil
}
