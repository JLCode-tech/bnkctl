// Package tags provides Azure resource tagging conventions for azrbnkctl.
package tags

const (
	TagCluster   = "azrbnkctl:cluster"
	TagComponent = "azrbnkctl:component"
	TagManaged   = "azrbnkctl:managed"
)

// ForCluster returns the standard Azure resource tags for a cluster component.
func ForCluster(clusterName, component string, extra map[string]string) map[string]*string {
	result := make(map[string]*string)

	cName := clusterName
	cComp := component
	cMgd := "true"

	result[TagCluster] = &cName
	result[TagComponent] = &cComp
	result[TagManaged] = &cMgd

	for k, v := range extra {
		val := v
		result[k] = &val
	}

	return result
}

// IsManagedByCluster checks if the Azure resource tag map contains the expected cluster tag.
func IsManagedByCluster(resourceTags map[string]*string, clusterName string) bool {
	if resourceTags == nil {
		return false
	}
	val, ok := resourceTags[TagCluster]
	if !ok || val == nil {
		return false
	}
	return *val == clusterName
}
