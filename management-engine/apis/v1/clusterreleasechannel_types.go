// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
)

// ClusterReleaseChannelSpec lists the releases available through a channel.
// +nvke:catalog:view
type ClusterReleaseChannelSpec struct {
	// Releases maps Kubernetes version selectors to named ClusterRelease candidates.
	// A major.minor selector permits patch upgrades within that minor.
	// A major.minor.patch selector fixes the Kubernetes version and may include an opaque suffix.
	// A suffixed selector names an exact published choice. Its spelling does not imply upgrade order.
	// References under any selector are alternatives in operator preference order.
	// The selector bound keeps validation within Kubernetes' CEL cost budget.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=65536
	// +kubebuilder:validation:XValidation:rule="self.all(version, size(version) <= 128 && version.matches('^(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)([.](0|[1-9][0-9]*)([-+._][a-zA-Z0-9][a-zA-Z0-9._+-]*)?)?$'))",message="release keys must be major.minor or major.minor.patch with an optional build suffix and at most 128 characters"
	Releases map[string]ClusterReleaseReferences `json:"releases"`
	// Lifecycle controls catalog discovery and communicates support.
	// +optional
	// +kubebuilder:default={}
	// +nvke:catalog:include
	Lifecycle ReleaseLifecycle `json:"lifecycle"`
}

// ClusterReleaseReferences lists alternatives for one version selector in operator preference order.
// +kubebuilder:validation:MinItems=1
// +listType=map
// +listMapKey=name
type ClusterReleaseReferences []ClusterReleaseReference

// ClusterReleaseReference names a cluster-scoped ClusterRelease.
type ClusterReleaseReference struct {
	Name ResourceName `json:"name"`
	// MinimumUpgradeAge is the minimum time since this release was created before it is eligible for automatic adoption.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="minimumUpgradeAge must be a nonnegative duration"
	MinimumUpgradeAge metav1.Duration `json:"minimumUpgradeAge"`
}

// ClusterReleaseChannel is an operator-managed catalog of control-plane releases.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=clusterreleasechannels,singular=clusterreleasechannel
// +kubebuilder:storageversion
// +nvke:public:name=PublicClusterReleaseChannel
// +nvke:public:path=/v1/cluster-release-channels
// +nvke:public:metadata=name,creationTimestamp,deletionTimestamp,uid
type ClusterReleaseChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterReleaseChannelSpec `json:"spec"`
}

// ClusterReleaseChannelList is a list of release channels.
// +kubebuilder:object:root=true
type ClusterReleaseChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterReleaseChannel `json:"items"`
}

// ListVersions skips unusable selectors so an invalid version cannot block catalog discovery.
// Valid selectors sort by descending Kubernetes version with lexical ties for opaque suffixes.
func (in ClusterReleaseChannel) ListVersions() []string {
	selectors := make([]string, 0, len(in.Spec.Releases))
	versions := make(map[string]*version.Version, len(in.Spec.Releases))
	for selector := range in.Spec.Releases {
		parsed, err := parseChannelVersion(selector)
		if err != nil {
			continue
		}
		selectors = append(selectors, selector)
		versions[selector] = parsed
	}
	slices.SortFunc(selectors, func(left, right string) int {
		leftVersion, rightVersion := versions[left], versions[right]
		if leftVersion.LessThan(rightVersion) {
			return 1
		}
		if rightVersion.LessThan(leftVersion) {
			return -1
		}
		// Prefer the minor selector when it is numerically equal to a .0 patch.
		if difference := len(leftVersion.Components()) - len(rightVersion.Components()); difference != 0 {
			return difference
		}
		return strings.Compare(left, right)
	})
	return selectors
}

func parseChannelVersion(selector string) (*version.Version, error) {
	// A numeric build suffix is opaque too, so only parse the first three components.
	components := strings.SplitN(selector, ".", 4)
	return version.ParseGeneric(strings.Join(components[:min(len(components), 3)], "."))
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterReleaseChannel{}, &ClusterReleaseChannelList{})
		return nil
	})
}
