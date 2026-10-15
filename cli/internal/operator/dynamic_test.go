// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// gvrs maps each plural resource to the list kind the fake client needs.
var gvrs = map[schema.GroupVersionResource]string{
	enginev1.SchemeGroupVersion.WithResource("machineimages"):    "MachineImageList",
	enginev1.SchemeGroupVersion.WithResource("clusterreleases"):  "ClusterReleaseList",
	enginev1.SchemeGroupVersion.WithResource("nodepoolreleases"): "NodePoolReleaseList",
}

// newFake returns a Dynamic over a fake cluster holding objs.
func newFake(objs ...runtime.Object) (*Dynamic, *dynamicfake.FakeDynamicClient) {
	fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs, objs...)
	return newDynamicWithClient(fake), fake
}

// stored returns a cluster object as the API server would hold it, with the
// transport fields a portable resource must not expose.
func stored(kind Kind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": enginev1.SchemeGroupVersion.String(),
		"kind":       string(kind),
		"metadata": map[string]any{
			"name":              name,
			"uid":               "0f4e0a4e-1b1b-4c1c-8d8d-000000000001",
			"resourceVersion":   "42",
			"generation":        int64(1),
			"creationTimestamp": time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"internal.nvke.nvidia.com/note":                    "kept",
			},
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": spec,
	}}
	return u
}

func releaseSpec(deprecated bool) map[string]any {
	return map[string]any{
		"machineImageRef": map[string]any{"name": "image-a"},
		"machineInit":     map[string]any{"userDataTemplate": "#cloud-config\n"},
		"lifecycle":       map[string]any{"deprecated": deprecated},
	}
}

func TestDynamicCreateSendsAManifestWithStrictValidation(t *testing.T) {
	t.Parallel()

	c, fake := newFake()
	var gotObject *unstructured.Unstructured
	var gotOptions metav1.CreateOptions
	fake.PrependReactor("create", "clusterreleases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateActionImpl)
		gotObject = create.Object.(*unstructured.Unstructured)
		gotOptions = create.CreateOptions
		return false, nil, nil
	})

	created, err := c.Create(context.Background(), KindClusterRelease, Resource{
		Metadata: Metadata{Name: "rel-1", Annotations: map[string]string{"a": "b"}},
		Spec:     releaseSpec(false),
	})

	require.NoError(t, err)
	require.NotNil(t, gotObject)
	assert.Equal(t, "internal.nvke.nvidia.com/v1", gotObject.GetAPIVersion())
	assert.Equal(t, "ClusterRelease", gotObject.GetKind())
	assert.Equal(t, "rel-1", gotObject.GetName())
	assert.Equal(t, map[string]string{"a": "b"}, gotObject.GetAnnotations())
	assert.Empty(t, gotObject.GetNamespace(), "cluster-scoped: no namespace")
	assert.Equal(t, "Strict", gotOptions.FieldValidation, "an unknown spec field must fail rather than be dropped")
	assert.Equal(t, "rel-1", created.Metadata.Name)
}

func TestDynamicCreateAnnotationsAbsent(t *testing.T) {
	t.Parallel()

	c, fake := newFake()
	var gotObject *unstructured.Unstructured
	fake.PrependReactor("create", "machineimages", func(action clienttesting.Action) (bool, runtime.Object, error) {
		gotObject = action.(clienttesting.CreateActionImpl).Object.(*unstructured.Unstructured)
		return false, nil, nil
	})

	_, err := c.Create(context.Background(), KindMachineImage, Resource{Metadata: Metadata{Name: "img"}, Spec: map[string]any{"architecture": "amd64"}})

	require.NoError(t, err)
	assert.Empty(t, gotObject.GetAnnotations(), "nvkectl adds no annotations of its own")
}

