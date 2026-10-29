package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"github.com/JLCode-tech/bnkctl/pkg/bnkengine"
	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/genai"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aiinferencee2e"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aisemanticcache"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aitokencounting"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/clusterwidewatch"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/corefiles"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/cwcadminaccess"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/egresssnat"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/externalresourcepool"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/grpcroute"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/httproutee2e"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/httptrafficsplit"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/multivip"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/proxyprotocoll4"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/tcpl4lb"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/udpl4lb"
	"github.com/JLCode-tech/bnkctl/pkg/selfupdate"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	pkgtest "github.com/JLCode-tech/bnkctl/pkg/test"
	"github.com/JLCode-tech/bnkctl/pkg/topology"
	"github.com/spf13/cobra"
)

func newBNKCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bnk",
		Short: "BNK overlay management subcommands (heal, resync, scan, migrate, upgrade, status)",
	}

	var resyncNS, resyncName, resyncGatewayClass string
	var resyncAll, resyncWatch, resyncJSON bool
	var resyncDebounce time.Duration
	resyncCmd := &cobra.Command{
		Use:   "resync",
		Short: "Resync BNK HTTPRoute pool members, persistence profiles, and release quirks",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			u, err := p.DiscoverUnderlay(cmd.Context(), cl, st)
			if err != nil {
				return err
			}
			eng, err := buildEngine(cmd.Context(), cl, st, u, flags)
			if err != nil {
				return err
			}
			if flags.DryRun || eng.Dyn == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] bnk resync reconciled for %s/%s (BNK %s)\n",
					p.Name(), u.ClusterName, eng.Profile.Version)
				return nil
			}

			rOpts := bnk.ResyncOptions{
				Namespace:      resyncNS,
				Name:           resyncName,
				AllInNamespace: resyncAll,
				GatewayClass:   resyncGatewayClass,
				DryRun:         flags.DryRun,
			}
			if rOpts.Name == "" && !rOpts.AllInNamespace && rOpts.GatewayClass == "" {
				if len(args) > 0 {
					rOpts.Name = args[0]
					if rOpts.Namespace == "" {
						rOpts.Namespace = "default"
					}
				} else if rOpts.Namespace != "" {
					rOpts.AllInNamespace = true
				} else {
					rOpts.GatewayClass = "f5-gateway-class"
				}
			}

			if resyncWatch {
				return bnk.WatchHTTPRoutes(cmd.Context(), eng.Dyn, bnk.WatchOptions{
					ResyncOptions: rOpts,
					Debounce:      resyncDebounce,
				})
			}

			var res bnk.ResyncResult
			if rOpts.Name != "" {
				res, err = bnk.ResyncHTTPRoutes(cmd.Context(), eng.Dyn, rOpts)
			} else {
				res, err = bnk.ResyncAll(cmd.Context(), eng.Dyn, rOpts)
			}
			if err != nil {
				return err
			}
			if resyncJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			for _, r := range res.Resynced {
				fmt.Fprintf(cmd.OutOrStdout(), "[resync] %s/%s (%d rules, applied=%v)\n", r.Namespace, r.Name, r.Rules, r.Applied)
			}
			if eng.K8s != nil {
				if _, herr := bnkengine.RunHeal(cmd.Context(), eng.K8s, eng.Dyn, bnkengine.HealOptions{
					Cluster: cl,
					DryRun:  flags.DryRun,
				}); herr != nil {
					return herr
				}
			}
			return nil
		},
	}
	resyncCmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	resyncCmd.Flags().StringVarP(&resyncNS, "namespace", "n", "", "Namespace for HTTPRoute resync")
	resyncCmd.Flags().StringVar(&resyncName, "name", "", "Specific HTTPRoute name to resync")
	resyncCmd.Flags().BoolVar(&resyncAll, "all", false, "Resync all HTTPRoutes in the target namespace")
	resyncCmd.Flags().StringVar(&resyncGatewayClass, "gateway-class", "", "Resync all HTTPRoutes attached to this GatewayClass")
	resyncCmd.Flags().BoolVar(&resyncWatch, "watch", false, "Watch EndpointSlices and trigger HTTPRoute resync on backend changes")
	resyncCmd.Flags().DurationVar(&resyncDebounce, "debounce", 2*time.Second, "Debounce window when --watch is enabled")
	resyncCmd.Flags().BoolVar(&resyncJSON, "json", false, "Emit resync result as JSON")

	var scanJSON, acceptLegacy bool
	scanCmd := &cobra.Command{
		Use:   "scan [cluster.yaml]",
		Short: "Scan BNK overlay CRDs, MCP endpoints, and release quirks without mutating cluster resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			u, err := p.DiscoverUnderlay(cmd.Context(), cl, st)
			if err != nil {
				return err
			}
			eng, err := buildEngine(cmd.Context(), cl, st, u, flags)
			if err != nil {
				return err
			}
			if flags.DryRun || eng.Dyn == nil {
				for _, chk := range bnkengine.AllHealChecks {
					fmt.Fprintf(cmd.OutOrStdout(), "[scan] %-28s healthy (dry-run)\n", chk)
				}
				return nil
			}
			idx, err := bnkscan.Scan(cmd.Context(), eng.Dyn, eng.K8s, bnkscan.Options{
				AcceptLegacy: acceptLegacy,
			})
			if err != nil {
				return err
			}
			rep, err := bnkscan.FormatReport(idx, scanJSON)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), rep)
			if scanJSON {
				return nil
			}
			findings, err := bnkengine.DetectAll(cmd.Context(), eng.K8s, eng.Dyn, bnkengine.HealOptions{
				Cluster: cl,
				DryRun:  true,
			})
			if err != nil {
				return err
			}
			for _, f := range findings {
				status := "healthy"
				if !f.Healthy {
					status = "unhealthy"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[scan] %-28s %-10s %s\n", f.Check, status, f.Description)
			}
			return nil
		},
	}
	scanCmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	scanCmd.Flags().BoolVar(&scanJSON, "json", false, "Output scan report as JSON")
	scanCmd.Flags().BoolVar(&acceptLegacy, "accept-legacy", false, "Treat BNK 2.3 clusters without Infra CR as ready")

	cmd.AddCommand(
		newHealCmd(flags),
		resyncCmd,
		scanCmd,
		newMigrateCmd(flags),
		newUpgradeCmd(flags),
		newStatusCmd(flags),
	)
	return cmd
}

