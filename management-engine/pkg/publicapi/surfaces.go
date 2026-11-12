// SPDX-License-Identifier: Apache-2.0

// Package publicapi hosts the tenant and operator APIs and selects their resources.
// Generated registrations bind public types to datastore resources.
// Tenant and Operator choose which registrations each server mounts.
package publicapi

import (
	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// Tenant exposes workload cluster operations and read-only catalog choices.
func Tenant() []resource.Registration {
	return []resource.Registration{
		NVKECluster(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		resource.NewReadRegistration[v1.ControlPlaneClass]("/v1/control-plane-classes", resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind("ControlPlaneClass"), Resource: "controlplaneclasses",
			Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
		}),
		resource.NewDependentReadRegistration[v1.ClusterReleaseChannel]("/v1/cluster-release-channels", resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind("ClusterReleaseChannel"), Resource: "clusterreleasechannels",
			Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
		}, prepareCatalogDependencies(
			"ClusterRelease", "clusterreleases", (*v1.ClusterRelease).GetName)),
		resource.NewDependentReadRegistration[v1.NodePoolRelease]("/v1/node-profiles", resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind("NodePoolRelease"), Resource: "nodepoolreleases",
			Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
		}, prepareCatalogDependencies("MachineImage", "machineimages", (*v1.MachineImage).GetName),
			"spec.profile", "spec.tag"),
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
