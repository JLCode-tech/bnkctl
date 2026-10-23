package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
)

const (
	// FLOReleaseName is the Helm release name of the F5 Lifecycle Operator.
	FLOReleaseName = "f5-lifecycle-operator"
	// FLONamespace is the namespace the FLO release lives in.
	FLONamespace = "f5-cne-core"
	// FLOChartRef is the OCI reference of the FLO chart on repo.f5.com.
	FLOChartRef = "oci://repo.f5.com/charts/f5-lifecycle-operator"
	// FLOValuesTemplatePath is the embedded flo-values template.
	FLOValuesTemplatePath = "shared/flo-values.yaml.tmpl"

	// CertManagerReleaseName is the Helm release name of cert-manager.
	CertManagerReleaseName = "cert-manager"
	// CertManagerNamespace is the namespace the cert-manager release lives in.
	CertManagerNamespace = "cert-manager"
	// CertManagerChartRef is the OCI reference of the cert-manager chart on quay.io.
	CertManagerChartRef = "oci://quay.io/jetstack/charts/cert-manager"

	floRegistryHost       = "repo.f5.com"
	floHelmInstallTimeout = 15 * time.Minute
)

// HelmInstaller is the testability interface wrapping the Helm SDK actions
// used by FLO installation and BNK upgrade.
type HelmInstaller interface {
	List(namespace, filter string) ([]*release.Release, error)
	Install(releaseName, namespace string, chart *chart.Chart, values map[string]interface{}) (*release.Release, error)
	Upgrade(releaseName, namespace string, chart *chart.Chart, values map[string]interface{}) (*release.Release, error)
	Uninstall(releaseName, namespace string) error
	PullAndLoad(chartRef, version string) (*chart.Chart, error)
}

type realHelmInstaller struct {
	actionConfig *action.Configuration
	settings     *cli.EnvSettings
}

func (r *realHelmInstaller) List(namespace, filter string) ([]*release.Release, error) {
	l := action.NewList(r.actionConfig)
	l.Filter = filter
	l.AllNamespaces = false
	l.SetStateMask()
	return l.Run()
}

func (r *realHelmInstaller) Install(releaseName, namespace string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	inst := action.NewInstall(r.actionConfig)
	inst.ReleaseName = releaseName
	inst.Namespace = namespace
	inst.Wait = true
	inst.Timeout = floHelmInstallTimeout
	inst.CreateNamespace = false
	return inst.Run(ch, values)
}

func (r *realHelmInstaller) Upgrade(releaseName, namespace string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	upg := action.NewUpgrade(r.actionConfig)
	upg.Namespace = namespace
	upg.Wait = true
	upg.Timeout = floHelmInstallTimeout
	rel, err := upg.Run(releaseName, ch, values)
	if err != nil {
		return nil, err
	}
	return rel, nil
}

func (r *realHelmInstaller) Uninstall(releaseName, _ string) error {
	uns := action.NewUninstall(r.actionConfig)
	uns.IgnoreNotFound = true
	_, err := uns.Run(releaseName)
	return err
}

func (r *realHelmInstaller) PullAndLoad(chartRef, version string) (*chart.Chart, error) {
	tmpDir, err := os.MkdirTemp("", "flo-chart-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir for chart pull: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pull := action.NewPullWithOpts(action.WithConfig(r.actionConfig))
	pull.Settings = r.settings
	pull.Version = version
	pull.DestDir = tmpDir
	pull.Untar = true
	if _, err := pull.Run(chartRef); err != nil {
		return nil, fmt.Errorf("pull chart %s@%s: %w", chartRef, version, err)
	}

	chartDir := filepath.Join(tmpDir, filepath.Base(chartRef))
	ch, err := loader.Load(chartDir)
	if err != nil {
		return nil, fmt.Errorf("load chart from %s: %w", chartDir, err)
	}
	return ch, nil
}

// NewHelmInstaller logs in to repo.f5.com with the FAR key (when non-empty) and returns a
// HelmInstaller bound to the FLO namespace of the cluster kubeconfigPath points at.
func NewHelmInstaller(kubeconfigPath, farKeyB64 string) (HelmInstaller, error) {
	return NewHelmInstallerForNamespace(kubeconfigPath, FLONamespace, farKeyB64)
}

// NewHelmInstallerForNamespace returns a HelmInstaller bound to the given namespace.
func NewHelmInstallerForNamespace(kubeconfigPath, namespace, farKeyB64 string) (HelmInstaller, error) {
	regClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create helm registry client: %w", err)
	}

	if farKeyB64 != "" {
		if err := regClient.Login(floRegistryHost,
			registry.LoginOptBasicAuth("_json_key_base64", farKeyB64),
		); err != nil {
			return nil, fmt.Errorf("helm registry login %s: %w", floRegistryHost, err)
		}
	}

	if namespace == "" {
		namespace = FLONamespace
	}

	settings := cli.New()
	if kubeconfigPath != "" {
		settings.KubeConfig = kubeconfigPath
	}

	actionConfig := new(action.Configuration)
	logFn := func(format string, v ...interface{}) {
		fmt.Fprintf(os.Stderr, "[helm] "+format+"\n", v...)
	}
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, "secret", logFn); err != nil {
		return nil, fmt.Errorf("init helm action config: %w", err)
	}
	actionConfig.RegistryClient = regClient

	return &realHelmInstaller{
		actionConfig: actionConfig,
		settings:     settings,
	}, nil
}

// NewCertManagerHelmInstaller returns an unauthenticated OCI HelmInstaller bound to
// the cert-manager namespace of the cluster kubeconfigPath points at.
func NewCertManagerHelmInstaller(kubeconfigPath string) (HelmInstaller, error) {
	regClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create helm registry client: %w", err)
	}
	settings := cli.New()
	if kubeconfigPath != "" {
		settings.KubeConfig = kubeconfigPath
	}
	actionConfig := new(action.Configuration)
	logFn := func(format string, v ...interface{}) {
		fmt.Fprintf(os.Stderr, "[helm] "+format+"\n", v...)
	}
	if err := actionConfig.Init(settings.RESTClientGetter(), CertManagerNamespace, "secret", logFn); err != nil {
		return nil, fmt.Errorf("init cert-manager helm action config: %w", err)
	}
	actionConfig.RegistryClient = regClient

	return &realHelmInstaller{
		actionConfig: actionConfig,
		settings:     settings,
	}, nil
}

// HelmValuesEqual reports whether two Helm values maps are semantically equal by
// comparing their canonical JSON representations.
func HelmValuesEqual(a, b map[string]interface{}) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}
