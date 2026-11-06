// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"context"
	"fmt"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// prepareCatalogDependencies defers reads to a per-batch loader, avoiding a request per offering.
func prepareCatalogDependencies[Dependency any](
	kind string,
	collection string,
	dependencyName func(*Dependency) string,
) resource.CatalogDependencyPreparation[map[string]Dependency] {
	return func(cfg resource.ClientConfig) (resource.CatalogDependencyLoader[map[string]Dependency], error) {
		listDependencies, err := resource.NewSourceLister[Dependency](cfg, resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind(kind), Resource: collection, Scope: resource.ClusterScoped,
		})
		if err != nil {
			return nil, fmt.Errorf("prepare catalog dependency %s: %w", collection, err)
		}
		return func(ctx context.Context) (map[string]Dependency, error) {
			dependencies, err := listDependencies(ctx)
			if err != nil {
				return nil, fmt.Errorf("list catalog dependency %s: %w", collection, err)
			}
			dependenciesByName := make(map[string]Dependency, len(dependencies))
			for _, dependency := range dependencies {
				dependenciesByName[dependencyName(&dependency)] = dependency
			}
			return dependenciesByName, nil
		}, nil
	}
}
