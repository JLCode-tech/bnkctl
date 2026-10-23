package k8s

import (
	"errors"
	"fmt"
	"io"

	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/kubectl/pkg/describe"
)

// DescribeOptions captures the flag-parsed inputs to `awsbnkctl k describe`.
//
// We delegate to k8s.io/kubectl/pkg/describe for the heavy lifting —
// that's the same library kubectl/oc themselves use internally, so
// output is byte-equivalent for every kind kubectl knows about and
// generic via the unstructured GenericDescriber for kinds it doesn't.
type DescribeOptions struct {
	Args           []string
	Namespace      string
	AllNamespaces  bool
	LabelSelector  string
	ShowEvents     bool
	KubeconfigPath string

	IOStreams genericiooptions.IOStreams
}

// Run resolves the resource selector(s) into Infos, picks a describer
// per Info's GroupKind, and writes the output to o.IOStreams.Out.
func (o *DescribeOptions) Run() error {
	if len(o.Args) == 0 {
		return errors.New("at least one resource type required (e.g. `pod foo`, `nodes`)")
	}
	if o.IOStreams.Out == nil {
		o.IOStreams.Out = io.Discard
	}

	getter := newRESTClientGetter(o.KubeconfigPath, o.Namespace)

	settings := describe.DescriberSettings{
		ShowEvents: o.ShowEvents,
		ChunkSize:  500,
	}

	r := resource.NewBuilder(getter).
		Unstructured().
		NamespaceParam(o.Namespace).
		DefaultNamespace().
		AllNamespaces(o.AllNamespaces).
		LabelSelectorParam(o.LabelSelector).
		ResourceTypeOrNameArgs(true, o.Args...).
		ContinueOnError().
		Latest().
		Flatten().
		Do()

	if err := r.Err(); err != nil {
		return err
	}

	first := true
	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		mapping := info.ResourceMapping()
		describer, derr := describe.Describer(getter, mapping)
		if derr != nil {
			return fmt.Errorf("no describer for %s: %w", mapping.GroupVersionKind, derr)
		}
		out, derr := describer.Describe(info.Namespace, info.Name, settings)
		if derr != nil {
			return derr
		}
		if !first {
			// kubectl separates multi-resource describe output with a
			// blank line + form feed.
			fmt.Fprintln(o.IOStreams.Out)
			fmt.Fprintln(o.IOStreams.Out, "---")
		}
		first = false
		_, werr := io.WriteString(o.IOStreams.Out, out)
		return werr
	})
}
