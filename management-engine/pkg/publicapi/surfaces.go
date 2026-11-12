// SPDX-License-Identifier: Apache-2.0

// Package publicapi hosts the tenant and operator APIs and selects their resources.
// Generated registrations bind public types to datastore resources.
// Tenant and Operator choose which registrations each server mounts.
package publicapi

import "github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"

// Tenant exposes create, get, list, update, and delete operations for workload clusters.
func Tenant() []resource.Registration {
	return []resource.Registration{
		NVKECluster(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
	}
}

// Operator exposes create, get, list, update, and delete operations for images, releases, channels, and classes.
func Operator() []resource.Registration {
	return []resource.Registration{
		ClusterRelease(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		MachineImage(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		ControlPlaneClass(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		ClusterReleaseChannel(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		NodePoolRelease(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
	}
}
