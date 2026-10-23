package migrate

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
)

// Options tunes Translate.
type Options struct {
	// InfraName names the generated Infra CR. Default DefaultInfraName.
	InfraName string
	// GatewayClassName is the BNK GatewayClass the EgressGateways reference.
	// Empty = the class of the first BNK Gateway, else the first GatewayClass
	// whose controllerName starts with "f5.com/".
	GatewayClassName string
	// ZoneByNetwork maps a VLAN name to the availability zone of its subnet.
	// The 2.4 controller groups TMMs per zone and only counts pools that carry
	// one when availability zones are enforced, so bnkctl fills this from cluster.yaml.
	ZoneByNetwork map[string]string
	// SingleSelfIP keeps each self-IP pool at exactly the 2.3 address. The
	// default widens it to the /27 block around it (intent.SelfIPPoolRange),
	// because the F5 IPAM controller splits a pool into per-device blocks and
	// never allocates from a single address.
	SingleSelfIP bool
	// TargetVersion is the target BNK version (e.g. "2.4.0", "2.5.0") when specified via --to.
	TargetVersion string
	// ServedGatewayAPIVersion overrides the apiVersion used for Infra, GatewaySettings,
	// EgressGateway, SecPolicy, and NetPolicy (defaults to gateway.k8s.f5.com/v1alpha1 on 2.4,
	// or gateway.k8s.f5.com/v1 when TargetVersion >= 2.5).
	ServedGatewayAPIVersion string
}

func (o Options) infraName() string {
	if o.InfraName != "" {
		return o.InfraName
	}
	return DefaultInfraName
}

func (o Options) gatewayAPIVersion() string {
	if strings.TrimSpace(o.ServedGatewayAPIVersion) != "" {
		return strings.TrimSpace(o.ServedGatewayAPIVersion)
	}
	cleanVer := strings.TrimPrefix(strings.TrimSpace(o.TargetVersion), "v")
	if strings.HasPrefix(cleanVer, "2.5") || strings.HasPrefix(cleanVer, "2.6") {
		return GatewayF5Group + "/v1"
	}
	return GatewayF5Group + "/" + GatewayF5Version
}

// translator carries the state of one Translate call.
type translator struct {
	inv  *Inventory
	opts Options
	plan *Plan

	nads         []string                              // CNEInstance spec.networkAttachments, trunk order
	externalNets []string                              // VLAN network names with internal != true
	ipams        []map[string]any                      // Infra spec.ipams
	ipamSeen     map[string]bool                       // ipam names already added
	attachments  []map[string]any                      // Infra spec.networkAttachments
	vrfs         []map[string]any                      // Infra spec.vrfs
	networks     []map[string]any                      // Infra spec.networks
	routes       []map[string]any                      // Infra spec.staticRoutes
	extGateway   string                                // first host of the external VLAN subnet (default route next hop)
	egressDef    map[string]any                        // Infra spec.egressDefaults
	settings     map[string]*unstructured.Unstructured // key ns/name
	warned       map[string]bool
}

// Translate derives the 2.4 configuration for inv. It never touches the
// cluster; Apply does. An inventory without VLANs yields no Infra CR (and a
// warning), because the controller cannot program networks it does not know.
func Translate(inv *Inventory, opts Options) (*Plan, error) {
	if inv == nil {
		return nil, fmt.Errorf("translate: nil inventory")
	}
	t := &translator{
		inv:      inv,
		opts:     opts,
		plan:     &Plan{},
		ipamSeen: map[string]bool{},
		settings: map[string]*unstructured.Unstructured{},
		warned:   map[string]bool{},
	}
	t.loadNADs()
	t.vlans()
	t.vrfList()
	t.vxlans()
	t.staticRoutes()
	t.snatpools()
	t.gateways()
	t.egresses()
	t.policies()
	t.buildInfra()
	t.collectSettings()
	if len(inv.Vlans) == 0 {
		t.warn("no F5SPKVlan found in %s: no Infra CR generated (the 2.4 controller needs the VLANs in Infra spec.networks)", inv.Namespace)
	}
	return t.plan, nil
}

// warn records a warning once.
func (t *translator) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if t.warned[msg] {
		return
	}
	t.warned[msg] = true
	t.plan.Warnings = append(t.plan.Warnings, msg)
}

// loadNADs reads CNEInstance spec.networkAttachments: the Nth entry is TMM
// trunk 1.N (or socket 0.(N+1)), which is how a 2.3 F5SPKVlan interfaces entry maps to a NAD.
func (t *translator) loadNADs() {
	if t.inv.CNEInstance == nil {
		return
	}
	list, _, _ := unstructured.NestedStringSlice(t.inv.CNEInstance.Object, "spec", "networkAttachments")
	t.nads = list
}

