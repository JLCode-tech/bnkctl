package gcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	containerpb "cloud.google.com/go/container/apiv1/containerpb"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/JLCode-tech/bnkctl/internal/providers/gcp/labels"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

const (
	tmmNodeLabelKey   = "app"
	tmmNodeLabelValue = "f5-tmm"
	gkeNodepoolLabel  = "cloud.google.com/gke-nodepool"

	gkeOpPollInterval = 10 * time.Second
	gkeOpTimeout      = 30 * time.Minute
	clusterTimeout    = 30 * time.Minute

	// tmmNodeListPollInterval/tmmNodeListMaxAttempts back off the node-label
	// step after the tmm-pool node pool reports RUNNING: on real GKE, node
	// objects can lag a few seconds to tens of seconds behind the node pool's
	// own RUNNING status, so an immediate empty List is not necessarily an
	// error yet. 6 attempts * 10s = up to ~50s of polling before giving up,
	// matching the "few retries over ~30-60s" window.
	tmmNodeListPollInterval = 10 * time.Second
	tmmNodeListMaxAttempts  = 6
)

// Phase20Cluster creates the GKE cluster (DPv2 + multi-networking + ip-alias,
// default pool) then the tmm-pool (f5-dataplane=true, 2 additional node/pod
// networks) and agent-pool (1 additional node/pod network), then labels the
// tmm-pool's nodes app=f5-tmm. Idempotent: GetCluster/GetNodePool gate every
// create call, matching bash bin/20-cluster.sh.
func Phase20Cluster(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	fmt.Fprintf(os.Stderr, "[phase 20] cluster: cluster=%s\n", cl.Metadata.Name)

	ensureGCPDefaults(cl)

	if dryRun {
		printPhase20Plan(cl)
		return nil
	}

	if clients == nil || clients.GKE == nil {
		return nil
	}

	project := cl.Metadata.Project
	zone := cl.Metadata.Zone
	name := cl.Metadata.Name

	clusterObj, err := ensureCluster(ctx, clients.GKE, project, zone, name, cl)
	if err != nil {
		return fmt.Errorf("phase20: cluster: %w", err)
	}

	tmm := cl.ClusterSpec.TmmPool
	if err := ensureNodePool(ctx, clients.GKE, project, zone, name, buildTMMPoolConfig(cl)); err != nil {
		return fmt.Errorf("phase20: tmm-pool: %w", err)
	}
	agent := cl.ClusterSpec.AgentPool
	if err := ensureNodePool(ctx, clients.GKE, project, zone, name, buildAgentPoolConfig(cl)); err != nil {
		return fmt.Errorf("phase20: agent-pool: %w", err)
	}

	// AttachK8s is only called if not already attached — lets tests pre-seed
	// clients.K8s with a fake clientset, since AttachK8s's real path
	// (GetCluster -> BuildRESTConfig) requires live Application Default
	// Credentials.
	if clients.K8s == nil {
		if err := clients.AttachK8s(ctx, name, zone); err != nil {
			return fmt.Errorf("phase20: attaching k8s client: %w", err)
		}
	}

	if st != nil && st.KubeconfigPath() != "" && clusterObj.GetEndpoint() != "" {
		if kcBytes, err := FetchAndGenerateKubeconfig(ctx, clients, cl); err == nil {
			if err := WriteKubeconfigFile(st.KubeconfigPath(), kcBytes); err != nil {
				fmt.Fprintf(os.Stderr, "[phase 20] warn: writing kubeconfig to %s: %v\n", st.KubeconfigPath(), err)
			} else {
				st.Set("KUBECONFIG_PATH", st.KubeconfigPath())
				st.Set("KUBECONFIG", st.KubeconfigPath())
				fmt.Fprintf(os.Stderr, "[phase 20] wrote cluster kubeconfig to %s\n", st.KubeconfigPath())
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 20] labeling %s nodes app=f5-tmm\n", tmm.Name)
	if err := labelTMMNodes(ctx, clients.K8s, tmm.Name, tmmNodeListPollInterval); err != nil {
		return fmt.Errorf("phase20: labeling tmm nodes: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[phase 20] cluster OK: %s + %s + %s\n", name, tmm.Name, agent.Name)
	return nil
}

func printPhase20Plan(cl *intent.Cluster) {
	cs := cl.ClusterSpec
	fmt.Fprintf(os.Stderr,
		"[phase 20] would create GKE cluster %s (DPv2+multinet, default %s x%d) + %s (%s x%d, +2 node-nets +2 pod-nets) + %s (%s x%d, +1 node-net +1 pod-net)\n",
		cl.Metadata.Name, cs.DefaultPool.MachineType, cs.DefaultPool.NodeCount,
		cs.TmmPool.Name, cs.TmmPool.MachineType, cs.TmmPool.NodeCount,
		cs.AgentPool.Name, cs.AgentPool.MachineType, cs.AgentPool.NodeCount,
	)
}

// ensureCluster creates the GKE cluster if absent, or waits for RUNNING if
// it already exists in a transitional state. Returns the RUNNING cluster.
func ensureCluster(ctx context.Context, gke GKEAPI, project, zone, name string, cl *intent.Cluster) (*containerpb.Cluster, error) {
	clusterNameStr := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, zone, name)
	existing, err := gke.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterNameStr})
	if err == nil {
		switch existing.GetStatus() {
		case containerpb.Cluster_RUNNING:
			fmt.Fprintf(os.Stderr, "[phase 20] cluster %s already exists, status=RUNNING, skipping create\n", name)
			return existing, nil
		case containerpb.Cluster_ERROR:
			return nil, fmt.Errorf("cluster %s is in ERROR state", name)
		default:
			fmt.Fprintf(os.Stderr, "[phase 20] cluster %s already exists, status=%s, waiting for RUNNING\n", name, existing.GetStatus())
			return waitForClusterRunning(ctx, gke, project, zone, name)
		}
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("GetCluster: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[phase 20] creating GKE cluster %s (DPv2 + multi-networking + ip-alias)\n", name)
	op, err := gke.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  fmt.Sprintf("projects/%s/locations/%s", project, zone),
		Cluster: buildClusterConfig(cl),
	})
	if err != nil {
		return nil, fmt.Errorf("CreateCluster: %w", err)
	}
	if err := waitForGKEOp(ctx, gke, project, zone, op.GetName()); err != nil {
		return nil, fmt.Errorf("waiting for cluster create: %w", err)
	}
	return waitForClusterRunning(ctx, gke, project, zone, name)
}

