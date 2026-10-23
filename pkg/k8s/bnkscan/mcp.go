package bnkscan

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DiscoverMCP finds the MCP tool servers exposed through BNK Gateways. A route
// counts as MCP when it carries AnnotationProtocol=mcp or when one of its path
// matches contains "/mcp". The persistence profile, iRules and SecPolicies of
// the parent Gateway are attached to every endpoint. Results are sorted by
// namespace/route.
func DiscoverMCP(r *raw) []MCPEndpoint {
	if r == nil {
		return nil
	}
	gateways := map[string]*unstructured.Unstructured{}
	for _, gw := range r.gateways {
		gateways[gw.GetNamespace()+"/"+gw.GetName()] = gw
	}
	profiles := map[string]*unstructured.Unstructured{}
	for _, p := range r.persistence {
		profiles[p.GetNamespace()+"/"+p.GetName()] = p
	}
	netByGateway := groupByTargetGateway(r.netPolicies)
	secByGateway := groupByTargetGateway(r.secPolicies)

	var out []MCPEndpoint
	for _, rt := range r.httpRoutes {
		parents := parentRefs(rt)
		gwKey, sectionName := "", ""
		var gw *unstructured.Unstructured
		for _, p := range parents {
			if g, ok := gateways[p.key]; ok {
				gw, gwKey, sectionName = g, p.key, p.sectionName
				break
			}
		}
		if gwKey == "" && len(parents) > 0 {
			gwKey, sectionName = parents[0].key, parents[0].sectionName
		}

		var persistence *PersistenceInfo
		var irules []string
		for _, np := range netByGateway[gwKey] {
			for _, ext := range extensionRefs(np) {
				switch ext.kind {
				case "F5BigCneIrule":
					irules = append(irules, ext.name)
				case "F5BigPersistenceProfile":
					if persistence == nil {
						persistence = persistenceInfo(np, ext.name, profiles)
					}
				}
			}
		}
		sort.Strings(irules)
		irules = dedupe(irules)

		reason := classifyRoute(rt)
		if reason == "" {
			continue
		}

		ns, name := splitKey(gwKey)
		ep := MCPEndpoint{
			Route:            rt.GetName(),
			Namespace:        rt.GetNamespace(),
			Gateway:          name,
			GatewayNamespace: ns,
			GatewayReady:     r.gatewayReady[gwKey],
			Hostnames:        stringSlice(rt.Object, "spec", "hostnames"),
			Paths:            routePaths(rt),
			Backends:         backendRefs(rt),
			IRules:           irules,
			Persistence:      persistence,
			Reason:           reason,
		}
		for _, sp := range secByGateway[gwKey] {
			ep.SecPolicies = append(ep.SecPolicies, sp.GetName())
		}
		if gw != nil {
			ep.Addresses = gatewayAddresses(gw)
			ep.Listeners = gatewayListeners(gw, sectionName)
			ep.URLs = endpointURLs(ep.Addresses, gw, sectionName, ep.Paths)
		}
		ep.Auth = routeAuth(rt, irules)
		if tools := rt.GetAnnotations()[AnnotationTools]; tools != "" {
			for _, t := range strings.Split(tools, ",") {
				if t = strings.TrimSpace(t); t != "" {
					ep.Tools = append(ep.Tools, ToolDef{Name: t})
				}
			}
		}
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Route < out[j].Route
	})
	return out
}

// classifyRoute returns why the route is an MCP endpoint, or "".
func classifyRoute(rt *unstructured.Unstructured) string {
	if strings.EqualFold(rt.GetAnnotations()[AnnotationProtocol], "mcp") {
		return "annotation " + AnnotationProtocol + "=mcp"
	}
	for _, p := range routePaths(rt) {
		if isMCPPath(p) {
			return "path " + p
		}
	}
	return ""
}

func isMCPPath(p string) bool {
	return strings.Contains(strings.ToLower(p), "/mcp")
}

