// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tenantClientFactory creates a client for a tenant cluster.
type tenantClientFactory interface {
	ClientForCluster(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error)
}

type kubeconfigTenantClientFactory struct {
	managementClient client.Reader
	scheme           *runtime.Scheme
}

func newKubeconfigTenantClientFactory(
	managementClient client.Reader,
	scheme *runtime.Scheme,
) tenantClientFactory {
	return &kubeconfigTenantClientFactory{managementClient: managementClient, scheme: scheme}
}

func (f *kubeconfigTenantClientFactory) ClientForCluster(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) (client.Client, error) {
	kubeconfigBytes, err := kubeconfig.FromSecret(ctx, f.managementClient, client.ObjectKeyFromObject(cluster))
	if err != nil {
		return nil, fmt.Errorf("read tenant kubeconfig: %w", err)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parse tenant kubeconfig: %w", err)
	}

	tenantClient, err := client.New(restConfig, client.Options{Scheme: f.scheme})
	if err != nil {
		return nil, fmt.Errorf("create tenant client: %w", err)
	}
	return tenantClient, nil
}
