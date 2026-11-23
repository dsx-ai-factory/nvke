// SPDX-License-Identifier: Apache-2.0

// Package publicapi hosts the tenant and operator APIs and selects their resources.
// Generated registrations bind public types to datastore resources.
// Tenant and Operator choose which registrations each server mounts.
package publicapi

import (
	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// Surface is one public API: its routes and the spelling of its contract.
type Surface struct {
	Name          string
	Naming        resource.Naming
	Registrations []resource.Registration
	Overview      string
}

// Tenant exposes workload cluster operations and read-only catalog choices.
func Tenant() Surface {
	return Surface{
		Name:   "tenant",
		Naming: resource.SnakeCase,
		Overview: "Manage workload clusters in the server-configured namespace and read the global catalog.\n" +
			"Callers cannot select a namespace through the API.",
		Registrations: []resource.Registration{
			NVKECluster(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
			resource.NewReadRegistration[v1.ControlPlaneClass]("/v1/control-plane-classes", resource.ResourceInfo{
				GVK: v1.SchemeGroupVersion.WithKind("ControlPlaneClass"), Resource: "controlplaneclasses",
				Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
			}),
			resource.NewDependentReadRegistration[v1.ClusterReleaseChannel](
				"/v1/cluster-release-channels", resource.ResourceInfo{
					GVK: v1.SchemeGroupVersion.WithKind("ClusterReleaseChannel"), Resource: "clusterreleasechannels",
					Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
				}, prepareCatalogDependencies("ClusterRelease", "clusterreleases", (*v1.ClusterRelease).GetName)),
			resource.NewDependentReadRegistration[v1.NodeProfile]("/v1/node-profiles", resource.ResourceInfo{
				GVK: v1.SchemeGroupVersion.WithKind("NodeProfile"), Resource: "nodeprofiles",
				Scope: resource.ClusterScoped, Verbs: []resource.Operation{resource.OpGet, resource.OpList},
			}, prepareCatalogDependencies("MachineImage", "machineimages", (*v1.MachineImage).GetName),
				"spec.profile", "spec.tag"),
		},
	}
}

// Operator exposes create, get, list, update, and delete operations for images, releases, channels, and classes.
func Operator() Surface {
	return Surface{
		Name:   "operator",
		Naming: resource.SnakeCase,
		Overview: "Publish the machine images, releases, release channels, " +
			"control-plane classes,\nand node profiles used by tenants. These resources are global.",
		Registrations: []resource.Registration{
			ClusterRelease(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
			MachineImage(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
			ControlPlaneClass(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
			ClusterReleaseChannel(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
			NodeProfile(resource.OpCreate, resource.OpGet, resource.OpList, resource.OpUpdate, resource.OpDelete),
		},
	}
}
