package manifests

import "testing"

// TestFS_ManifestsEmbedded is a sanity check that the go:embed directive
// compiles and picks up files from every declared directory. static/,
// multus/, cert-manager/, and flo/ carry real content; templates/ holds
// text/template sources for dynamic rendering (Task C/D/later phases).
func TestFS_ManifestsEmbedded(t *testing.T) {
	for _, path := range []string{
		"static/00-gateway-api-crds.yaml",
		"static/01-namespaces.yaml",
		"static/02-bnk-gateway-crds.yaml",
		"static/04-issuers.yaml",
		"static/07-node-sysctls.json",
		"multus/02-multus-rbac.yaml",
		"multus/02-multus-crd.yaml",
		"multus/02-multus-configmap.yaml",
		"multus/02-multus-ds.yaml",
		"cert-manager/cert-manager-v1.16.1.yaml",
		"gie/gie-crds.yaml",
		"gie/gateway-api-inference-extension-v1.5.0.yaml",
		"templates/namespaces.yaml.tmpl",
		"templates/nads-hostdevice.yaml.tmpl",
		"templates/cneinstance.yaml.tmpl",
		"templates/cne-controller-rbac.yaml.tmpl",
		"templates/vlans-routes.yaml.tmpl",
		"templates/license-cr.yaml.tmpl",
		"templates/egress.yaml.tmpl",
		"templates/agent-route-setter.yaml.tmpl",
		"templates/ingress-vxlan-routes.yaml.tmpl",
		"templates/README.md",
	} {
		if _, err := FS.ReadFile(path); err != nil {
			t.Errorf("FS.ReadFile(%q): %v", path, err)
		}
	}
}
