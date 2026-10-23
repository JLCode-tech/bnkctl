package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/bnkengine"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/migrate"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	"github.com/spf13/cobra"
)

func newUpCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up [cluster.yaml]",
		Short: "Provision cloud underlay and deploy BNK overlay stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			u, err := p.ProvisionUnderlay(cmd.Context(), cl, st, flags.DryRun)
			if err != nil {
				return fmt.Errorf("provisioning %s underlay: %w", p.Name(), err)
			}
			eng, err := buildEngine(cmd.Context(), cl, st, u, flags)
			if err != nil {
				return err
			}
			_, err = eng.Install(cmd.Context(), bnkengine.Options{
				DryRun: flags.DryRun,
				Log:    cmd.OutOrStdout(),
			})
			return err
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	return cmd
}

func newDownCmd(flags *GlobalFlags) *cobra.Command {
	var deleteRG bool
	var yes bool
	cmd := &cobra.Command{
		Use:   "down [cluster.yaml]",
		Short: "Tear down BNK overlay stack and destroy cloud underlay",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			cl, st, p, err := resolveRuntime(flags)
			if err != nil {
				return err
			}
			if deleteRG && st != nil {
				st.Set("DELETE_RESOURCE_GROUP", "true")
			}
			_ = yes
			u, err := p.DiscoverUnderlay(cmd.Context(), cl, st)
			if err != nil {
				return err
			}
			eng, err := buildEngine(cmd.Context(), cl, st, u, flags)
			if err != nil {
				return err
			}
			if err := eng.Uninstall(cmd.Context(), bnkengine.Options{
				DryRun: flags.DryRun,
				Log:    cmd.OutOrStdout(),
			}); err != nil {
				return err
			}
			return p.DestroyUnderlay(cmd.Context(), cl, st, flags.DryRun)
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	cmd.Flags().BoolVar(&deleteRG, "delete-rg", false, "Also delete the Azure Resource Group during teardown")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip interactive confirmation prompt")
	return cmd
}

func newUpgradeCmd(flags *GlobalFlags) *cobra.Command {
	var targetVersion string
	var toAlias string
	var floChart string
	var floVersionAlias string
	var namespace string
	var instanceName string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "upgrade [cluster.yaml]",
		Short: "Upgrade BNK overlay to a target release version",
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
			ver := targetVersion
			if ver == "" {
				ver = toAlias
			}
			if ver == "" {
				ver = flags.BNKVersion
			}
			if ver == "" {
				ver = bnk.DefaultManifestVersion
			}

			kc := u.KubeconfigPath
			if kc == "" && flags.Kubeconfig != "" {
				kc = flags.Kubeconfig
			}
			if kc == "" && st != nil {
				kc = st.KubeconfigPath()
			}

			// If dry-run or no live cluster kubeconfig is explicitly attached to the workspace,
			// execute engine-level upgrade validation/rendering.
			if flags.DryRun && kc == "" {
				_, err = eng.Upgrade(cmd.Context(), bnkengine.Options{
					DryRun:        flags.DryRun,
					TargetVersion: ver,
					Log:           cmd.OutOrStdout(),
				})
				return err
			}

			if kc != "" {
				if _, statErr := os.Stat(kc); statErr == nil {
					cfg, cfgErr := k8s.BuildRESTConfig(kc)
					if cfgErr == nil {
						cs, _ := k8s.NewClientsetFromConfig(cfg)
						dyn, _ := k8s.NewDynamicFromConfig(cfg)
						applier, _ := migrate.NewDynamicApplier(cfg)
						helmInst, helmErr := k8s.NewHelmInstaller(kc, "")
						if helmErr == nil && dyn != nil {
							canonicalVer, _ := migrate.CanonicalManifestVersion(ver)
							if canonicalVer == "" {
								canonicalVer = bnk.DefaultManifestVersion
							}
							resolvedFLO := floChart
							if resolvedFLO == "" {
								resolvedFLO = floVersionAlias
							}
							if resolvedFLO == "" {
								resolvedFLO = flags.FLOVersion
							}
							if resolvedFLO == "" {
								if known, ok := bnk.FLOChartFor(canonicalVer); ok {
									resolvedFLO = known
								} else if prof, rErr := release.Resolve(cmd.Context(), canonicalVer, effectiveArchivePath(cl), nil); rErr == nil {
									resolvedFLO = prof.FLOChartVersion
								}
							}
							var rbacBytes []byte
							if rbacTmpl, rErr := manifests.FS.ReadFile("shared/cne-controller-rbac.yaml.tmpl"); rErr == nil {
								rbacBytes, _ = render.RenderCNEControllerRBAC(rbacTmpl, cl)
							}
							var reconcileID func(context.Context) error
							if u.ReconcileControllerIdentity != nil {
								reconcileID = func(ctx context.Context) error {
									return u.ReconcileControllerIdentity(ctx, cs, dyn)
								}
							}
							res, upErr := migrate.Upgrade(cmd.Context(), migrate.UpgradeDeps{
								Helm:    helmInst,
								Dyn:     dyn,
								K8s:     cs,
								Applier: applier,
							}, migrate.UpgradeOptions{
								Namespace:                   namespace,
								CNEInstanceName:             instanceName,
								ManifestVersion:             ver,
								FLOChart:                    resolvedFLO,
								SAKeyOrArchive:              effectiveArchivePath(cl),
								DryRun:                      flags.DryRun,
								ControllerRBAC:              rbacBytes,
								ServiceCIDR:                 u.ServiceCIDR,
								ReconcileControllerIdentity: reconcileID,
								Log:                         cmd.OutOrStdout(),
							})
							if jsonOut && res != nil {
								enc := json.NewEncoder(cmd.OutOrStdout())
								enc.SetIndent("", "  ")
								_ = enc.Encode(res)
							}
							return upErr
						}
					}
				}
			}

			_, err = eng.Upgrade(cmd.Context(), bnkengine.Options{
				DryRun:        flags.DryRun,
				TargetVersion: ver,
				Log:           cmd.OutOrStdout(),
			})
			return err
		},
	}
	cmd.Flags().StringVar(&targetVersion, "to-version", "", "Target BNK release version")
	cmd.Flags().StringVar(&toAlias, "to", "", "Target BNK release version (alias for --to-version)")
	cmd.Flags().StringVar(&floChart, "flo-chart", "", "Override f5-lifecycle-operator Helm chart version")
	cmd.Flags().StringVar(&floVersionAlias, "flo-version", "", "Override f5-lifecycle-operator Helm chart version (alias for --flo-chart)")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", bnkconst.InstanceNamespace, "CNEInstance namespace")
	cmd.Flags().StringVar(&instanceName, "instance", "", "CNEInstance name (defaults to single instance in namespace)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit structured JSON upgrade report")
	return cmd
}

