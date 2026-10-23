// Package labels defines the gkebnkctl label scheme.
//
// Unlike AWS, GCP does not provide universal resource-tag/filter discovery —
// VPC networks, subnetworks, and firewall rules are not consistently
// labelable in a way that can be filtered on the way AWS tags are (see
// docs/SPEC-go-tracer.md §4). Idempotency here is deterministic naming
// (Get(name) → skip-if-exists), NOT label-discovery.
//
// Labels are applied only where GCP supports them (GKE cluster
// resourceLabels, node pools, compute instances) — for cost attribution /
// inventory purposes only, never as a rebuild key.
package labels

import (
	"fmt"
	"strings"
)

// Key constants for the mandatory gkebnkctl labels. GCP label keys/values
// must be lowercase alphanumerics, dashes, and underscores (RFC1035-ish) —
// callers are responsible for ensuring cluster/component names conform.
const (
	KeyCluster = "gkebnkctl-cluster"
	KeyManaged = "gkebnkctl-managed"
)

// OwnerMarker returns the description string stamped onto GCP resources that
// cannot carry labels. Parseable, stable, and safe to substring-match.
func OwnerMarker(cluster string) string {
	return fmt.Sprintf("%s=true %s=%s", KeyManaged, KeyCluster, cluster)
}

// ParseOwner extracts the cluster name and managed status from a description string.
func ParseOwner(description string) (cluster string, managed bool) {
	parts := strings.Fields(description)
	for _, p := range parts {
		if p == KeyManaged+"=true" {
			managed = true
		} else if strings.HasPrefix(p, KeyCluster+"=") {
			cluster = strings.TrimPrefix(p, KeyCluster+"=")
		}
	}
	return
}

// Required returns the mandatory gkebnkctl labels for a resource.
// clusterName is the value of metadata.name from cluster.yaml.
func Required(clusterName string) map[string]string {
	return map[string]string{
		KeyCluster: clusterName,
		KeyManaged: "true",
	}
}

// Merge combines one or more label maps into a single deduplicated map.
// Later maps override earlier maps on key conflicts. The mandatory
// gkebnkctl:* keys should be the first map so user-supplied labels cannot
// silently remove them.
//
// Usage:
//
//	labels.Merge(labels.Required(cl.Metadata.Name), cl.Metadata.Labels, cl.Tags)
func Merge(maps ...map[string]string) map[string]string {
	merged := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged
}
