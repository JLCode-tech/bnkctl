package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// FormatPlan formats the migration plan as a YAML or JSON string.
func FormatPlan(inv *Inventory, plan *Plan, asJSON bool) (string, error) {
	var buf bytes.Buffer
	if asJSON {
		if err := WriteJSON(&buf, inv, plan); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
	if err := WriteYAML(&buf, plan); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// WriteYAML writes the plan as a multi-document YAML stream in apply order,
// each document preceded by a comment naming its origin. This is what
// `bnk migrate-2.4 --dry-run` prints; `bnkctl k apply -f -` accepts it.
func WriteYAML(w io.Writer, plan *Plan) error {
	first := true
	for _, obj := range plan.Objects() {
		if !first {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := fmt.Fprintf(w, "# %s\n", describe(obj)); err != nil {
			return err
		}
		b, err := yaml.Marshal(obj.Object)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", describe(obj), err)
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON writes the plan as a JSON document: the objects in apply order
// plus the warnings.
func WriteJSON(w io.Writer, inv *Inventory, plan *Plan) error {
	objs := plan.Objects()
	items := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		items = append(items, o.Object)
	}
	doc := map[string]any{
		"schema":          "awsbnkctl.migrate-2.4/v1",
		"namespace":       inv.Namespace,
		"manifestVersion": inv.ManifestVersion(),
		"inventory":       inv.Summary(),
		"objects":         items,
		"warnings":        plan.Warnings,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// WriteReport writes the human-readable summary: what was found, what will be
// created, and the warnings. Goes to stderr so the YAML on stdout stays clean.
func WriteReport(w io.Writer, inv *Inventory, plan *Plan) {
	fmt.Fprintf(w, "[migrate-2.4] inventory of %s (CNEInstance manifestVersion %q):\n", inv.Namespace, inv.ManifestVersion())
	for _, line := range inv.Summary() {
		fmt.Fprintf(w, "[migrate-2.4]   %s\n", line)
	}
	fmt.Fprintf(w, "[migrate-2.4] plan: %d object(s)\n", len(plan.Objects()))
	for _, obj := range plan.Objects() {
		fmt.Fprintf(w, "[migrate-2.4]   %s\n", describe(obj))
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "[migrate-2.4] warning: %s\n", warn)
	}
}

// describe returns "Kind namespace/name" (or "Kind name" for cluster-scoped).
func describe(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return fmt.Sprintf("%s %s/%s", obj.GetKind(), ns, obj.GetName())
	}
	return fmt.Sprintf("%s %s", obj.GetKind(), obj.GetName())
}
