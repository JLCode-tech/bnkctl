package bnkengine

import (
	"context"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRenderAllMatrix(t *testing.T) {
	clouds := []string{intent.ProviderAWS, intent.ProviderAzure, intent.ProviderGCP}
	versions := []string{"2.3.2-3.2598.3-0.0.464", "2.4.0", "2.5.0"}

	for _, cloud := range clouds {
		for _, ver := range versions {
			t.Run(cloud+"_"+ver, func(t *testing.T) {
				cl := &intent.Cluster{
					Cloud: &intent.CloudSpec{Provider: cloud},
					Metadata: intent.Metadata{
						Name:   "test-" + cloud,
						Region: "us-east-1",
					},
					Network: intent.Network{
						Subnets: intent.Subnets{
							PrivateList: []intent.SubnetSpec{
								{CIDR: "10.0.11.0/24", AZ: "us-east-1a"},
								{CIDR: "10.0.12.0/24", AZ: "us-east-1b"},
							},
						},
					},
					Bnk: &intent.BnkSpec{
						ManifestVersion: ver,
						DeploymentSize:  "Small",
						ControllerEnv:   map[string]string{"CUSTOM_CTRL_FLAG": "enabled"},
						TmmEnv:          map[string]string{"CUSTOM_TMM_FLAG": "1"},
						FLOValues:       map[string]any{"customFloKey": "customFloVal"},
						ExtraManifests:  []string{"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra-cm\n  namespace: f5-cne-core\n"},
					},
				}

				u, err := provider.BuildUnderlayFromIntent(cl)
				if err != nil {
					t.Fatalf("BuildUnderlayFromIntent: %v", err)
				}
				prof, err := release.Resolve(context.Background(), ver, "", nil)
				if err != nil {
					t.Fatalf("release.Resolve: %v", err)
				}

				rendered, err := RenderAll(cl, u, prof, "test-jwt-token")
				if err != nil {
					t.Fatalf("RenderAll: %v", err)
				}

				all := string(rendered.AllManifests())
				if !strings.Contains(all, "kind: CNEInstance") {
					t.Errorf("missing CNEInstance in rendered manifests")
				}
				if !strings.Contains(all, "CUSTOM_CTRL_FLAG") || !strings.Contains(all, "CUSTOM_TMM_FLAG") {
					t.Errorf("missing pass-through env overrides in CNEInstance")
				}
				if !strings.Contains(all, TEEMCertURL) {
					t.Errorf("missing teemCertUrl in License CR")
				}
				if !strings.Contains(string(rendered.FLOValuesYAML), "customFloKey") {
					t.Errorf("missing floValues override in FLOValuesYAML")
				}
				if !strings.Contains(all, "name: extra-cm") {
					t.Errorf("missing extraManifests in AllManifests")
				}
				if !strings.Contains(string(rendered.NamespacesYAML), "name: f5-cne-core") {
					t.Errorf("missing f5-cne-core in NamespacesYAML")
				}
				if !strings.Contains(string(rendered.NamespacesYAML), "name: f5-maintenance") || !strings.Contains(string(rendered.NamespacesYAML), "name: f5-app") {
					t.Errorf("missing f5-maintenance or f5-app in NamespacesYAML")
				}
				if cloud == intent.ProviderAzure {
					if !strings.Contains(all, "TMM_GENERIC_SOCKET_DRIVER") {
						t.Errorf("expected TMM_GENERIC_SOCKET_DRIVER for Azure socket driver")
					}
				}

				if strings.HasPrefix(ver, "2.3") {
					if !strings.Contains(all, "kind: F5SPKVlan") || !strings.Contains(all, "kind: F5SPKSnatpool") {
						t.Errorf("expected F5SPKVlan and F5SPKSnatpool for 2.3 release")
					}
				} else {
					if !strings.Contains(all, "kind: Infra") || !strings.Contains(all, "kind: GatewaySettings") {
						t.Errorf("expected Infra + GatewaySettings for %s release", ver)
					}
					for _, requiredField := range []string{
						"ipams:",
						"ipPools:",
						"rangeStart:",
						"rangeEnd:",
						"networkAttachmentDefinitions:",
						"ipamRefs:",
						"networkAttachmentRef:",
						"egressDefaults:",
						"staticRoutes:",
						"sourceNATPools:",
						"egressConfigs:",
						"ingressConfig:",
						"defaultListenerNetwork:",
						"parametersRef:",
						"sourceSelector:",
					} {
						if !strings.Contains(all, requiredField) {
							t.Errorf("expected %q in rendered BNK %s manifests", requiredField, ver)
						}
					}
					if !strings.Contains(all, "USE_GATEWAY_SETTINGS") || !strings.Contains(all, "MAX_ACTIVE_TMM_REPLICAS") {
						t.Errorf("expected USE_GATEWAY_SETTINGS and MAX_ACTIVE_TMM_REPLICAS for %s release", ver)
					}
					if !strings.Contains(all, "TMM_SOCAT_DISABLE") {
						t.Errorf("expected TMM_SOCAT_DISABLE in rendered BNK %s CNEInstance", ver)
					}
					for _, routeEntry := range []string{"name: default", "name: service-cidr", "name: pod-cidr", "- 10.0.11.0/24", "- 10.0.12.0/24"} {
						if !strings.Contains(all, routeEntry) {
							t.Errorf("expected Infra staticRoutes entry %q in rendered BNK %s manifests", routeEntry, ver)
						}
					}
					if strings.HasPrefix(ver, "2.4.1") || strings.HasPrefix(ver, "2.5") {
						if !strings.Contains(all, "kind: F5BigDnsCache") || !strings.Contains(all, "name: aigw-dns-resolver") {
							t.Errorf("expected F5BigDnsCache/aigw-dns-resolver in rendered BNK %s manifests", ver)
						}
					}
					if strings.Contains(all, "availabilityZone:") {
						t.Errorf("availabilityZone must be omitted when PLATFORM_TYPE != AWS in rendered BNK %s manifests", ver)
					}
					if cloud == intent.ProviderAWS {
						if !strings.Contains(all, "name: tmm-init") || !strings.Contains(all, "bigdb tm.tcpsegmentationoffload disable") {
							t.Errorf("expected ConfigMap/tmm-init with TSO/LRO disable for AWS in BNK %s manifests", ver)
						}
					}
					if strings.Contains(all, "value: \"PCIDEVICE_INTEL_COM_") {
						t.Errorf("ROBIN_VFIO_RESOURCE_N must use short uppercase interface name without PCIDEVICE_INTEL_COM_ prefix")
					}
					if !strings.Contains(all, "eppNamespaces") {
						t.Errorf("expected eppNamespaces for %s release", ver)
					}
				}
			})
		}
	}
}

func TestEngineDryRunLifecycle(t *testing.T) {
	cl := &intent.Cluster{
		Cloud: &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata: intent.Metadata{
			Name:   "dryrun-lab",
			Region: "us-west-2",
		},
		Bnk: &intent.BnkSpec{
			ManifestVersion: "2.3.2-3.2598.3-0.0.464",
		},
	}
	eng, err := New(cl, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if _, err := eng.Install(ctx, Options{DryRun: true}); err != nil {
		t.Fatalf("Install dry-run: %v", err)
	}
	if _, err := eng.Upgrade(ctx, Options{DryRun: true, TargetVersion: "2.4.0", MigrateInPlace: true}); err != nil {
		t.Fatalf("Upgrade dry-run: %v", err)
	}
	if eng.Profile.Version != "2.4.0" {
		t.Fatalf("expected upgraded profile version 2.4.0, got %s", eng.Profile.Version)
	}
	if err := eng.Uninstall(ctx, Options{DryRun: true}); err != nil {
		t.Fatalf("Uninstall dry-run: %v", err)
	}
}

func TestEnsureCWCCertsAndHeal(t *testing.T) {
	ctx := context.Background()
	k8sClient := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-cne-controller",
				Namespace: bnkconst.InstanceNamespace,
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "controller",
								Env: []corev1.EnvVar{
									{Name: "SOME_VAR", Value: "null"},
								},
							},
						},
					},
				},
			},
		},
	)

	cl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderGCP},
		Metadata: intent.Metadata{Name: "heal-lab"},
		Bnk:      &intent.BnkSpec{ManifestVersion: "2.4.0"},
		Egress:   intent.EgressSpec{TestNamespace: "bnk-test"},
	}

	findings, err := RunHeal(ctx, k8sClient, nil, HealOptions{Cluster: cl})
	if err != nil {
		t.Fatalf("RunHeal: %v", err)
	}
	if len(findings) != len(AllHealChecks) {
		t.Fatalf("expected %d findings, got %d", len(AllHealChecks), len(findings))
	}
	for _, f := range findings {
		if !f.Healthy {
			t.Errorf("check %s unhealthy: %s (err=%s)", f.Check, f.Description, f.Error)
		}
	}
}