// nadForInterface maps a 2.3 trunk interface ("1.1" or socket "0.2") to the NAD name.
func (t *translator) nadForInterface(iface string) (string, bool) {
	parts := strings.Split(iface, ".")
	if len(parts) != 2 {
		return "", false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", false
	}
	idx := n - 1
	if parts[0] == "0" {
		idx = n - 2
	}
	if idx < 0 || idx >= len(t.nads) {
		return "", false
	}
	return t.nads[idx], true
}

func (t *translator) addIpam(name string, pools []map[string]any) {
	if t.ipamSeen[name] || len(pools) == 0 {
		return
	}
	t.ipamSeen[name] = true
	t.ipams = append(t.ipams, map[string]any{"name": name, "ipPools": toAnySlice(pools)})
}

// vlans turns every F5SPKVlan into a self-IP pool, a network attachment and a
// VLAN network. Names are kept: the ZebOS interface a BGP ConfigMap refers to
// is the network name.
func (t *translator) vlans() {
	for _, v := range t.inv.Vlans {
		name := stringAt(v.Object, "spec", "name")
		if name == "" {
			name = v.GetName()
		}
		internal, _, _ := unstructured.NestedBool(v.Object, "spec", "internal")
		if !internal {
			t.externalNets = append(t.externalNets, name)
		}
		zone := t.opts.ZoneByNetwork[name]

		// Self IPs -> IPAM pool.
		selfIPs, _, _ := unstructured.NestedStringSlice(v.Object, "spec", "selfip_v4s")
		if !internal && t.extGateway == "" && len(selfIPs) > 0 {
			if plen, ok := intAt(v.Object, "spec", "prefixlen_v4"); ok {
				t.extGateway = firstHost(selfIPs[0], int(plen))
			}
		}
		var pools []map[string]any
		for _, ip := range selfIPs {
			pool := map[string]any{}
			start, end := ip, ip
			if !t.opts.SingleSelfIP {
				if s, e, err := intent.SelfIPPoolRange(ip); err == nil {
					start, end = s, e
				} else {
					t.warn("F5SPKVlan %s: self IP %s kept as a single-address pool (%v); the IPAM controller may not allocate from it", name, ip, err)
				}
			}
			pool["rangeStart"] = start
			pool["rangeEnd"] = end
			if zone != "" {
				pool["availabilityZone"] = zone
			}
			pools = append(pools, pool)
		}
		if v6, _, _ := unstructured.NestedStringSlice(v.Object, "spec", "selfip_v6s"); len(v6) > 0 {
			t.warn("F5SPKVlan %s: IPv6 self IPs %v not carried over (add an IPv6 ipPool to Infra ipam %s-selfip by hand)", name, v6, name)
		}
		if len(pools) == 0 {
			t.warn("F5SPKVlan %s has no selfip_v4s: network %s gets no ipamRefs", name, name)
		}
		if zone == "" {
			t.warn("F5SPKVlan %s: no availability zone known for its pool; the 2.4 controller only counts zoned pools when availability zones are enforced (pass --config so bnkctl reads network topology)", name)
		}
		ipamName := name + "-selfip"
		t.addIpam(ipamName, pools)

		// Trunk interface -> NAD -> network attachment.
		attachName := name + "-attach"
		ifaces, _, _ := unstructured.NestedStringSlice(v.Object, "spec", "interfaces")
		var nadNames []string
		for _, iface := range ifaces {
			if nad, ok := t.nadForInterface(iface); ok {
				nadNames = append(nadNames, nad)
			} else {
				t.warn("F5SPKVlan %s: interface %q has no matching CNEInstance networkAttachments entry; set Infra networkAttachments[%s].networkAttachmentDefinitions by hand", name, iface, attachName)
			}
		}
		nadRefs := make([]map[string]any, 0, len(nadNames))
		for _, n := range nadNames {
			nadRefs = append(nadRefs, map[string]any{"name": n})
		}
		t.attachments = append(t.attachments, map[string]any{
			"name":                         attachName,
			"networkAttachmentDefinitions": toAnySlice(nadRefs),
		})

		// Network.
		vlan := map[string]any{
			"networkAttachmentRef": map[string]any{"name": attachName},
		}
		tag, found := intAt(v.Object, "spec", "tag")
		if found {
			vlan["tag"] = tag
		} else {
			vlan["tag"] = int64(0)
		}
		if mtu, ok := intAt(v.Object, "spec", "mtu"); ok && mtu > 0 {
			vlan["mtu"] = mtu
		}
		if len(pools) > 0 {
			vlan["ipamRefs"] = []any{map[string]any{"name": ipamName}}
		}
		t.networks = append(t.networks, map[string]any{"name": name, "type": "vlan", "vlan": vlan})

		if svc, _, _ := unstructured.NestedSlice(v.Object, "spec", "allowed_services"); len(svc) > 0 {
			t.warn("F5SPKVlan %s: allowed_services dropped; the 2.4 Infra CR has no per-VLAN allowed-services and BGP tcp/179 + BFD udp/3784 reach the routing container by default", name)
		}
	}
	sort.Strings(t.externalNets)
}