// buildClusterConfig maps cluster.yaml + intent derived fields 1:1 onto the
// gcloud flags in 20-cluster.sh: --enable-ip-alias, --enable-dataplane-v2,
// --enable-multi-networking, --cluster-secondary-range-name,
// --services-secondary-range-name. ResourceLabels includes mandatory gkebnkctl
// labels (gkebnkctl-cluster, gkebnkctl-managed) merged with user-supplied
// cl.Metadata.Labels.
func buildClusterConfig(cl *intent.Cluster) *containerpb.Cluster {
	main := cl.Network.Main
	return &containerpb.Cluster{
		Name:                  cl.Metadata.Name,
		InitialClusterVersion: cl.ClusterSpec.KubernetesVersion,
		Network:               main.VPC,
		Subnetwork:            main.Subnet,
		ResourceLabels:        labels.Merge(labels.Required(cl.Metadata.Name), cl.Metadata.Labels),
		IpAllocationPolicy: &containerpb.IPAllocationPolicy{
			UseIpAliases:               true,
			ClusterSecondaryRangeName:  main.PodsRangeName,
			ServicesSecondaryRangeName: main.SvcsRangeName,
		},
		NetworkConfig: &containerpb.NetworkConfig{
			DatapathProvider:      containerpb.DatapathProvider_ADVANCED_DATAPATH,
			EnableMultiNetworking: true,
		},
		NodePools: []*containerpb.NodePool{
			{
				Name:             "default-pool",
				InitialNodeCount: int32(cl.ClusterSpec.DefaultPool.NodeCount), // #nosec G115 -- node counts are small operator-set values
				Config: &containerpb.NodeConfig{
					MachineType: cl.ClusterSpec.DefaultPool.MachineType,
				},
			},
		},
	}
}

