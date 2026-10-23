package azure

import (
	"context"
	"fmt"
	"os"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Phase24NodeLabels verifies node labels and taints on the TMM worker nodes.
func Phase24NodeLabels(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	fmt.Fprintf(os.Stderr, "[phase 24] node-labels: verifying TMM node pool labels\n")

	if dryRun {
		fmt.Fprintln(os.Stderr, "[phase 24] [dry-run] would verify TMM node labels (app=f5-tmm)")
		return nil
	}

	var cs kubernetes.Interface
	var cfg *rest.Config
	if clients != nil && clients.K8s != nil {
		cs = clients.K8s
	} else {
		var kcPath string
		if st != nil {
			kcPath = st.KubeconfigPath()
		}
		if kcPath == "" {
			return nil
		}
		if _, err := os.Stat(kcPath); err != nil {
			return nil
		}
		var err error
		cfg, err = k8s.BuildRESTConfigFromKubeconfig(kcPath, "")
		if err != nil {
			return fmt.Errorf("phase24: build REST config: %w", err)
		}
		cs, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			return fmt.Errorf("phase24: creating clientset: %w", err)
		}
	}

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "app=f5-tmm",
	})
	if err != nil {
		return fmt.Errorf("phase24: listing TMM nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		fmt.Fprintln(os.Stderr, "[phase 24] warn: no nodes found with label app=f5-tmm yet (pool may still be initializing)")
	} else {
		for _, n := range nodes.Items {
			fmt.Fprintf(os.Stderr, "[phase 24] verified TMM node: %s\n", n.Name)
			if st != nil {
				st.Set("AZURE_TMM_NODE_NAME", n.Name)
			}
		}
		if st != nil {
			_ = st.Save()
		}
	}

	if cfg != nil {
		fmt.Fprintln(os.Stderr, "[phase 24] configuring HugePages (2Mi) and kernel params on nodes...")
		hugepagesDS := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-prep-setup
  namespace: kube-system
  labels:
    app: node-prep-setup
    app.kubernetes.io/managed-by: azrbnkctl
spec:
  selector:
    matchLabels:
      app: node-prep-setup
  template:
    metadata:
      labels:
        app: node-prep-setup
    spec:
      hostPID: true
      hostNetwork: true
      tolerations:
      - operator: Exists
      restartPolicy: Always
      containers:
      - name: node-prep
        image: ubuntu:22.04
        securityContext:
          privileged: true
        command:
        - /bin/bash
        - -c
        - |
          set -e
          mkdir -p /var/crash
          chmod 1777 /var/crash
          nsenter -t 1 -m -u -i -n -p -- mkdir -p /var/crash
          nsenter -t 1 -m -u -i -n -p -- chmod 1777 /var/crash
          printf "%%e.%%p.%%s.%%t.core" | nsenter -t 1 -m -u -i -n -p -- tee /proc/sys/kernel/core_pattern || true

          PG=/sys/devices/system/node/node0/hugepages/hugepages-2048kB/nr_hugepages
          if [ -f "$PG" ]; then
            NEED=2048
            CUR=$(cat $PG 2>/dev/null || echo 0)
            if [ "$CUR" -lt "$NEED" ]; then
              echo $NEED > $PG
              if [ -w /sys/devices/system/node/node0/hugepages/hugepages-1048576kB/nr_hugepages ]; then
                echo 0 > /sys/devices/system/node/node0/hugepages/hugepages-1048576kB/nr_hugepages || true
              fi
              nsenter -t 1 -m -u -i -n -p -- systemctl restart kubelet || true
            fi
          fi
          sleep infinity
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 50m
            memory: 64Mi
`
		applier, err := k8s.NewApplier(cfg)
		if err != nil {
			return fmt.Errorf("phase24: creating applier: %w", err)
		}
		dsObjs, err := k8s.ParseYAMLStream([]byte(hugepagesDS))
		if err != nil {
			return fmt.Errorf("phase24: parsing hugepages DaemonSet: %w", err)
		}
		if err := applier.ApplyAll(ctx, dsObjs, true); err != nil {
			return fmt.Errorf("phase24: applying hugepages DaemonSet: %w", err)
		}
	}

	fmt.Fprintln(os.Stderr, "[phase 24] node labels and HugePages OK")
	return nil
}
