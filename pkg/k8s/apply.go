package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// FieldManager is the field-manager string used for server-side apply operations.
const FieldManager = "bnkctl"

// ApplyOptions captures the flag-parsed inputs to `bnkctl k apply`.
type ApplyOptions struct {
	Filename       string
	Namespace      string
	Force          bool
	KubeconfigPath string

	IOStreams genericiooptions.IOStreams
}

// Run resolves Filename to a slice of unstructured objects and SSA-applies each via the dynamic client.
func (o *ApplyOptions) Run(ctx context.Context) error {
	if o.Filename == "" {
		return errors.New("`-f` filename is required (file, directory, or `-` for stdin)")
	}
	if o.IOStreams.Out == nil {
		o.IOStreams.Out = io.Discard
	}

	cfg, err := BuildRESTConfig(o.KubeconfigPath)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memCacheClient{disc})

	objs, err := o.loadObjects()
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return errors.New("no resources found in input")
	}

	for _, obj := range objs {
		if err := o.applyOne(ctx, dyn, mapper, obj); err != nil {
			return err
		}
	}
	return nil
}

func (o *ApplyOptions) loadObjects() ([]*unstructured.Unstructured, error) {
	if o.Filename == "-" {
		in := o.IOStreams.In
		if in == nil {
			in = os.Stdin
		}
		return parseYAMLStream(in)
	}
	st, err := os.Stat(o.Filename)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", o.Filename, err)
	}
	if !st.IsDir() {
		f, err := os.Open(o.Filename)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return parseYAMLStream(f)
	}
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(o.Filename, name)); err == nil {
			return loadKustomization(o.Filename)
		}
	}
	var out []*unstructured.Unstructured
	walkErr := filepath.WalkDir(o.Filename, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		f, err := os.Open(p) // #nosec G122 G304
		if err != nil {
			return err
		}
		defer f.Close()
		objs, err := parseYAMLStream(f)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, objs...)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

func loadKustomization(path string) ([]*unstructured.Unstructured, error) {
	fs := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(fs, path)
	if err != nil {
		return nil, fmt.Errorf("kustomize build: %w", err)
	}
	yml, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("kustomize emit: %w", err)
	}
	return parseYAMLStream(bytes.NewReader(yml))
}

func parseYAMLStream(r io.Reader) ([]*unstructured.Unstructured, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseYAMLStream(data)
}

func splitYAML(data []byte) [][]byte {
	return SplitYAMLDocs(data)
}

func (o *ApplyOptions) applyOne(
	ctx context.Context,
	dyn dynamic.Interface,
	mapper meta.RESTMapper,
	obj *unstructured.Unstructured,
) error {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		return fmt.Errorf("resource missing kind: %s", obj.GetName())
	}
	mapping, err := RESTMappingWithRefresh(mapper, gvk)
	if err != nil {
		return fmt.Errorf("REST mapping for %s: %w", gvk, err)
	}

	ns := obj.GetNamespace()
	if ns == "" {
		ns = o.Namespace
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace && ns == "" {
		ns = "default"
	}
	obj.SetNamespace(ns)

	body, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("marshaling %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}

	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = dyn.Resource(mapping.Resource)
	}

	applied, err := ri.Patch(
		ctx,
		obj.GetName(),
		types.ApplyPatchType,
		body,
		applyPatchOptions(o.Force),
	)
	if err != nil {
		return fmt.Errorf("server-side apply %s %s/%s: %w",
			mapping.Resource.Resource, ns, obj.GetName(), err)
	}

	fmt.Fprintf(o.IOStreams.Out, "%s/%s %s\n",
		strings.ToLower(mapping.Resource.Resource),
		applied.GetName(),
		appliedVerb(applied))
	return nil
}

func appliedVerb(_ *unstructured.Unstructured) string {
	return "configured"
}

func applyPatchOptions(force bool) metav1.PatchOptions {
	po := metav1.PatchOptions{
		FieldManager: FieldManager,
	}
	if force {
		t := true
		po.Force = &t
	}
	return po
}

type memCacheClient struct{ discovery.DiscoveryInterface }

func (memCacheClient) Fresh() bool { return true }
func (memCacheClient) Invalidate() {}

// Applier server-side-applies unstructured objects against a live Kubernetes cluster.
type Applier struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

// NewApplier builds a dynamic client + deferred-discovery RESTMapper from rest.Config.
func NewApplier(cfg *rest.Config) (*Applier, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("apply: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("apply: discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(NewNoopCachedDiscovery(disc))
	return &Applier{dyn: dyn, mapper: mapper}, nil
}

// NewApplierFromInterfaces constructs an Applier directly from interfaces (for testing or shared clients).
func NewApplierFromInterfaces(dyn dynamic.Interface, mapper meta.RESTMapper) *Applier {
	return &Applier{dyn: dyn, mapper: mapper}
}

// Apply applies a single unstructured object via Server-Side Apply.
func (a *Applier) Apply(ctx context.Context, obj *unstructured.Unstructured, force bool) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		return nil, fmt.Errorf("apply: resource missing kind: %s", obj.GetName())
	}
	mapping, err := RESTMappingWithRefresh(a.mapper, gvk)
	if err != nil {
		return nil, fmt.Errorf("apply: REST mapping for %s: %w", gvk, err)
	}

	ns := obj.GetNamespace()
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if ns == "" {
			ns = "default"
		}
		obj.SetNamespace(ns)
		ri = a.dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = a.dyn.Resource(mapping.Resource)
	}

	body, err := yaml.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("apply: marshaling %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}

	po := metav1.PatchOptions{FieldManager: FieldManager}
	if force {
		t := true
		po.Force = &t
	}
	applied, err := ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, body, po)
	if err != nil {
		return nil, fmt.Errorf("apply: server-side apply %s %s/%s: %w",
			mapping.Resource.Resource, ns, obj.GetName(), err)
	}
	return applied, nil
}

// ApplyAll applies each object in objs in order.
func (a *Applier) ApplyAll(ctx context.Context, objs []*unstructured.Unstructured, force bool) error {
	for _, obj := range objs {
		if _, err := a.Apply(ctx, obj, force); err != nil {
			return err
		}
	}
	return nil
}

// RESTMappingWithRefresh resolves gvk to a RESTMapping, retrying once against
// a reset discovery cache if the kind is initially unknown.
func RESTMappingWithRefresh(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil || !meta.IsNoMatchError(err) {
		return mapping, err
	}
	deferred, ok := mapper.(*restmapper.DeferredDiscoveryRESTMapper)
	if !ok {
		return nil, err
	}
	deferred.Reset()
	return mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
}

// NewNoopCachedDiscovery adapts a discovery.DiscoveryInterface into a CachedDiscoveryInterface.
func NewNoopCachedDiscovery(delegate discovery.DiscoveryInterface) discovery.CachedDiscoveryInterface {
	return memCacheClient{DiscoveryInterface: delegate}
}

// ParseYAMLStream splits a multi-document YAML byte stream into unstructured objects.
func ParseYAMLStream(data []byte) ([]*unstructured.Unstructured, error) {
	parts := SplitYAMLDocs(data)
	out := make([]*unstructured.Unstructured, 0, len(parts))
	for _, doc := range parts {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &u.Object); err != nil {
			return nil, fmt.Errorf("apply: parsing yaml: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}
