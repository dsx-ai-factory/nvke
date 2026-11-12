// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// sourceStub can return partial values alongside errors to verify that readers discard them.
type sourceStub[S any] struct {
	items     []S
	named     S
	err       error
	requested string
	namespace string
}

func (s *sourceStub[S]) Get(_ context.Context, name string) (S, error) {
	s.requested = name
	return s.named, s.err
}

func (s *sourceStub[S]) List(context.Context) ([]S, string, error) {
	namespace := s.namespace
	if namespace == "" {
		namespace = "org-a"
	}
	return s.items, namespace, s.err
}

func TestDirectCatalogReads(t *testing.T) {
	deprecated := v1.ControlPlaneClass{
		Spec: v1.ControlPlaneClassSpec{
			ControlPlane: v1.ControlPlaneClassConfig{Replicas: new(int32(3))}, Lifecycle: v1.ReleaseLifecycle{Deprecated: true},
		},
		Name: "deprecated",
	}
	hidden := deprecated
	hidden.Name, hidden.Spec.Lifecycle.Hidden = "hidden", true
	source := &sourceStub[v1.ControlPlaneClass]{items: []v1.ControlPlaneClass{hidden, deprecated}, named: hidden}
	reader := directReader(source)
	page, err := reader.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "deprecated", page.Items[0].Metadata.Name)
	require.True(t, page.Items[0].Spec.Lifecycle.Deprecated)
	view, err := reader.Get(t.Context(), "hidden")
	require.NoError(t, err)
	require.Equal(t, "hidden", source.requested)
	require.Equal(t, "hidden", view.Metadata.Name)
}

func TestDependentCatalogReads(t *testing.T) {
	node := func(name, profile, tag, image string) v1.NodePoolRelease {
		return v1.NodePoolRelease{
			Spec: v1.NodePoolReleaseSpec{
				Profile: v1.ResourceName(profile), Tag: tag,
				MachineImageRef: v1.MachineImageReference{Name: v1.ResourceName(image)},
			},
			Name: name,
		}
	}
	deprecated := node("older", "gpu", "2026.10", "image")
	deprecated.Spec.Lifecycle.Deprecated = true
	hidden := node("hidden", "gpu", "2026.11", "image")
	hidden.Spec.Lifecycle.Hidden = true
	source := &sourceStub[v1.NodePoolRelease]{items: []v1.NodePoolRelease{
		node("legacy", "gpu", "", "image"), node("unresolved", "gpu", "2026.12", "missing"),
		node("latest", "gpu", "2026.11", "image"), hidden, deprecated,
	}, named: hidden}
	var loads int
	project := dependentProjector[v1.NodePoolRelease](func(context.Context) (map[string]v1.MachineImage, error) {
		loads++
		return map[string]v1.MachineImage{"image": {Spec: v1.MachineImageSpec{
			OperatingSystem: v1.OperatingSystem{Family: "ubuntu", Version: "24.04"}, Architecture: "amd64",
		}}}, nil
	})
	reader := &projectedReader[v1.NodePoolRelease, v1.CatalogEntry[v1.NodeProfileCatalogSpec]]{
		source: source, project: project,
	}
	page, err := reader.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, loads, "load once per visible batch")
	require.Len(t, page.Items, 2, "declined offerings are removed")
	require.Equal(t, []string{"latest", "older"},
		[]string{page.Items[0].Metadata.Name, page.Items[1].Metadata.Name}, "name order")
	require.True(t, page.Items[1].Spec.Lifecycle.Deprecated)
	view, err := reader.Get(t.Context(), "hidden")
	require.NoError(t, err)
	require.Equal(t, "hidden", source.requested)
	require.Equal(t, "hidden", view.Metadata.Name)
	require.Equal(t, 2, loads)
	source.named = source.items[0]
	_, err = reader.Get(t.Context(), "legacy")
	statusCode(t, err, 404)
	require.Equal(t, 3, loads)
	for _, items := range [][]v1.NodePoolRelease{nil, {hidden}} {
		source.items = items
		page, err = reader.List(t.Context(), ListOptions{})
		require.NoError(t, err)
		require.Empty(t, page.Items)
		require.Equal(t, 3, loads, "no visible offerings need no dependencies")
	}
}