func (t *translator) vrfList() {
	for _, v := range t.inv.Vrfs {
		entry := map[string]any{"name": v.GetName()}
		if strict, ok, _ := unstructured.NestedBool(v.Object, "spec", "strictIsolation"); ok {
			entry["strictIsolation"] = strict
		}
		if ic, _, _ := unstructured.NestedStringSlice(v.Object, "spec", "interconnectVrf"); len(ic) > 0 {
			t.warn("Vrf %s: interconnectVrf %v has no Infra field; keep the Vrf CR for it", v.GetName(), ic)
		}
		t.vrfs = append(t.vrfs, entry)
	}
}

// vxlans turns every Vxlan CR into an Infra vxlan network with a VTEP pool.
func (t *translator) vxlans() {
	for _, v := range t.inv.Vxlans {
		name := v.GetName()
		vx := map[string]any{}
		if vni, ok := intAt(v.Object, "spec", "vni"); ok && vni > 0 {
			vx["vni"] = vni
		}
		if mtu, ok := intAt(v.Object, "spec", "mtu"); ok && mtu > 0 {
			vx["mtu"] = mtu
		}
		if port, ok := intAt(v.Object, "spec", "port"); ok && port > 0 {
			vx["port"] = port
		}
		if vrf := stringAt(v.Object, "spec", "vrf"); vrf != "" {
			vx["vrfRef"] = map[string]any{"name": vrf}
		}
		plen4, ok4 := intAt(v.Object, "spec", "ipv4PrefixLen")
		cidr := cidrFrom(stringAt(v.Object, "spec", "ipv4Subnet"), plen4, ok4)
		if cidr == "" {
			plenL, okL := intAt(v.Object, "spec", "localIpPrefixlen")
			cidr = cidrFrom(stringAt(v.Object, "spec", "localIpSubnet"), plenL, okL)
		}
		poolName := name + "-vtep"
		if cidr != "" {
			t.addIpam(poolName, []map[string]any{{"cidr": cidr}})
		} else {
			t.warn("Vxlan %s: no ipv4Subnet/localIpSubnet; Infra network %s gets no VTEP pool", name, name)
		}
		if vlan := stringAt(v.Object, "spec", "vlan"); vlan != "" {
			vx["networkRef"] = map[string]any{"name": vlan}
			if cidr != "" {
				vx["ipamRefs"] = []any{map[string]any{"name": poolName}}
			}
		} else if cidr != "" {
			vx["vtepIpamRefs"] = []any{map[string]any{"name": poolName}}
		} else {
			t.warn("Vxlan %s: neither an underlay vlan nor a VTEP subnet; the Infra CRD requires one of networkRef / vtepIpamRefs", name)
		}
		if evpn, _, _ := unstructured.NestedBool(v.Object, "spec", "evpn"); evpn {
			t.warn("Vxlan %s: evpn has no Infra field; EVPN stays in the BGP configuration", name)
		}
		t.networks = append(t.networks, map[string]any{"name": name, "type": "vxlan", "vxlan": vx})
	}
}

// staticRoutes copies gateway-type F5SPKStaticRoutes. A cluster without any
// gets the default route the fresh 2.4 path always renders (0.0.0.0/0 via the
// external VLAN gateway, the first host of its subnet): without it TMM
// can only answer clients on the external subnet itself.
func (t *translator) staticRoutes() {
	for _, r := range t.inv.StaticRoutes {
		name := r.GetName()
		typ := stringAt(r.Object, "spec", "type")
		if typ != "" && typ != "gateway" {
			t.warn("F5SPKStaticRoute %s: type %q not translated (Infra staticRoutes carry nextHop or networkRef routes only)", name, typ)
			continue
		}
		dest := stringAt(r.Object, "spec", "destination")
		prefix, ok := intAt(r.Object, "spec", "prefixLen")
		if dest == "" || !ok {
			t.warn("F5SPKStaticRoute %s: destination/prefixLen missing, skipped", name)
			continue
		}
		gw := stringAt(r.Object, "spec", "gateway")
		if gw == "" {
			t.warn("F5SPKStaticRoute %s: no gateway, skipped", name)
			continue
		}
		t.routes = append(t.routes, map[string]any{
			"name":         name,
			"destinations": []any{fmt.Sprintf("%s/%d", dest, prefix)},
			"nextHop":      gw,
		})
	}
	if len(t.routes) == 0 && t.extGateway != "" {
		t.routes = append(t.routes, map[string]any{
			"name":         render.InfraRouteDefault,
			"destinations": []any{"0.0.0.0/0"},
			"nextHop":      t.extGateway,
		})
		t.warn("no F5SPKStaticRoute: Infra staticRoutes gets the default route 0.0.0.0/0 via %s (external VLAN gateway), as bnkctl up renders it; without it TMM answers only clients on the external subnet", t.extGateway)
	}
}