func TestDetectAndRemediateUnhealthyChecks(t *testing.T) {
	ctx := context.Background()
	k8sClient := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-toda-fluentd-custom",
				Namespace: bnkconst.InstanceNamespace,
			},
			Data: map[string]string{
				"fluentd.conf": "<match f5-tmm*>\n  @type copy\n  #<store>\n  #  @type stdout\n  #</store>\n</match>\n",
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-dssm",
				Namespace: bnkconst.InstanceNamespace,
			},
			Data: map[string]string{
				"probe.sh": "redis-cli --tls --cert /tls.crt ping",
			},
		},
	)

	scheme := runtime.NewScheme()
	cneGVR := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
	ppGVR := schema.GroupVersionResource{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-persistence-profiles"}
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cneGVR: "CNEInstanceList",
		ppGVR:  "F5BigPersistenceProfileList",
	},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "k8s.f5.com/v1",
				"kind":       "CNEInstance",
				"metadata": map[string]any{
					"name":      bnkconst.CRCNEInstanceName,
					"namespace": bnkconst.InstanceNamespace,
				},
				"spec": map[string]any{
					"manifestVersion": "2.4.0",
					"tmm":             map[string]any{},
				},
			},
		},
	)
	if _, err := dynClient.Resource(ppGVR).Namespace("default").Create(ctx, &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5BigPersistenceProfile",
			"metadata": map[string]any{
				"name":      "source-addr-pp",
				"namespace": "default",
			},
			"spec": map[string]any{
				"method": "source-address",
			},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed F5BigPersistenceProfile: %v", err)
	}

	cl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata: intent.Metadata{Name: "heal-unhealthy"},
		Bnk:      &intent.BnkSpec{ManifestVersion: "2.4.0"},
	}
	u := &provider.UnderlayContext{
		ServiceCIDR: "10.96.0.0/12",
		PodCIDR:     "10.244.0.0/16",
	}

	detected, err := DetectAll(ctx, k8sClient, dynClient, HealOptions{Cluster: cl, Underlay: u})
	if err != nil {
		t.Fatalf("DetectAll: %v", err)
	}
	unhealthyByName := map[HealCheckID]bool{}
	for _, f := range detected {
		if !f.Healthy {
			unhealthyByName[f.Check] = true
		}
	}
	for _, expected := range []HealCheckID{
		HealTMMLogStream,
		HealDSSMProbe,
		HealTMMK8sRoutes,
		HealEPPNamespaces,
		HealPersistenceProfileSync,
	} {
		if !unhealthyByName[expected] {
			t.Errorf("expected check %s to be detected as unhealthy", expected)
		}
	}

	remediated, err := RunHeal(ctx, k8sClient, dynClient, HealOptions{Cluster: cl, Underlay: u})
	if err != nil {
		t.Fatalf("RunHeal: %v", err)
	}
	for _, f := range remediated {
		if !f.Healthy {
			t.Errorf("expected check %s to be healthy after RunHeal, got err=%s desc=%s", f.Check, f.Error, f.Description)
		}
	}

	// Verify F5BigPersistenceProfile was mirrored to InstanceNamespace
	if _, err := dynClient.Resource(ppGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, "source-addr-pp", metav1.GetOptions{}); err != nil {
		t.Errorf("expected F5BigPersistenceProfile mirrored to %s: %v", bnkconst.InstanceNamespace, err)
	}
}