func TestCatalogReadErrors(t *testing.T) {
	t.Run("selected field", func(t *testing.T) {
		page, err := viewPage([]v1.CatalogEntry[map[string]any]{{Spec: map[string]any{"profile": 42}}},
			ListOptions{PageSize: 1}, fields.OneTermEqualSelector("spec.profile", ""), "catalog")
		statusCode(t, err, 500)
		require.Empty(t, page.Items)
		require.ErrorContains(t, errors.Unwrap(err), "spec.profile")
	})
	failure := errors.New("read failed")
	for _, phase := range []string{"source", "dependencies"} {
		t.Run(phase, func(t *testing.T) {
			source := &sourceStub[v1.NodePoolRelease]{items: []v1.NodePoolRelease{{}}}
			if phase == "source" {
				source.err = failure
			}
			loads := 0
			project := dependentProjector[v1.NodePoolRelease](func(context.Context) (map[string]v1.MachineImage, error) {
				loads++
				return nil, failure
			})
			reader := &projectedReader[v1.NodePoolRelease, v1.CatalogEntry[v1.NodeProfileCatalogSpec]]{
				source: source, project: project,
			}
			page, err := reader.List(t.Context(), ListOptions{})
			require.ErrorIs(t, err, failure)
			require.Nil(t, page.Items)
			view, err := reader.Get(t.Context(), "example")
			require.ErrorIs(t, err, failure)
			require.Empty(t, view)
			if phase == "source" {
				require.Zero(t, loads, "source errors must stop dependency loading")
			} else {
				require.Equal(t, 2, loads)
			}
		})
	}

	t.Run("projection", func(t *testing.T) {
		invalidTime := metav1.NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
		invalid := v1.ControlPlaneClass{Spec: v1.ControlPlaneClassSpec{
			Lifecycle: v1.ReleaseLifecycle{SupportedUntil: &invalidTime},
		}}
		reader := directReader(&sourceStub[v1.ControlPlaneClass]{items: []v1.ControlPlaneClass{{}, invalid}, named: invalid})
		page, err := reader.List(t.Context(), ListOptions{})
		require.Error(t, err)
		require.Nil(t, page.Items, "projection errors discard earlier successful entries")
		view, err := reader.Get(t.Context(), "invalid")
		require.Error(t, err)
		require.Empty(t, view)
	})
	for _, failure := range []error{
		apierrors.NewNotFound(schema.GroupResource{Resource: "private-resources"}, "private-name"),
		apierrors.NewInvalid(schema.GroupKind{Kind: "Private"}, "private-name",
			field.ErrorList{field.Invalid(field.NewPath("spec", "privateField"), "private-value", "private-message")}),
	} {
		reader := &projectedReader[v1.NodePoolRelease, v1.CatalogEntry[v1.NodeProfileCatalogSpec]]{
			source: &sourceStub[v1.NodePoolRelease]{},
			project: dependentProjector[v1.NodePoolRelease](func(context.Context) (map[string]v1.MachineImage, error) {
				return nil, failure
			}),
		}
		_, err := reader.Get(t.Context(), "example")
		require.ErrorIs(t, err, failure)
		if apierrors.IsNotFound(failure) {
			statusCode(t, err, 503)
		}
		data, marshalErr := json.Marshal(err)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(data), "private", "dependency diagnostics use the public view, not the backing type")
	}
}

func directReader(
	source readSource[v1.ControlPlaneClass],
) *projectedReader[v1.ControlPlaneClass, v1.CatalogEntry[v1.CatalogControlPlaneClassSpec]] {
	return &projectedReader[v1.ControlPlaneClass, v1.CatalogEntry[v1.CatalogControlPlaneClassSpec]]{
		source:  source,
		project: directProjector[v1.ControlPlaneClass, v1.CatalogEntry[v1.CatalogControlPlaneClassSpec]],
	}
}

