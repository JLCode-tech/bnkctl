// Package bnkscan indexes the BNK resources of a cluster and reports their
// readiness for the BNK 2.4 Gateway model.
//
// It reads the gateway.k8s.f5.com/v1alpha1 kinds (Infra, GatewaySettings,
// EgressGateway, SecPolicy, NetPolicy), the Gateway API objects BNK programs,
// the F5BigPersistenceProfile CRs that pin MCP sessions and the f5-cne-controller
// rollout, then evaluates the conditions a working 2.4 cluster shows:
//
//	Infra      Programmed=True
//	Gateway    Accepted=True and Programmed=True
//	controller every desired replica available
//
// The CRD groups served by the API server decide the Generation: a cluster
// serving gateway.k8s.f5net.com is 2.3, gateway.k8s.f5.com is 2.4, both at once
// is mixed. Legacy kinds are counted, never treated as an error, so a cluster
// mid-migration produces a complete index.
//
// DiscoverMCP walks the HTTPRoutes (annotation bnk.f5.com/protocol=mcp or an
// "/mcp" path) to find the MCP tool servers exposed through BNK Gateways, with
// the VIPs, hostnames, persistence profile and iRules that govern them. ProbeTools optionally speaks JSON-RPC to one of those
// endpoints to fetch its tool catalogue.
package bnkscan