// snatpools turns F5SPKSnatpool address lists into IPAM pools the
// GatewaySettings sourceNATPools reference.
func (t *translator) snatpools() {
	for _, s := range t.inv.Snatpools {
		name := s.GetName()
		lists, _, _ := unstructured.NestedSlice(s.Object, "spec", "addressList")
		var pools []map[string]any
		for _, l := range lists {
			addrs, ok := l.([]any)
			if !ok {
				continue
			}
			for _, a := range addrs {
				ip, _ := a.(string)
				if net.ParseIP(ip) == nil {
					continue
				}
				pool := map[string]any{"rangeStart": ip, "rangeEnd": ip}
				if zone := t.externalZone(); zone != "" {
					pool["availabilityZone"] = zone
				}
				pools = append(pools, pool)
			}
		}
		if len(pools) == 0 {
			t.warn("F5SPKSnatpool %s: no IPv4 addresses found, skipped", name)
			continue
		}
		t.addIpam(snatIpamName(name), pools)
	}
}

func snatIpamName(snatpool string) string { return snatpool + "-snat" }

// bnkGatewayClass returns the GatewayClass the plan binds egress to.
func (t *translator) bnkGatewayClass(bnkGateways []*unstructured.Unstructured) string {
	if t.opts.GatewayClassName != "" {
		return t.opts.GatewayClassName
	}
	if len(bnkGateways) > 0 {
		return stringAt(bnkGateways[0].Object, "spec", "gatewayClassName")
	}
	for _, gc := range t.inv.GatewayClasses {
		if isBNKController(stringAt(gc.Object, "spec", "controllerName")) {
			return gc.GetName()
		}
	}
	return ""
}

func isBNKController(controllerName string) bool {
	return strings.HasPrefix(controllerName, "f5.com/")
}

// bnkGateways filters inv.Gateways to those on a BNK GatewayClass. With no
// GatewayClass visible every Gateway is taken (and a warning raised).
func (t *translator) bnkGateways() []*unstructured.Unstructured {
	classes := map[string]bool{}
	for _, gc := range t.inv.GatewayClasses {
		if isBNKController(stringAt(gc.Object, "spec", "controllerName")) {
			classes[gc.GetName()] = true
		}
	}
	if t.opts.GatewayClassName != "" {
		classes[t.opts.GatewayClassName] = true
	}
	var out []*unstructured.Unstructured
	for _, gw := range t.inv.Gateways {
		cls := stringAt(gw.Object, "spec", "gatewayClassName")
		if len(classes) == 0 {
			t.warn("no GatewayClass with an f5.com/ controllerName found; treating every Gateway as a BNK Gateway")
			out = append(out, gw)
			continue
		}
		if classes[cls] {
			out = append(out, gw)
		}
	}
	return out
}

