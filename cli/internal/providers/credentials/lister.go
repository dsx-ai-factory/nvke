// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/dsx-ai-factory/nvke/cli/internal/mgmtcluster"
	"github.com/dsx-ai-factory/nvke/cli/internal/providers"
)

// Lister lists the Identity objects of one kind in one namespace.
type Lister interface {
	List(ctx context.Context, kind providers.IdentityKind, namespace string) ([]Identity, error)
}

// ListerFactory builds a Lister for the management cluster that a kubeconfig
// points at.
type ListerFactory func(kubeconfigPath string) (Lister, error)

// DynamicLister is a Lister that reads Identities through the Kubernetes
// dynamic client, without the providers' Go types.
type DynamicLister struct {
	client dynamic.Interface
}

// NewDynamicLister builds a DynamicLister from a management cluster kubeconfig
// path. It does not fall back to KUBECONFIG or ~/.kube/config. Its signature
// matches ListerFactory.
func NewDynamicLister(kubeconfigPath string) (Lister, error) {
	cfg, err := mgmtcluster.RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return newDynamicListerWithClient(client), nil
}

// newDynamicListerWithClient builds a DynamicLister from an existing dynamic
// client, bypassing kubeconfig resolution. Tests use it to inject a fake.
func newDynamicListerWithClient(client dynamic.Interface) *DynamicLister {
	return &DynamicLister{client: client}
}

// List returns every Identity of kind in namespace. An error from the API
// server is returned as reported, so callers can test it with the predicates in
// k8s.io/apimachinery/pkg/api/errors.
func (l *DynamicLister) List(ctx context.Context, kind providers.IdentityKind, namespace string) ([]Identity, error) {
	gvr := schema.GroupVersionResource{Group: kind.Group, Version: kind.Version, Resource: kind.Resource}
	list, err := l.client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, decode(&list.Items[i]))
	}
	return out, nil
}
