// SPDX-License-Identifier: Apache-2.0

package v1

import "github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"

// CatalogView projects the class using source-field opt-ins.
func (in ControlPlaneClass) CatalogView() (CatalogEntry[CatalogControlPlaneClassSpec], error) {
	return projection.JSON[CatalogEntry[CatalogControlPlaneClassSpec]](in)
}

// CatalogVisible reports list discoverability. Named reads can still return hidden offerings.
func (in ControlPlaneClass) CatalogVisible() bool { return !in.Spec.Lifecycle.Hidden }