// buildTMMPoolConfig maps cluster.yaml cluster.tmmPool onto
// --additional-node-network (external, plus internal when dual-interface) +
// --additional-pod-network (external, plus internal when dual-interface, max-pods-per-node)
// + --node-labels=f5-dataplane=true.
func buildTMMPoolConfig(cl *intent.Cluster) *containerpb.NodePool {
	pool := cl.ClusterSpec.TmmPool
	ext := cl.Network.External
	in := cl.Network.Internal
	maxPods := &containerpb.MaxPodsConstraint{MaxPodsPerNode: int64(pool.MaxPodsPerNode)}

	nodeNets := []*containerpb.AdditionalNodeNetworkConfig{
		{Network: ext.VPC, Subnetwork: ext.Subnet},
	}
	podNets := []*containerpb.AdditionalPodNetworkConfig{
		{Subnetwork: ext.Subnet, SecondaryPodRange: ext.PodsRangeName, MaxPodsPerNode: maxPods},
	}
	if cl.IsDualInterface() {
		nodeNets = append(nodeNets, &containerpb.AdditionalNodeNetworkConfig{Network: in.VPC, Subnetwork: in.Subnet})
		podNets = append(podNets, &containerpb.AdditionalPodNetworkConfig{Subnetwork: in.Subnet, SecondaryPodRange: in.PodsRangeName, MaxPodsPerNode: maxPods})
	}

	return &containerpb.NodePool{
		Name:             pool.Name,
		InitialNodeCount: int32(pool.NodeCount), // #nosec G115 -- node counts are small operator-set values
		Config: &containerpb.NodeConfig{
			MachineType: pool.MachineType,
			Labels:      map[string]string{"f5-dataplane": "true"},
		},
		NetworkConfig: &containerpb.NodeNetworkConfig{
			AdditionalNodeNetworkConfigs: nodeNets,
			AdditionalPodNetworkConfigs:  podNets,
		},
	}
}

// buildAgentPoolConfig maps cluster.yaml cluster.agentPool onto a single
// --additional-node-network + --additional-pod-network on the external VPC
// only.
func buildAgentPoolConfig(cl *intent.Cluster) *containerpb.NodePool {
	pool := cl.ClusterSpec.AgentPool
	ext := cl.Network.External
	maxPods := &containerpb.MaxPodsConstraint{MaxPodsPerNode: int64(pool.MaxPodsPerNode)}

	return &containerpb.NodePool{
		Name:             pool.Name,
		InitialNodeCount: int32(pool.NodeCount), // #nosec G115 -- node counts are small operator-set values
		Config: &containerpb.NodeConfig{
			MachineType: pool.MachineType,
			Labels:      map[string]string{"f5-dataplane": "true"},
		},
		NetworkConfig: &containerpb.NodeNetworkConfig{
			AdditionalNodeNetworkConfigs: []*containerpb.AdditionalNodeNetworkConfig{
				{Network: ext.VPC, Subnetwork: ext.Subnet},
			},
			AdditionalPodNetworkConfigs: []*containerpb.AdditionalPodNetworkConfig{
				{Subnetwork: ext.Subnet, SecondaryPodRange: ext.PodsRangeName, MaxPodsPerNode: maxPods},
			},
		},
	}
}

// ensureNodePool creates pool on cluster clusterName if it doesn't already
// exist (or waits for RUNNING if it's mid-provisioning).
func ensureNodePool(ctx context.Context, gke GKEAPI, project, zone, clusterName string, pool *containerpb.NodePool) error {
	nodePoolNameStr := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", project, zone, clusterName, pool.GetName())
	existing, err := gke.GetNodePool(ctx, &containerpb.GetNodePoolRequest{
		Name: nodePoolNameStr,
	})
	if err == nil {
		switch existing.GetStatus() {
		case containerpb.NodePool_RUNNING:
			fmt.Fprintf(os.Stderr, "[phase 20] node pool %s already exists, skipping create\n", pool.GetName())
			return nil
		case containerpb.NodePool_ERROR:
			return fmt.Errorf("node pool %s is in ERROR state", pool.GetName())
		default:
			fmt.Fprintf(os.Stderr, "[phase 20] node pool %s already exists, status=%s, waiting for RUNNING\n", pool.GetName(), existing.GetStatus())
			return waitForNodePoolRunning(ctx, gke, project, zone, clusterName, pool.GetName())
		}
	}
	if !isNotFound(err) {
		return fmt.Errorf("GetNodePool %s: %w", pool.GetName(), err)
	}

	fmt.Fprintf(os.Stderr, "[phase 20] creating node pool %s (%s x%d)\n", pool.GetName(), pool.GetConfig().GetMachineType(), pool.GetInitialNodeCount())
	op, err := gke.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, zone, clusterName), NodePool: pool,
	})
	if err != nil {
		return fmt.Errorf("CreateNodePool %s: %w", pool.GetName(), err)
	}
	if err := waitForGKEOp(ctx, gke, project, zone, op.GetName()); err != nil {
		return fmt.Errorf("waiting for node pool %s create: %w", pool.GetName(), err)
	}
	return waitForNodePoolRunning(ctx, gke, project, zone, clusterName, pool.GetName())
}

