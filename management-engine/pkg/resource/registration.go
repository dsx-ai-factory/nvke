// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// Registration captures a typed resource so a surface can mount several resource types.
type Registration struct {
	// Mount checks datastore capabilities and installs the registered routes.
	// Wrap the completed surface with WithLimits.
	Mount func(*http.ServeMux, ClientConfig) error
}

// NewRegistration binds a public collection path to its datastore identity and enabled operations.
// Empty Verbs enables no operations. Datastore requests begin only when Mount is called.
func NewRegistration[E ResourceHandler[I], I ResourceHandler[E]](path string, info ResourceInfo) Registration {
	info.Verbs = slices.Clone(info.Verbs)
	return Registration{Mount: func(mux *http.ServeMux, cfg ClientConfig) error {
		server, err := New[E, I](cfg, info)
		if err != nil {
			return err
		}
		Register(mux, path, server, info.Verbs...)
		return nil
	}}
}

// checkResource verifies served identity, scope, status and the verbs actually used by CRUDL.
// Discovery avoids requiring CRD read access. Schema validation remains the datastore's responsibility.
func checkResource(cfg *rest.Config, client *http.Client, info ResourceInfo) error {
	disco, err := discovery.NewDiscoveryClientForConfigAndClient(cfg, client)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resources, err := disco.ServerResourcesForGroupVersionWithContext(ctx, info.GVK.GroupVersion().String())
	if err != nil {
		return fmt.Errorf("discovering resource: %w", err)
	}
	hasResource, hasStatus := false, false
	for _, r := range resources.APIResources {
		hasStatus = hasStatus || r.Name == info.Resource+"/status"
		if r.Name != info.Resource {
			continue
		}
		hasResource = true
		if r.Kind != info.GVK.Kind || r.Namespaced != (info.Scope == Namespaced) {
			return fmt.Errorf("resource scope or kind differs from registration")
		}
		for _, op := range info.Verbs {
			verb := string(op)
			if op == OpCreate || op == OpUpdate {
				verb = "patch"
			}
			if !slices.Contains(r.Verbs, verb) ||
				(op == OpUpdate && !slices.Contains(r.Verbs, "get")) {
				return fmt.Errorf("resource does not support %s and its required reads", op)
			}
		}
	}
	if !hasResource || resources.GroupVersion != info.GVK.GroupVersion().String() {
		return fmt.Errorf("resource version was not discovered")
	}
	if hasStatus != info.HasStatus {
		return fmt.Errorf("resource status capability differs from registration")
	}
	return nil
}