func newScenariosCmd(flags *GlobalFlags) *cobra.Command {
	var runAll bool
	runScenarios := func(cmd *cobra.Command, args []string, all bool) error {
		cl, st, _, err := resolveRuntime(flags)
		if err != nil {
			return err
		}
		var targetList []scenarios.Scenario
		if all {
			targetList = scenarios.All()
		} else {
			if len(args) == 0 {
				return fmt.Errorf("specify a scenario name or --all")
			}
			for _, name := range args {
				sc := scenarios.Find(name)
				if sc == nil {
					return fmt.Errorf("unknown scenario %q", name)
				}
				targetList = append(targetList, sc)
			}
		}
		sctx := &scenarios.Context{
			Ctx:          cmd.Context(),
			Cluster:      cl,
			State:        st,
			WorkspaceDir: flags.StateDir,
			Out:          cmd.OutOrStdout(),
			DryRun:       flags.DryRun,
		}
		for _, sc := range targetList {
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] scenario %-24s (%s)\n", sc.Name(), sc.Title())
				continue
			}
			if _, err := sc.Manifests(sctx); err != nil {
				return fmt.Errorf("scenario %s manifests: %w", sc.Name(), err)
			}
			if err := sc.Apply(sctx); err != nil {
				return fmt.Errorf("scenario %s apply: %w", sc.Name(), err)
			}
			res := sc.Verify(sctx)
			if !res.AllPassed() {
				return fmt.Errorf("scenario %s failed: %s", sc.Name(), res.Summary)
			}
		}
		return nil
	}

	cmd := &cobra.Command{
		Use:   "scenarios",
		Short: "List, inspect, and execute BNK validation scenarios",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runAll || len(args) > 0 {
				return runScenarios(cmd, args, runAll)
			}
			return cmd.Help()
		},
	}
	cmd.Flags().BoolVar(&runAll, "all", false, "Execute all registered scenarios")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered BNK validation scenarios",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, sc := range scenarios.All() {
				fmt.Fprintf(cmd.OutOrStdout(), "%-24s %s\n", sc.Name(), sc.Title())
			}
			return nil
		},
	}
	showCmd := &cobra.Command{
		Use:   "show <scenario-id>",
		Short: "Show metadata and topology for a scenario",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc := scenarios.Find(args[0])
			if sc == nil {
				return fmt.Errorf("unknown scenario %q", args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ID:       %s\nName:     %s\nSummary:  %s\n", sc.Name(), sc.Title(), sc.Description())
			return nil
		},
	}
	var subRunAll bool
	runCmd := &cobra.Command{
		Use:   "run [scenario-id...]",
		Short: "Execute one or more registered BNK validation scenarios",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenarios(cmd, args, subRunAll)
		},
	}
	runCmd.Flags().BoolVar(&subRunAll, "all", false, "Execute all registered scenarios")

	cmd.AddCommand(listCmd, showCmd, runCmd)
	return cmd
}

func newTestCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Run BNK traffic probes (DNS, TCP/UDP connectivity) and scenario test suites",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Test runner ready for %s cluster %q (%d scenarios registered)\n",
				p.Name(), cl.EffectiveName(), len(scenarios.All()))
			_ = st
			return nil
		},
	}

	var dnsTarget, dnsType, dnsServer, dnsEDNS string
	var dnsIterations int
	var dnsTimeout time.Duration
	var dnsJSON bool
	dnsCmd := &cobra.Command{
		Use:   "dns [domain]",
		Short: "Execute a DNS probe against a target resolver or BNK DNS VIP",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := dnsTarget
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				return fmt.Errorf("target domain is required (pass positional arg or --target)")
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] test dns target=%s type=%s server=%s iterations=%d\n",
					target, dnsType, dnsServer, dnsIterations)
				return nil
			}
			res, err := pkgtest.RunDNS(cmd.Context(), pkgtest.DNSProbeConfig{
				Target:     target,
				Type:       dnsType,
				Server:     dnsServer,
				Iterations: dnsIterations,
				Timeout:    dnsTimeout,
				EDNSSubnet: dnsEDNS,
			})
			if err != nil {
				return err
			}
			if dnsJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "DNS %s (@%s) -> rcode=%s answers=%d p50=%.2fms p95=%.2fms\n",
				res.Target, res.Server, res.Rcode, len(res.Answers), res.LatencyMs.P50, res.LatencyMs.P95)
			return nil
		},
	}
	dnsCmd.Flags().StringVar(&dnsTarget, "target", "", "Target domain name to resolve")
	dnsCmd.Flags().StringVar(&dnsType, "type", "A", "DNS record type (A, AAAA, TXT, CNAME, MX, NS)")
	dnsCmd.Flags().StringVar(&dnsServer, "server", "8.8.8.8:53", "DNS server host:port")
	dnsCmd.Flags().IntVar(&dnsIterations, "iterations", 1, "Number of probe iterations")
	dnsCmd.Flags().DurationVar(&dnsTimeout, "timeout", 2*time.Second, "Per-query timeout")
	dnsCmd.Flags().StringVar(&dnsEDNS, "edns-subnet", "", "Optional EDNS0 client subnet CIDR")
	dnsCmd.Flags().BoolVar(&dnsJSON, "json", false, "Emit DNS probe results as JSON")

	var connTarget, connProto string
	var connCount int
	var connTimeout time.Duration
	var connJSON bool
	connCmd := &cobra.Command{
		Use:     "connectivity [host:port]",
		Aliases: []string{"conn", "tcp"},
		Short:   "Execute a TCP/UDP connectivity probe against a VIP or service endpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := connTarget
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				return fmt.Errorf("target host:port is required (pass positional arg or --target)")
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] test connectivity target=%s proto=%s count=%d\n",
					target, connProto, connCount)
				return nil
			}
			res, err := pkgtest.RunConnectivity(cmd.Context(), pkgtest.ConnectivityConfig{
				Target:  target,
				Proto:   connProto,
				Count:   connCount,
				Timeout: connTimeout,
			})
			if err != nil {
				return err
			}
			if connJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Connectivity %s/%s -> %d/%d ok (loss=%.1f%% avg=%.2fms)\n",
				res.Protocol, res.Target, res.Successful, res.Total, res.LossPercent, res.AvgRTTMs)
			return nil
		},
	}
	connCmd.Flags().StringVar(&connTarget, "target", "", "Target host:port to dial")
	connCmd.Flags().StringVar(&connProto, "proto", "tcp", "Transport protocol (tcp or udp)")
	connCmd.Flags().IntVar(&connCount, "count", 3, "Number of dial attempts")
	connCmd.Flags().DurationVar(&connTimeout, "timeout", 3*time.Second, "Per-attempt dial timeout")
	connCmd.Flags().BoolVar(&connJSON, "json", false, "Emit connectivity probe results as JSON")

	cmd.AddCommand(dnsCmd, connCmd)
	return cmd
}

