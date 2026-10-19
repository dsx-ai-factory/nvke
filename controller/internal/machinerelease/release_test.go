// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package machinerelease_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/machinerelease"
)

func TestResolveClusterReleaseIncludesDeprecatedAndReturnsImage(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("ubuntu-24", "amd64", "fde", "24.04", "2026-03-23", "#!ipxe\necho boot\n"),
		clusterRelease("nvke-old", "ubuntu-24", true),
	)

	release, img, err := machinerelease.ResolveClusterRelease(t.Context(), reader, "nvke-old")
	require.NoError(t, err)
	assert.True(t, release.Spec.Lifecycle.Deprecated)
	assert.Equal(t, "#cloud-config\n", release.Spec.MachineInit.UserDataTemplate)
	assert.Equal(t, "#!ipxe\necho boot\n", img.Spec.NICo.IPXEScript)
	assert.Equal(t, "amd64", img.Spec.Architecture)
	assert.Equal(t, "fde", img.Spec.Variant)
	assert.Equal(t, "24.04", img.Spec.OperatingSystem.Version)
	assert.Equal(t, "2026-03-23", img.Spec.BuildDate)

	version, injected, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.NoError(t, err)
	assert.Equal(t, "1.31.12", version)
	assert.False(t, injected)
}

func TestResolveNodePoolReleaseInjected(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("ubuntu-24", "arm64", "", "24.04", "2026-09-21", "#!ipxe\n"),
		nodePoolRelease("nvke-injected", "ubuntu-24", "", true, false, "#cloud-config\nhostname: {{ .Hostname }}\n"),
	)

	release, img, err := machinerelease.ResolveNodePoolRelease(t.Context(), reader, "nvke-injected")
	require.NoError(t, err)
	assert.False(t, release.Spec.Lifecycle.Deprecated)
	assert.Equal(t, "arm64", img.Spec.Architecture)

	version, injected, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.NoError(t, err)
	assert.Empty(t, version)
	assert.True(t, injected)
}

func TestResolveHasNoFallbackBetweenKinds(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		clusterRelease("only-cluster", "img", false),
	)

	_, _, err := machinerelease.ResolveNodePoolRelease(t.Context(), reader, "only-cluster")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `NodePoolRelease "only-cluster" is not installed`)
}

func TestResolveMissingRelease(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveNodePoolRelease(t.Context(), fakeClient(t), "missing")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `NodePoolRelease "missing" is not installed`)
}

func TestResolveEmptyName(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveClusterRelease(t.Context(), fakeClient(t), "")
	require.ErrorContains(t, err, "release name is empty")
}

func TestResolveMissingMachineImage(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveClusterRelease(t.Context(), fakeClient(t, clusterRelease("nvke-old", "gone", false)), "nvke-old")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `MachineImage "gone" is not installed`)
}

func TestParseKubernetesVersionAnnotationsRejectsBadInjectionAnnotation(t *testing.T) {
	t.Parallel()

	release := clusterRelease("nvke-old", "ubuntu-24", false)
	release.Annotations[enginev1.MachineInitKubernetesVersionInjectedAnnotation] = "maybe"

	_, _, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), "maybe")
}

func TestCheckCRDsPassesWhenKindsAreRegistered(t *testing.T) {
	t.Parallel()

	require.NoError(t, machinerelease.CheckCRDs(t.Context(), fakeClient(t)))
}

func TestCheckCRDsFailsWhenKindsAreNotInScheme(t *testing.T) {
	t.Parallel()

	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	err := machinerelease.CheckCRDs(t.Context(), reader)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking MachineImage")
}

// An API server that serves the group without the kind is what a cluster the
// release CRDs were never applied to looks like, and it is the case the
// startup check exists for.
func TestCheckCRDsNamesTheKindThatIsNotInstalled(t *testing.T) {
	t.Parallel()

	notInstalled := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: enginev1.GroupVersion.Group, Kind: "MachineImage"}}

	err := machinerelease.CheckCRDs(t.Context(), failingClient(t, notInstalled))
	require.ErrorContains(t, err, "MachineImage CRD is not installed")
}

