// Package migrate turns a running BNK 2.3.x configuration into its BNK 2.4
// shape and drives the in-place FLO + CNEInstance upgrade.
//
// Two commands use it:
//
//   - awsbnkctl bnk migrate-2.4: Inspect lists the legacy CRs (F5SPKVlan,
//     F5SPKStaticRoute, Vrf, Vxlan, F5SPKEgress, F5SPKSnatpool, F5BnkGateway,
//     BNKSecPolicy, BNKNetPolicy and the Gateways they serve); Translate turns
//     them into one Infra CR, per-namespace GatewaySettings, Gateway
//     parametersRef patches, EgressGateways and gateway.k8s.f5.com policies;
//     Apply server-side-applies the result.
//   - awsbnkctl bnk upgrade: Upgrade moves the f5-lifecycle-operator Helm
//     release to the 2.4 chart, patches the CNEInstance (manifestVersion,
//     USE_GATEWAY_SETTINGS) and waits for the controller and TMM to come back.
//
// Everything here works on unstructured objects so it needs no F5 Go types
// and can be exercised with the fake dynamic client.
package migrate
