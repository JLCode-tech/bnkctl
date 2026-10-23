# Templates (`internal/k8s/manifests/templates/`)

This directory contains Go `text/template` files (`{{ .Field }}`).

These templates are rendered via `internal/k8s/render` against typed `Vars` structs, and then applied via dynamic SSA.

---

## Available Templates

### Fixed Document Sets
The following templates render a fixed set of documents:

- `nads-hostdevice.yaml.tmpl` (Task C, Phase35)
- `cneinstance.yaml.tmpl` (Task C, Phase38)
- `license-cr.yaml.tmpl` (Task C, Phase40)
- `vlans-routes.yaml.tmpl` (Task C, Phase41)
- `egress.yaml.tmpl` (Task D, Phase50)
- `agent-route-setter.yaml.tmpl` (Task D, Phase50)

### Variable-Length Lists
- `ingress-vxlan-routes.yaml.tmpl` (Phase51)
  It renders a variable-length list of `F5SPKStaticRoute` objects based on Phase51-discovered per-agent-node data.
  > [!NOTE]
  > See the doc comment in `internal/gcp/phases/phase51_ingress_vxlan.go` for details on the asymmetric-routing fix this template addresses.

---

## Notes

The discovery pod is implemented as a typed `corev1.Pod` Go struct (`buildDiscoveryPod` in `internal/gcp/phases/phase25_discover.go`). Because there is no F5/CRD-specific unstructured content in that pod spec, a Go template added no value over the native typed builder.
