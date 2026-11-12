// SPDX-License-Identifier: Apache-2.0

package v1

// CatalogEntry shares the catalog envelope without exposing Kubernetes metadata.
// +kubebuilder:object:generate=false
type CatalogEntry[Spec any] struct {
	Metadata CatalogMetadata `json:"metadata"`
	Spec     Spec            `json:"spec"`
}

// CatalogMetadata identifies the Kubernetes handle, including for a node profile.
// +kubebuilder:object:generate=false
type CatalogMetadata struct {
	Name string `json:"name"`
}

// GetName returns the stable public handle used for catalog ordering and continuation.
func (in CatalogEntry[Spec]) GetName() string { return in.Metadata.Name }