func TestRenderAIGateway241AndHeal(t *testing.T) {
	ctx := context.Background()
	cl := &intent.Cluster{
		Cloud: &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata: intent.Metadata{
			Name:   "aigw-lab",
			Region: "ap-southeast-1",
		},
		Bnk: &intent.BnkSpec{
			ManifestVersion: "2.4.1-3.2850.1-0.0.9",
			FLOVersion:      "v2.30.1-0.0.8",
			DeploymentSize:  "Small",
			AIGateway: &intent.BnkAIGatewaySpec{
				Enabled:            true,
				AIMetrics:          true,
				ConsoleURL:         "http://f5-ai-console-webui.aigateway:8080/v1/services/default",
				DefaultEgressRoute: true,
				DNSResolver:        true,
				DSSMSentinel:       true,
				MultiNUMASpanning:  true,
			},
		},
	}

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		t.Fatalf("BuildUnderlayFromIntent: %v", err)
	}
	prof, err := release.Resolve(ctx, cl.Bnk.ManifestVersion, "", nil)
	if err != nil {
		t.Fatalf("release.Resolve: %v", err)
	}

	rendered, err := RenderAll(cl, u, prof, "test-jwt-token")
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	all := string(rendered.AllManifests())
	for _, want := range []string{
		"aiGateway:",
		"aiMetrics: true",
		"consoleURL: http://f5-ai-console-webui.aigateway:8080/v1/services/default",
		"loggingSubsystem:",
		"TMM_ENFORCE_SINGLE_NUMA_NODE",
		"kind: F5BigDnsCache",
		"name: aigw-dns-resolver",
		"cacheType: net-resolver",
		"forwardZone: .",
		"name: default-egress",
		"- 0.0.0.0/0",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in rendered BNK 2.4.1 AI Gateway manifests", want)
		}
	}
	if !strings.Contains(string(rendered.FLOValuesYAML), "sentinel:") {
		t.Errorf("expected dssm.sentinel.enabled in FLOValuesYAML, got:\n%s", string(rendered.FLOValuesYAML))
	}
	if !strings.Contains(string(rendered.NamespacesYAML), "name: aigateway") {
		t.Errorf("expected aigateway namespace in NamespacesYAML, got:\n%s", string(rendered.NamespacesYAML))
	}

	// Test HealAIGatewayProfile + HealAIGWDNSResolver detect and remediate
	k8sClient := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-cne-controller",
				Namespace: bnkconst.InstanceNamespace,
			},
		},
	)
	scheme := runtime.NewScheme()
	cneGVR := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cneGVR:                  "CNEInstanceList",
		healAIGatewayProfileGVR: "AIGatewayProfileList",
		healAIModelGVR:          "AIModelList",
		healAIProviderGVR:       "AIProviderList",
		healDNSCacheGVR:         "F5BigDnsCacheList",
	},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "inference.k8s.f5.com/v1alpha1",
				"kind":       "AIGatewayProfile",
				"metadata": map[string]any{
					"name":      "bedrock-profile",
					"namespace": "default",
				},
				"status": map[string]any{
					"conditions": []any{
						map[string]any{
							"type":    "Programmed",
							"status":  "False",
							"message": "AI profile default/bedrock-profile is not prepared",
						},
					},
				},
			},
		},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "inference.k8s.f5.com/v1alpha1",
				"kind":       "AIModel",
				"metadata": map[string]any{
					"name":      "claude-sonnet-4-5",
					"namespace": "default",
				},
			},
		},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "inference.k8s.f5.com/v1alpha1",
				"kind":       "AIProvider",
				"metadata": map[string]any{
					"name":      "aws-bedrock-provider",
					"namespace": "default",
				},
			},
		},
	)

	detected, err := DetectAll(ctx, k8sClient, dynClient, HealOptions{
		Cluster: cl,
		Profile: prof,
		Checks:  []HealCheckID{HealAIGatewayProfile, HealAIGWDNSResolver},
	})
	if err != nil {
		t.Fatalf("DetectAll: %v", err)
	}
	if len(detected) != 2 || detected[0].Healthy || detected[1].Healthy {
		t.Fatalf("expected HealAIGatewayProfile and HealAIGWDNSResolver to be detected as unhealthy, got %+v", detected)
	}

	remediated, err := RunHeal(ctx, k8sClient, dynClient, HealOptions{
		Cluster: cl,
		Profile: prof,
		Checks:  []HealCheckID{HealAIGatewayProfile, HealAIGWDNSResolver},
	})
	if err != nil {
		t.Fatalf("RunHeal: %v", err)
	}
	for _, r := range remediated {
		if !r.Healthy || !r.Healed {
			t.Fatalf("expected check %s to be healed, got %+v", r.Check, r)
		}
	}
	dnsObj, err := dynClient.Resource(healDNSCacheGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, "aigw-dns-resolver", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected aigw-dns-resolver to be created by HealAIGWDNSResolver: %v", err)
	}
	if ct, _, _ := unstructured.NestedString(dnsObj.Object, "spec", "cacheType"); ct != "net-resolver" {
		t.Fatalf("expected cacheType=net-resolver, got %q", ct)
	}
}