func TestProjectedContinuation(t *testing.T) {
	source := &sourceStub[v1.NodePoolRelease]{}
	for _, name := range []string{"z", "a", "hidden", "declined", "other"} {
		source.items = append(source.items, v1.NodePoolRelease{Name: name, Spec: v1.NodePoolReleaseSpec{
			Profile: "gpu", Tag: "current", MachineImageRef: v1.MachineImageReference{Name: "image"},
		}})
	}
	source.items[2].Spec.Lifecycle.Hidden = true
	source.items[3].Spec.Tag = ""
	source.items[4].Spec.Profile = "cpu"
	reader := &projectedReader[v1.NodePoolRelease, v1.CatalogEntry[v1.NodeProfileCatalogSpec]]{
		source: source, fields: []string{"spec.profile", "spec.tag"}, identity: "node-profiles",
		project: dependentProjector[v1.NodePoolRelease](func(context.Context) (map[string]v1.MachineImage, error) {
			return map[string]v1.MachineImage{"image": {}}, nil
		}),
	}
	opts := ListOptions{PageSize: 1, FieldSelector: "spec.profile=gpu,spec.tag=current"}
	first, err := reader.List(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, "a", first.Items[0].Metadata.Name)
	require.True(t, first.HasMore, "hide, omit and filter before pagination")
	opts.PageKey, opts.PageSize, opts.FieldSelector = first.PageKey, 2, "spec.tag=current,spec.profile=gpu"
	next, err := reader.List(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, next.Items, 1)
	require.Equal(t, "z", next.Items[0].Metadata.Name)
	require.False(t, next.HasMore)
	for _, change := range []string{"selector", "identity", "namespace", "public result", "malformed"} {
		t.Run(change, func(t *testing.T) {
			query := opts
			copy := *reader
			status := 400
			switch change {
			case "selector":
				query.FieldSelector = "spec.profile=cpu"
			case "identity":
				copy.identity = "another-surface"
			case "namespace":
				copy.source = &sourceStub[v1.NodePoolRelease]{items: source.items, namespace: "org-b"}
			case "public result":
				items := slices.Clone(source.items)
				items[0].Spec.Lifecycle.Deprecated = true
				copy.source = &sourceStub[v1.NodePoolRelease]{items: items}
				status = 410
			case "malformed":
				query.PageKey = "not-a-cursor"
			}
			page, err := copy.List(t.Context(), query)
			statusCode(t, err, status)
			require.Empty(t, page.Items)
		})
	}
}

func TestSourceListerCompleteness(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		calls, scopes := 0, 0
		cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, "500", r.URL.Query().Get("limit"))
			if calls == 2 {
				require.Equal(t, "backend-token", r.URL.Query().Get("continue"))
				if failSecond {
					return jsonResponse(r, 403, &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonForbidden, Code: 403})
				}
			}
			next := ""
			if calls == 1 {
				next = "backend-token"
			}
			return jsonResponse(r, 200, map[string]any{"apiVersion": "example.test/v1", "kind": "WidgetList",
				"metadata": map[string]any{"continue": next}, "items": []any{map[string]any{
					"metadata": map[string]any{"name": "backing"}, "spec": map[string]any{"lifecycle": map[string]any{"hidden": true}},
				}}})
		}))
		cfg.Namespace = func(context.Context) (string, error) { scopes++; return "org-a", nil }
		list, err := NewSourceLister[v1.ControlPlaneClass](cfg, widgetInfo())
		require.NoError(t, err)
		items, err := list(t.Context())
		require.Equal(t, 2, calls)
		require.Equal(t, 1, scopes, "one trusted scope for the whole enumeration")
		if failSecond {
			require.Error(t, err)
			require.Nil(t, items, "a later failure must discard earlier pages")
		} else {
			require.NoError(t, err)
			require.Len(t, items, 2)
			require.True(t, items[0].Spec.Lifecycle.Hidden, "dependency lists retain hidden backing resources")
		}
	}
	t.Run("total limit", func(t *testing.T) {
		items := make([]map[string]any, maxSourceItems, maxSourceItems+1)
		for i := range items {
			items[i] = map[string]any{"metadata": map[string]any{"name": "backing"}}
		}
		cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			start := 0
			if continuation := r.URL.Query().Get("continue"); continuation != "" {
				var err error
				start, err = strconv.Atoi(continuation)
				require.NoError(t, err)
			}
			end := min(start+500, len(items))
			next := ""
			if end < len(items) {
				next = strconv.Itoa(end)
			}
			return jsonResponse(r, 200, map[string]any{"apiVersion": "example.test/v1", "kind": "WidgetList",
				"metadata": map[string]any{"continue": next}, "items": items[start:end]})
		}))
		list, err := NewSourceLister[v1.ControlPlaneClass](cfg, widgetInfo())
		require.NoError(t, err)
		values, err := list(t.Context())
		require.NoError(t, err)
		require.Len(t, values, maxSourceItems)
		items = append(items, items[0])
		values, err = list(t.Context())
		statusCode(t, err, 503)
		require.Nil(t, values, "overflow must not return a truncated catalog")
	})
}