type parentRef struct {
	key         string // namespace/name
	sectionName string
}

func parentRefs(rt *unstructured.Unstructured) []parentRef {
	items, _, _ := unstructured.NestedSlice(rt.Object, "spec", "parentRefs")
	var out []parentRef
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		ns, _ := m["namespace"].(string)
		if ns == "" {
			ns = rt.GetNamespace()
		}
		section, _ := m["sectionName"].(string)
		out = append(out, parentRef{key: ns + "/" + name, sectionName: section})
	}
	return out
}

// routePaths returns the path match values, MCP paths first, "/" when the
// route has no path match.
func routePaths(rt *unstructured.Unstructured) []string {
	rules, _, _ := unstructured.NestedSlice(rt.Object, "spec", "rules")
	var mcp, other []string
	for _, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		matches, _ := rm["matches"].([]any)
		for _, m := range matches {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			path, _ := mm["path"].(map[string]any)
			v, _ := path["value"].(string)
			if v == "" {
				continue
			}
			if isMCPPath(v) {
				mcp = append(mcp, v)
			} else {
				other = append(other, v)
			}
		}
	}
	out := dedupe(append(mcp, other...))
	if len(out) == 0 {
		out = []string{"/"}
	}
	return out
}

// backendRefs returns namespace/service:port for every backendRef.
func backendRefs(rt *unstructured.Unstructured) []string {
	rules, _, _ := unstructured.NestedSlice(rt.Object, "spec", "rules")
	var out []string
	for _, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		refs, _ := rm["backendRefs"].([]any)
		for _, b := range refs {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			name, _ := bm["name"].(string)
			if name == "" {
				continue
			}
			ns, _ := bm["namespace"].(string)
			if ns == "" {
				ns = rt.GetNamespace()
			}
			s := ns + "/" + name
			if port, ok := bm["port"]; ok {
				s += fmt.Sprintf(":%v", port)
			}
			out = append(out, s)
		}
	}
	return dedupe(out)
}

// gatewayAddresses returns spec.addresses then status.addresses values.
func gatewayAddresses(gw *unstructured.Unstructured) []string {
	var out []string
	for _, path := range [][]string{{"spec", "addresses"}, {"status", "addresses"}} {
		items, _, _ := unstructured.NestedSlice(gw.Object, path...)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if v, _ := m["value"].(string); v != "" {
				out = append(out, v)
			}
		}
	}
	return dedupe(out)
}

type listener struct {
	name, protocol string
	port           int64
}

func listenersOf(gw *unstructured.Unstructured, sectionName string) []listener {
	items, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	var out []listener
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		l := listener{}
		l.name, _ = m["name"].(string)
		l.protocol, _ = m["protocol"].(string)
		l.port = nestedNumber(m, "port")
		if sectionName != "" && l.name != sectionName {
			continue
		}
		out = append(out, l)
	}
	return out
}

// gatewayListeners renders name:protocol:port for the Gateway listeners.
func gatewayListeners(gw *unstructured.Unstructured, sectionName string) []string {
	var out []string
	for _, l := range listenersOf(gw, sectionName) {
		out = append(out, fmt.Sprintf("%s:%s:%d", l.name, l.protocol, l.port))
	}
	return out
}

// endpointURLs builds scheme://address[:port]/path for the HTTP(S) listeners.
// Only the MCP paths are used; when none is MCP the first path is.
func endpointURLs(addresses []string, gw *unstructured.Unstructured, sectionName string, paths []string) []string {
	var urlPaths []string
	for _, p := range paths {
		if isMCPPath(p) {
			urlPaths = append(urlPaths, p)
		}
	}
	if len(urlPaths) == 0 && len(paths) > 0 {
		urlPaths = paths[:1]
	}
	var out []string
	for _, addr := range addresses {
		for _, l := range listenersOf(gw, sectionName) {
			scheme := ""
			switch strings.ToUpper(l.protocol) {
			case "HTTP":
				scheme = "http"
			case "HTTPS":
				scheme = "https"
			default:
				continue
			}
			host := addr
			if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
				host = "[" + addr + "]"
			}
			if !(scheme == "http" && l.port == 80) && !(scheme == "https" && l.port == 443) && l.port != 0 {
				host = fmt.Sprintf("%s:%d", host, l.port)
			}
			for _, p := range urlPaths {
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				out = append(out, scheme+"://"+host+p)
			}
		}
	}
	return dedupe(out)
}