// gateways builds a GatewaySettings per BNK Gateway and the parametersRef
// patch that binds them. The listener pool comes from the F5BnkGateway of the
// same namespace (same name preferred, else the only one), falling back to
// the Gateway's static addresses.
func (t *translator) gateways() {
	byNS := map[string][]*unstructured.Unstructured{}
	for _, b := range t.inv.BnkGateways {
		byNS[b.GetNamespace()] = append(byNS[b.GetNamespace()], b)
	}
	used := map[string]bool{}
	for _, gw := range t.bnkGateways() {
		ns, name := gw.GetNamespace(), gw.GetName()
		var pools []string
		if b := pickBnkGateway(byNS[ns], name); b != nil {
			used[ns+"/"+b.GetName()] = true
			pools = t.listenerPools(b)
		} else {
			pools = t.staticPool(gw)
		}
		if len(pools) == 0 {
			t.warn("Gateway %s/%s: no F5BnkGateway in its namespace and no static address; no GatewaySettings generated (the VIP source is unknown)", ns, name)
			continue
		}
		gs := t.settingsFor(ns, name)
		ipamRefs := make([]any, 0, len(pools))
		for _, p := range pools {
			ipamRefs = append(ipamRefs, map[string]any{"name": p})
		}
		networkRefs := make([]any, 0, len(t.externalNets))
		for _, n := range t.externalNets {
			networkRefs = append(networkRefs, map[string]any{"name": n})
		}
		if len(networkRefs) == 0 {
			t.warn("Gateway %s/%s: no external F5SPKVlan found; GatewaySettings %s has no networkRefs", ns, name, name)
		}
		listener := map[string]any{
			"ipamRefs":        ipamRefs,
			"sourceNATConfig": map[string]any{"type": "Automap"},
		}
		if len(networkRefs) > 0 {
			listener["networkRefs"] = networkRefs
		}
		_ = unstructured.SetNestedField(gs.Object, listener, "spec", "ingressConfig", "defaultListenerNetwork")
		t.warn("ingress SNAT is Automap on every GatewaySettings (the 2.3 default); set sourceNATConfig.type None to keep client addresses")

		patch := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GatewayAPIGroup + "/v1",
			"kind":       "Gateway",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec": map[string]any{
				"infrastructure": map[string]any{
					"parametersRef": map[string]any{
						"group": GatewayF5Group,
						"kind":  "GatewaySettings",
						"name":  name,
					},
				},
			},
		}}
		t.plan.GatewayPatches = append(t.plan.GatewayPatches, patch)
	}
	for _, b := range t.inv.BnkGateways {
		if !used[b.GetNamespace()+"/"+b.GetName()] {
			t.warn("F5BnkGateway %s/%s serves no BNK Gateway; its pool is not carried over", b.GetNamespace(), b.GetName())
		}
	}
}

