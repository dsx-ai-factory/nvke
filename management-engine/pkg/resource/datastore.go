// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

const (
	requestTimeout  = 30 * time.Second
	maxPageKeyBytes = 16384
)

// resourceServer enforces projected intent, trusted namespace scope and resourceVersion preconditions.
type resourceServer[E ResourceHandler[I], I ResourceHandler[E]] struct {
	cfg      ClientConfig
	info     ResourceInfo
	resource dynamic.NamespaceableResourceInterface
}

// New constructs a Kubernetes-backed ResourceServer for external E and internal I.
// It contacts discovery to check the resource's identity, scope, status subresource and required verbs.
// Discovery needs no CRD read access. Each operation resolves its request-specific scope later.
func New[E ResourceHandler[I], I ResourceHandler[E]](cfg ClientConfig, info ResourceInfo) (ResourceServer[E], error) {
	if cfg.Datastore == nil || (info.Scope == Namespaced && cfg.Namespace == nil) {
		return nil, fmt.Errorf("datastore and a namespace resolver for namespaced resources are required")
	}
	if info.GVK.Empty() || info.Resource == "" {
		return nil, fmt.Errorf("resource metadata is incomplete")
	}
	if info.Scope != Namespaced && info.Scope != ClusterScoped {
		return nil, fmt.Errorf("resource scope is invalid")
	}
	if info.FieldManager == "" {
		info.FieldManager = "nvke-resource"
	}
	cfg.Datastore = rest.CopyConfig(cfg.Datastore)
	cfg.Datastore.Timeout = requestTimeout
	httpClient, err := rest.HTTPClientFor(cfg.Datastore)
	if err != nil {
		return nil, fmt.Errorf("creating datastore HTTP client: %w", err)
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg.Datastore, httpClient)
	if err != nil {
		return nil, fmt.Errorf("creating datastore client: %w", err)
	}
	info.Verbs = slices.Clone(info.Verbs)
	if err = checkResource(cfg.Datastore, httpClient, info); err != nil {
		return nil, err
	}
	return &resourceServer[E, I]{
		cfg: cfg, info: info,
		resource: dyn.Resource(info.GVK.GroupVersion().WithResource(info.Resource)),
	}, nil
}

// request checks the registered operation and resolves trusted scope from the request context.
// The returned datastore handle uses the service's credentials.
func (c *resourceServer[E, I]) request(ctx context.Context, op Operation) (dynamic.ResourceInterface, string, error) {
	if !slices.Contains(c.info.Verbs, op) {
		return nil, "", problem(http.StatusMethodNotAllowed, methodNotAllowedCode, "The operation is not supported.")
	}
	if c.info.Scope == ClusterScoped {
		return c.resource, "", nil
	}
	namespace, err := c.cfg.Namespace(ctx)
	if err != nil {
		return nil, "", Unauthorized()
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, "", Forbidden()
	}
	return c.resource.Namespace(namespace), namespace, nil
}

// project decodes the internal type and converts it into the approved public shape.
func project[E ResourceHandler[I], I ResourceHandler[E]](obj *unstructured.Unstructured) (E, error) {
	var zero E
	internal, err := projection.JSON[I](obj.Object)
	if err != nil {
		return zero, problem(http.StatusInternalServerError, projectionFailedCode, "The resource cannot be represented.")
	}
	value, err := internal.Convert()
	if err != nil {
		return zero, problem(http.StatusInternalServerError, projectionFailedCode, "The resource cannot be represented.")
	}
	return value, nil
}

// result projects the stored object and returns its resourceVersion through per-call options.
func (c *resourceServer[E, I]) result(ctx context.Context, obj *unstructured.Unstructured) (E, error) {
	value, err := project[E, I](obj)
	if err != nil {
		return value, err
	}
	requestOptions(ctx).ETag = obj.GetResourceVersion()
	return value, nil
}

