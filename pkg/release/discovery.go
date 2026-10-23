package release

import (
	"strings"

	"k8s.io/client-go/discovery"
)

// DiscoverServedAPIVersions inspects the live Kubernetes API server's preferred
// resources to dynamically resolve the served apiVersion for F5 Gateway CRDs
// (gateway.k8s.f5.com) and Gateway API Inference Extension CRDs
// (inference.networking.k8s.io / inference.networking.x-k8s.io).
//
// If discovery fails or the CRDs are not yet installed on the cluster, the
// profile's catalog defaults remain intact.
func DiscoverServedAPIVersions(disc discovery.DiscoveryInterface, profile *ReleaseProfile) {
	if disc == nil || profile == nil {
		return
	}
	lists, err := disc.ServerPreferredResources()
	if len(lists) == 0 {
		_, lists, err = disc.ServerGroupsAndResources()
	}
	if err != nil && len(lists) == 0 {
		return
	}
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv := list.GroupVersion
		switch {
		case strings.HasPrefix(gv, "gateway.k8s.f5.com/"):
			profile.ServedGatewayAPIVersion = gv
		case strings.HasPrefix(gv, "inference.k8s.f5.com/"):
			profile.ServedF5InferenceAPIVersion = gv
		case strings.HasPrefix(gv, "inference.networking.k8s.io/"):
			profile.ServedInferenceAPIVersion = gv
		case strings.HasPrefix(gv, "inference.networking.x-k8s.io/") &&
			!strings.HasPrefix(profile.ServedInferenceAPIVersion, "inference.networking.k8s.io/"):
			profile.ServedInferenceAPIVersion = gv
		}
	}
}
