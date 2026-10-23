# Scenario: http-routing-e2e

**Rating**: Green  
**F5 how-to**: HTTP traffic steering with Gateway API HTTPRoute (how-to #8)  
**Namespace**: `awsbnkctl-scn-httproute-e2e` (default)

## What it tests

End-to-end HTTP traffic through the BNK data plane via the jumphost.

```
Operator laptop
     │
     ▼
awsbnkctl scenarios run http-routing-e2e -f cluster.yaml
     │
     ├─[1/3] Render 5 manifests to .awsbnkctl/<cluster>/artifacts/scenarios/http-routing-e2e/
     │         01-namespace.yaml         — Namespace
     │         02-gatewaysettings.yaml    — GatewaySettings (listener context, scenario-owned)
     │         03-nginx.yaml             — nginx Deployment + ConfigMap + Service
     │         04-gateway.yaml           — Gateway spec.addresses=[VIP]
     │         05-httproute.yaml         — HTTPRoute host=awsbnkctl.local → nginx
     │
     ├─[2/3] Apply via SSA (internal/k8s.ApplyOptions — live RESTMapper)
     │
     └─[3/3] Verify:
              1. nginx Available
              2. Gateway Programmed=True
              3. HTTPRoute Accepted=True + ResolvedRefs=True
              4. pkg/bnk.ResyncHTTPRoutes (pool-member refresh)
              5. SSH+EICE → jumphost → curl --interface <BNK_EXT_ENI_IP> http://<VIP>/ × N
              6. Assert N/N HTTP 200
```

## Prerequisites

1. `awsbnkctl up` completed with `testing.jumphost.enabled: true` in cluster.yaml.
2. State keys present in `.awsbnkctl/<cluster>/state.env`:
   - `JUMPHOST_INSTANCE_ID`
   - `JUMPHOST_BNK_EXT_ENI_IP`
3. `aws` CLI and `ssh` on PATH — the verification step opens an EC2 Instance
   Connect tunnel and curls the VIP from the jumphost. See `internal/jumphost`.

## VIP derivation

Default VIP = `<network.dataPath.external.cidr network>.100`  
e.g. `10.0.10.0/24` → `10.0.10.100`.  
Override with `--vip` on the CLI.

## Cleanup

```sh
awsbnkctl scenarios clean http-routing-e2e --config <cluster.yaml>
```

Deletes the `awsbnkctl-scn-httproute-e2e` namespace. Idempotent.

## Reproduce manually

```sh
# After running the scenario once:
ssh -o ProxyCommand="aws ec2-instance-connect open-tunnel --instance-id <ID> --region <r>" \
    ec2-user@<ID> \
    "curl -sS --interface <BNK_EXT_ENI_IP> http://<VIP>/"
# Expected: awsbnkctl-scenario-httproute-e2e-OK
```