func TestDynamicGetProjectsToPortableShape(t *testing.T) {
	t.Parallel()

	c, _ := newFake(stored(KindClusterRelease, "rel-1", releaseSpec(true)))

	got, err := c.Get(context.Background(), KindClusterRelease, "rel-1")

	require.NoError(t, err)
	assert.Equal(t, Metadata{
		Name:        "rel-1",
		Annotations: map[string]string{"internal.nvke.nvidia.com/note": "kept"},
	}, got.Metadata, "kubectl bookkeeping, uid, resourceVersion and managedFields are not part of the portable form")
	assert.Equal(t, releaseSpec(true), got.Spec)
	assert.WithinDuration(t, time.Now().Add(-3*time.Hour), got.CreatedAt, time.Minute)
}

func TestDynamicProjectionRoundTripsThroughDecode(t *testing.T) {
	t.Parallel()

	c, _ := newFake(stored(KindNodePoolRelease, "rel-1", releaseSpec(false)))
	got, err := c.Get(context.Background(), KindNodePoolRelease, "rel-1")
	require.NoError(t, err)

	doc, err := json.Marshal(got)
	require.NoError(t, err)
	again, err := Decode(KindNodePoolRelease, doc)

	require.NoError(t, err)
	assert.Equal(t, got.Metadata, again.Metadata)
	assert.Equal(t, got.Spec, again.Spec)
}

func TestDynamicListIsOrderedByName(t *testing.T) {
	t.Parallel()

	c, _ := newFake(
		stored(KindMachineImage, "b-image", map[string]any{"architecture": "arm64"}),
		stored(KindMachineImage, "a-image", map[string]any{"architecture": "amd64"}),
	)

	got, err := c.List(context.Background(), KindMachineImage)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a-image", got[0].Metadata.Name)
	assert.Equal(t, "b-image", got[1].Metadata.Name)
}

