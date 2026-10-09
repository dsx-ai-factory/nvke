// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	tenantAccessKubeconfigNamespace = constants.NamespaceNKESystem
	tenantAccessKubeconfigKey       = "value"
	tenantAPIServerProbeTimeout     = 5 * time.Second
)

var ErrTenantClientNotReady = errors.New("tenant client not ready")

// TenantClientProvider gives CAPI controllers, which are driven by CAPI objects
// rather than controller cluster rows, a way to reach the corresponding tenant cluster.
type TenantClientProvider interface {
	ClientForCluster(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error)
}

func NewClientFromKubeconfig(kubeconfigBytes []byte, addToSchemes ...AddToSchemeFunc) (client.Client, error) {
	restConfig, err := NewRESTConfigFromKubeconfig(kubeconfigBytes)
	if err != nil {
		return nil, err
	}

	tenantClient, err := NewClientFromRESTConfig(restConfig, addToSchemes...)
	if err != nil {
		return nil, err
	}

	return tenantClient, nil
}

func NewRESTConfigFromKubeconfig(kubeconfigBytes []byte) (*rest.Config, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	return restConfig, nil
}

func NewClientFromRESTConfig(restConfig *rest.Config, addToSchemes ...AddToSchemeFunc) (client.Client, error) {
	scheme, err := NewRuntimeScheme(addToSchemes...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tenant client scheme: %w", err)
	}

	tenantClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create tenant cluster client: %w", err)
	}

	return tenantClient, nil
}

// DBClusterTenantClientProvider returns shared tenant-cluster Kubernetes clients
// for controller DB clusters. CAPI clusters, and Nico clusters when tenant-access
// consume is enabled, read nke-system/<name>-kubeconfig (TenantAccess). Nico
// keeps the node-agent path as the disabled-state rollout fallback.
type DBClusterTenantClientProvider struct {
	database            db.DB
	managementClient    client.Reader
	nodeAgentClient     agent.Client
	tenantAccessEnabled bool

	clientsMu sync.RWMutex
	clients   map[uuid.UUID]tenantClientCacheEntry
}

type tenantClientCacheEntry struct {
	cacheKey string
	client   client.Client
}

func NewDBClusterTenantClientProvider(
	database db.DB,
	managementClient client.Reader,
	nodeAgentClient agent.Client,
	tenantAccessEnabled bool,
) *DBClusterTenantClientProvider {
	return &DBClusterTenantClientProvider{
		database:            database,
		managementClient:    managementClient,
		nodeAgentClient:     nodeAgentClient,
		tenantAccessEnabled: tenantAccessEnabled,
		clients:             make(map[uuid.UUID]tenantClientCacheEntry),
	}
}

func (p *DBClusterTenantClientProvider) ClientForDBCluster(ctx context.Context, cluster db.Cluster) (client.Client, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		provider, err := cluster.CAPIProvider()
		if err != nil {
			return nil, err
		}
		if capi.IsEmptyManifest(provider.InfraCluster) {
			return nil, fmt.Errorf("cluster %s must define infra.cluster", cluster.ID)
		}

		return p.clientFromTenantAccessSecret(ctx, cluster)
	case db.ProviderTypeNico:
		if _, err := cluster.NicoProvider(); err != nil {
			return nil, err
		}

		if p.tenantAccessEnabled {
			return p.clientFromTenantAccessSecret(ctx, cluster)
		}

		nodes, err := p.database.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
		if err != nil {
			return nil, fmt.Errorf("failed to list nodes for cluster %s: %w", cluster.ID, err)
		}
		return p.clientFromControlPlaneNodeAgent(ctx, cluster, nodes)
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}

