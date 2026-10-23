// Package cli implements the unified bnkctl command-line interface and
// cloud-pinned entrypoints (awsbnkctl, azrbnkctl, gkebnkctl).
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers"
	"github.com/JLCode-tech/bnkctl/pkg/bnkengine"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/spf13/cobra"
)

// Version is the CLI build version (injected via -ldflags at build time).
var Version = "v2.0.0-unified"

// GlobalFlags holds persistent CLI flags shared across all subcommands.
type GlobalFlags struct {
	Cloud      string
	ConfigPath string
	StateDir   string
	BNKVersion string
	FLOVersion string
	Kubeconfig string
	DryRun     bool
}

// NewRootCmd constructs the root cobra.Command for bnkctl or one of the cloud-pinned
// entrypoints (when defaultCloud is "aws", "azure", or "gcp").
func NewRootCmd(defaultCloud string) *cobra.Command {
	useName := "bnkctl"
	shortDesc := "Unified F5 BNK multi-cloud lifecycle & scenario CLI (AWS, Azure, GCP)"
	switch defaultCloud {
	case intent.ProviderAWS:
		useName = "awsbnkctl"
		shortDesc = "F5 BNK lifecycle & scenario CLI for AWS EKS (unified engine)"
	case intent.ProviderAzure:
		useName = "azrbnkctl"
		shortDesc = "F5 BNK lifecycle & scenario CLI for Azure AKS (unified engine)"
	case intent.ProviderGCP:
		useName = "gkebnkctl"
		shortDesc = "F5 BNK lifecycle & scenario CLI for GCP GKE (unified engine)"
	}

	flags := &GlobalFlags{
		Cloud:      defaultCloud,
		ConfigPath: "cluster.yaml",
	}

	root := &cobra.Command{
		Use:           useName,
		Short:         shortDesc,
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flags.Cloud, "cloud", defaultCloud, "Target cloud provider (aws, azure, gcp); inferred from cluster.yaml when omitted")
	pf.StringVarP(&flags.ConfigPath, "config", "c", "cluster.yaml", "Path to cluster.yaml intent file")
	pf.StringVar(&flags.StateDir, "state-dir", "", "Override state directory (defaults to .bnkctl/<cluster-name>)")
	pf.StringVar(&flags.BNKVersion, "bnk-version", "", "Override BNK release version (e.g. 2.3.2-3.2598.3-0.0.464, 2.4.0, 2.5.0)")
	pf.StringVar(&flags.FLOVersion, "flo-version", "", "Override FLO Helm chart version")
	pf.StringVar(&flags.Kubeconfig, "kubeconfig", "", "Override kubeconfig path")
	pf.BoolVar(&flags.DryRun, "dry-run", false, "Validate and render all underlay/overlay operations without mutating cloud or cluster resources")

	root.AddCommand(
		newUpCmd(flags),
		newDownCmd(flags),
		newUpgradeCmd(flags),
		newMigrateCmd(flags),
		newHealCmd(flags),
		newStatusCmd(flags),
		newValidateCmd(flags),
		newDoctorCmd(flags),
		newBNKCmd(flags),
		newScenariosCmd(flags),
		newTestCmd(flags),
		newKCmd(flags),
		newJumphostCmd(flags),
		newTopologyCmd(flags),
		newForgeCmd(flags),
		newGenAICmd(flags),
		newVersionCmd(useName),
		newSelfUpdateCmd(useName),
		newSelfCmd(useName),
	)

	return root
}

// Execute runs the unified bnkctl CLI and exits 1 on error.
func Execute() {
	ExecuteForCloud("")
}

