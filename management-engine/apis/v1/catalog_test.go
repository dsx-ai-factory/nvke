// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestControlPlaneClassCatalogView(t *testing.T) {
	past := metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	class := ControlPlaneClass{Spec: ControlPlaneClassSpec{
		ControlPlane: ControlPlaneClassConfig{Replicas: new(int32(3))},
		Lifecycle:    ReleaseLifecycle{Hidden: true, SupportedUntil: &past},
	}}
	classView, err := class.CatalogView()
	require.NoError(t, err)
	assertCatalogJSON(t, classView.Spec.Lifecycle, `{"deprecated":false,"supportedUntil":"2000-01-01T00:00:00Z"}`)
	require.False(t, class.CatalogVisible())
	class.Spec.Lifecycle.Hidden, class.Spec.Lifecycle.Deprecated = false, true
	require.True(t, class.CatalogVisible(), "deprecation/support expiration do not hide")
	*classView.Spec.ControlPlane.Replicas = 5
	require.Equal(t, int32(3), *class.Spec.ControlPlane.Replicas, "projection owns its pointers")
	originalTime := past
	classView.Spec.Lifecycle.SupportedUntil.Time = classView.Spec.Lifecycle.SupportedUntil.Add(time.Hour)
	require.Equal(t, originalTime, *class.Spec.Lifecycle.SupportedUntil, "projection owns its pointers")
	class.Spec.Lifecycle.SupportedUntil = nil
	classView, err = class.CatalogView()
	require.NoError(t, err)
	assertCatalogJSON(t, classView.Spec.Lifecycle, `{"deprecated":true}`)
}

func TestClusterReleaseChannelCatalogView(t *testing.T) {
	past := metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	release := func(name, full string, life ReleaseLifecycle) ClusterRelease {
		value := ClusterRelease{Spec: ClusterReleaseSpec{Lifecycle: life}}
		value.SetName(name)
		value.SetAnnotations(map[string]string{MachineInitKubernetesVersionAnnotation: full})
		return value
	}
	releases := map[string]ClusterRelease{
		"deprecated": release("deprecated", "1.35.9", ReleaseLifecycle{Deprecated: true}),
		"invalid":    release("invalid", "vv1.35.9", ReleaseLifecycle{}),
		"hidden":     release("hidden", "1.35.9", ReleaseLifecycle{Hidden: true}),
		"preview":    release("preview", " v1.35.2-rc.1+vendor.7 ", ReleaseLifecycle{SupportedUntil: &past}),
		"later":      release("later", "1.35.8", ReleaseLifecycle{}),
		"exact":      release("exact", "1.35.2+vendor.7", ReleaseLifecycle{SupportedUntil: &past}),
		"older":      release("older", "1.34.0", ReleaseLifecycle{}),
	}
	channel := ClusterReleaseChannel{Spec: ClusterReleaseChannelSpec{
		Releases: map[string]ClusterReleaseReferences{
			"1.35": {
				{Name: "missing"}, {Name: "deprecated"}, {Name: "invalid"}, {Name: "hidden"},
				{Name: "exact"}, {Name: "later"}, {Name: "preview"},
			},
			"1.35.2": {{Name: "later"}, {Name: "preview"}, {Name: "exact"}},
			"1.35.9": {{Name: "hidden"}},
			"1.34":   {{Name: "older"}},
			"1.34.0": {{Name: "older"}},
		},
		Lifecycle: ReleaseLifecycle{Hidden: true, Deprecated: true, SupportedUntil: &past}}}
	channel.SetName("stable")
	channelView, found, err := channel.CatalogView(releases)
	require.NoError(t, err)
	require.True(t, found)
	// Operator order wins over a newer patch, and support dates come from the same advertised release.
	assertCatalogJSON(t, channelView, `{
		"metadata": {
			"name": "stable"
		},
		"spec": {
			"defaultVersion": "1.35.2",
			"versions": [
				{
					"version": "1.35.2",
					"kubernetesVersion": "1.35.2+vendor.7",
					"supportedUntil": "2000-01-01T00:00:00Z"
				},
				{
					"version": "1.35",
					"kubernetesVersion": "1.35.2+vendor.7",
					"supportedUntil": "2000-01-01T00:00:00Z"
				},
				{
					"version": "1.34",
					"kubernetesVersion": "1.34.0"
				},
				{
					"version": "1.34.0",
					"kubernetesVersion": "1.34.0"
				}
			],
			"lifecycle": {
				"deprecated": true,
				"supportedUntil": "2000-01-01T00:00:00Z"
			}
		}
	}`)
	require.False(t, channel.CatalogVisible())
	channel.Spec.Lifecycle.Hidden = false
	require.True(t, channel.CatalogVisible())
	_, found, err = channel.CatalogView(map[string]ClusterRelease{"hidden": releases["hidden"]})
	require.NoError(t, err)
	require.False(t, found, "a channel with no discoverable releases has no catalog entry")
}