// intent restricts writes to public identity and supplied spec fields before datastore admission.
// Original JSON preserves omissions and explicit false, zero, empty arrays and optional null values
// that would otherwise be changed by marshaling a typed internal resource.
func (c *resourceServer[E, I]) intent(ctx context.Context, op Operation, in E) (*unstructured.Unstructured, error) {
	supplied := requestOptions(ctx).body
	if supplied == nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, Invalid()
		}
		supplied, err = decodeStrict(bytes.NewReader(data), new(E))
		if err != nil {
			return nil, Invalid()
		}
	}
	for key := range supplied {
		if key != "metadata" && key != "spec" {
			return nil, Invalid(FieldError{Field: "/" + key, Code: readOnlyCode, Message: "The field is read-only."})
		}
	}
	metadata, _ := supplied["metadata"].(map[string]any)
	for key := range metadata {
		if key != "name" && key != "uid" {
			return nil, Invalid(FieldError{Field: "/metadata/" + key, Code: readOnlyCode, Message: "The field is read-only."})
		}
	}
	if spec, found := supplied["spec"]; found {
		if _, ok := spec.(map[string]any); !ok {
			return nil, Invalid(FieldError{Field: "/spec", Code: invalidCode, Message: "Spec must be an object."})
		}
	}
	identity := &unstructured.Unstructured{Object: supplied}
	if err := validName(identity.GetName()); err != nil {
		return nil, err
	}
	if op == OpCreate && identity.GetUID() != "" {
		return nil, Invalid(FieldError{Field: uidPointer, Code: readOnlyCode, Message: "UID is read-only."})
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	converted, err := in.Convert()
	if err != nil {
		return nil, err
	}
	if err = converted.Validate(); err != nil {
		return nil, err
	}
	data, err := projection.JSON[map[string]any](converted)
	if err != nil {
		return nil, err
	}
	internal := &unstructured.Unstructured{Object: data}
	if internal.GetName() != identity.GetName() || internal.GetUID() != identity.GetUID() {
		return nil, fmt.Errorf("resource conversion changed identity")
	}
	// Select only writable fields, even when the internal type contains private metadata or status.
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetName(identity.GetName())
	obj.SetUID(identity.GetUID())
	if spec, found := supplied["spec"]; found {
		obj.Object["spec"] = spec
	}
	obj.SetGroupVersionKind(c.info.GVK)
	return obj, nil
}

// apply submits the manager's complete intent without forcing ownership conflicts.
// Server-side apply releases omitted fields. Fields owned only by this manager are removed or reset to defaults.
func (c *resourceServer[E, I]) apply(
	ctx context.Context,
	r dynamic.ResourceInterface,
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encoding intent: %w", err)
	}
	force := false
	return r.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: c.info.FieldManager, Force: &force, FieldValidation: "Strict",
	})
}

// Create atomically creates only. Duplicate and racing attempts cannot replace a resource.
func (c *resourceServer[E, I]) Create(ctx context.Context, in E) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	obj, err := c.intent(ctx, OpCreate, in)
	if err != nil {
		return zero, err
	}
	r, namespace, err := c.request(ctx, OpCreate)
	if err != nil {
		return zero, err
	}
	obj.SetNamespace(namespace)
	// CRDs reject version-zero updates, so this apply can only create.
	// This relies on the CRD strategy. Resources allowing unconditional updates are unsupported.
	// https://github.com/kubernetes/apiserver/blob/v0.37.1/pkg/registry/generic/registry/store.go#L734-L805
	obj.SetResourceVersion("0")
	created, err := c.apply(ctx, r, obj)
	if duplicateApply(err) {
		return zero, problem(http.StatusConflict, alreadyExistsCode, "The resource already exists.")
	}
	if err != nil {
		return zero, translate[E](err)
	}
	return c.result(ctx, created)
}

// Get projects one resource and returns its resourceVersion.
func (c *resourceServer[E, I]) Get(ctx context.Context, name string) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	if err := validName(name); err != nil {
		return zero, err
	}
	r, _, err := c.request(ctx, OpGet)
	if err != nil {
		return zero, err
	}
	obj, err := r.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return zero, translate[E](err)
	}
	return c.result(ctx, obj)
}