// ExecuteForCloud runs the CLI pinned to defaultCloud ("", "aws", "azure", "gcp").
func ExecuteForCloud(defaultCloud string) {
	cmd := NewRootCmd(defaultCloud)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// applyPositionalConfig allows commands such as `bnkctl down clusters/foo/cluster.yaml`
// (or a directory containing cluster.yaml) to populate flags.ConfigPath when --config/-c
// was not explicitly overridden.
func applyPositionalConfig(flags *GlobalFlags, args []string) {
	if flags == nil || len(args) == 0 {
		return
	}
	arg := strings.TrimSpace(args[0])
	if arg == "" {
		return
	}
	if flags.ConfigPath == "" || flags.ConfigPath == "cluster.yaml" {
		flags.ConfigPath = arg
	}
}

func resolveRuntime(flags *GlobalFlags) (*intent.Cluster, *state.State, provider.Provider, error) {
	var cl *intent.Cluster
	configPath := strings.TrimSpace(flags.ConfigPath)
	if configPath != "" {
		if info, err := os.Stat(configPath); err == nil && info.IsDir() {
			candidate := filepath.Join(configPath, "cluster.yaml")
			if _, cErr := os.Stat(candidate); cErr == nil {
				configPath = candidate
			}
		}
		if _, err := os.Stat(configPath); err == nil {
			loaded, err := intent.Load(configPath)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("loading %s: %w", configPath, err)
			}
			cl = loaded
		} else if configPath != "cluster.yaml" {
			return nil, nil, nil, fmt.Errorf("loading %s: %w", configPath, err)
		}
	}
	loadDotEnvFiles(configPath)
	if cl == nil {
		cloud := flags.Cloud
		if cloud == "" {
			cloud = intent.ProviderAWS
		}
		cl = &intent.Cluster{
			APIVersion: intent.APIVersion,
			Kind:       intent.Kind,
			Cloud:      &intent.CloudSpec{Provider: cloud},
			Metadata:   intent.Metadata{Name: "bnk-cluster", Region: defaultRegionForCloud(cloud)},
		}
	}
	if flags.Cloud != "" {
		cl.Cloud = &intent.CloudSpec{Provider: flags.Cloud}
	}
	if flags.BNKVersion != "" {
		if cl.Bnk == nil {
			cl.Bnk = &intent.BnkSpec{}
		}
		cl.Bnk.ManifestVersion = flags.BNKVersion
	}
	if flags.FLOVersion != "" {
		if cl.Bnk == nil {
			cl.Bnk = &intent.BnkSpec{}
		}
		cl.Bnk.FLOVersion = flags.FLOVersion
	}

	p, err := providers.Resolve(flags.Cloud, cl)
	if err != nil {
		return nil, nil, nil, err
	}

	stateDir := flags.StateDir
	if stateDir == "" {
		stateDir = discoverStateDir(cl, p.Name(), flags.DryRun)
	}
	st, err := state.Load(stateDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading state from %s: %w", stateDir, err)
	}
	if flags.Kubeconfig != "" {
		st.Set("KUBECONFIG", flags.Kubeconfig)
	}

	return cl, st, p, nil
}

func isTestProcess() bool {
	return os.Getenv("GO_TEST_BINARY") != "" || strings.HasSuffix(filepath.Base(os.Args[0]), ".test")
}

// loadDotEnvFiles loads KEY=VALUE pairs from .env in the current working directory
// or ancestors of configPath without overriding existing process environment variables.
func loadDotEnvFiles(configPath string) {
	if isTestProcess() {
		return
	}
	candidates := []string{".env"}
	if configPath != "" {
		dir := filepath.Dir(configPath)
		for i := 0; i < 4 && dir != "" && dir != "/" && dir != "."; i++ {
			candidates = append(candidates, filepath.Join(dir, ".env"))
			dir = filepath.Dir(dir)
		}
	}
	seen := map[string]bool{}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		data, err := os.ReadFile(abs) // #nosec G304 -- workspace .env file discovery
		if err != nil {
			continue
		}
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			idx := strings.IndexByte(line, '=')
			if idx <= 0 {
				continue
			}
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
				v = v[1 : len(v)-1]
			}
			if k != "" && os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}

func discoverStateDir(cl *intent.Cluster, cloudName string, dryRun bool) string {
	name := ""
	relState := ""
	if cl != nil {
		name = cl.EffectiveName()
		relState = cl.StateDir()
	}
	if name == "" {
		name = "default"
	}
	if relState != "" {
		candidates := []string{relState}
		if cl != nil && cl.SourcePath != "" {
			dir := filepath.Dir(cl.SourcePath)
			for i := 0; i < 4 && dir != "" && dir != "/" && dir != "."; i++ {
				candidates = append(candidates, filepath.Join(dir, relState))
				dir = filepath.Dir(dir)
			}
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				return c
			}
		}
	}
	if dryRun || isTestProcess() {
		return filepath.Join(os.TempDir(), "bnkctl-state", cloudName, name)
	}
	if relState != "" {
		return relState
	}
	return filepath.Join(os.TempDir(), "bnkctl-state", cloudName, name)
}

func effectiveArchivePath(cl *intent.Cluster) string {
	if cl == nil {
		return ""
	}
	if cl.Derived.FARArchiveFullPath != "" {
		return cl.Derived.FARArchiveFullPath
	}
	if cl.Bnk != nil {
		return cl.Bnk.FARArchive
	}
	return ""
}

func buildEngine(ctx context.Context, cl *intent.Cluster, st *state.State, u *provider.UnderlayContext, flags *GlobalFlags) (*bnkengine.Engine, error) {
	ver := flags.BNKVersion
	if ver == "" && cl != nil && cl.Bnk != nil {
		ver = cl.Bnk.ManifestVersion
	}
	flo := flags.FLOVersion
	if flo == "" && cl != nil && cl.Bnk != nil {
		flo = cl.Bnk.FLOVersion
	}
	prof, err := release.Resolve(ctx, ver, effectiveArchivePath(cl), nil)
	if err != nil {
		return nil, err
	}
	if flo != "" {
		prof.FLOChartVersion = flo
	}
	return bnkengine.New(cl, st, u, prof)
}

func defaultRegionForCloud(cloud string) string {
	switch cloud {
	case intent.ProviderAzure:
		return "eastus2"
	case intent.ProviderGCP:
		return "us-central1"
	default:
		return "ap-southeast-2"
	}
}