func TestClusterReleaseChannelCatalogBuilds(t *testing.T) {
	versions := []string{"1.35.2+sha.abc", "1.35.2-nvke.7", "1.35.2.10", "1.35.2.2", "1.35.2.9999999999999999999999", "1.35.2_custom"}
	channel := ClusterReleaseChannel{Spec: ClusterReleaseChannelSpec{Releases: map[string]ClusterReleaseReferences{}}}
	for _, selector := range versions {
		channel.Spec.Releases[selector] = ClusterReleaseReferences{{Name: "missing"}, {Name: "preview"}, {Name: "stable"}}
	}
	// An overflowing patch is unusable, while the large fourth component above remains an opaque suffix.
	channel.Spec.Releases["1.35.99999999999999999999"] = ClusterReleaseReferences{{Name: "stable"}}
	channel.Spec.Releases["1.35.3_wrong"] = ClusterReleaseReferences{{Name: "stable"}}
	channel.Spec.Releases["1.35.2_preview"] = ClusterReleaseReferences{{Name: "preview"}}
	releases := map[string]ClusterRelease{
		"stable":  {ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{MachineInitKubernetesVersionAnnotation: "v1.35.2+vendor.7"}}},
		"preview": {ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{MachineInitKubernetesVersionAnnotation: "1.35.2-rc.1"}}},
	}
	view, found, err := channel.CatalogView(releases)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, versions[0], view.Spec.DefaultVersion)
	require.Len(t, view.Spec.Versions, len(versions))
	// Build suffixes are opaque and sort lexically, with mismatched patches and prereleases omitted.
	for index, selector := range versions {
		require.Equal(t, selector, view.Spec.Versions[index].Version)
		require.Equal(t, "1.35.2+vendor.7", view.Spec.Versions[index].KubernetesVersion)
	}
}

func TestClusterReleaseChannelVersionFilter(t *testing.T) {
	const largeBuild = "1.34.4.9999999999999999999999999"
	channel := ClusterReleaseChannel{Spec: ClusterReleaseChannelSpec{Releases: map[string]ClusterReleaseReferences{}}}
	for _, selector := range []string{"1.34", "1.34.4", "1.34.4-custom.1", "1.34.4-custom.2", largeBuild, "1.34.5", "1.35", "1.340"} {
		channel.Spec.Releases[selector] = ClusterReleaseReferences{{Name: "release"}}
	}
	for _, test := range []struct {
		selector string
		want     []string
	}{
		{"1.34", []string{"1.34", "1.34.4", "1.34.4-custom.1", "1.34.4-custom.2", largeBuild, "1.34.5"}},
		{"1.34.4", []string{"1.34.4", "1.34.4-custom.1", "1.34.4-custom.2", largeBuild}},
		{"1.34.4-custom.1", []string{"1.34.4-custom.1"}},
		{largeBuild, []string{largeBuild}},
		{"1.34.40", nil},
		{"1.34.4-custom", nil},
	} {
		t.Run(test.selector, func(t *testing.T) {
			filtered, err := channel.FilterCatalogVersion(test.selector)
			require.NoError(t, err)
			require.ElementsMatch(t, test.want, filtered.ListVersions(), "retain published choices without inventing aliases")
			clear(filtered.Spec.Releases)
			require.Len(t, channel.Spec.Releases, 8, "filtering owns its map")
		})
	}
	for _, selector := range []string{"", "1", "01.34", "v1.34", "1.34.4-", "1.34.4/a", "1.34.4 custom",
		"1.34.9999999999999999999999999", "1.34.4-" + strings.Repeat("a", 122)} {
		_, err := channel.FilterCatalogVersion(selector)
		require.Error(t, err, selector)
	}
}

func TestNodePoolReleaseCatalogView(t *testing.T) {
	past := metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	node := NodePoolRelease{Spec: NodePoolReleaseSpec{
		Profile: "ubuntu-gpu", Tag: "2026.11", MachineImageRef: MachineImageReference{Name: "image"},
		Lifecycle: ReleaseLifecycle{Hidden: true, Deprecated: true, SupportedUntil: &past}}}
	images := map[string]MachineImage{"image": {}}
	nodeView, found, err := node.CatalogView(images)
	require.NoError(t, err)
	require.True(t, found)
	assertCatalogJSON(t, nodeView.Spec.Lifecycle, `{"deprecated":true,"supportedUntil":"2000-01-01T00:00:00Z"}`)
	require.False(t, node.CatalogVisible())
	node.Spec.Lifecycle.Hidden = false
	require.True(t, node.CatalogVisible())
	_, found, err = node.CatalogView(nil)
	require.NoError(t, err)
	require.False(t, found, "unresolved image cannot produce a usable profile")
	node.Spec.Tag = ""
	_, found, err = node.CatalogView(images)
	require.NoError(t, err)
	require.False(t, found, "legacy releases without a complete profile/tag pair are not offerings")
}

func assertCatalogJSON(t *testing.T, value any, expected string) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.JSONEq(t, expected, string(data))
}
