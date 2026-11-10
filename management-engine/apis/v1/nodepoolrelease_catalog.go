// SPDX-License-Identifier: Apache-2.0

package v1

import "github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"

// NodeProfileCatalogSpec composes the included release and joined image fields.
// +kubebuilder:object:generate=false
type NodeProfileCatalogSpec struct {
	CatalogNodePoolReleaseSpec `json:",inline"`
	CatalogMachineImageSpec    `json:",inline"`
}

// CatalogView joins only the referenced image, preserving the release's Kubernetes handle.
// Releases without a profile, tag, or referenced image have no catalog entry.
func (in NodePoolRelease) CatalogView(images map[string]MachineImage) (CatalogEntry[NodeProfileCatalogSpec], bool, error) {
	view := CatalogEntry[NodeProfileCatalogSpec]{Metadata: CatalogMetadata{Name: in.Name}}
	if in.Spec.Profile == "" || in.Spec.Tag == "" {
		return view, false, nil
	}
	release, err := projection.JSON[CatalogNodePoolReleaseSpec](in.Spec)
	view.Spec.CatalogNodePoolReleaseSpec = release
	if err != nil {
		return view, false, err
	}
	image, found := images[string(in.Spec.MachineImageRef.Name)]
	if !found {
		return view, false, nil
	}
	view.Spec.CatalogMachineImageSpec, err = projection.JSON[CatalogMachineImageSpec](image.Spec)
	return view, err == nil, err
}

// CatalogVisible reports list discoverability. Named reads can still return hidden offerings.
func (in NodePoolRelease) CatalogVisible() bool { return !in.Spec.Lifecycle.Hidden }
