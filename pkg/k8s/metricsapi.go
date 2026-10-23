package k8s

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
)

// MetricsAPIGroupVersion is what metrics-server registers.
const MetricsAPIGroupVersion = "metrics.k8s.io/v1beta1"

// MetricsAPIAvailable reports whether the API server can serve
// metrics.k8s.io/v1beta1, which is true only when metrics-server runs and its
// APIService is Available. The error explains the failure otherwise.
func MetricsAPIAvailable(cs kubernetes.Interface) error {
	if _, err := cs.Discovery().ServerResourcesForGroupVersion(MetricsAPIGroupVersion); err != nil {
		return fmt.Errorf("%s not served: %w", MetricsAPIGroupVersion, err)
	}
	return nil
}

// WaitForMetricsAPI polls MetricsAPIAvailable until it succeeds or timeout.
func WaitForMetricsAPI(ctx context.Context, cs kubernetes.Interface, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := MetricsAPIAvailable(cs)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
