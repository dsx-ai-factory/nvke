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
	capikubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	capisecret "sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	tenantAccessKubeconfigNamespace = "nke-system"
	tenantAccessKubeconfigKey       = "value"
	tenantAPIServerProbeTimeout     = 5 * time.Second
)

var ErrTenantClientNotReady = errors.New("tenant client not ready")

// TenantClientProvider gives controllers and activities a management-cluster
// independent way to build clients for tenant clusters.
type TenantClientProvider interface {
	ClientForCluster(ctx context.Context, cluster *clusterv1.Cluster, addToSchemes ...AddToSchemeFunc) (client.Client, error)
}

// CAPIKubeconfigTenantClientProvider reads the standard Cluster API
// <cluster-name>-kubeconfig Secret from the management cluster.
type CAPIKubeconfigTenantClientProvider struct {
	managementClient client.Reader
}

func NewCAPIKubeconfigTenantClientProvider(managementClient client.Reader) *CAPIKubeconfigTenantClientProvider {
	return &CAPIKubeconfigTenantClientProvider{
		managementClient: managementClient,
	}
}

func (p *CAPIKubeconfigTenantClientProvider) ClientForCluster(
	ctx context.Context,
	cluster *clusterv1.Cluster,
	addToSchemes ...AddToSchemeFunc,
) (client.Client, error) {
	// Re-read the kubeconfig on each request so kubeconfig rotation is observed
	// without requiring controller restarts or explicit cache invalidation.
	kubeconfigBytes, err := capikubeconfig.FromSecret(ctx, p.managementClient, client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Name,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: CAPI kubeconfig secret %s/%s not found: %w", ErrTenantClientNotReady, cluster.Namespace, cluster.Name, err)
		}
		return nil, fmt.Errorf("failed to get CAPI kubeconfig secret: %w", err)
	}

	tenantClient, err := NewClientFromKubeconfig(kubeconfigBytes, addToSchemes...)
	if err != nil {
		return nil, fmt.Errorf("failed to create client from CAPI kubeconfig secret: %w", err)
	}

	return tenantClient, nil
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
// for CWE DB clusters. Site-managed clusters use the kubeconfig published by the
// tenant-access controller when enabled, with the existing agent path kept
// as the disabled-state rollout fallback. CAPI clusters use the standard CAPI
// Secret plane.
type DBClusterTenantClientProvider struct {
	database            db.DB
	managementClient    client.Reader
	nodeAgentClient     agent.Client
	tenantAccessEnabled bool

	clientsMu sync.RWMutex
	clients   map[uuid.UUID]tenantClientCacheEntry
}

type tenantClientCacheEntry struct {
	secretVersion string
	client        client.Client
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
			return nil, fmt.Errorf("cluster %s must define capiInfraCluster when provider type is %q", cluster.ID, db.ProviderTypeCAPI)
		}

		return p.clientForCAPICluster(ctx, cluster)
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

func (p *DBClusterTenantClientProvider) clientForCAPICluster(ctx context.Context, cluster db.Cluster) (client.Client, error) {
	capiCluster := &clusterv1.Cluster{}
	if err := p.managementClient.Get(ctx, client.ObjectKey{
		Namespace: capi.ClusterNamespace(cluster.Name),
		Name:      cluster.Name,
	}, capiCluster); err != nil {
		return nil, fmt.Errorf("failed to get CAPI cluster: %w", err)
	}

	kubeconfigSecret, err := capisecret.Get(ctx, p.managementClient, client.ObjectKey{
		Namespace: capiCluster.Namespace,
		Name:      capiCluster.Name,
	}, capisecret.Kubeconfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: CAPI kubeconfig secret %s/%s not found: %w", ErrTenantClientNotReady, capiCluster.Namespace, capisecret.Name(capiCluster.Name, capisecret.Kubeconfig), err)
		}
		return nil, fmt.Errorf("failed to get CAPI kubeconfig secret: %w", err)
	}

	kubeconfigBytes := kubeconfigSecret.Data[capisecret.KubeconfigDataName]
	if len(kubeconfigBytes) == 0 {
		return nil, fmt.Errorf("%w: CAPI kubeconfig secret %s/%s has no non-empty %q key", ErrTenantClientNotReady, kubeconfigSecret.Namespace, kubeconfigSecret.Name, capisecret.KubeconfigDataName)
	}

	tenantClient, err := p.clientFromSecretVersion(cluster.ID, kubeconfigSecret, kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenant client from CAPI kubeconfig secret: %w", err)
	}
	return tenantClient, nil
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

	tenantClient, err := p.clientFromSecretVersion(cluster.ID, secret, kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create client from published tenant kubeconfig secret %s/%s: %w", tenantAccessKubeconfigNamespace, secretName, err)
	}
	return tenantClient, nil
}

func (p *DBClusterTenantClientProvider) clientFromSecretVersion(
	clusterID uuid.UUID,
	secret *corev1.Secret,
	kubeconfigBytes []byte,
) (client.Client, error) {
	secretVersion := secret.Namespace + "/" + secret.Name + "@" + secret.ResourceVersion
	p.clientsMu.RLock()
	cached := p.clients[clusterID]
	p.clientsMu.RUnlock()
	if cached.client != nil && cached.secretVersion == secretVersion {
		return cached.client, nil
	}

	tenantClient, err := NewClientFromKubeconfig(kubeconfigBytes)
	if err != nil {
		return nil, err
	}

	// Client construction may race for the same Secret version. Keep and return
	// the first successfully constructed instance for that version.
	p.clientsMu.Lock()
	cached = p.clients[clusterID]
	if cached.client != nil && cached.secretVersion == secretVersion {
		tenantClient = cached.client
	} else {
		p.clients[clusterID] = tenantClientCacheEntry{
			secretVersion: secretVersion,
			client:        tenantClient,
		}
	}
	p.clientsMu.Unlock()

	return tenantClient, nil
}

// clientFromControlPlaneNodeAgent gets kubeconfigs from joined control-plane
// agents and returns the first one whose apiserver endpoint is reachable
// from the CWE process. This path remains available while tenant access is
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