func resolveKubeconfigFromState(flags *GlobalFlags, st *state.State) string {
	kc := ""
	if flags != nil {
		kc = flags.Kubeconfig
	}
	if kc == "" && st != nil {
		kc = st.Get("KUBECONFIG")
		if kc == "" {
			kc = st.Get("KUBECONFIG_PATH")
		}
		if kc != "" {
			if _, statErr := os.Stat(kc); statErr != nil {
				if candidate := st.KubeconfigPath(); candidate != "" {
					kc = candidate
				}
			}
		} else if _, statErr := os.Stat(st.KubeconfigPath()); statErr == nil {
			kc = st.KubeconfigPath()
		}
	}
	return kc
}

func resolveKubeconfigForK(flags *GlobalFlags) (string, error) {
	_, st, _, err := resolveRuntime(flags)
	if err != nil {
		return "", err
	}
	return resolveKubeconfigFromState(flags, st), nil
}

func newKCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "k",
		Short: "In-process Kubernetes client-go commands with cluster template variable rendering",
	}

	varsCmd := &cobra.Command{
		Use:   "vars",
		Short: "Print resolved NetVars template variables for the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			vars := scenarios.TemplateVars(cl, st)
			keys := make([]string, 0, len(vars))
			for k := range vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%v\n", k, vars[k])
			}
			return nil
		},
	}

	var renderFile string
	renderCmd := &cobra.Command{
		Use:   "render [file]",
		Short: "Render a manifest template using cluster NetVars and state variables",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			target := renderFile
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				return fmt.Errorf("manifest file is required (pass positional arg or -f)")
			}
			var raw []byte
			if target == "-" {
				raw, err = io.ReadAll(os.Stdin)
			} else {
				// #nosec G304 -- operator-specified template file path
				raw, err = os.ReadFile(target)
			}
			if err != nil {
				return err
			}
			rendered, err := scenarios.RenderTemplate(string(raw), scenarios.TemplateVars(cl, st))
			if err != nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), rendered)
			return err
		},
	}
	renderCmd.Flags().StringVarP(&renderFile, "filename", "f", "", "Template manifest file to render (or - for stdin)")

	var getNS, getOutput, getSelector string
	var getAllNS bool
	getCmd := &cobra.Command{
		Use:   "get <resource> [name...]",
		Short: "Display one or many Kubernetes resources via in-process client-go",
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k get %s (ns=%s all=%v selector=%s output=%s)\n",
					strings.Join(args, " "), getNS, getAllNS, getSelector, getOutput)
				return nil
			}
			opts := &k8s.GetOptions{
				Args:           args,
				Namespace:      getNS,
				AllNamespaces:  getAllNS,
				LabelSelector:  getSelector,
				Output:         getOutput,
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run()
		},
	}
	getCmd.Flags().StringVarP(&getNS, "namespace", "n", "", "Target Kubernetes namespace")
	getCmd.Flags().BoolVarP(&getAllNS, "all-namespaces", "A", false, "List the requested object(s) across all namespaces")
	getCmd.Flags().StringVarP(&getSelector, "selector", "l", "", "Label selector (e.g. app=f5-tmm)")
	getCmd.Flags().StringVarP(&getOutput, "output", "o", "", "Output format (yaml, json, wide, name, jsonpath=...)")

	var descNS, descSelector string
	var descAllNS, descEvents bool
	describeCmd := &cobra.Command{
		Use:   "describe <resource> [name...]",
		Short: "Show details of a specific resource or group of resources via in-process client-go",
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k describe %s (ns=%s all=%v)\n",
					strings.Join(args, " "), descNS, descAllNS)
				return nil
			}
			opts := &k8s.DescribeOptions{
				Args:           args,
				Namespace:      descNS,
				AllNamespaces:  descAllNS,
				LabelSelector:  descSelector,
				ShowEvents:     descEvents,
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run()
		},
	}
	describeCmd.Flags().StringVarP(&descNS, "namespace", "n", "", "Target Kubernetes namespace")
	describeCmd.Flags().BoolVarP(&descAllNS, "all-namespaces", "A", false, "Describe objects across all namespaces")
	describeCmd.Flags().StringVarP(&descSelector, "selector", "l", "", "Label selector")
	describeCmd.Flags().BoolVar(&descEvents, "show-events", true, "If true, display events related to the described object")

	var applyFile, applyNS string
	var applyForce, applyRender bool
	applyCmd := &cobra.Command{
		Use:   "apply -f <filename>",
		Short: "Server-side apply a manifest file, directory, or kustomization via in-process client-go",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			kc := resolveKubeconfigFromState(flags, st)
			target := applyFile
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				return fmt.Errorf("`-f` filename is required")
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k apply -f %s (ns=%s force=%v render=%v)\n",
					target, applyNS, applyForce, applyRender)
				return nil
			}
			if applyRender && target != "-" {
				// #nosec G304 -- operator-specified manifest file path
				raw, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				rendered, err := scenarios.RenderTemplate(string(raw), scenarios.TemplateVars(cl, st))
				if err != nil {
					return err
				}
				tmp, err := os.CreateTemp("", "bnkctl-apply-*.yaml")
				if err != nil {
					return err
				}
				tmpPath := tmp.Name()
				defer os.Remove(tmpPath)
				if _, err := tmp.WriteString(rendered); err != nil {
					_ = tmp.Close()
					return err
				}
				_ = tmp.Close()
				target = tmpPath
			}
			opts := &k8s.ApplyOptions{
				Filename:       target,
				Namespace:      applyNS,
				Force:          applyForce,
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run(cmd.Context())
		},
	}
	applyCmd.Flags().StringVarP(&applyFile, "filename", "f", "", "Filename, directory, or - for stdin")
	applyCmd.Flags().StringVarP(&applyNS, "namespace", "n", "", "Target Kubernetes namespace")
	applyCmd.Flags().BoolVar(&applyForce, "force", true, "Force server-side apply conflict resolution")
	applyCmd.Flags().BoolVar(&applyRender, "render", false, "Render cluster NetVars template expressions before applying")

	var delNS, delSelector, delCascade string
	var delAllNS, delForce bool
	var delGracePeriod int
	deleteCmd := &cobra.Command{
		Use:   "delete <resource> [name...]",
		Short: "Delete resources by resource/name or label selector via in-process client-go",
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k delete %s (ns=%s selector=%s)\n",
					strings.Join(args, " "), delNS, delSelector)
				return nil
			}
			opts := &k8s.DeleteOptions{
				Args:           args,
				Namespace:      delNS,
				AllNamespaces:  delAllNS,
				LabelSelector:  delSelector,
				Force:          delForce,
				GracePeriod:    delGracePeriod,
				Cascade:        k8s.DeleteCascade(delCascade),
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run(cmd.Context())
		},
	}
	deleteCmd.Flags().StringVarP(&delNS, "namespace", "n", "", "Target Kubernetes namespace")
	deleteCmd.Flags().BoolVarP(&delAllNS, "all-namespaces", "A", false, "Delete objects across all namespaces")
	deleteCmd.Flags().StringVarP(&delSelector, "selector", "l", "", "Label selector")
	deleteCmd.Flags().BoolVar(&delForce, "force", false, "Immediate deletion (implies grace-period=0)")
	deleteCmd.Flags().IntVar(&delGracePeriod, "grace-period", -1, "Period of time in seconds given to the resource to terminate gracefully")
	deleteCmd.Flags().StringVar(&delCascade, "cascade", string(k8s.CascadeBackground), "Cascade deletion policy (background, foreground, orphan)")

	var logsNS, logsContainer, logsSince string
	var logsFollow, logsPrevious bool
	var logsTail int64
	logsCmd := &cobra.Command{
		Use:   "logs <pod>",
		Short: "Stream pod container logs via in-process client-go",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			sinceSec, err := k8s.ParseSinceDuration(logsSince)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k logs %s (ns=%s container=%s follow=%v)\n",
					args[0], logsNS, logsContainer, logsFollow)
				return nil
			}
			opts := &k8s.LogsOptions{
				PodName:        args[0],
				Namespace:      logsNS,
				Container:      logsContainer,
				Follow:         logsFollow,
				Previous:       logsPrevious,
				SinceSeconds:   sinceSec,
				TailLines:      logsTail,
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run(cmd.Context())
		},
	}
	logsCmd.Flags().StringVarP(&logsNS, "namespace", "n", "", "Target Kubernetes namespace")
	logsCmd.Flags().StringVar(&logsContainer, "container", "", "Container name")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Specify if the logs should be streamed")
	logsCmd.Flags().BoolVarP(&logsPrevious, "previous", "p", false, "Print the logs for the previous instance of the container")
	logsCmd.Flags().StringVar(&logsSince, "since", "", "Only return logs newer than a relative duration like 5s, 2m, or 3h")
	logsCmd.Flags().Int64Var(&logsTail, "tail", -1, "Lines of recent log file to display")

	var execNS, execContainer string
	var execStdin, execTTY bool
	execCmd := &cobra.Command{
		Use:   "exec <pod> -- <command> [args...]",
		Short: "Execute a command in a container via in-process SPDY stream",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			podName := args[0]
			commandArgs := args[1:]
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k exec %s -- %s (ns=%s container=%s)\n",
					podName, strings.Join(commandArgs, " "), execNS, execContainer)
				return nil
			}
			opts := &k8s.ExecOptions{
				PodName:        podName,
				Namespace:      execNS,
				Container:      execContainer,
				Stdin:          execStdin,
				TTY:            execTTY,
				Command:        commandArgs,
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run(cmd.Context())
		},
	}
	execCmd.Flags().StringVarP(&execNS, "namespace", "n", "", "Target Kubernetes namespace")
	execCmd.Flags().StringVar(&execContainer, "container", "", "Container name")
	execCmd.Flags().BoolVarP(&execStdin, "stdin", "i", false, "Pass stdin to the container")
	execCmd.Flags().BoolVarP(&execTTY, "tty", "t", false, "Stdin is a TTY")

	var pfNS string
	portForwardCmd := &cobra.Command{
		Use:   "port-forward <pod-or-service> <local>:<remote> [...]",
		Short: "Forward one or more local ports to a pod via in-process SPDY tunnel",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := resolveKubeconfigForK(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] k port-forward %s %s (ns=%s)\n",
					args[0], strings.Join(args[1:], " "), pfNS)
				return nil
			}
			opts := &k8s.PortForwardOptions{
				PodName:        args[0],
				Namespace:      pfNS,
				Ports:          args[1:],
				KubeconfigPath: kc,
				IOStreams: k8s.IOStreams{
					In:     os.Stdin,
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
				},
			}
			return opts.Run(cmd.Context())
		},
	}
	portForwardCmd.Flags().StringVarP(&pfNS, "namespace", "n", "", "Target Kubernetes namespace")

	cmd.AddCommand(
		varsCmd,
		renderCmd,
		getCmd,
		describeCmd,
		applyCmd,
		deleteCmd,
		logsCmd,
		execCmd,
		portForwardCmd,
	)
	return cmd
}

func newJumphostCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jumphost",
		Short: "Execute commands or probes via the cluster jumphost",
	}
	execCmd := &cobra.Command{
		Use:   "exec -- <command>",
		Short: "Run a command on the jumphost",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] jumphost (%s): %s\n", p.Name(), strings.Join(args, " "))
				return nil
			}
			runner, err := p.OpenJumphostSession(cmd.Context(), cl, st)
			if err != nil {
				return err
			}
			out, err := runner.Run(cmd.Context(), strings.Join(args, " "), nil)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.AddCommand(execCmd)
	return cmd
}

func newTopologyCmd(flags *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "topology [cluster.yaml]",
		Short: "Render cluster network and TMM dataplane topology",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			m := topology.Build(cl, st)
			fmt.Fprintln(cmd.OutOrStdout(), topology.RenderASCII(m))
			return nil
		},
	}
}

func newForgeCmd(flags *GlobalFlags) *cobra.Command {
	var mcpURL, restURL string
	cmd := &cobra.Command{
		Use:   "forge",
		Short: "F5 Forge telemetry, registration, and MCP operations",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, _, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forge subsystem ready for %s/%s\n", p.Name(), cl.EffectiveName())
			return nil
		},
	}
	cmd.PersistentFlags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	cmd.PersistentFlags().StringVar(&mcpURL, "mcp-url", "", "Forge MCP endpoint URL (defaults to BNKCTL_FORGE_MCP_URL or http://localhost:8081/mcp/)")
	cmd.PersistentFlags().StringVar(&restURL, "forge-url", "", "Forge REST endpoint URL (defaults to http://localhost:8000)")
	cmd.PersistentFlags().StringVar(&restURL, "url", "", "Forge REST endpoint URL (alias for --forge-url)")

	var projectName, envName string
	var postScan bool
	registerCmd := &cobra.Command{
		Use:   "register [cluster.yaml]",
		Short: "Register the managed cluster with F5 Forge via MCP",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			wsDir := flags.StateDir
			if wsDir == "" && st != nil && st.Dir() != "" {
				wsDir = st.Dir()
			}
			if wsDir == "" {
				wsDir = "."
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge register cluster=%s cloud=%s workspace=%s\n",
					cl.EffectiveName(), p.Name(), wsDir)
				return nil
			}
			kcPath := resolveKubeconfigFromState(flags, st)
			var kcBytes []byte
			if kcPath != "" {
				// #nosec G304 -- operator or workspace kubeconfig file path
				kcBytes, _ = os.ReadFile(kcPath)
			}
			effMCPURL := mcpURL
			effRESTURL := restURL
			effProjectName := projectName
			effEnv := envName
			var credTemplateID int
			var awsProfile string
			if cl != nil {
				awsProfile = cl.Metadata.Profile
				if cl.Forge != nil {
					if effMCPURL == "" && cl.Forge.MCPURL != "" {
						effMCPURL = cl.Forge.MCPURL
					}
					if effRESTURL == "" {
						effRESTURL = cl.Forge.ResolveURL()
					}
					if effProjectName == "" && cl.Forge.ProjectName != "" {
						effProjectName = cl.Forge.ProjectName
					}
					if !cmd.Flags().Changed("environment") && cl.Forge.Environment != "" {
						effEnv = cl.Forge.Environment
					}
					credTemplateID = cl.Forge.CredentialTemplateID
				}
			}
			client := forge.NewClient(effMCPURL)
			res, err := forge.Register(cmd.Context(), client, forge.RegisterRequest{
				ForgeRESTURL:         effRESTURL,
				WorkspaceName:        cl.EffectiveName(),
				WorkspaceDir:         wsDir,
				ProjectName:          effProjectName,
				ClusterName:          cl.EffectiveName(),
				Region:               cl.EffectiveRegion(),
				Kubeconfig:           kcBytes,
				AWSProfile:           awsProfile,
				Environment:          effEnv,
				ProjectType:          "cloud-" + string(p.Name()),
				CloudProvider:        string(p.Name()),
				CredentialTemplateID: credTemplateID,
				PostRegisterScan:     postScan,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered %s in Forge (project=%d cluster=%d)\n",
				res.Link.ClusterName, res.Link.ProjectID, res.Link.ClusterID)
			return nil
		},
	}
	registerCmd.Flags().StringVar(&projectName, "project", "", "Forge project name")
	registerCmd.Flags().StringVar(&envName, "environment", "dev", "Forge project environment")
	registerCmd.Flags().BoolVar(&postScan, "scan", true, "Run post-registration cluster scan and BNK health smoke check")

	var purge bool
	unregisterCmd := &cobra.Command{
		Use:   "unregister [cluster.yaml]",
		Short: "Unregister the workspace cluster from F5 Forge",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			wsDir := flags.StateDir
			if wsDir == "" && st != nil && st.Dir() != "" {
				wsDir = st.Dir()
			}
			if wsDir == "" {
				wsDir = "."
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge unregister workspace=%s purge=%v\n", wsDir, purge)
				return nil
			}
			effMCPURL := mcpURL
			effRESTURL := restURL
			var creds forge.RestCreds
			if st != nil && effMCPURL == "" {
				effMCPURL = st.Get("FORGE_MCP_URL")
			}
			if cl != nil && cl.Forge != nil {
				if effMCPURL == "" && cl.Forge.MCPURL != "" {
					effMCPURL = cl.Forge.MCPURL
				}
				if effRESTURL == "" {
					effRESTURL = cl.Forge.ResolveURL()
				}
				username := cl.Forge.ResolveUsername()
				password, _ := cl.Forge.ResolvePassword()
				creds = forge.RestCreds{Username: username, Password: password}
			}
			cleanState := func() {
				if st == nil {
					return
				}
				for _, k := range []string{
					"FORGE_PROJECT_ID",
					"FORGE_CLUSTER_ID",
					"FORGE_MCP_URL",
					"FORGE_REGISTERED",
					"FORGE_STATUS",
					"FORGE_LINK_PATH",
					"FORGE_REGISTERED_AT",
					"FORGE_SYNCED",
				} {
					st.Delete(k)
				}
				_ = st.Save()
			}
			client := forge.NewClient(effMCPURL)
			if err := forge.Unregister(cmd.Context(), client, wsDir, purge); err != nil {
				existingLink, linkErr := forge.ReadLink(wsDir)
				if existingLink != nil && effRESTURL == "" && existingLink.ForgeURL != "" {
					effRESTURL = existingLink.ForgeURL
				}
				var stateLink *forge.Link
				if existingLink != nil {
					stateLink = existingLink
				} else if st != nil {
					projID, _ := strconv.Atoi(st.Get("FORGE_PROJECT_ID"))
					clusID, _ := strconv.Atoi(st.Get("FORGE_CLUSTER_ID"))
					if projID > 0 || clusID > 0 {
						clusterName := ""
						if cl != nil {
							clusterName = cl.EffectiveName()
						}
						stateLink = &forge.Link{
							ProjectID:   projID,
							ClusterID:   clusID,
							ClusterName: clusterName,
							Status:      "registered",
						}
					}
				}
				if stateLink != nil {
					if errors.Is(err, os.ErrNotExist) {
						if lErr := forge.UnregisterLink(cmd.Context(), client, stateLink, wsDir, purge); lErr == nil {
							_ = forge.UnregisterREST(cmd.Context(), effRESTURL, stateLink, creds)
							cleanState()
							fmt.Fprintf(cmd.OutOrStdout(), "Unregistered %s from Forge (workspace %s)\n", stateLink.ClusterName, wsDir)
							return nil
						}
					}
					if rErr := forge.UnregisterREST(cmd.Context(), effRESTURL, stateLink, creds); rErr == nil {
						_ = forge.RemoveLink(wsDir)
						cleanState()
						fmt.Fprintf(cmd.OutOrStdout(), "Unregistered %s from Forge REST (workspace %s)\n", stateLink.ClusterName, wsDir)
						return nil
					}
				}
				if cl != nil && cl.EffectiveName() != "" {
					mcpErr := forge.UnregisterMCPByName(cmd.Context(), client, cl.EffectiveName(), purge)
					restErr := forge.UnregisterRESTByName(cmd.Context(), effRESTURL, cl.EffectiveName(), creds)
					if mcpErr == nil || restErr == nil {
						_ = forge.RemoveLink(wsDir)
						cleanState()
						fmt.Fprintf(cmd.OutOrStdout(), "Unregistered %s from Forge (workspace %s)\n", cl.EffectiveName(), wsDir)
						return nil
					}
					if errors.Is(linkErr, os.ErrNotExist) && stateLink == nil {
						cleanState()
						fmt.Fprintf(cmd.OutOrStdout(), "Workspace %s is already unregistered from Forge\n", wsDir)
						return nil
					}
					return fmt.Errorf("%w (mcp fallback: %v; rest fallback: %v)", err, mcpErr, restErr)
				}
				return err
			}
			cleanState()
			fmt.Fprintf(cmd.OutOrStdout(), "Unregistered workspace %s from Forge\n", wsDir)
			return nil
		},
	}
	unregisterCmd.Flags().BoolVar(&purge, "purge", false, "Delete both the Forge cluster and its parent project")

	statusCmd := &cobra.Command{
		Use:   "status [cluster.yaml]",
		Short: "Check the workspace's F5 Forge registration link and server reachability",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			wsDir := flags.StateDir
			if wsDir == "" && st != nil && st.Dir() != "" {
				wsDir = st.Dir()
			}
			if wsDir == "" {
				wsDir = "."
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge status workspace=%s\n", wsDir)
				return nil
			}
			effMCPURL := mcpURL
			if st != nil && effMCPURL == "" {
				effMCPURL = st.Get("FORGE_MCP_URL")
			}
			if cl != nil && cl.Forge != nil && effMCPURL == "" && cl.Forge.MCPURL != "" {
				effMCPURL = cl.Forge.MCPURL
			}
			client := forge.NewClient(effMCPURL)
			stRes, err := forge.Status(cmd.Context(), client, wsDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forge Link: project=%s (%d) cluster=%s (%d) reachable=%v\n",
				stRes.Link.ProjectName, stRes.Link.ProjectID, stRes.Link.ClusterName, stRes.Link.ClusterID, stRes.Reachable)
			return stRes.Err
		},
	}

	var govClusterID int
	var govClusterName string
	var govAsJSON bool
	resolveForgeGovernanceTarget := func(cmd *cobra.Command, cl *intent.Cluster, st *state.State, explicitClusterID int) (int, string, forge.RestCreds, error) {
		wsDir := flags.StateDir
		if wsDir == "" && st != nil && st.Dir() != "" {
			wsDir = st.Dir()
		}
		if wsDir == "" {
			wsDir = "."
		}
		effRESTURL := restURL
		var creds forge.RestCreds
		if cl != nil && cl.Forge != nil {
			if effRESTURL == "" {
				effRESTURL = cl.Forge.ResolveURL()
			}
			username := cl.Forge.ResolveUsername()
			password, _ := cl.Forge.ResolvePassword()
			creds = forge.RestCreds{Username: username, Password: password}
		}
		cid := explicitClusterID
		if link, lErr := forge.ReadLink(wsDir); lErr == nil && link != nil {
			if cid <= 0 {
				cid = link.ClusterID
			}
			if effRESTURL == "" && link.ForgeURL != "" {
				effRESTURL = link.ForgeURL
			}
		}
		if cid <= 0 && st != nil {
			cid, _ = strconv.Atoi(st.Get("FORGE_CLUSTER_ID"))
		}
		if cid <= 0 {
			lookupName := strings.TrimSpace(govClusterName)
			if lookupName == "" && cl != nil {
				lookupName = cl.EffectiveName()
			}
			if lookupName != "" {
				if resolvedID, rErr := forge.ResolveClusterIDByName(cmd.Context(), effRESTURL, lookupName, creds); rErr == nil && resolvedID > 0 {
					cid = resolvedID
				}
			}
		}
		if cid <= 0 {
			return 0, effRESTURL, creds, fmt.Errorf("forge cluster ID not found (pass --cluster-id, --cluster-name, or run `forge register` first)")
		}
		return cid, effRESTURL, creds, nil
	}

	governanceCmd := &cobra.Command{
		Use:   "governance",
		Short: "Multi-Cluster AI Gateway & Dependency Fabric operations via F5 Forge",
	}
	governanceCmd.PersistentFlags().IntVar(&govClusterID, "cluster-id", 0, "Forge cluster ID (defaults to workspace forge_link.json)")
	governanceCmd.PersistentFlags().StringVar(&govClusterName, "cluster-name", "", "Forge cluster name (fallback when --cluster-id or forge_link.json is absent)")
	governanceCmd.PersistentFlags().BoolVar(&govAsJSON, "json", false, "Emit raw JSON output")

	govOverviewCmd := &cobra.Command{
		Use:   "overview [cluster.yaml]",
		Short: "Fetch the Multi-Cluster AI Gateway dependency graph and readiness checklist",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge governance overview cluster=%s cluster-id=%d\n", cl.EffectiveName(), govClusterID)
				return nil
			}
			cid, effRESTURL, creds, err := resolveForgeGovernanceTarget(cmd, cl, st, govClusterID)
			if err != nil {
				return err
			}
			res, err := forge.GovernanceOverview(cmd.Context(), effRESTURL, cid, creds)
			if err != nil {
				return err
			}
			if govAsJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Summary())
			return nil
		},
	}

	var remAction, remNamespace, remTargetName string
	govRemediateCmd := &cobra.Command{
		Use:   "remediate [action] [cluster.yaml]",
		Short: "Trigger a BNK 2.4.1 AI Gateway remediation action in F5 Forge",
		RunE: func(cmd *cobra.Command, args []string) error {
			action := strings.TrimSpace(remAction)
			if action == "" && len(args) > 0 {
				action = strings.TrimSpace(args[0])
				if len(args) > 1 {
					applyPositionalConfig(flags, args[1:])
				}
			} else if len(args) > 0 {
				applyPositionalConfig(flags, args)
			}
			if action == "" {
				return fmt.Errorf("remediation action is required (pass positional <action> or --action)")
			}
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("namespace") && cl != nil && cl.EffectiveProvider() == intent.ProviderAWS {
				remNamespace = "f5-cne-system"
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge governance remediate action=%s namespace=%s target=%s cluster=%s\n", action, remNamespace, remTargetName, cl.EffectiveName())
				return nil
			}
			cid, effRESTURL, creds, err := resolveForgeGovernanceTarget(cmd, cl, st, govClusterID)
			if err != nil {
				return err
			}
			res, err := forge.GovernanceRemediate(cmd.Context(), effRESTURL, cid, forge.GovernanceRemediateRequest{
				Action:     action,
				Namespace:  remNamespace,
				TargetName: remTargetName,
			}, creds)
			if err != nil {
				return err
			}
			if govAsJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "remediate action=%s status=%s success=%v message=%s\n", res.Action, res.Status, res.Success, res.Message)
			return nil
		},
	}
	govRemediateCmd.Flags().StringVar(&remAction, "action", "", "Remediation action name (e.g., restart_cne_controller, sync_dns_cache, ensure_default_route, patch_semantic_cache)")
	govRemediateCmd.Flags().StringVarP(&remNamespace, "namespace", "n", "f5-bnk", "Target Kubernetes namespace for remediation")
	govRemediateCmd.Flags().StringVar(&remTargetName, "target-name", "", "Target resource name for remediation")

	var probeVIP, probePath, probeModel, probeModelID, probeRouteName, probeNamespace, probeVirtualKey, probePrompt string
	var probePort, probeMaxTokens int
	var probeSimulateFailover bool
	var probeHeaders []string
	govTestReqCmd := &cobra.Command{
		Use:   "test-request [cluster.yaml]",
		Short: "Send a live chat-completion test probe through the cluster's AI Gateway VIP via F5 Forge",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			vip := probeVIP
			if vip == "" && cl != nil {
				if v, vErr := cl.DefaultVIP(); vErr == nil {
					vip = v
				}
			}
			effModel := probeModel
			if effModel == "" {
				effModel = probeModelID
			}
			effModelID := probeModelID
			if effModelID == "" {
				effModelID = probeModel
			}
			if !cmd.Flags().Changed("namespace") && cl != nil && cl.EffectiveProvider() == intent.ProviderAWS {
				probeNamespace = "f5-cne-system"
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge governance test-request route=%s namespace=%s gateway-ip=%s port=%d path=%s model=%s\n",
					probeRouteName, probeNamespace, vip, probePort, probePath, effModel)
				return nil
			}
			cid, effRESTURL, creds, err := resolveForgeGovernanceTarget(cmd, cl, st, govClusterID)
			if err != nil {
				return err
			}
			hdrMap := map[string]string{}
			for _, h := range probeHeaders {
				if k, v, ok := strings.Cut(h, "="); ok {
					hdrMap[strings.TrimSpace(k)] = strings.TrimSpace(v)
				} else if k, v, ok := strings.Cut(h, ":"); ok {
					hdrMap[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			vk := strings.TrimSpace(probeVirtualKey)
			if vk == "" {
				for k, v := range hdrMap {
					if strings.EqualFold(k, "Authorization") {
						trimmed := strings.TrimSpace(v)
						if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
							vk = strings.TrimSpace(trimmed[7:])
						} else {
							vk = trimmed
						}
					}
				}
			}
			if vk == "" {
				vk = strings.TrimSpace(os.Getenv("AIGW_VIRTUAL_KEY"))
			}
			if vk == "" {
				vk = strings.TrimSpace(os.Getenv("BNKCTL_VIRTUAL_KEY"))
			}
			res, err := forge.GovernanceTestRequest(cmd.Context(), effRESTURL, cid, forge.GovernanceTestRequestPayload{
				RouteName:              probeRouteName,
				Namespace:              probeNamespace,
				VirtualKey:             vk,
				Prompt:                 probePrompt,
				Model:                  effModel,
				ModelID:                effModelID,
				SimulatePrimaryFailure: probeSimulateFailover,
				Path:                   probePath,
			}, creds)
			if err != nil {
				return err
			}
			if govAsJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Summary())
			if res.ResponseText != "" {
				fmt.Fprintln(cmd.OutOrStdout(), res.ResponseText)
			} else if bodyStr := res.ResponseBodyString(); bodyStr != "" {
				fmt.Fprintln(cmd.OutOrStdout(), bodyStr)
			}
			return nil
		},
	}
	govTestReqCmd.Flags().StringVar(&probeRouteName, "route-name", "", "Target AIRoute / HTTPRoute name")
	govTestReqCmd.Flags().StringVarP(&probeNamespace, "namespace", "n", "f5-bnk", "Target route namespace")
	govTestReqCmd.Flags().StringVar(&probeVirtualKey, "virtual-key", "", "DSSM Virtual API key (f5vk_...)")
	govTestReqCmd.Flags().BoolVar(&probeSimulateFailover, "simulate-failover", false, "Simulate primary model failure to test fallback routing")
	govTestReqCmd.Flags().StringVar(&probeVIP, "gateway-ip", "", "AI Gateway VIP address (defaults to cluster external VIP)")
	govTestReqCmd.Flags().IntVar(&probePort, "port", 80, "AI Gateway listener port")
	govTestReqCmd.Flags().StringVar(&probePath, "path", "/v1/chat/completions", "Inference request path")
	govTestReqCmd.Flags().StringVar(&probeModel, "model", "", "Target AIModel name")
	govTestReqCmd.Flags().StringVar(&probeModelID, "model-id", "", "Explicit model ID override")
	govTestReqCmd.Flags().StringVar(&probePrompt, "prompt", "Summarize enterprise AI Gateway readiness in one sentence.", "Prompt message")
	govTestReqCmd.Flags().IntVar(&probeMaxTokens, "max-tokens", 128, "Maximum completion tokens")
	govTestReqCmd.Flags().StringSliceVarP(&probeHeaders, "header", "H", nil, "Additional request headers (Key=Value)")

	var bootTenant, bootOrgID, bootTeamID, bootPrincipalID, bootKeyName, bootModel string
	var bootRPM, bootTPM int
	var bootBudget float64
	govBootstrapCmd := &cobra.Command{
		Use:   "bootstrap [cluster.yaml]",
		Short: "Provision DSSM AI Gateway org, team, principal, and virtual API key (f5vk_...) via F5 Forge",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, _, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] forge governance bootstrap tenant=%s org=%s team=%s principal=%s key=%s\n",
					bootTenant, bootOrgID, bootTeamID, bootPrincipalID, bootKeyName)
				return nil
			}
			cid, effRESTURL, creds, err := resolveForgeGovernanceTarget(cmd, cl, st, govClusterID)
			if err != nil {
				return err
			}
			var allowedModels []string
			if strings.TrimSpace(bootModel) != "" {
				allowedModels = []string{strings.TrimSpace(bootModel)}
			}
			orgRes, err := forge.GovernanceCreateOrg(cmd.Context(), effRESTURL, cid, forge.GovernanceOrgCreateRequest{
				Tenant:           bootTenant,
				Name:             bootOrgID,
				MonthlyBudgetUSD: bootBudget,
				RPMLimit:         bootRPM,
				TPMLimit:         bootTPM,
			}, creds)
			if err != nil {
				return fmt.Errorf("create org: %w", err)
			}
			orgID := orgRes.ID
			if orgID == "" {
				orgID = bootOrgID
			}
			if bootBudget > 0 {
				_, _ = forge.GovernanceCreateBudget(cmd.Context(), effRESTURL, cid, forge.GovernanceBudgetCreateRequest{
					Tenant:   bootTenant,
					Name:     bootOrgID + "-monthly",
					Scope:    "org",
					TargetID: orgID,
					LimitUSD: bootBudget,
					Period:   "monthly",
				}, creds)
			}
			teamRes, err := forge.GovernanceCreateTeam(cmd.Context(), effRESTURL, cid, forge.GovernanceTeamCreateRequest{
				Tenant:        bootTenant,
				OrgID:         orgID,
				Name:          bootTeamID,
				AllowedModels: allowedModels,
				RPMLimit:      bootRPM,
				TPMLimit:      bootTPM,
			}, creds)
			if err != nil {
				return fmt.Errorf("create team: %w", err)
			}
			teamID := teamRes.ID
			if teamID == "" {
				teamID = bootTeamID
			}
			prinRes, err := forge.GovernanceCreatePrincipal(cmd.Context(), effRESTURL, cid, forge.GovernancePrincipalCreateRequest{
				Tenant:        bootTenant,
				TeamID:        teamID,
				Name:          bootPrincipalID,
				Kind:          "agent",
				Role:          "developer",
				AllowedModels: allowedModels,
				RPMLimit:      bootRPM,
				TPMLimit:      bootTPM,
			}, creds)
			if err != nil {
				return fmt.Errorf("create principal: %w", err)
			}
			principalID := prinRes.ID
			if principalID == "" {
				principalID = bootPrincipalID
			}
			if bootRPM > 0 || bootTPM > 0 {
				_, _ = forge.GovernanceCreateRateLimit(cmd.Context(), effRESTURL, cid, forge.GovernanceRateLimitCreateRequest{
					Tenant:            bootTenant,
					Name:              bootPrincipalID + "-ratelimit",
					Scope:             "principal",
					TargetID:          principalID,
					RequestsPerMinute: bootRPM,
					TokensPerMinute:   bootTPM,
				}, creds)
			}
			keyRes, err := forge.GovernanceCreateVirtualKey(cmd.Context(), effRESTURL, cid, forge.GovernanceVirtualKeyCreateRequest{
				Tenant:           bootTenant,
				PrincipalID:      principalID,
				Name:             bootKeyName,
				AllowedModels:    allowedModels,
				RPMLimit:         bootRPM,
				TPMLimit:         bootTPM,
				MonthlyBudgetUSD: bootBudget,
			}, creds)
			if err != nil {
				return fmt.Errorf("create virtual key: %w", err)
			}
			if govAsJSON {
				b, _ := json.MarshalIndent(keyRes, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "tenant=%s org=%s team=%s principal=%s key_id=%s api_key=%s cleartext_key=%s\n",
				bootTenant, orgID, teamID, principalID, keyRes.KeyID, keyRes.APIKey, keyRes.CleartextKey)
			return nil
		},
	}
	govBootstrapCmd.Flags().StringVar(&bootTenant, "tenant", "default-tenant", "AITenant name")
	govBootstrapCmd.Flags().StringVar(&bootOrgID, "org", "f5-org", "DSSM Organization name/ID")
	govBootstrapCmd.Flags().StringVar(&bootTeamID, "team", "platform-ai", "DSSM Team name/ID")
	govBootstrapCmd.Flags().StringVar(&bootPrincipalID, "principal", "demo-agent", "DSSM Principal name/ID")
	govBootstrapCmd.Flags().StringVar(&bootKeyName, "key-name", "demo-agent-key", "Virtual API key name")
	govBootstrapCmd.Flags().StringVar(&bootModel, "model", "", "Allowed model ID for the virtual key")
	govBootstrapCmd.Flags().Float64Var(&bootBudget, "budget-usd", 1000.0, "Monthly organization budget in USD")
	govBootstrapCmd.Flags().IntVar(&bootRPM, "rpm-limit", 60, "Requests per minute limit")
	govBootstrapCmd.Flags().IntVar(&bootTPM, "tpm-limit", 100000, "Tokens per minute limit")

	governanceCmd.AddCommand(govOverviewCmd, govRemediateCmd, govTestReqCmd, govBootstrapCmd)

	cmd.AddCommand(registerCmd, unregisterCmd, statusCmd, governanceCmd)
	return cmd
}

func newGenAICmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "genai",
		Aliases: []string{"benchmark"},
		Short:   "GenAI inference and token-counting benchmark suite",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "GenAI benchmark subsystem ready for %s/%s\n", p.Name(), cl.EffectiveName())
			return nil
		},
	}

	var artifactPath string
	var asJSON bool
	parseCmd := &cobra.Command{
		Use:   "parse [profile_export_aiperf.json]",
		Short: "Parse an aiperf benchmark artifact into Forge GenAI metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := artifactPath
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				return fmt.Errorf("aiperf artifact path is required (pass positional arg or -f)")
			}
			if flags.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] genai parse %s\n", target)
				return nil
			}
			// #nosec G304 -- operator-specified aiperf artifact file path
			raw, err := os.ReadFile(target)
			if err != nil {
				return err
			}
			m, err := genai.ParseAiperfArtifact(raw)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(m)
			}
			for _, line := range m.Summary() {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}
	parseCmd.Flags().StringVarP(&artifactPath, "file", "f", "", "Path to profile_export_aiperf.json")
	parseCmd.Flags().BoolVar(&asJSON, "json", false, "Emit parsed GenAI metrics as JSON")

	cmd.AddCommand(parseCmd)
	return cmd
}

func newVersionCmd(binName string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print CLI version and embedded BNK release catalog versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", binName, Version)
			return nil
		},
	}
}

func newSelfUpdateCmd(binName string) *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update CLI binary from GitHub releases",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(cmd, binName, checkOnly)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Check for updates without replacing the binary")
	return cmd
}

func newSelfCmd(binName string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Manage the CLI binary itself (update, version)",
	}
	var checkOnly bool
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Update CLI binary from GitHub releases",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(cmd, binName, checkOnly)
		},
	}
	updateCmd.Flags().BoolVar(&checkOnly, "check", false, "Check for updates without replacing the binary")
	cmd.AddCommand(updateCmd, newVersionCmd(binName))
	return cmd
}

func runSelfUpdate(cmd *cobra.Command, _ string, checkOnly bool) error {
	return selfupdate.Update(cmd.Context(), Version, selfupdate.DefaultRepo, checkOnly, cmd.OutOrStdout())
}
