// SPDX-License-Identifier: Apache-2.0

// Package resource serves stateless CRUDL APIs backed by Kubernetes custom resources.
// ResourceHandler validates and converts resource values. ResourceServer persists them.
// Registrations mount HTTP routes for the resource pairs selected by the host.
// The host supplies datastore access, trusted namespace resolution, authentication,
// authorization, and HTTP limits.
package resource

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Operation identifies a public operation, independently of datastore verbs.
type Operation string

// Supported public operations.
const (
	OpCreate Operation = "create"
	OpGet    Operation = "get"
	OpList   Operation = "list"
	OpUpdate Operation = "update"
	OpDelete Operation = "delete"
)

// Scope specifies the datastore scope of a resource.
type Scope string

// Supported resource scopes.
const (
	Namespaced    Scope = "Namespaced"
	ClusterScoped Scope = "Cluster"
)

// ResourceHandler validates and converts either side of an external/internal resource pair.
// Paired type parameters use E for external resources and I for internal resources.
// T is the destination type, so the same contract supports conversion in either direction.
type ResourceHandler[T any] interface {
	// Validate contains only public-specific rules. Datastore admission owns schema validation.
	Validate() error
	// Convert selects the destination shape and preserves identity and writable spec field names, structure and values.
	// Renaming or transforming writable fields is unsupported.
	Convert() (T, error)
}

// ResourceServer provides HTTP-independent CRUDL over external resources E.
// New pairs E with its internal datastore type I through ResourceHandler validation and conversion.
// Create, Get and Update project stored I into E and set RequestOptions.ETag when attached with WithOptions.
// Direct writes use E's JSON field presence. Use optional pointers and omit zero read-only fields.
type ResourceServer[E any] interface {
	// Create validates and applies writable intent for a new resource. An existing name is rejected.
	// Datastore admission supplies defaults, and the configured field manager owns the supplied fields.
	Create(context.Context, E) (E, error)
	// Update requires RequestOptions.IfMatch from a prior ETag, attached with WithOptions.
	// It applies the manager's complete writable intent without forcing ownership conflicts.
	// Omitted fields owned only by this manager are removed or reset to datastore defaults.
	Update(context.Context, string, E) (E, error)
	// Get returns one resource projected into E within the host-resolved scope.
	Get(context.Context, string) (E, error)
	// List projects one datastore snapshot page into E, with a default size of 100 and a maximum of 500.
	// Pass PageKey unchanged to continue. The datastore controls continuation validity and expiry.
	List(context.Context, ListOptions) (List[E], error)
	// Delete requires RequestOptions.IfMatch from a prior ETag, attached with WithOptions.
	// Success acknowledges deletion without waiting for finalizers or dependent resources to disappear.
	Delete(context.Context, string, DeleteOptions) error
}

// ResourceInfo registers datastore identity and enabled operations separately from resource values.
// Empty Verbs enables no operations. An empty FieldManager uses the shared "nvke-resource" manager.
type ResourceInfo struct {
	GVK          schema.GroupVersionKind
	Resource     string
	Scope        Scope
	Verbs        []Operation
	HasStatus    bool
	FieldManager string
}

// ClientConfig supplies datastore access and trusted request scope.
// The engine does not implement caller authentication or authorization.
type ClientConfig struct {
	Datastore *rest.Config
	Surface   string
	// Namespace is required only for namespaced resources and resolves scope once per operation.
	// The trusted host supplies it. The namespace must not come from untrusted resource input.
	Namespace func(context.Context) (string, error)
}

// RequestOptions carries concurrency information for one operation.
// IfMatch is the required resourceVersion. Create, Get and Update populate ETag on success.
// Allocate a fresh value for each call and do not share it across goroutines.
type RequestOptions struct {
	IfMatch string
	ETag    string
	body    map[string]any
}

type optionsKey struct{}

// WithOptions attaches per-call concurrency options to a context without changing CRUDL signatures.
func WithOptions(ctx context.Context, opts *RequestOptions) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

// requestOptions returns this call's options, or empty options when none were supplied.
func requestOptions(ctx context.Context) *RequestOptions {
	if opts, ok := ctx.Value(optionsKey{}).(*RequestOptions); ok && opts != nil {
		return opts
	}
	return &RequestOptions{}
}

// List contains a bounded datastore snapshot page.
type List[E any] struct {
	Items   []E    `json:"items"`
	PageKey string `json:"pageKey"`
	HasMore bool   `json:"hasMore"`
}

// ListOptions are the only public list controls.
type ListOptions struct {
	PageSize int
	PageKey  string
}

// DeleteOptions are the only public delete controls.
type DeleteOptions struct{ PropagationPolicy string }

// ServerOptions configures WithLimits. Zero values use its default body size and deadline.
type ServerOptions struct {
	MaxBodyBytes   int64
	RequestTimeout time.Duration
}
