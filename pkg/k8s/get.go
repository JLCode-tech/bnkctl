package k8s

import (
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/kubectl/pkg/scheme"
)

// GetOptions captures the flag-parsed inputs to `awsbnkctl k get`.
//
// Args is the trailing positional list (e.g. ["pods"], ["pod", "foo"],
// or ["pods,services"]). Namespace + AllNamespaces are mutually
// exclusive at the CLI; LabelSelector is the kubectl `-l` value.
//
// Output is the kubectl `-o` value (yaml, json, wide, name,
// jsonpath=..., go-template=..., or "" for the human tabular default).
//
// KubeconfigPath is passed through to BuildRESTConfig — empty for the
// host default, "in-cluster" for in-pod use.
type GetOptions struct {
	Args           []string
	Namespace      string
	AllNamespaces  bool
	LabelSelector  string
	Output         string
	KubeconfigPath string

	// IOStreams is the destination for printer output and human-only
	// stderr noise. Defaults applied in Run() if zero-valued.
	IOStreams genericiooptions.IOStreams
}

// Run executes a kubectl-equivalent `get` against the configured
// kubeconfig. Output formatting is delegated to cli-runtime's
// PrintFlags so `-o yaml/json/wide/name/jsonpath/go-template` matches
// kubectl byte-for-byte.
func (o *GetOptions) Run() error {
	if len(o.Args) == 0 {
		return errors.New("at least one resource type required (e.g. `pods`, `nodes`, `pods/foo`)")
	}
	if o.IOStreams.Out == nil {
		o.IOStreams.Out = io.Discard
	}
	if o.IOStreams.ErrOut == nil {
		o.IOStreams.ErrOut = io.Discard
	}

	getter := newRESTClientGetter(o.KubeconfigPath, o.Namespace)

	// Build a printer that matches the kubectl output flags. PrintFlags
	// defaults to a no-op printer; we only set it for non-default
	// outputs and use the table path otherwise.
	pf := genericclioptions.NewPrintFlags("").WithTypeSetter(scheme.Scheme)
	out := o.Output
	pf.OutputFormat = &out

	// resource.Builder lifts the heavy lifting: discovery, RESTMapper,
	// plural/singular/short-name resolution, label selectors, the
	// AllNamespaces flag and the type/name positional grammar.
	b := resource.NewBuilder(getter).
		Unstructured().
		NamespaceParam(o.Namespace).
		DefaultNamespace().
		AllNamespaces(o.AllNamespaces).
		LabelSelectorParam(o.LabelSelector).
		ResourceTypeOrNameArgs(true, o.Args...).
		ContinueOnError().
		Latest().
		Flatten()

	r := b.Do()
	if err := r.Err(); err != nil {
		return err
	}

	infos, err := r.Infos()
	if err != nil {
		return err
	}

	// Empty result with no items: kubectl prints "No resources found"
	// (with namespace if scoped). Match that behaviour.
	if len(infos) == 0 {
		if o.Namespace != "" && !o.AllNamespaces {
			fmt.Fprintf(o.IOStreams.ErrOut, "No resources found in %s namespace.\n", o.Namespace)
		} else {
			fmt.Fprintln(o.IOStreams.ErrOut, "No resources found")
		}
		return nil
	}

	// For -o yaml/json/name/jsonpath/template: print each Info's
	// runtime.Object via the configured printer. Build a List wrapper
	// when there are multiple Infos so the output matches kubectl
	// "List" semantics.
	switch o.Output {
	case "", "wide":
		// Tabular fallback (default + wide). cli-runtime's table
		// printer renders v1.Table objects when the server returns
		// them; otherwise it falls back to a NAME column.
		tp := printers.NewTablePrinter(printers.PrintOptions{
			WithNamespace: o.AllNamespaces,
			Wide:          o.Output == "wide",
		})
		for _, info := range infos {
			if err := tp.PrintObj(info.Object, o.IOStreams.Out); err != nil {
				return err
			}
		}
		return nil
	default:
		printer, err := pf.ToPrinter()
		if err != nil {
			return fmt.Errorf("setting up output format %q: %w", o.Output, err)
		}
		if len(infos) == 1 {
			return printer.PrintObj(infos[0].Object, o.IOStreams.Out)
		}
		// Multiple objects — wrap in an UnstructuredList so the printer
		// produces a single List document.
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("List")
		for _, info := range infos {
			if u, ok := info.Object.(*unstructured.Unstructured); ok {
				list.Items = append(list.Items, *u)
				continue
			}
			// resource.Builder().Unstructured() should always give us
			// Unstructured; defensive copy just in case.
			u := &unstructured.Unstructured{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(asMap(info.Object), u); err == nil {
				list.Items = append(list.Items, *u)
			}
		}
		return printer.PrintObj(list, o.IOStreams.Out)
	}
}

// asMap is a defensive helper that pulls a map[string]interface{} out
// of a runtime.Object that wasn't already Unstructured. The expected
// path goes through Unstructured() above; this exists so the function
// degrades gracefully rather than panicking.
func asMap(obj runtime.Object) map[string]interface{} {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.UnstructuredContent()
	}
	return map[string]interface{}{}
}

// IsNotFound reports whether err is a kubernetes "not found" error.
// Helper for callers (CLI) that want to surface a clean exit code.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