// pickBnkGateway returns the F5BnkGateway for a Gateway: same name first,
// else the single one in the namespace.
func pickBnkGateway(candidates []*unstructured.Unstructured, gatewayName string) *unstructured.Unstructured {
	for _, c := range candidates {
		if c.GetName() == gatewayName {
			return c
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

// listenerPools adds one Infra IPAM per F5BnkGateway listener network and
// returns their names.
func (t *translator) listenerPools(b *unstructured.Unstructured) []string {
	items, _, _ := unstructured.NestedSlice(b.Object, "spec", "ingressConfig", "defaultListenerNetworks")
	base := b.GetNamespace() + "-" + b.GetName()
	var names []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		poolName := base
		if len(items) > 1 {
			if n, _ := m["name"].(string); n != "" {
				poolName = base + "-" + strings.ToLower(strings.ReplaceAll(n, "_", "-"))
			}
		}
		pool := map[string]any{}
		start, _ := m["startAddress"].(string)
		end, _ := m["endAddress"].(string)
		cidr, _ := m["ipv4BaseCidr"].(string)
		if cidr == "" {
			cidr, _ = m["ipv6BaseCidr"].(string)
		}
		switch {
		case start != "" && end != "":
			pool["rangeStart"], pool["rangeEnd"] = start, end
		case cidr != "":
			pool["cidr"] = cidr
		default:
			t.warn("F5BnkGateway %s/%s: listener network without addresses skipped", b.GetNamespace(), b.GetName())
			continue
		}
		if zone := t.externalZone(); zone != "" {
			pool["availabilityZone"] = zone
		}
		t.addIpam(poolName, []map[string]any{pool})
		names = append(names, poolName)
	}
	return names
}

// staticPool builds a pool from a Gateway's static IPv4 addresses.
func (t *translator) staticPool(gw *unstructured.Unstructured) []string {
	addrs, _, _ := unstructured.NestedSlice(gw.Object, "spec", "addresses")
	var ips []net.IP
	for _, a := range addrs {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ != "" && typ != "IPAddress" {
			continue
		}
		v, _ := m["value"].(string)
		if ip := net.ParseIP(v).To4(); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil
	}
	sort.Slice(ips, func(i, j int) bool { return bytesCompare(ips[i], ips[j]) < 0 })
	pool := map[string]any{"rangeStart": ips[0].String(), "rangeEnd": ips[len(ips)-1].String()}
	if zone := t.externalZone(); zone != "" {
		pool["availabilityZone"] = zone
	}
	name := gw.GetNamespace() + "-" + gw.GetName() + "-static"
	t.addIpam(name, []map[string]any{pool})
	t.warn("Gateway %s/%s: listener pool %s is only its static address(es); widen it before adding Gateways in that namespace", gw.GetNamespace(), gw.GetName(), name)
	return []string{name}
}

// externalZone is the zone of the first external VLAN, used for VIP pools.
func (t *translator) externalZone() string {
	for _, n := range t.externalNets {
		if z := t.opts.ZoneByNetwork[n]; z != "" {
			return z
		}
	}
	return ""
}

// settingsFor returns the GatewaySettings ns/name, creating it on first use.
func (t *translator) settingsFor(ns, name string) *unstructured.Unstructured {
	key := ns + "/" + name
	if gs, ok := t.settings[key]; ok {
		return gs
	}
	gs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": t.opts.gatewayAPIVersion(),
		"kind":       "GatewaySettings",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	t.settings[key] = gs
	return gs
}

// egresses turns every F5SPKEgress into Infra egressDefaults (tunnel), one
// GatewaySettings egressConfig + EgressGateway per tenant namespace and, for
// firewallEnforcedPolicy, a SecPolicy that targets the EgressGateway.
func (t *translator) egresses() {
	gwClass := t.bnkGatewayClass(t.bnkGateways())
	for _, e := range t.inv.Egresses {
		name := e.GetName()
		vx, _, _ := unstructured.NestedMap(e.Object, "spec", "pseudoCNIConfig", "vxlan")
		tmmIf, _ := vx["tmmInterfaceName"].(string)
		if tmmIf == "" {
			tmmIf = t.defaultTunnelNetwork()
			if tmmIf != "" {
				t.warn("F5SPKEgress %s: no vxlan.tmmInterfaceName; tunnel network defaults to %s", name, tmmIf)
			}
		}
		def := map[string]any{}
		if tmmIf != "" {
			def["networkRef"] = map[string]any{"name": tmmIf}
		}
		subnet, _ := vx["ipv4Subnet"].(string)
		if plen, ok := anyInt(vx["ipv4PrefixLen"]); ok && subnet != "" {
			def["subnet"] = fmt.Sprintf("%s/%d", subnet, plen)
		}
		if port, ok := anyInt(vx["port"]); ok && port > 0 {
			def["port"] = port
		}
		if v6, _ := vx["ipv6Subnet"].(string); v6 != "" {
			t.warn("F5SPKEgress %s: IPv6 tunnel subnet %s has no Infra egressDefaults field", name, v6)
		}
		if nodeIf, _ := vx["nodeInterfaceName"].(string); nodeIf != "" {
			t.warn("F5SPKEgress %s: vxlan.nodeInterfaceName %q dropped; in 2.4 the product creates the node side of the tunnel", name, nodeIf)
		}
		if t.egressDef == nil {
			if len(def) > 0 {
				t.egressDef = def
			}
		} else if fmt.Sprint(t.egressDef) != fmt.Sprint(def) && len(def) > 0 {
			t.warn("F5SPKEgress %s: tunnel settings differ from the first egress; Infra egressDefaults keeps the first", name)
		}

		snat := t.snatConfig(e)
		namespaces, _, _ := unstructured.NestedStringSlice(e.Object, "spec", "pseudoCNIConfig", "namespaces")
		if len(namespaces) == 0 {
			t.warn("F5SPKEgress %s: pseudoCNIConfig.namespaces is empty; no EgressGateway generated", name)
		}
		for _, k := range []string{"dnsNat46Enabled", "dualStackEnabled", "debugLogEnabled", "egressSnatpoolProtectionEnabled"} {
			if b, ok, _ := unstructured.NestedBool(e.Object, "spec", k); ok && b {
				t.warn("F5SPKEgress %s: %s has no 2.4 equivalent", name, k)
			}
		}
		if n, ok := intAt(e.Object, "spec", "maxTmmReplicas"); ok && n > 0 {
			t.warn("F5SPKEgress %s: maxTmmReplicas has no 2.4 equivalent", name)
		}
		fw := stringAt(e.Object, "spec", "firewallEnforcedPolicy")

		for _, ns := range namespaces {
			gs := t.settingsFor(ns, name)
			cfg := map[string]any{"name": name, "sourceNATConfig": snat}
			if tmmIf != "" {
				cfg["networkRef"] = map[string]any{"name": tmmIf}
			}
			existing, _, _ := unstructured.NestedSlice(gs.Object, "spec", "egressConfigs")
			_ = unstructured.SetNestedSlice(gs.Object, append(existing, cfg), "spec", "egressConfigs")
			if poolRef, ok := snat["sourceNATPoolRef"].(map[string]any); ok {
				t.addSourceNATPool(gs, poolRef["name"].(string))
			}

			eg := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": t.opts.gatewayAPIVersion(),
				"kind":       "EgressGateway",
				"metadata":   map[string]any{"name": name, "namespace": ns},
				"spec": map[string]any{
					"gatewayClassName": gwClass,
					"infrastructure": map[string]any{
						"parametersRef": map[string]any{"name": name, "sectionName": name},
					},
					"sourceSelector": map[string]any{
						"selectionMode": "NamespaceSelector",
						"namespaces":    map[string]any{"matchNames": []any{ns}},
					},
				},
			}}
			if gwClass == "" {
				t.warn("EgressGateway %s/%s: no BNK GatewayClass found; fill spec.gatewayClassName (or pass --gateway-class)", ns, name)
			}
			t.plan.EgressGateways = append(t.plan.EgressGateways, eg)

			if fw != "" {
				t.plan.Policies = append(t.plan.Policies, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": t.opts.gatewayAPIVersion(),
					"kind":       "SecPolicy",
					"metadata":   map[string]any{"name": fw, "namespace": ns},
					"spec": map[string]any{
						"targetRefs": []any{map[string]any{
							"group": GatewayF5Group, "kind": "EgressGateway", "name": name,
						}},
						"extensionRefs": []any{map[string]any{
							"group": LegacyGroup, "kind": "F5BigFwPolicy", "name": fw,
						}},
					},
				}})
				t.warn("F5SPKEgress %s: firewallEnforcedPolicy %s becomes SecPolicy %s/%s; the F5BigFwPolicy (and its address lists) must exist in namespace %s", name, fw, ns, fw, ns)
			}
		}
	}
}