func TestEngineUninstallCleansCRsPVCsAndLoadBalancers(t *testing.T) {
	ctx := context.Background()
	cl := &intent.Cluster{
		Cloud:    &intent.CloudSpec{Provider: intent.ProviderGCP},
		Metadata: intent.Metadata{Name: "uninstall-lab", Region: "australia-southeast1"},
		Bnk:      &intent.BnkSpec{ManifestVersion: "2.4.1-3.2850.1-0.0.9"},
	}
	eng, err := New(cl, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	k8sClient := fake.NewClientset(
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "data-f5-dssm-db-0",
				Namespace:  "f5-bnk",
				Finalizers: []string{"kubernetes.io/pvc-protection"},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-wide-controller",
				Namespace: bnkconst.UtilsNamespace,
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bnk-ext-lb",
				Namespace: bnkconst.InstanceNamespace,
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeLoadBalancer,
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bnk-internal-clusterip",
				Namespace: bnkconst.InstanceNamespace,
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
			},
		},
	)

	ficInfraGVR := schema.GroupVersionResource{Group: "fic.f5.com", Version: "v1alpha1", Resource: "infras"}
	aigwGVR := schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigateways"}
	tenantGVR := schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"}
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range bnkOverlayCleanupGVRs() {
		listKinds[gvr] = gvr.Resource + "List"
	}
	listKinds[ficInfraGVR] = "InfraList"
	listKinds[aigwGVR] = "AIGatewayList"
	listKinds[tenantGVR] = "AITenantList"
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds,
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "fic.f5.com/v1alpha1",
				"kind":       "Infra",
				"metadata": map[string]any{
					"name":       "f5-bnk-infra",
					"namespace":  "f5-bnk",
					"finalizers": []any{"fic.f5.com/finalizer"},
				},
			},
		},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "inference.k8s.f5.com/v1alpha1",
				"kind":       "AIGateway",
				"metadata": map[string]any{
					"name":       "vertex-gw",
					"namespace":  bnkconst.InstanceNamespace,
					"finalizers": []any{"inference.k8s.f5.com/finalizer"},
				},
			},
		},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "inference.k8s.f5.com/v1alpha1",
				"kind":       "AITenant",
				"metadata": map[string]any{
					"name":       "default-tenant",
					"finalizers": []any{"inference.k8s.f5.com/tenant-finalizer"},
				},
			},
		},
	)

	eng.K8s = k8sClient
	eng.Dyn = dynClient

	if err := eng.Uninstall(ctx, Options{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if list, err := dynClient.Resource(ficInfraGVR).Namespace("f5-bnk").List(ctx, metav1.ListOptions{}); err != nil || len(list.Items) != 0 {
		t.Errorf("expected fic.f5.com Infra in f5-bnk to be deleted, got %v (err=%v)", list, err)
	}
	if list, err := dynClient.Resource(aigwGVR).Namespace(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{}); err != nil || len(list.Items) != 0 {
		t.Errorf("expected AIGateway in %s to be deleted, got %v (err=%v)", bnkconst.InstanceNamespace, list, err)
	}
	if list, err := dynClient.Resource(tenantGVR).List(ctx, metav1.ListOptions{}); err != nil || len(list.Items) != 0 {
		t.Errorf("expected cluster-scoped AITenant to be deleted, got %v (err=%v)", list, err)
	}
	if pvcs, err := k8sClient.CoreV1().PersistentVolumeClaims("f5-bnk").List(ctx, metav1.ListOptions{}); err != nil || len(pvcs.Items) != 0 {
		t.Errorf("expected PVCs in f5-bnk to be deleted, got %d (err=%v)", len(pvcs.Items), err)
	}
	if pvcs, err := k8sClient.CoreV1().PersistentVolumeClaims(bnkconst.UtilsNamespace).List(ctx, metav1.ListOptions{}); err != nil || len(pvcs.Items) != 0 {
		t.Errorf("expected PVCs in %s to be deleted, got %d (err=%v)", bnkconst.UtilsNamespace, len(pvcs.Items), err)
	}
	if _, err := k8sClient.CoreV1().Services(bnkconst.InstanceNamespace).Get(ctx, "bnk-ext-lb", metav1.GetOptions{}); err == nil {
		t.Errorf("expected LoadBalancer Service bnk-ext-lb to be deleted")
	}
	if _, err := k8sClient.CoreV1().Services(bnkconst.InstanceNamespace).Get(ctx, "bnk-internal-clusterip", metav1.GetOptions{}); err != nil {
		t.Errorf("expected ClusterIP Service bnk-internal-clusterip to be preserved: %v", err)
	}
}
