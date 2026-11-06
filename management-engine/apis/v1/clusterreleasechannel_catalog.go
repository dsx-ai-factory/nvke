// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

// ChannelCatalogSpec adds resource-owned derived values to the generated source projection.
// +kubebuilder:object:generate=false
type ChannelCatalogSpec struct {
	CatalogClusterReleaseChannelSpec `json:",inline"`
	DefaultVersion                   string                  `json:"defaultVersion"`
	Versions                         []ChannelCatalogVersion `json:"versions"`
}

// ChannelCatalogVersion is a published Kubernetes version selector and its resolved version.
// +kubebuilder:object:generate=false
type ChannelCatalogVersion struct {
	Version           string       `json:"version"`
	KubernetesVersion string       `json:"kubernetesVersion"`
	SupportedUntil    *metav1.Time `json:"supportedUntil,omitempty"`
}

// CatalogView selects the newest visible, nondeprecated matching release per selector.
// An empty channel has no catalog entry. ListVersions determines the default and display order.
func (in ClusterReleaseChannel) CatalogView(releases map[string]ClusterRelease) (CatalogEntry[ChannelCatalogSpec], bool, error) {
	view := CatalogEntry[ChannelCatalogSpec]{Metadata: CatalogMetadata{Name: in.Name}}
	projected, err := projection.JSON[CatalogClusterReleaseChannelSpec](in.Spec)
	view.Spec.CatalogClusterReleaseChannelSpec = projected
	if err != nil {
		return view, false, err
	}
	for _, selector := range in.ListVersions() {
		selectorVersion, err := parseChannelVersion(selector)
		if err != nil {
			return view, false, err
		}
		var latestVersion *version.Version
		var latestRelease ClusterRelease
		for _, reference := range in.Spec.Releases[selector] {
			release, found := releases[string(reference.Name)]
			if !found || !release.DeletionTimestamp.IsZero() || release.Spec.Lifecycle.Hidden || release.Spec.Lifecycle.Deprecated {
				continue
			}
			parsed, err := version.ParseSemantic(release.Annotations[MachineInitKubernetesVersionAnnotation])
			if err != nil || parsed.Major() != selectorVersion.Major() || parsed.Minor() != selectorVersion.Minor() {
				continue
			}
			if len(selectorVersion.Components()) == 3 && (parsed.Patch() != selectorVersion.Patch() || parsed.PreRelease() != "") {
				continue
			}
			if latestVersion == nil || parsed.GreaterThan(latestVersion) {
				latestVersion, latestRelease = parsed, release
			}
		}
		if latestVersion != nil {
			view.Spec.Versions = append(view.Spec.Versions, ChannelCatalogVersion{
				Version:           selector,
				KubernetesVersion: latestVersion.String(),
				SupportedUntil:    latestRelease.Spec.Lifecycle.SupportedUntil.DeepCopy(),
			})
		}
	}
	if len(view.Spec.Versions) == 0 {
		return view, false, nil
	}
	view.Spec.DefaultVersion = view.Spec.Versions[0].Version
	return view, true, nil
}

// CatalogVisible reports list discoverability. Named reads can still return hidden offerings.
func (in ClusterReleaseChannel) CatalogVisible() bool { return !in.Spec.Lifecycle.Hidden }
