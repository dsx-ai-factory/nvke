// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

// namedView supplies stable, unique names for ordering and continuation.
type namedView interface{ GetName() string }

// visibleSource controls LIST discovery. Named GET bypasses this predicate.
type visibleSource interface{ CatalogVisible() bool }

// versionFilteredSource validates selectors on its zero value before any datastore read.
type versionFilteredSource[S any] interface {
	FilterCatalogVersion(string) (S, error)
}

// maxSourceItems bounds each complete primary or dependency scan before public filtering.
const maxSourceItems = 10000

type directView[E any] interface {
	visibleSource
	CatalogView() (E, error)
}
type dependentView[D, E any] interface {
	visibleSource
	// CatalogView returns false when dependencies cannot produce an offering, including named reads.
	CatalogView(D) (E, bool, error)
}

// CatalogDependencyLoader loads the dependencies shared by one nonempty catalog batch.
type CatalogDependencyLoader[D any] = func(context.Context) (D, error)

// CatalogDependencyPreparation prepares a dependency loader when the registration is mounted.
type CatalogDependencyPreparation[D any] = func(ClientConfig) (CatalogDependencyLoader[D], error)

// catalogProjector handles a batch so dependent views can share a single dependency load.
type catalogProjector[S, E any] = func(context.Context, []S) ([]E, error)

// NewReadRegistration mounts read-only views produced by the source's CatalogView method.
// Selectable fields are explicit public JSON paths supporting equality filters.
// Datastore access and discovery begin at Mount. The source requires no write conversion or validation.
// Views omit ETags. Lists filter before paging in name order, and public result changes expire continuations.
// Named reads include hidden sources.
func NewReadRegistration[S directView[E], E namedView](path string, info ResourceInfo, fields ...string) Registration {
	return readRegistration(path, info, fields, func(_ ClientConfig) (catalogProjector[S, E], error) {
		return directProjector[S, E], nil
	})
}

// NewDependentReadRegistration mounts read-only views with a typed dependency loader.
// Preparation runs once at Mount. Its loader runs once per nonempty batch. Source methods
// own eligibility and joins. Load failures discard the entire result.
// Responses and pagination follow NewReadRegistration's contract.
func NewDependentReadRegistration[S dependentView[D, E], D any, E namedView](
	path string,
	info ResourceInfo,
	prepareDependencies CatalogDependencyPreparation[D],
	fields ...string,
) Registration {
	return readRegistration(path, info, fields, func(cfg ClientConfig) (catalogProjector[S, E], error) {
		if prepareDependencies == nil {
			return nil, fmt.Errorf("a dependency loader is required")
		}
		loadDependencies, err := prepareDependencies(cfg)
		if err != nil {
			return nil, err
		}
		if loadDependencies == nil {
			return nil, fmt.Errorf("a dependency loader is required")
		}
		return dependentProjector[S, D, E](loadDependencies), nil
	})
}

// NewSourceLister constructs a typed, complete Kubernetes list loader for dependencies.
// It checks list capability at construction, resolves trusted scope once per call and retains
// hidden backing resources. A read registration's loader can assemble these values into its typed dependencies.
// Collections exceeding 10,000 items fail without returning partial values.
func NewSourceLister[S any](cfg ClientConfig, info ResourceInfo) (func(context.Context) ([]S, error), error) {
	info.Verbs = []Operation{OpList}
	store, err := newDatastore(cfg, info)
	if err != nil {
		return nil, err
	}
	source := &datastoreSource[S]{store}
	return func(ctx context.Context) ([]S, error) {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		items, _, err := source.List(ctx)
		return items, err
	}, nil
}

func readRegistration[S visibleSource, E namedView](
	path string,
	info ResourceInfo,
	fields []string,
	prepareProjector func(ClientConfig) (catalogProjector[S, E], error),
) Registration {
	info.Verbs = slices.Clone(info.Verbs)
	fields = slices.Clone(fields)
	return Registration{Mount: func(mux *http.ServeMux, cfg ClientConfig) error {
		for _, op := range info.Verbs {
			if op != OpGet && op != OpList {
				return fmt.Errorf("read registrations support only get and list")
			}
		}
		if err := validateFields[E](fields); err != nil {
			return err
		}
		projectCatalog, err := prepareProjector(cfg)
		if err != nil {
			return err
		}
		store, err := newDatastore(cfg, info)
		if err != nil {
			return err
		}
		reader := &projectedReader[S, E]{source: &datastoreSource[S]{store}, project: projectCatalog, fields: fields,
			identity: store.cfg.Datastore.Host + "/" + cfg.Surface + "/" + path + "/" +
				info.GVK.String() + "/" + info.Resource + "/" + string(info.Scope)}
		Register(mux, path, reader, nil, fields, info.Verbs...)
		return nil
	}}
}

// readSource returns complete source lists and their trusted namespace for continuation binding.
type readSource[S any] interface {
	Get(context.Context, string) (S, error)
	List(context.Context) ([]S, string, error)
}

type datastoreSource[S any] struct{ *datastore }

