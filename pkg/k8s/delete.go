package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
)

// DeleteCascade is the kubectl --cascade enum.
type DeleteCascade string

const (
	CascadeBackground DeleteCascade = "background"
	CascadeForeground DeleteCascade = "foreground"
	CascadeOrphan     DeleteCascade = "orphan"
)

// DeleteOptions captures the flag-parsed inputs to `awsbnkctl k delete`.
//
// GracePeriod < 0 means use the resource's default; 0 means immediate
// (with Force). Cascade picks the propagation policy.
type DeleteOptions struct {
	Args           []string
	Namespace      string
	AllNamespaces  bool
	LabelSelector  string
	Force          bool
	GracePeriod    int
	Cascade        DeleteCascade
	KubeconfigPath string

	IOStreams genericiooptions.IOStreams
}

// Run resolves the resource selector(s) and deletes each via the
// dynamic client through cli-runtime's resource.Helper.
func (o *DeleteOptions) Run(ctx context.Context) error {
	if len(o.Args) == 0 {
		return errors.New("at least one resource type required (e.g. `pod foo`, `pods -l app=x`)")
	}
	if o.IOStreams.Out == nil {
		o.IOStreams.Out = io.Discard
	}

	getter := newRESTClientGetter(o.KubeconfigPath, o.Namespace)

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

	policy := propagationFor(o.Cascade)
	delOpts := metav1.DeleteOptions{}
	if policy != nil {
		delOpts.PropagationPolicy = policy
	}
	if o.Force && o.GracePeriod < 0 {
		// kubectl --force implies grace-period 0.
		zero := int64(0)
		delOpts.GracePeriodSeconds = &zero
	} else if o.GracePeriod >= 0 {
		gp := int64(o.GracePeriod)
		delOpts.GracePeriodSeconds = &gp
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		helper := resource.NewHelper(info.Client, info.Mapping)
		_, derr := helper.DeleteWithOptions(info.Namespace, info.Name, &delOpts)
		if derr != nil {
			return fmt.Errorf("deleting %s %s: %w", info.Mapping.Resource.Resource, info.Name, derr)
		}
		fmt.Fprintf(o.IOStreams.Out, "%s/%s deleted\n",
			info.Mapping.Resource.Resource, info.Name)
		return nil
	})
}

// propagationFor maps the user-facing cascade enum to the
// metav1.DeletionPropagation pointer the API expects.
func propagationFor(c DeleteCascade) *metav1.DeletionPropagation {
	switch c {
	case CascadeBackground:
		p := metav1.DeletePropagationBackground
		return &p
	case CascadeForeground:
		p := metav1.DeletePropagationForeground
		return &p
	case CascadeOrphan:
		p := metav1.DeletePropagationOrphan
		return &p
	}
	return nil
}
