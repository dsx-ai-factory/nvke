// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

const (
	// CatalogVersionPattern is the syntax accepted by the catalog's version query parameter.
	CatalogVersionPattern = `^(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)([.](0|[1-9][0-9]*)([-+._][a-zA-Z0-9][a-zA-Z0-9._+-]*)?)?$`
	// MaxCatalogVersionLength bounds the ASCII version selector, including any build suffix.
	MaxCatalogVersionLength = 128
)

var channelVersionSelectorPattern = regexp.MustCompile(CatalogVersionPattern)

// FilterCatalogVersion retains published choices in a minor or patch family, or one exact suffixed choice.
func (in ClusterReleaseChannel) FilterCatalogVersion(selector string) (ClusterReleaseChannel, error) {
	if len(selector) > MaxCatalogVersionLength || !channelVersionSelectorPattern.MatchString(selector) {
		return ClusterReleaseChannel{}, fmt.Errorf("invalid channel version filter")
	}
	parsed, err := parseChannelVersion(selector)
	if err != nil {
		return ClusterReleaseChannel{}, fmt.Errorf("parse channel version filter: %w", err)
	}
	selected := make(map[string]ClusterReleaseReferences)
	for published, references := range in.Spec.Releases {
		matches := published == selector
		if selector == parsed.String() {
			if len(parsed.Components()) == 2 {
				matches = matches || strings.HasPrefix(published, selector+".")
			} else if suffix, found := strings.CutPrefix(published, selector); found && suffix != "" {
				matches = strings.ContainsRune("-+._", rune(suffix[0]))
			}
		}
		if matches {
			selected[published] = references
		}
	}
	in.Spec.Releases = selected
	return in, nil
}

// ChannelCatalogSpec adds resource-owned derived values to the generated source projection.
// +kubebuilder:object:generate=false
type ChannelCatalogSpec struct {
	CatalogClusterReleaseChannelSpec `json:",inline"`
	DefaultVersion                   string                  `json:"defaultVersion" description:"The newest version this channel offers: the first entry of versions."`
	Versions                         []ChannelCatalogVersion `json:"versions" description:"The versions this channel offers, newest first."`
}

// ChannelCatalogVersion describes the first discoverable release advertised under a published selector.
// +kubebuilder:object:generate=false
type ChannelCatalogVersion struct {
	Version           string       `json:"version" description:"The version to name when a cluster selects this channel."`
	KubernetesVersion string       `json:"kubernetesVersion" description:"The Kubernetes version of the release this version currently resolves to."`
	SupportedUntil    *metav1.Time `json:"supportedUntil,omitempty" description:"When support for that release ends."`
}

// CatalogView preserves the operator's reference preference within each published selector.
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
		for _, reference := range in.Spec.Releases[selector] {
			release, found := releases[string(reference.Name)]
			if !found || !release.DeletionTimestamp.IsZero() || release.Spec.Lifecycle.Hidden || release.Spec.Lifecycle.Deprecated {
				continue
			}
			parsed, err := version.ParseSemantic(release.Spec.Kubernetes.Version)
			if err != nil || parsed.Major() != selectorVersion.Major() || parsed.Minor() != selectorVersion.Minor() {
				continue
			}
			if len(selectorVersion.Components()) == 3 && (parsed.Patch() != selectorVersion.Patch() || parsed.PreRelease() != "") {
				continue
			}
			view.Spec.Versions = append(view.Spec.Versions, ChannelCatalogVersion{
				Version:           selector,
				KubernetesVersion: parsed.String(),
				SupportedUntil:    release.Spec.Lifecycle.SupportedUntil.DeepCopy(),
			})
			break
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
