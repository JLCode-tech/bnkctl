package azure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
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
		applier, err := k8s.NewApplier(cfg)
		if err != nil {
			return fmt.Errorf("phase24: creating applier: %w", err)
		}

		fmt.Fprintln(os.Stderr, "[phase 24] configuring CoreDNS endpoint_pod_names for BNK mTLS...")
		restartCoreDNS := false
		if customCM, gErr := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "coredns-custom", metav1.GetOptions{}); gErr == nil && customCM != nil {
			if _, hasOverride := customCM.Data["bnk-endpoint-pod-names.override"]; hasOverride {
				delete(customCM.Data, "bnk-endpoint-pod-names.override")
				if _, uErr := cs.CoreV1().ConfigMaps("kube-system").Update(ctx, customCM, metav1.UpdateOptions{}); uErr == nil {
					restartCoreDNS = true
				}
			}
		}
		if cm, gErr := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "coredns", metav1.GetOptions{}); gErr == nil && cm != nil {
			if corefile, ok := cm.Data["Corefile"]; ok && !strings.Contains(corefile, "endpoint_pod_names") {
				cm.Data["Corefile"] = strings.Replace(corefile, "pods insecure\n", "pods insecure\n            endpoint_pod_names\n", 1)
				if _, uErr := cs.CoreV1().ConfigMaps("kube-system").Update(ctx, cm, metav1.UpdateOptions{}); uErr == nil {
					restartCoreDNS = true
				}
			}
		}
		if restartCoreDNS {
			_ = cs.CoreV1().Pods("kube-system").DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
				LabelSelector: "k8s-app=kube-dns",
			})
		}

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

          while true; do
            nsenter -t 1 -m -u -i -n -p -- sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true
            nsenter -t 1 -m -u -i -n -p -- sysctl -w net.bridge.bridge-nf-call-iptables=0 >/dev/null 2>&1 || true
            for br in br-ext br-int; do
              if nsenter -t 1 -n -- ip link show "$br" >/dev/null 2>&1; then
                nsenter -t 1 -n -- ip link set "$br" up 2>/dev/null || true
                nsenter -t 1 -m -u -i -n -p -- ethtool -K "$br" tx off rx off tso off gso off gro off 2>/dev/null || true
                for p in $(nsenter -t 1 -m -u -i -n -p -- ls "/sys/class/net/${br}/brif" 2>/dev/null); do
                  nsenter -t 1 -m -u -i -n -p -- ethtool -K "$p" tx off rx off tso off gso off gro off 2>/dev/null || true
                done
                nsenter -t 1 -n -- iptables -C FORWARD -i "$br" -j ACCEPT 2>/dev/null || nsenter -t 1 -n -- iptables -I FORWARD 1 -i "$br" -j ACCEPT 2>/dev/null || true
                nsenter -t 1 -n -- iptables -C FORWARD -o "$br" -j ACCEPT 2>/dev/null || nsenter -t 1 -n -- iptables -I FORWARD 1 -o "$br" -j ACCEPT 2>/dev/null || true
              fi
            done
            nsenter -t 1 -n -- iptables -t nat -C POSTROUTING -o br-ext ! -s 10.0.17.0/24 -j MASQUERADE 2>/dev/null || nsenter -t 1 -n -- iptables -t nat -A POSTROUTING -o br-ext ! -s 10.0.17.0/24 -j MASQUERADE 2>/dev/null || true
            nsenter -t 1 -n -- iptables -t nat -C POSTROUTING -s 10.0.17.0/24 ! -d 10.0.17.0/24 -j MASQUERADE 2>/dev/null || nsenter -t 1 -n -- iptables -t nat -A POSTROUTING -s 10.0.17.0/24 ! -d 10.0.17.0/24 -j MASQUERADE 2>/dev/null || true
            nsenter -t 1 -n -- iptables -t nat -C POSTROUTING -s 10.0.18.0/24 ! -d 10.0.18.0/24 -j MASQUERADE 2>/dev/null || nsenter -t 1 -n -- iptables -t nat -A POSTROUTING -s 10.0.18.0/24 ! -d 10.0.18.0/24 -j MASQUERADE 2>/dev/null || true

            TMM_PID=$(nsenter -t 1 -m -u -i -n -p -- pgrep -f "/bin/tmm" 2>/dev/null | head -n 1 || true)
            if [ -n "$TMM_PID" ]; then
              for inf in net1 net2; do
                if nsenter -t 1 -m -u -i -p -- nsenter -t "$TMM_PID" -n -- ip link show "$inf" >/dev/null 2>&1; then
                  nsenter -t 1 -m -u -i -p -- nsenter -t "$TMM_PID" -n -- ip addr flush dev "$inf" 2>/dev/null || true
                  nsenter -t 1 -m -u -i -p -- nsenter -t "$TMM_PID" -n -- ip link set "$inf" up promisc on 2>/dev/null || true
                  nsenter -t 1 -m -u -i -p -- nsenter -t "$TMM_PID" -n -- ethtool -K "$inf" tx off rx off tso off gso off gro off 2>/dev/null || true
                fi
              done
            fi
            sleep 3
          done
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 50m
            memory: 64Mi
`
		dsObjs, err := k8s.ParseYAMLStream([]byte(hugepagesDS))
		if err != nil {
			return fmt.Errorf("phase24: parsing hugepages DaemonSet: %w", err)
		}
		if err := applier.ApplyAll(ctx, dsObjs, true); err != nil {
			return fmt.Errorf("phase24: applying hugepages DaemonSet: %w", err)
		}

		// Wait up to 90s for TMM node to report allocatable hugepages-2Mi
		hpDeadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(hpDeadline) {
			tmmNodes, lErr := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "app=f5-tmm"})
			if lErr == nil && len(tmmNodes.Items) > 0 {
				q, ok := tmmNodes.Items[0].Status.Allocatable[corev1.ResourceName("hugepages-2Mi")]
				if ok && !q.IsZero() {
					fmt.Fprintf(os.Stderr, "[phase 24] verified allocatable hugepages-2Mi=%s on %s\n", q.String(), tmmNodes.Items[0].Name)
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}

	fmt.Fprintln(os.Stderr, "[phase 24] node labels and HugePages OK")
	return nil
}