func newMigrateCmd(flags *GlobalFlags) *cobra.Command {
	var targetVersion string
	var toAlias string
	var namespace string
	var infraName string
	var gatewayClass string
	var singleSelfIP bool
	var singleSelfIPAlias bool
	var applyChanges bool
	var cleanupLegacy bool
	var jsonOut bool

	cmd := &cobra.Command{
		Use:     "migrate [cluster.yaml]",
		Aliases: []string{"migrate-2.4"},
		Short:   "Migrate BNK overlay across architectural release boundaries (e.g. 2.3 legacy-spk -> 2.4/2.5 gateway-infra)",
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
			ver := targetVersion
			if ver == "" {
				ver = toAlias
			}
			if ver == "" {
				ver = flags.BNKVersion
			}
			if ver == "" {
				ver = bnk.DefaultManifestVersion
			}

			kc := u.KubeconfigPath
			if kc == "" && flags.Kubeconfig != "" {
				kc = flags.Kubeconfig
			}
			if kc == "" && st != nil {
				kc = st.KubeconfigPath()
			}

			if kc != "" {
				if _, statErr := os.Stat(kc); statErr == nil {
					cfg, cfgErr := k8s.BuildRESTConfig(kc)
					if cfgErr == nil {
						dyn, dynErr := k8s.NewDynamicFromConfig(cfg)
						if dynErr == nil {
							inv, iErr := migrate.Inspect(cmd.Context(), dyn, namespace)
							if iErr != nil {
								return iErr
							}
							targetProf, _ := release.Resolve(cmd.Context(), ver, effectiveArchivePath(cl), nil)
							servedVer := ""
							if targetProf != nil {
								servedVer = targetProf.ServedGatewayAPIVersion
							}
							plan, tErr := migrate.Translate(inv, migrate.Options{
								InfraName:               infraName,
								GatewayClassName:        gatewayClass,
								SingleSelfIP:            singleSelfIP || singleSelfIPAlias,
								TargetVersion:           ver,
								ServedGatewayAPIVersion: servedVer,
							})
							if tErr != nil {
								return tErr
							}
							if jsonOut {
								if err := migrate.WriteJSON(cmd.OutOrStdout(), inv, plan); err != nil {
									return err
								}
							} else {
								migrate.WriteReport(cmd.ErrOrStderr(), inv, plan)
								if err := migrate.WriteYAML(cmd.OutOrStdout(), plan); err != nil {
									return err
								}
							}
							if (!flags.DryRun || applyChanges) && !flags.DryRun && !plan.Empty() {
								applier, aErr := migrate.NewDynamicApplier(cfg)
								if aErr != nil {
									return aErr
								}
								if err := migrate.Apply(cmd.Context(), applier, plan, cmd.OutOrStdout()); err != nil {
									return err
								}
								if cleanupLegacy {
									if err := migrate.CleanupLegacy(cmd.Context(), dyn, inv, cmd.OutOrStdout()); err != nil {
										return err
									}
								}
							}
							return nil
						}
					}
				}
			}

			_, err = eng.Migrate(cmd.Context(), bnkengine.Options{
				DryRun:         flags.DryRun,
				TargetVersion:  ver,
				MigrateInPlace: true,
				Log:            cmd.OutOrStdout(),
			})
			return err
		},
	}
	cmd.Flags().StringVar(&targetVersion, "to-version", "", "Target BNK release version for migration")
	cmd.Flags().StringVar(&toAlias, "to", "", "Target BNK release version for migration (alias for --to-version)")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", bnkconst.InstanceNamespace, "CNEInstance namespace to inspect")
	cmd.Flags().StringVar(&infraName, "infra-name", migrate.DefaultInfraName, "Name of the generated Infra CR")
	cmd.Flags().StringVar(&gatewayClass, "gateway-class", "", "GatewayClass name for migrated EgressGateways")
	cmd.Flags().BoolVar(&singleSelfIP, "single-self-ip", false, "Keep self-IP pool at a single address instead of widening to /27")
	cmd.Flags().BoolVar(&singleSelfIPAlias, "single-selfip", false, "Keep self-IP pool at a single address instead of widening to /27 (alias)")
	cmd.Flags().BoolVar(&applyChanges, "apply", false, "Apply translated Gateway/Infra CRs to the cluster")
	cmd.Flags().BoolVar(&cleanupLegacy, "cleanup", false, "Delete legacy BNK 2.3 F5SPK* CRs after applying 2.4+ CRs")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit migration inventory and plan as JSON")
	return cmd
}

func newHealCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "heal [cluster.yaml]",
		Short: "Run BNK self-healing and release quirk reconciliations",
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
			if flags.DryRun || eng.K8s == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] heal reconciled for %s/%s (BNK %s)\n",
					p.Name(), u.ClusterName, eng.Profile.Version)
				return nil
			}
			_, err = bnkengine.RunHeal(cmd.Context(), eng.K8s, eng.Dyn, bnkengine.HealOptions{
				Cluster:  cl,
				Underlay: u,
				Profile:  eng.Profile,
				DryRun:   flags.DryRun,
			})
			return err
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	return cmd
}

func newStatusCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status [cluster.yaml]",
		Aliases: []string{"inspect"},
		Short:   "Inspect cloud underlay, BNK release profile, and active quirks",
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
			fmt.Fprintf(cmd.OutOrStdout(), "Cloud Provider: %s\n", p.Name())
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster Name:   %s\n", u.ClusterName)
			fmt.Fprintf(cmd.OutOrStdout(), "Region:         %s\n", u.Region)
			fmt.Fprintf(cmd.OutOrStdout(), "Dataplane:      %s\n", u.DataplaneDriver)
			fmt.Fprintf(cmd.OutOrStdout(), "BNK Release:    %s (train %s, model %s)\n",
				eng.Profile.Version, eng.Profile.Train.String(), eng.Profile.NetworkModel)
			fmt.Fprintf(cmd.OutOrStdout(), "FLO Chart:      %s\n", eng.Profile.FLOChartVersion)
			return nil
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	return cmd
}

func newValidateCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate [cluster.yaml]",
		Short: "Validate cluster.yaml intent and dry-run render all BNK overlay manifests",
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
			rendered, err := bnkengine.RenderAll(cl, u, eng.Profile, "validate-jwt")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Validated %s cluster %q (%s, BNK %s, %d networks)\n",
				p.Name(), u.ClusterName, rendered.NetworkModel, rendered.ReleaseVersion, len(u.Networks))
			return nil
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	return cmd
}

func newDoctorCmd(flags *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor [cluster.yaml]",
		Short: "Run end-to-end diagnostic checks on cloud intent, release profile, kubeconfig, and BNK overlay health",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyPositionalConfig(flags, args)
			out := cmd.OutOrStdout()
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

			fmt.Fprintf(out, "[doctor] Cloud Provider:   %s (region=%s, cluster=%s)\n", p.Name(), u.Region, u.ClusterName)
			fmt.Fprintf(out, "[doctor] Dataplane Driver: %s (%d network attachments)\n", u.DataplaneDriver, len(u.Networks))
			for _, n := range u.Networks {
				fmt.Fprintf(out, "[doctor]   network %-10s role=%s tmm=%s cidr=%s gw=%s\n",
					n.Name, n.Role, n.TMMInterface, n.SubnetCIDR, n.GatewayIP)
			}
			fmt.Fprintf(out, "[doctor] BNK Release:      %s (train=%s, model=%s, flo=%s, gatewayAPI=%s)\n",
				eng.Profile.Version, eng.Profile.Train.String(), eng.Profile.NetworkModel,
				eng.Profile.FLOChartVersion, eng.Profile.ServedGatewayAPIVersion)
			if cl.Bnk != nil && cl.Bnk.AIGateway != nil && cl.Bnk.AIGateway.Enabled {
				fmt.Fprintf(out, "[doctor] AI Gateway 2.4.1: enabled (aiMetrics=%v, dssmSentinel=%v, dnsResolver=%v, defaultEgressRoute=%v, multiNUMASpanning=%v)\n",
					cl.Bnk.AIGateway.AIMetrics, cl.Bnk.AIGateway.DSSMSentinel, cl.Bnk.AIGateway.DNSResolver,
					cl.Bnk.AIGateway.DefaultEgressRoute, cl.Bnk.AIGateway.MultiNUMASpanning)
			}

			rendered, err := bnkengine.RenderAll(cl, u, eng.Profile, "doctor-jwt")
			if err != nil {
				return fmt.Errorf("manifest render check failed: %w", err)
			}
			fmt.Fprintf(out, "[doctor] Manifest Render:  ok (%d bytes total)\n", len(rendered.AllManifests()))

			kc := u.KubeconfigPath
			if kc == "" {
				kc = resolveKubeconfigFromState(flags, st)
			}
			if flags.DryRun || strings.TrimSpace(kc) == "" {
				fmt.Fprintf(out, "[doctor] Live K8s Probe:   skipped (dry-run or no cluster kubeconfig attached)\n")
				return nil
			}
			if _, statErr := os.Stat(kc); statErr != nil {
				fmt.Fprintf(out, "[doctor] Live K8s Probe:   kubeconfig %s not found on disk\n", kc)
				return nil
			}
			cfg, err := k8s.BuildRESTConfig(kc)
			if err != nil {
				return fmt.Errorf("kubeconfig %s invalid: %w", kc, err)
			}
			cs, err := k8s.NewClientsetFromConfig(cfg)
			if err != nil {
				return err
			}
			dyn, err := k8s.NewDynamicFromConfig(cfg)
			if err != nil {
				return err
			}
			idx, err := bnkscan.Scan(cmd.Context(), dyn, cs, bnkscan.Options{AcceptLegacy: true})
			if err != nil {
				return fmt.Errorf("live BNK scan failed: %w", err)
			}
			bnkscan.WriteReport(out, idx)
			findings, err := bnkengine.DetectAll(cmd.Context(), cs, dyn, bnkengine.HealOptions{
				Cluster:  cl,
				Underlay: u,
				Profile:  eng.Profile,
				DryRun:   true,
			})
			if err != nil {
				return err
			}
			for _, f := range findings {
				status := "healthy"
				if !f.Healthy {
					status = "unhealthy"
				}
				fmt.Fprintf(out, "[doctor] quirk %-24s %-10s %s\n", f.Check, status, f.Description)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&flags.ConfigPath, "file", "f", flags.ConfigPath, "Path to cluster.yaml intent file (alias for --config)")
	return cmd
}
