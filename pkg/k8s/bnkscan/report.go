package bnkscan

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteReport prints the index as operator-facing text.
func WriteReport(w io.Writer, idx *Index) {
	fmt.Fprintf(w, "BNK API generation: %s", idx.Generation)
	if len(idx.Groups) > 0 {
		fmt.Fprintf(w, "  (CRD groups: %s)", strings.Join(idx.Groups, ", "))
	}
	fmt.Fprintln(w)
	if idx.Controller.Found {
		fmt.Fprintf(w, "controller %s/%s: %d/%d available\n", idx.Controller.Namespace, idx.Controller.Name, idx.Controller.Available, idx.Controller.Desired)
	} else {
		fmt.Fprintf(w, "controller %s/%s: not found\n", idx.Controller.Namespace, idx.Controller.Name)
	}
	if idx.TMM.Found {
		fmt.Fprintf(w, "tmm %s/%s: %d/%d ready\n", idx.TMM.Namespace, idx.TMM.Name, idx.TMM.Ready, idx.TMM.Desired)
		for _, p := range idx.TMM.Pending {
			fmt.Fprintf(w, "  WAIT %s\n", p)
		}
	}

	section := func(title string, objs []Object) {
		if len(objs) == 0 {
			return
		}
		fmt.Fprintf(w, "%s (%d)\n", title, len(objs))
		for _, o := range objs {
			mark := "ok  "
			if !o.Ready {
				mark = "WAIT"
			}
			line := fmt.Sprintf("  %s %s", mark, o.ID())
			if o.Detail != "" {
				line += "  " + o.Detail
			}
			fmt.Fprintln(w, line)
		}
	}
	section("Infra", idx.Infras)
	section("GatewaySettings", idx.GatewaySettings)
	section("Gateway", idx.Gateways)
	section("HTTPRoute", idx.HTTPRoutes)
	section("EgressGateway", idx.EgressGateways)
	section("SecPolicy", idx.SecPolicies)
	section("NetPolicy", idx.NetPolicies)
	section("AppPolicy", idx.AppPolicies)
	section("F5BigPersistenceProfile", idx.PersistenceProfiles)
	section("F5BigDnsCache", idx.DNSCaches)
	section("F5BigDnsApp", idx.DNSApps)
	section("F5BigDnsZone", idx.DNSZones)
	section("F5BigCnatPolicy", idx.CGNATPolicies)
	section("F5DiameterEndpoint", idx.DiameterEndpoints)
	section("AITenant", idx.AITenants)
	section("AICredential", idx.AICredentials)
	section("AIProvider", idx.AIProviders)
	section("AIModel", idx.AIModels)
	section("AIGatewayProfile", idx.AIGatewayProfiles)
	section("AIGateway", idx.AIGateways)
	if p := idx.AIGatewayPosture; p != nil {
		sc := p.DefaultStorageClass
		if sc == "" {
			sc = "none"
		}
		fmt.Fprintf(w, "AI Gateway 2.4.1 posture: f5-aigateway=%d/%d ready=%v aigw-dns-resolver=%v dssm-sentinel=%v default-sc=%s clickhouse=%v (pvcs=%d)\n",
			p.F5AIGatewayAvailable, p.F5AIGatewayDesired, p.F5AIGatewayReady, p.AIGWDNSResolverPresent, p.DSSMSentinelReady, sc, p.ClickHouseReady, p.ClickHousePVCsBound)
	}

	if idx.Legacy.Any() {
		fmt.Fprintf(w, "legacy 2.3 CRs: F5BnkGateway=%d BNKSecPolicy=%d BNKNetPolicy=%d F5SPKVlan=%d F5SPKEgress=%d\n",
			idx.Legacy.BnkGateways, idx.Legacy.BNKSecPolicy, idx.Legacy.BNKNetPolicy, idx.Legacy.F5SPKVlans, idx.Legacy.F5SPKEgresses)
	}
	if len(idx.Missing) > 0 {
		fmt.Fprintf(w, "CRDs not served: %s\n", strings.Join(idx.Missing, ", "))
	}

	if len(idx.MCP) > 0 {
		fmt.Fprintf(w, "MCP endpoints (%d)\n", len(idx.MCP))
		for _, e := range idx.MCP {
			fmt.Fprintf(w, "  %s/%s  via Gateway %s/%s", e.Namespace, e.Route, e.GatewayNamespace, e.Gateway)
			if !e.GatewayReady {
				fmt.Fprint(w, " (not programmed)")
			}
			fmt.Fprintln(w)
			for _, u := range e.URLs {
				fmt.Fprintf(w, "    url:         %s\n", u)
			}
			if len(e.Hostnames) > 0 {
				fmt.Fprintf(w, "    hostnames:   %s\n", strings.Join(e.Hostnames, ", "))
			}
			if len(e.Backends) > 0 {
				fmt.Fprintf(w, "    backends:    %s\n", strings.Join(e.Backends, ", "))
			}
			fmt.Fprintf(w, "    auth:        %s\n", e.Auth)
			if len(e.IRules) > 0 {
				fmt.Fprintf(w, "    irules:      %s\n", strings.Join(e.IRules, ", "))
			}
			if len(e.SecPolicies) > 0 {
				fmt.Fprintf(w, "    secpolicies: %s\n", strings.Join(e.SecPolicies, ", "))
			}
			if p := e.Persistence; p != nil {
				state := "programmed"
				if !p.Programmed {
					state = "not programmed"
				}
				fmt.Fprintf(w, "    persistence: %s (%s, %s", p.Profile, p.Type, state)
				if p.Listener != "" {
					fmt.Fprintf(w, ", listener %s", p.Listener)
				}
				fmt.Fprintln(w, ")")
			} else {
				fmt.Fprintln(w, "    persistence: none")
			}
			if names := e.ToolNames(); len(names) > 0 {
				fmt.Fprintf(w, "    tools:       %s\n", strings.Join(names, ", "))
			}
			fmt.Fprintf(w, "    reason:      %s\n", e.Reason)
		}
	}

	if problems := idx.Problems(); len(problems) > 0 {
		fmt.Fprintln(w, "not ready:")
		for _, p := range problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
	} else {
		fmt.Fprintln(w, "ready: every 2.4 readiness check passed")
	}
}

// WriteJSON prints the index as indented JSON.
func WriteJSON(w io.Writer, idx *Index) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(idx)
}

// FormatReport formats the scan index as operator-facing text or JSON.
func FormatReport(idx *Index, asJSON bool) (string, error) {
	var sb strings.Builder
	if asJSON {
		if err := WriteJSON(&sb, idx); err != nil {
			return "", err
		}
		return sb.String(), nil
	}
	WriteReport(&sb, idx)
	return sb.String(), nil
}