// Get retains internal fields for projection. Its result is not a public response.
func (s *datastoreSource[S]) Get(ctx context.Context, name string) (S, error) {
	var zero S
	r, _, err := s.request(ctx, OpGet)
	if err != nil {
		return zero, err
	}
	obj, err := r.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return zero, err
	}
	value, err := projection.JSON[S](obj.Object)
	if err != nil {
		return zero, projectionFailure(err)
	}
	return value, nil
}

func directProjector[S directView[E], E any](_ context.Context, sources []S) ([]E, error) {
	return projectViews(sources, func(source S) (E, bool, error) {
		view, err := source.CatalogView()
		return view, err == nil, err
	})
}

// List exhausts Kubernetes snapshot pages under one scope resolution and request deadline.
func (s *datastoreSource[S]) List(ctx context.Context) ([]S, string, error) {
	r, namespace, err := s.request(ctx, OpList)
	if err != nil {
		return nil, "", err
	}
	items := []S{}
	pageKey := ""
	for {
		page, err := r.List(ctx, metav1.ListOptions{Limit: 500, Continue: pageKey})
		if err != nil {
			return nil, "", err
		}
		if len(items)+len(page.Items) > maxSourceItems {
			return nil, "", problem(
				http.StatusServiceUnavailable, datastoreUnavailableCode, "The resource collection exceeds the read limit.",
			)
		}
		for _, obj := range page.Items {
			value, err := projection.JSON[S](obj.Object)
			if err != nil {
				return nil, "", projectionFailure(err)
			}
			items = append(items, value)
		}
		pageKey = page.GetContinue()
		if pageKey == "" {
			return items, namespace, nil
		}
	}
}

func dependentProjector[S dependentView[D, E], D, E any](
	loadDependencies CatalogDependencyLoader[D],
) catalogProjector[S, E] {
	return func(ctx context.Context, sources []S) ([]E, error) {
		if len(sources) == 0 {
			// An empty visible collection must not depend on unrelated backing-resource availability.
			return []E{}, nil
		}
		dependencies, err := loadDependencies(ctx)
		if err != nil {
			return nil, listFailure[E](err)
		}
		return projectViews(sources, func(source S) (E, bool, error) { return source.CatalogView(dependencies) })
	}
}

// projectViews discards partial results if any projection fails.
func projectViews[S, E any](sources []S, view func(S) (E, bool, error)) ([]E, error) {
	result := make([]E, 0, len(sources))
	for _, source := range sources {
		value, include, err := view(source)
		if err != nil {
			return nil, projectionFailure(err)
		}
		if include {
			result = append(result, value)
		}
	}
	return result, nil
}

type projectedReader[S visibleSource, E namedView] struct {
	source   readSource[S]
	project  catalogProjector[S, E]
	fields   []string
	identity string
}

// WithVersionFilter isolates a source-owned filter and its continuation identity to one request.
func (r *projectedReader[S, E]) WithVersionFilter(selector string) (ResourceReader[E], error) {
	var source S
	filter, supported := any(source).(versionFilteredSource[S])
	if !supported {
		return nil, Invalid(FieldError{
			Field: versionParameter, Code: invalidQueryCode, Message: "Version filtering is not supported.",
		})
	}
	if _, err := filter.FilterCatalogVersion(selector); err != nil {
		return nil, Invalid(FieldError{Field: versionParameter, Code: invalidCode,
			Message: "Use a major.minor or major.minor.patch selector with an optional build suffix of at most 128 characters."})
	}
	filtered := *r
	filtered.identity += "/version=" + selector
	filtered.project = func(ctx context.Context, sources []S) ([]E, error) {
		selected := make([]S, len(sources))
		for index, source := range sources {
			value, err := any(source).(versionFilteredSource[S]).FilterCatalogVersion(selector)
			if err != nil {
				return nil, projectionFailure(err)
			}
			selected[index] = value
		}
		return r.project(ctx, selected)
	}
	return &filtered, nil
}

// Get bypasses LIST visibility for callers that know a name. Projection eligibility still applies.
func (r *projectedReader[S, E]) Get(ctx context.Context, name string) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	if err := validName(name); err != nil {
		return zero, err
	}
	source, err := r.source.Get(ctx, name)
	if err != nil {
		return zero, readFailure[E](err)
	}
	views, err := r.project(ctx, []S{source})
	if err != nil {
		return zero, err
	}
	if len(views) == 0 {
		return zero, problem(http.StatusNotFound, notFoundCode, "The resource was not found.")
	}
	return views[0], nil
}

// List filters and sorts complete visible projections so excluded sources cannot consume page slots.
// Continuations expire when the projected result changes. Independent resource reads are not a transactional snapshot.
func (r *projectedReader[S, E]) List(ctx context.Context, opts ListOptions) (List[E], error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero List[E]
	selector, err := listSelector(&opts, r.fields)
	if err != nil {
		return zero, err
	}
	sources, namespace, err := r.source.List(ctx)
	if err != nil {
		return zero, listFailure[E](err)
	}
	visible := make([]S, 0, len(sources))
	for _, source := range sources {
		if source.CatalogVisible() {
			visible = append(visible, source)
		}
	}
	views, err := r.project(ctx, visible)
	if err != nil {
		return zero, err
	}
	return viewPage(views, opts, selector, r.identity+"/"+namespace)
}