// snatConfig maps the 2.3 snatType to a 2.4 sourceNATConfig.
func (t *translator) snatConfig(e *unstructured.Unstructured) map[string]any {
	name := e.GetName()
	switch typ := stringAt(e.Object, "spec", "snatType"); typ {
	case "", "SRC_TRANS_AUTOMAP":
		return map[string]any{"type": "Automap"}
	case "SRC_TRANS_NONE":
		return map[string]any{"type": "None"}
	case "SRC_TRANS_SNATPOOL":
		pool := stringAt(e.Object, "spec", "egressSnatpool")
		if pool == "" {
			t.warn("F5SPKEgress %s: SRC_TRANS_SNATPOOL without egressSnatpool; using Automap", name)
			return map[string]any{"type": "Automap"}
		}
		if !t.ipamSeen[snatIpamName(pool)] {
			t.warn("F5SPKEgress %s: F5SPKSnatpool %s not found in %s; sourceNATPools[%s] references a missing Infra ipam %s", name, pool, t.inv.Namespace, pool, snatIpamName(pool))
		}
		return map[string]any{"type": "Pool", "sourceNATPoolRef": map[string]any{"name": pool}}
	default:
		t.warn("F5SPKEgress %s: unknown snatType %q; using Automap", name, typ)
		return map[string]any{"type": "Automap"}
	}
}

// addSourceNATPool adds a GatewaySettings sourceNATPools entry once.
func (t *translator) addSourceNATPool(gs *unstructured.Unstructured, pool string) {
	existing, _, _ := unstructured.NestedSlice(gs.Object, "spec", "sourceNATPools")
	for _, p := range existing {
		if m, ok := p.(map[string]any); ok && m["name"] == pool {
			return
		}
	}
	entry := map[string]any{
		"name":     pool,
		"ipamRefs": []any{map[string]any{"name": snatIpamName(pool)}},
	}
	_ = unstructured.SetNestedSlice(gs.Object, append(existing, entry), "spec", "sourceNATPools")
}

// defaultTunnelNetwork is the internal VLAN when there is one, else the
// first external VLAN (the bnkctl convention).
func (t *translator) defaultTunnelNetwork() string {
	for _, v := range t.inv.Vlans {
		if internal, _, _ := unstructured.NestedBool(v.Object, "spec", "internal"); internal {
			if n := stringAt(v.Object, "spec", "name"); n != "" {
				return n
			}
			return v.GetName()
		}
	}
	if len(t.externalNets) > 0 {
		return t.externalNets[0]
	}
	return ""
}

// policies re-homes BNKSecPolicy / BNKNetPolicy under gateway.k8s.f5.com.
func (t *translator) policies() {
	apiVer := t.opts.gatewayAPIVersion()
	for _, p := range t.inv.SecPolicies {
		t.plan.Policies = append(t.plan.Policies, rehomePolicy(p, "SecPolicy", apiVer))
	}
	for _, p := range t.inv.NetPolicies {
		t.plan.Policies = append(t.plan.Policies, rehomePolicy(p, "NetPolicy", apiVer))
	}
}