// List returns one datastore snapshot page with a default size of 100 and a maximum of 500.
// Kubernetes owns continuation validity and expiry. Request scope always comes from the host.
func (c *resourceServer[E, I]) List(ctx context.Context, opts ListOptions) (List[E], error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	page := List[E]{Items: []E{}}
	if opts.PageSize == 0 {
		opts.PageSize = 100
	}
	if opts.PageSize < 1 || opts.PageSize > 500 {
		return page, Invalid(FieldError{
			Field: pageSizeParameter, Code: invalidCode, Message: "Page size must be between 1 and 500.",
		})
	}
	if len(opts.PageKey) > maxPageKeyBytes {
		return page, Invalid(FieldError{Field: pageKeyParameter, Code: invalidCode, Message: "The page key is too long."})
	}
	r, _, err := c.request(ctx, OpList)
	if err != nil {
		return page, err
	}
	list, err := r.List(ctx, metav1.ListOptions{Limit: int64(opts.PageSize), Continue: opts.PageKey})
	if opts.PageKey != "" && apierrors.IsBadRequest(err) {
		return page, Invalid(FieldError{Field: pageKeyParameter, Code: invalidCode, Message: "The page key is invalid."})
	}
	if err != nil {
		return page, translate[E](err)
	}
	for i := range list.Items {
		value, projectErr := project[E, I](&list.Items[i])
		if projectErr != nil {
			return page, projectErr
		}
		page.Items = append(page.Items, value)
	}
	page.PageKey = list.GetContinue()
	page.HasMore = page.PageKey != ""
	return page, nil
}

// validName rejects invalid Kubernetes object names before a datastore request is made.
func validName(name string) error {
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return Invalid(FieldError{Field: namePointer, Code: invalidCode, Message: "A valid resource name is required."})
	}
	return nil
}

// expectedVersion requires a concrete resourceVersion, excluding Kubernetes' unconditional zero value.
func expectedVersion(version string) error {
	if version == "" {
		return problem(
			http.StatusPreconditionRequired, preconditionRequiredCode, "A resource version precondition is required.",
		)
	}
	if strings.Trim(version, "0") == "" || strings.ContainsAny(version, "\" ,*\t\r\n") {
		return Invalid(FieldError{Code: invalidPreconditionCode, Message: "Use a nonzero resource version."})
	}
	return nil
}

// Update applies this manager's complete writable intent at the caller's resourceVersion.
// Omitted fields owned only by this manager are removed or reset to defaults.
// Adopting other writers' spec fields is outside the PoC.
// The fetched UID prevents apply from recreating an object deleted after the read.
func (c *resourceServer[E, I]) Update(ctx context.Context, name string, in E) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	if err := validName(name); err != nil {
		return zero, err
	}
	r, namespace, err := c.request(ctx, OpUpdate)
	if err != nil {
		return zero, err
	}
	version := requestOptions(ctx).IfMatch
	if err := expectedVersion(version); err != nil {
		return zero, err
	}
	current, err := r.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return zero, translate[E](err)
	}
	if current.GetResourceVersion() != version {
		return zero, problem(http.StatusPreconditionFailed, preconditionFailedCode, "The resource has changed.")
	}
	obj, err := c.intent(ctx, OpUpdate, in)
	if err != nil {
		return zero, err
	}
	if obj.GetName() != name {
		return zero, Invalid(FieldError{
			Field: namePointer, Code: identityMismatchCode, Message: "The body name must match the request path.",
		})
	}
	if obj.GetUID() != "" && obj.GetUID() != current.GetUID() {
		return zero, Invalid(FieldError{
			Field: uidPointer, Code: identityMismatchCode, Message: "UID does not match the resource.",
		})
	}
	obj.SetNamespace(namespace)
	obj.SetUID(current.GetUID())
	obj.SetResourceVersion(version)
	updated, err := c.apply(ctx, r, obj)
	if resourceVersionConflict(err) {
		return zero, problem(http.StatusPreconditionFailed, preconditionFailedCode, "The resource has changed.")
	}
	if err != nil {
		return zero, translate[E](err)
	}
	return c.result(ctx, updated)
}

// Delete requests deletion at the caller's resourceVersion without rereading or retrying.
// Success acknowledges the delete request. It does not wait for finalizers or dependent resources to disappear.
func (c *resourceServer[E, I]) Delete(ctx context.Context, name string, opts DeleteOptions) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := validName(name); err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	switch opts.PropagationPolicy {
	case "", "background":
	case "foreground":
		policy = metav1.DeletePropagationForeground
	default:
		return Invalid(FieldError{
			Field: propagationPolicyParameter, Code: invalidCode, Message: "Use background or foreground propagation.",
		})
	}
	r, _, err := c.request(ctx, OpDelete)
	if err != nil {
		return err
	}
	version := requestOptions(ctx).IfMatch
	if err := expectedVersion(version); err != nil {
		return err
	}
	err = r.Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{ResourceVersion: &version},
	})
	if resourceVersionConflict(err) {
		return problem(http.StatusPreconditionFailed, preconditionFailedCode, "The resource has changed.")
	}
	if err != nil {
		return translate[E](err)
	}
	return nil
}
