// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capisecret "sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const tenantAPITimeout = 30 * time.Second

// tenantClientFactory creates a client for a tenant cluster.
type tenantClientFactory interface {
	ClientForCluster(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error)
}

type kubeconfigTenantClientFactory struct {
	managementClient client.Reader
	scheme           *runtime.Scheme
	newClient        func(*rest.Config, client.Options) (client.Client, error)

	mu     sync.Mutex
	cached map[client.ObjectKey]cachedTenantClient
}

type cachedTenantClient struct {
	resourceVersion string
	client          client.Client
}

func newKubeconfigTenantClientFactory(
	managementClient client.Reader,
	scheme *runtime.Scheme,
) tenantClientFactory {
	return &kubeconfigTenantClientFactory{
		managementClient: managementClient,
		scheme:           scheme,
		newClient:        client.New,
		cached:           map[client.ObjectKey]cachedTenantClient{},
	}
}

func (f *kubeconfigTenantClientFactory) ClientForCluster(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) (client.Client, error) {
	clusterKey := client.ObjectKeyFromObject(cluster)
	configSecret, err := capisecret.Get(ctx, f.managementClient, clusterKey, capisecret.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("read tenant kubeconfig: %w", err)
	}
	kubeconfigBytes, ok := configSecret.Data[capisecret.KubeconfigDataName]
	if !ok {
		return nil, fmt.Errorf("read tenant kubeconfig: Secret %s/%s has no %q data",
			configSecret.Namespace, configSecret.Name, capisecret.KubeconfigDataName)
	}

	f.mu.Lock()
	entry, found := f.cached[clusterKey]
	f.mu.Unlock()
	if found && entry.resourceVersion == configSecret.ResourceVersion {
		return entry.client, nil
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parse tenant kubeconfig: %w", err)
	}
	restConfig.Timeout = tenantAPITimeout

	tenantClient, err := f.newClient(restConfig, client.Options{Scheme: f.scheme})
	if err != nil {
		return nil, fmt.Errorf("create tenant client: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Another reconcile can finish discovery while this one is building its
	// client. Reuse that result when both observed the same Secret version.
	if entry, found := f.cached[clusterKey]; found && entry.resourceVersion == configSecret.ResourceVersion {
		return entry.client, nil
	}
	f.cached[clusterKey] = cachedTenantClient{
		resourceVersion: configSecret.ResourceVersion,
		client:          tenantClient,
	}
	return tenantClient, nil
}
