// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/dsx-ai-factory/nke/cli/internal/mgmtcluster"
	enginev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

const (
	// requestTimeout bounds every request this package issues.
	requestTimeout = 30 * time.Second

	// strictFieldValidation makes the API server fail a request that carries a
	// field the resource's schema does not define, instead of silently dropping it.
	strictFieldValidation = "Strict"

	// kubectlAnnotationPrefix marks annotations kubectl writes for its own use.
	// They describe how a manifest was applied, not the resource, so they are
	// left out of the portable form.
	kubectlAnnotationPrefix = "kubectl.kubernetes.io/"
)

// Dynamic is a Client that reads and writes machine images and releases on a
// management cluster through the Kubernetes dynamic client.
type Dynamic struct {
	client dynamic.Interface
}

// NewDynamic builds a Client from a management cluster kubeconfig path. The
// caller must resolve the path (for example from the active site): NewDynamic
// does not fall back to KUBECONFIG or ~/.kube/config.
func NewDynamic(kubeconfigPath string) (*Dynamic, error) {
	cfg, err := mgmtcluster.RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return &Dynamic{client: client}, nil
}

// newDynamicWithClient builds a Client from an existing dynamic client,
// bypassing kubeconfig resolution. Tests use it to inject a fake.
func newDynamicWithClient(client dynamic.Interface) *Dynamic {
	return &Dynamic{client: client}
}

// resource returns the dynamic resource interface of the kind. Every kind is
// cluster-scoped.
func (d *Dynamic) resource(kind Kind) (dynamic.ResourceInterface, error) {
	var plural string
	switch kind {
	case KindMachineImage:
		plural = "machineimages"
	case KindClusterRelease:
		plural = "clusterreleases"
	case KindNodePoolRelease:
		plural = "nodepoolreleases"
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
	return d.client.Resource(enginev1.SchemeGroupVersion.WithResource(plural)), nil
}

// Create implements Client.
func (d *Dynamic) Create(ctx context.Context, kind Kind, r Resource) (Resource, error) {
	res, err := d.resource(kind)
	if err != nil {
		return Resource{}, err
	}
	obj := &unstructured.Unstructured{Object: Manifest(kind, r)}
	// Manifest builds annotations as a generic map; the setter stores them in
	// the typed form the dynamic client requires.
	obj.SetAnnotations(r.Metadata.Annotations)

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	created, err := res.Create(ctx, obj, metav1.CreateOptions{FieldValidation: strictFieldValidation})
	if err != nil {
		return Resource{}, classify(kind, r.Metadata.Name, err)
	}
	return project(created), nil
}

// Get implements Client.
func (d *Dynamic) Get(ctx context.Context, kind Kind, name string) (Resource, error) {
	res, err := d.resource(kind)
	if err != nil {
		return Resource{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Resource{}, classify(kind, name, err)
	}
	return project(obj), nil
}

// List implements Client.
func (d *Dynamic) List(ctx context.Context, kind Kind) ([]Resource, error) {
	res, err := d.resource(kind)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, classify(kind, "", err)
	}
	out := make([]Resource, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, project(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// Delete implements Client.
func (d *Dynamic) Delete(ctx context.Context, kind Kind, name string) error {
	res, err := d.resource(kind)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return classify(kind, name, err)
	}
	return nil
}

// SetDeprecated implements Client. It sends a merge patch of
// spec.lifecycle.deprecated alone, so no other field of the resource can change.
func (d *Dynamic) SetDeprecated(ctx context.Context, kind Kind, name string, deprecated bool) (Resource, error) {
	if !kind.SupportsDeprecation() {
		return Resource{}, fmt.Errorf("%s: %w", kind, ErrNoLifecycle)
	}
	res, err := d.resource(kind)
	if err != nil {
		return Resource{}, err
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"lifecycle": map[string]any{"deprecated": deprecated}},
	})
	if err != nil {
		return Resource{}, fmt.Errorf("encoding patch: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	patched, err := res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldValidation: strictFieldValidation})
	if err != nil {
		return Resource{}, classify(kind, name, err)
	}
	return project(patched), nil
}

// project returns the portable form of a stored resource: its name, annotations
// and spec, without the Kubernetes transport fields.
func project(obj *unstructured.Unstructured) Resource {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	var annotations map[string]string
	for k, v := range obj.GetAnnotations() {
		if strings.HasPrefix(k, kubectlAnnotationPrefix) {
			continue
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[k] = v
	}
	return Resource{
		Metadata:  Metadata{Name: obj.GetName(), Annotations: annotations},
		Spec:      spec,
		CreatedAt: obj.GetCreationTimestamp().Time,
	}
}

// classify turns a Kubernetes API error into one of this package's errors,
// naming the resource. An error it does not recognise is returned wrapped.
func classify(kind Kind, name string, err error) error {
	subject := strings.ToLower(string(kind))
	if name != "" {
		subject = fmt.Sprintf("%s %q", subject, name)
	}
	switch {
	case apierrors.IsNotFound(err) && isMissingType(err):
		return fmt.Errorf("%s: %w", subject, ErrCRDsNotInstalled)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s: %w", subject, ErrNotFound)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%s: %w: published content is immutable, so publish a change under a new name", subject, ErrAlreadyExists)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%s: %w: the site's management cluster kubeconfig lacks the required permissions (%w)", subject, ErrForbidden, err)
	default:
		return fmt.Errorf("%s: %w", subject, err)
	}
}

// isMissingType reports whether a NotFound error says the resource type itself
// is absent, as opposed to one object of it. The API server names the object in
// the error details for the second case only.
func isMissingType(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	return details == nil || details.Name == ""
}
