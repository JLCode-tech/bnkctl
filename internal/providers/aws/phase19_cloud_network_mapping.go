package aws

import (
	"context"
	"fmt"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const cloudNetworkMappingName = "cloud-network-mapping"

// Phase19CloudNetworkMapping renders and applies the cloud-network-mapping ConfigMap in f5-cne-system.
func Phase19CloudNetworkMapping(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 19 cloud-network-mapping: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 19 cloud-network-mapping: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || (c.Dynamic == nil && c.K8s == nil) {
		return fmt.Errorf("phase 19 cloud-network-mapping: kubernetes client is nil")
	}

	if c.K8s != nil {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: InstanceNamespace},
		}
		if _, err := c.K8s.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("phase 19 cloud-network-mapping: ensuring namespace %s: %w", InstanceNamespace, err)
		}
	}

	tmplBytes, err := manifests.FS.ReadFile("shared/cloud-network-mapping.yaml.tmpl")
	if err != nil {
		return fmt.Errorf("phase 19 cloud-network-mapping: reading template: %w", err)
	}
	rendered, err := render.RenderCloudNetworkMapping(tmplBytes, cl, st.Get)
	if err != nil {
		return fmt.Errorf("phase 19 cloud-network-mapping: %w", err)
	}
	if c.Dynamic != nil && c.RESTMapper != nil {
		if err := applyRawYAML(ctx, c, rendered, InstanceNamespace); err != nil {
			return fmt.Errorf("phase 19 cloud-network-mapping: applying ConfigMap: %w", err)
		}
	} else if c.K8s != nil {
		var cm corev1.ConfigMap
		if err := k8syaml.Unmarshal(rendered, &cm); err != nil {
			return fmt.Errorf("phase 19 cloud-network-mapping: decoding ConfigMap: %w", err)
		}
		ns := cm.Namespace
		if ns == "" {
			ns = InstanceNamespace
		}
		if _, err := c.K8s.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				if _, uerr := c.K8s.CoreV1().ConfigMaps(ns).Update(ctx, &cm, metav1.UpdateOptions{}); uerr != nil {
					return fmt.Errorf("phase 19 cloud-network-mapping: updating ConfigMap: %w", uerr)
				}
			} else {
				return fmt.Errorf("phase 19 cloud-network-mapping: creating ConfigMap: %w", err)
			}
		}
	}

	st.Set("CLOUD_NETWORK_MAPPING_APPLIED", "true")
	return st.Save()
}

// Phase19CloudNetworkMappingDown deletes the cloud-network-mapping ConfigMap from f5-cne-system.
func Phase19CloudNetworkMappingDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c != nil && c.K8s != nil {
		_ = c.K8s.CoreV1().ConfigMaps(InstanceNamespace).Delete(ctx, cloudNetworkMappingName, metav1.DeleteOptions{})
	}
	st.Delete("CLOUD_NETWORK_MAPPING_APPLIED")
	st.Delete("CLOUD_NETWORK_MAPPING_CM")
	st.Delete("CLOUD_NETWORK_MAPPING_APPLIED_AT")
	return st.Save()
}