// rehomePolicy copies name, namespace, labels, annotations and spec of a 2.3
// policy into the 2.4 kind. Status and the kubectl last-applied annotation are
// dropped.
func rehomePolicy(src *unstructured.Unstructured, kind, apiVer string) *unstructured.Unstructured {
	meta := map[string]any{"name": src.GetName(), "namespace": src.GetNamespace()}
	if l := src.GetLabels(); len(l) > 0 {
		meta["labels"] = stringMapToAny(l)
	}
	ann := map[string]string{}
	for k, v := range src.GetAnnotations() {
		if k == "kubectl.kubernetes.io/last-applied-configuration" {
			continue
		}
		ann[k] = v
	}
	if len(ann) > 0 {
		meta["annotations"] = stringMapToAny(ann)
	}
	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVer,
		"kind":       kind,
		"metadata":   meta,
		"spec":       spec,
	}}
}

// buildInfra assembles the Infra CR from the collected pieces.
func (t *translator) buildInfra() {
	if len(t.inv.Vlans) == 0 {
		return
	}
	spec := map[string]any{
		"ipams":              toAnySlice(t.ipams),
		"networkAttachments": toAnySlice(t.attachments),
		"networks":           toAnySlice(t.networks),
	}
	if len(t.vrfs) > 0 {
		spec["vrfs"] = toAnySlice(t.vrfs)
	}
	if len(t.routes) > 0 {
		spec["staticRoutes"] = toAnySlice(t.routes)
	}
	// The Infra always carries egressDefaults, as the fresh 2.4 path renders
	// it: without the block the controller defaults the tunnel subnet to
	// 10.0.0.0/16, which overlaps the self-IP pools of every 10.x VPC and
	// leaves the Infra Programmed=False (PartiallyInvalid,
	// EgressDefaultsSubnetConflict). Seen live 2026-09-14.
	if t.egressDef == nil {
		t.egressDef = map[string]any{}
		if net := t.defaultTunnelNetwork(); net != "" {
			t.egressDef["networkRef"] = map[string]any{"name": net}
		}
		t.egressDef["port"] = int64(render.InfraEgressPort)
		t.warn("no F5SPKEgress: Infra egressDefaults set to the bnkctl defaults (subnet %s, port %d); the controller default 10.0.0.0/16 would overlap the VPC self-IP pools", render.InfraEgressSubnet, render.InfraEgressPort)
	}
	if _, ok := t.egressDef["subnet"]; !ok {
		t.egressDef["subnet"] = render.InfraEgressSubnet
	}
	spec["egressDefaults"] = t.egressDef
	t.plan.Infra = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": t.opts.gatewayAPIVersion(),
		"kind":       "Infra",
		"metadata": map[string]any{
			"name":      t.opts.infraName(),
			"namespace": t.inv.Namespace,
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "awsbnkctl"},
		},
		"spec": spec,
	}}
}

// collectSettings orders the GatewaySettings by namespace then name.
func (t *translator) collectSettings() {
	keys := make([]string, 0, len(t.settings))
	for k := range t.settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.plan.GatewaySettings = append(t.plan.GatewaySettings, t.settings[k])
	}
	sort.Slice(t.plan.EgressGateways, func(i, j int) bool {
		return objKey(t.plan.EgressGateways[i]) < objKey(t.plan.EgressGateways[j])
	})
	sort.Slice(t.plan.GatewayPatches, func(i, j int) bool {
		return objKey(t.plan.GatewayPatches[i]) < objKey(t.plan.GatewayPatches[j])
	})
}

// ─── small helpers ──────────────────────────────────────────────────────────

func objKey(o *unstructured.Unstructured) string { return o.GetNamespace() + "/" + o.GetName() }

func stringAt(obj map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}

func intAt(obj map[string]any, fields ...string) (int64, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0, false
	}
	return anyInt(v)
}

func anyInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}

func cidrFrom(subnet string, prefix int64, ok bool) string {
	if subnet == "" || !ok {
		return ""
	}
	return fmt.Sprintf("%s/%d", subnet, prefix)
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func bytesCompare(a, b net.IP) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func joinStrings(s []string, sep string) string { return strings.Join(s, sep) }

// firstHost returns the first usable address of the subnet ip/plen (the
// subnet router), or "" when the input is not IPv4.
func firstHost(ip string, plen int) string {
	addr := net.ParseIP(ip).To4()
	if addr == nil || plen < 0 || plen > 30 {
		return ""
	}
	mask := net.CIDRMask(plen, 32)
	network := addr.Mask(mask)
	network[3]++
	return network.String()
}