// waitForGKEOp polls GetOperation until the operation reaches DONE.
func waitForGKEOp(ctx context.Context, gke GKEAPI, project, zone, opName string) error {
	return PollUntil(ctx, gkeOpPollInterval, gkeOpTimeout, func(ctx context.Context) (bool, error) {
		opNameStr := fmt.Sprintf("projects/%s/locations/%s/operations/%s", project, zone, opName)
		op, err := gke.GetOperation(ctx, &containerpb.GetOperationRequest{Name: opNameStr})
		if err != nil {
			return false, fmt.Errorf("GetOperation %s: %w", opName, err)
		}
		switch op.GetStatus() {
		case containerpb.Operation_DONE:
			if op.GetError() != nil {
				return false, fmt.Errorf("operation %s finished with error: %v", opName, op.GetError())
			}
			return true, nil
		case containerpb.Operation_ABORTING:
			return false, fmt.Errorf("operation %s is aborting: %s", opName, op.GetError().GetMessage())
		}
		return false, nil
	})
}

// waitForClusterRunning polls GetCluster until the cluster reaches RUNNING.
func waitForClusterRunning(ctx context.Context, gke GKEAPI, project, zone, name string) (*containerpb.Cluster, error) {
	var cluster *containerpb.Cluster
	err := PollUntil(ctx, clusterPollInterval, clusterTimeout, func(ctx context.Context) (bool, error) {
		clusterNameStr := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, zone, name)
		c, err := gke.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterNameStr})
		if err != nil {
			return false, fmt.Errorf("GetCluster %s: %w", name, err)
		}
		switch c.GetStatus() {
		case containerpb.Cluster_RUNNING:
			cluster = c
			return true, nil
		case containerpb.Cluster_ERROR, containerpb.Cluster_STOPPING:
			return false, fmt.Errorf("cluster %s in unexpected status %s", name, c.GetStatus())
		}
		return false, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("timeout waiting for cluster %s RUNNING", name)
		}
		return nil, err
	}
	return cluster, nil
}

// waitForNodePoolRunning polls GetNodePool until the pool reaches RUNNING.
func waitForNodePoolRunning(ctx context.Context, gke GKEAPI, project, zone, clusterName, poolName string) error {
	err := PollUntil(ctx, clusterPollInterval, clusterTimeout, func(ctx context.Context) (bool, error) {
		nodePoolNameStr := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", project, zone, clusterName, poolName)
		p, err := gke.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: nodePoolNameStr})
		if err != nil {
			return false, fmt.Errorf("GetNodePool %s: %w", poolName, err)
		}
		switch p.GetStatus() {
		case containerpb.NodePool_RUNNING:
			return true, nil
		case containerpb.NodePool_ERROR:
			return false, fmt.Errorf("node pool %s in ERROR state", poolName)
		}
		return false, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timeout waiting for node pool %s RUNNING", poolName)
		}
		return err
	}
	return nil
}

// labelTMMNodes labels every node in poolName's GKE node pool app=f5-tmm.
func labelTMMNodes(ctx context.Context, k8sClient kubernetes.Interface, poolName string, pollInterval time.Duration) error {
	var nodes *corev1.NodeList
	attempt := 0
	err := PollUntil(ctx, pollInterval, time.Duration(tmmNodeListMaxAttempts)*pollInterval+time.Second, func(ctx context.Context) (bool, error) {
		attempt++
		var err error
		nodes, err = k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: gkeNodepoolLabel + "=" + poolName,
		})
		if err != nil {
			return false, fmt.Errorf("listing nodes for pool %s: %w", poolName, err)
		}
		if len(nodes.Items) > 0 {
			return true, nil
		}
		if attempt >= tmmNodeListMaxAttempts {
			return false, fmt.Errorf("no nodes found for pool %s after %d attempts — node pool may still be provisioning", poolName, tmmNodeListMaxAttempts)
		}
		fmt.Fprintf(os.Stderr, "[phase 20] no nodes registered yet for pool %s (attempt %d/%d) — node objects can lag pool RUNNING status, retrying in %s\n",
			poolName, attempt, tmmNodeListMaxAttempts, pollInterval)
		return false, nil
	})
	if err != nil {
		return err
	}

	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, tmmNodeLabelKey, tmmNodeLabelValue))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[tmmNodeLabelKey] == tmmNodeLabelValue {
			continue
		}
		if _, err := k8sClient.CoreV1().Nodes().Patch(ctx, n.Name, ktypes.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("labeling node %s: %w", n.Name, err)
		}
	}
	return nil
}