func TestSelectUpgradeTargetSkipsDeprecatedAndPrefersNewestBuild(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img-old", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		image("img-new", "amd64", "", "22.04", "2026-01-20", "#!ipxe\n"),
		image("img-retired", "amd64", "", "22.04", "2026-02-01", "#!ipxe\n"),
		image("img-arm", "arm64", "", "22.04", "2026-03-01", "#!ipxe\n"),
		nodePoolRelease("old", "img-old", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("newer", "img-new", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("retired-newest", "img-retired", "1.31.9", false, true, "#cloud-config\n"),
		nodePoolRelease("other-arch", "img-arm", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("other-version", "img-new", "1.30.5", false, false, "#cloud-config\n"),
		nodePoolRelease("injected", "img-old", "", true, false, "#cloud-config\n"),
	)

	name, img, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "newer", name)
	assert.Equal(t, "2026-01-20", img.Spec.BuildDate)
}

func TestSelectUpgradeTargetKeepsTheCurrentOSFamily(t *testing.T) {
	t.Parallel()

	otherFamily := image("img-debian", "amd64", "", "22.04", "2026-02-01", "#!ipxe\n")
	otherFamily.Spec.OperatingSystem.Family = "debian"

	reader := fakeClient(t,
		image("img-ubuntu", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		otherFamily,
		nodePoolRelease("same-family", "img-ubuntu", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("other-family-newer", "img-debian", "1.31.9", false, false, "#cloud-config\n"),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "same-family", name, "the other family's image is newer, but an upgrade must not change distribution")
}

func TestSelectUpgradeTargetAcceptsInjectedRelease(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "fde", "24.04", "2026-09-21", "#!ipxe\n"),
		clusterRelease("injected", "img", false, injectedAnnotations()),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.35.0", image("current", "amd64", "fde", "24.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "injected", name)
}

func TestSelectUpgradeTargetSearchesOnlyTheRolesKind(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		nodePoolRelease("worker-only", "img", "1.31.9", false, false, "#cloud-config\n"),
	)

	_, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.ErrorContains(t, err, "no release found for Kubernetes version 1.31.9")
}

func TestSelectUpgradeTargetSkipsReleasesThatCannotBeResolved(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		nodePoolRelease("good", "img", "1.31.12", false, false, "#cloud-config\n"),
		nodePoolRelease("no-image", "gone", "1.31.12", false, false, "#cloud-config\n"),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.12", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "good", name)

	_, _, err = machinerelease.ResolveNodePoolRelease(t.Context(), reader, "no-image")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
}

func TestSelectUpgradeTargetWithoutCurrentMatchesOnlyBareImages(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		clusterRelease("cp", "img", false),
	)

	_, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.31.12", nil)
	require.ErrorContains(t, err, "no release found")
}

func fakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// failingClient fails every read, like an unreachable API server.
func failingClient(t *testing.T, err error) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return err
		},
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return err
		},
	}).Build()
}

func image(name, arch, variant, ubuntu, buildDate, ipxe string) *enginev1.MachineImage {
	return &enginev1.MachineImage{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: enginev1.MachineImageSpec{
			OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: ubuntu},
			Architecture:    arch,
			Variant:         variant,
			BuildDate:       buildDate,
			NICo:            enginev1.NICoBoot{IPXEScript: ipxe},
		},
	}
}

// clusterRelease builds a ClusterRelease that boots Kubernetes 1.31.12, or
// injects the version when annotations are given.
func clusterRelease(name, imageName string, deprecated bool, annotations ...map[string]string) *enginev1.ClusterRelease {
	release := &enginev1.ClusterRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: versionAnnotations("1.31.12", false),
		},
		Spec: enginev1.ClusterReleaseSpec{
			MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(imageName)},
			MachineInit:     enginev1.MachineInit{UserDataTemplate: "#cloud-config\n"},
			Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
	if len(annotations) > 0 {
		release.Annotations = annotations[0]
	}
	return release
}

func nodePoolRelease(name, imageName, version string, injected, deprecated bool, userData string) *enginev1.NodePoolRelease {
	return &enginev1.NodePoolRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: versionAnnotations(version, injected),
		},
		Spec: enginev1.NodePoolReleaseSpec{
			MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(imageName)},
			MachineInit:     enginev1.MachineInit{UserDataTemplate: userData},
			Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
}

func injectedAnnotations() map[string]string {
	return versionAnnotations("", true)
}

func versionAnnotations(version string, injected bool) map[string]string {
	injectedValue := "false"
	if injected {
		injectedValue = "true"
		version = ""
	}
	return map[string]string{
		enginev1.MachineInitKubernetesVersionAnnotation:         version,
		enginev1.MachineInitKubernetesVersionInjectedAnnotation: injectedValue,
	}
}