// ClientForCluster resolves the controller cluster row a CAPI Cluster stands for and
// returns that row's tenant client. The row is what carries the provider type
// that decides where the kubeconfig comes from, so CAPI controllers go through
// it rather than reading a kubeconfig out of the Cluster's namespace directly.
//
// clusters.name is uniquely indexed, and every CAPI Cluster the controller creates is named
// after its row, so the name is an unambiguous key.
func (p *DBClusterTenantClientProvider) ClientForCluster(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error) {
	clusters, err := p.database.ListClusters(ctx, db.ClusterFilters{Name: &cluster.Name})
	if err != nil {
		return nil, fmt.Errorf("failed to look up cluster row for CAPI cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	if len(clusters) == 0 {
		return nil, fmt.Errorf("no cluster row found for CAPI cluster %s/%s: %w", cluster.Namespace, cluster.Name, db.ErrNotFound)
	}

	return p.ClientForDBCluster(ctx, clusters[0])
}

func (p *DBClusterTenantClientProvider) clientFromTenantAccessSecret(ctx context.Context, cluster db.Cluster) (client.Client, error) {
	secretName := cluster.Name + "-kubeconfig"
	secret := &corev1.Secret{}
	if err := p.managementClient.Get(ctx, client.ObjectKey{
		Namespace: tenantAccessKubeconfigNamespace,
		Name:      secretName,
	}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf(
				"%w: published tenant kubeconfig secret %s/%s not found: %w",
				ErrTenantClientNotReady,
				tenantAccessKubeconfigNamespace,
				secretName,
				err,
			)
		}
		return nil, fmt.Errorf("failed to get published tenant kubeconfig secret %s/%s: %w", tenantAccessKubeconfigNamespace, secretName, err)
	}

	kubeconfigBytes := secret.Data[tenantAccessKubeconfigKey]
	if len(kubeconfigBytes) == 0 {
		return nil, fmt.Errorf(
			"%w: published tenant kubeconfig secret %s/%s has no non-empty %q key",
			ErrTenantClientNotReady,
			tenantAccessKubeconfigNamespace,
			secretName,
			tenantAccessKubeconfigKey,
		)
	}
	tenantClient, err := p.clientFromSecretVersion(cluster, secret, kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create client from published tenant kubeconfig secret %s/%s: %w", tenantAccessKubeconfigNamespace, secretName, err)
	}
	return tenantClient, nil
}

func (p *DBClusterTenantClientProvider) clientFromSecretVersion(
	cluster db.Cluster,
	secret *corev1.Secret,
	kubeconfigBytes []byte,
) (client.Client, error) {
	cacheKey := secret.Namespace + "/" + secret.Name + "@" + secret.ResourceVersion
	p.clientsMu.RLock()
	cached := p.clients[cluster.ID]
	p.clientsMu.RUnlock()
	if cached.client != nil && cached.cacheKey == cacheKey {
		return cached.client, nil
	}

	tenantClient, err := NewClientFromKubeconfig(kubeconfigBytes)
	if err != nil {
		return nil, err
	}

	// Client construction may race for the same Secret version. Keep and return
	// the first successfully constructed instance for that version.
	p.clientsMu.Lock()
	cached = p.clients[cluster.ID]
	if cached.client != nil && cached.cacheKey == cacheKey {
		tenantClient = cached.client
	} else {
		p.clients[cluster.ID] = tenantClientCacheEntry{
			cacheKey: cacheKey,
			client:   tenantClient,
		}
	}
	p.clientsMu.Unlock()

	return tenantClient, nil
}

// clientFromControlPlaneNodeAgent gets kubeconfigs from joined control-plane
// node-agents and returns the first one whose apiserver endpoint is reachable
// from the controller process. This path remains available while tenant access is
// disabled so the producer and consumer can be enabled with one rollout flag.
func (p *DBClusterTenantClientProvider) clientFromControlPlaneNodeAgent(ctx context.Context, cluster db.Cluster, nodes []db.Node) (client.Client, error) {
	var errs []error
	for _, node := range nodes {
		if !node.ControlPlane || node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}

		kubeconfig, err := p.nodeAgentClient.GetKubeconfig(ctx, node)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}

		restConfig, err := NewRESTConfigFromKubeconfig([]byte(kubeconfig))
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}

		if err := VerifyRESTConfig(ctx, restConfig, tenantAPIServerProbeTimeout); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}

		tenantClient, err := NewClientFromRESTConfig(restConfig)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}

		return tenantClient, nil
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("failed to create tenant client for cluster %s from control-plane node kubeconfigs: %w", cluster.ID, errors.Join(errs...))
	}

	return nil, fmt.Errorf("%w: cluster %s has no joined control-plane nodes to fetch kubeconfig from", ErrTenantClientNotReady, cluster.ID)
}

func VerifyRESTConfig(ctx context.Context, restConfig *rest.Config, timeout time.Duration) error {
	kubernetesClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := kubernetesClient.Discovery().RESTClient().Get().AbsPath("/version").Do(ctxWithTimeout).Raw(); err != nil {
		return fmt.Errorf("failed to reach Kubernetes apiserver /version endpoint: %w", err)
	}

	return nil
}