// routeAuth reads AnnotationAuth, falling back to "irule" when a governance
// iRule is attached and "unknown" otherwise.
func routeAuth(rt *unstructured.Unstructured, irules []string) string {
	if a := rt.GetAnnotations()[AnnotationAuth]; a != "" {
		return a
	}
	if len(irules) > 0 {
		return "irule"
	}
	return "unknown"
}

// groupByTargetGateway indexes policies by the namespace/name of each Gateway
// in their targetRefs. A targetRef without namespace targets the policy's own.
func groupByTargetGateway(policies []*unstructured.Unstructured) map[string][]*unstructured.Unstructured {
	out := map[string][]*unstructured.Unstructured{}
	for _, p := range policies {
		refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
		for _, r := range refs {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := m["kind"].(string)
			if kind != "" && kind != "Gateway" {
				continue
			}
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			ns, _ := m["namespace"].(string)
			if ns == "" {
				ns = p.GetNamespace()
			}
			out[ns+"/"+name] = append(out[ns+"/"+name], p)
		}
	}
	return out
}

type extensionRef struct {
	kind, name string
}

func extensionRefs(p *unstructured.Unstructured) []extensionRef {
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "extensionRefs")
	var out []extensionRef
	for _, r := range refs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		e := extensionRef{}
		e.kind, _ = m["kind"].(string)
		e.name, _ = m["name"].(string)
		if e.kind == "" {
			e.kind = "F5BigCneIrule" // the CRD default
		}
		if e.name != "" {
			out = append(out, e)
		}
	}
	return out
}

// persistenceInfo resolves the profile a NetPolicy attaches.
func persistenceInfo(np *unstructured.Unstructured, name string, profiles map[string]*unstructured.Unstructured) *PersistenceInfo {
	info := &PersistenceInfo{Profile: name, Namespace: np.GetNamespace(), Type: "unknown"}
	refs, _, _ := unstructured.NestedSlice(np.Object, "spec", "targetRefs")
	for _, r := range refs {
		if m, ok := r.(map[string]any); ok {
			if s, _ := m["sectionName"].(string); s != "" {
				info.Listener = s
				break
			}
		}
	}
	p, ok := profiles[np.GetNamespace()+"/"+name]
	if !ok {
		info.Type = "missing"
		return info
	}
	if t, _, _ := unstructured.NestedString(p.Object, "spec", "persistenceType"); t != "" {
		info.Type = t
	} else {
		info.Type = "SRC_ADDR"
	}
	info.Timeout = nestedNumber(p.Object, "spec", "timeout")
	for _, field := range []string{"mcpEncryptionPassphrase", "a2aEncryptionPassphrase"} {
		if s, _, _ := unstructured.NestedString(p.Object, "spec", field, "secretRef", "name"); s != "" {
			info.SecretName = s
		}
	}
	if c, ok := findCondition(conditionsOf(p.Object, "status", "conditions"), "Programmed"); ok {
		info.Programmed = c.Status == "True"
	}
	return info
}

// nestedNumber reads an integer that JSON decoding may have left as float64.
func nestedNumber(obj map[string]any, path ...string) int64 {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !found {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func stringSlice(obj map[string]any, path ...string) []string {
	out, _, _ := unstructured.NestedStringSlice(obj, path...)
	return out
}

func splitKey(key string) (ns, name string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