func TestDynamicListEmpty(t *testing.T) {
	t.Parallel()

	c, _ := newFake()

	got, err := c.List(context.Background(), KindClusterRelease)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDynamicDelete(t *testing.T) {
	t.Parallel()

	c, _ := newFake(stored(KindMachineImage, "img", map[string]any{}))

	require.NoError(t, c.Delete(context.Background(), KindMachineImage, "img"))

	_, err := c.Get(context.Background(), KindMachineImage, "img")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDynamicErrors(t *testing.T) {
	t.Parallel()

	gr := schema.GroupResource{Group: enginev1.GroupName, Resource: "clusterreleases"}
	missingType := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound,
		Message: "the server could not find the requested resource",
	}}

	tests := []struct {
		name    string
		verb    string
		apiErr  error
		call    func(c *Dynamic) error
		wantIs  error
		wantMsg string
	}{
		{
			name:   "get of a missing object",
			verb:   "get",
			apiErr: apierrors.NewNotFound(gr, "rel-1"),
			call:   func(c *Dynamic) error { _, err := c.Get(context.Background(), KindClusterRelease, "rel-1"); return err },
			wantIs: ErrNotFound, wantMsg: `clusterrelease "rel-1"`,
		},
		{
			name:   "delete of a missing object",
			verb:   "delete",
			apiErr: apierrors.NewNotFound(gr, "rel-1"),
			call:   func(c *Dynamic) error { return c.Delete(context.Background(), KindClusterRelease, "rel-1") },
			wantIs: ErrNotFound,
		},
		{
			name:   "list when the resource type is not installed",
			verb:   "list",
			apiErr: missingType,
			call:   func(c *Dynamic) error { _, err := c.List(context.Background(), KindClusterRelease); return err },
			wantIs: ErrCRDsNotInstalled,
		},
		{
			name:   "create when the resource type is not installed",
			verb:   "create",
			apiErr: missingType,
			call: func(c *Dynamic) error {
				_, err := c.Create(context.Background(), KindClusterRelease, Resource{Metadata: Metadata{Name: "rel-1"}, Spec: map[string]any{}})
				return err
			},
			wantIs: ErrCRDsNotInstalled,
		},
		{
			name:   "create of an existing name",
			verb:   "create",
			apiErr: apierrors.NewAlreadyExists(gr, "rel-1"),
			call: func(c *Dynamic) error {
				_, err := c.Create(context.Background(), KindClusterRelease, Resource{Metadata: Metadata{Name: "rel-1"}, Spec: map[string]any{}})
				return err
			},
			wantIs: ErrAlreadyExists, wantMsg: "new name",
		},
		{
			name:   "forbidden",
			verb:   "list",
			apiErr: apierrors.NewForbidden(gr, "", assert.AnError),
			call:   func(c *Dynamic) error { _, err := c.List(context.Background(), KindClusterRelease); return err },
			wantIs: ErrForbidden, wantMsg: "lacks the required permissions",
		},
		{
			name:    "an admission rejection keeps the server's message",
			verb:    "create",
			apiErr:  apierrors.NewInvalid(schema.GroupKind{Group: enginev1.GroupName, Kind: "ClusterRelease"}, "rel-1", nil),
			wantMsg: "ClusterRelease.internal.nvke.nvidia.com",
			call: func(c *Dynamic) error {
				_, err := c.Create(context.Background(), KindClusterRelease, Resource{Metadata: Metadata{Name: "rel-1"}, Spec: map[string]any{}})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, fake := newFake()
			fake.PrependReactor(tt.verb, "clusterreleases", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.apiErr
			})

			err := tt.call(c)

			require.Error(t, err)
			if tt.wantIs != nil {
				assert.ErrorIs(t, err, tt.wantIs)
			}
			if tt.wantMsg != "" {
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestDynamicSetDeprecatedChangesOnlyTheLifecycle(t *testing.T) {
	t.Parallel()

	c, fake := newFake(stored(KindNodePoolRelease, "rel-1", releaseSpec(false)))
	var patch clienttesting.PatchActionImpl
	fake.PrependReactor("patch", "nodepoolreleases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch = action.(clienttesting.PatchActionImpl)
		return false, nil, nil
	})

	got, err := c.SetDeprecated(context.Background(), KindNodePoolRelease, "rel-1", true)

	require.NoError(t, err)
	assert.JSONEq(t, `{"spec":{"lifecycle":{"deprecated":true}}}`, string(patch.Patch), "the patch carries nothing but the flag")
	assert.Equal(t, types.MergePatchType, patch.PatchType)
	assert.Equal(t, "Strict", patch.PatchOptions.FieldValidation)

	assert.Equal(t, releaseSpec(true), got.Spec, "everything else in the spec is untouched")
	assert.Equal(t, "rel-1", got.Metadata.Name)
}

func TestDynamicSetDeprecatedCanClearTheFlag(t *testing.T) {
	t.Parallel()

	c, _ := newFake(stored(KindClusterRelease, "rel-1", releaseSpec(true)))

	got, err := c.SetDeprecated(context.Background(), KindClusterRelease, "rel-1", false)

	require.NoError(t, err)
	assert.Equal(t, releaseSpec(false), got.Spec)
}

func TestDynamicSetDeprecatedMissingRelease(t *testing.T) {
	t.Parallel()

	c, _ := newFake()

	_, err := c.SetDeprecated(context.Background(), KindClusterRelease, "nope", true)

	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDynamicSetDeprecatedMachineImageHasNoLifecycle(t *testing.T) {
	t.Parallel()

	c, fake := newFake(stored(KindMachineImage, "img", map[string]any{}))

	_, err := c.SetDeprecated(context.Background(), KindMachineImage, "img", true)

	assert.ErrorIs(t, err, ErrNoLifecycle)
	for _, a := range fake.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "a machine image must never be patched")
	}
}

func TestNewDynamicNeedsAKubeconfig(t *testing.T) {
	t.Parallel()

	_, err := NewDynamic("")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nvkectl config set-site")
}

func TestNewDynamicUnreadableKubeconfig(t *testing.T) {
	t.Parallel()

	_, err := NewDynamic("/nonexistent/kubeconfig")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading management cluster kubeconfig")
}

func TestKindSupportsDeprecation(t *testing.T) {
	t.Parallel()

	assert.False(t, KindMachineImage.SupportsDeprecation())
	assert.True(t, KindClusterRelease.SupportsDeprecation())
	assert.True(t, KindNodePoolRelease.SupportsDeprecation())
}
