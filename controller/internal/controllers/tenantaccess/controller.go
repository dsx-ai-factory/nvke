// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tenantaccess publishes each cluster's admin kubeconfig into the
// management-cluster tenant-access Secret plane so downstream consumers can
// reach tenant clusters.
package tenantaccess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kube"
)

const (
	// KubeconfigSecretKey is the data key inside the per-cluster kubeconfig Secret.
	KubeconfigSecretKey = "value"

	// SecretsNamespace is the management-cluster namespace that holds the
	// per-cluster Secrets this controller owns (kubeconfig + trust bundle). It is
	// exported because the teardown activity deletes those Secrets on cluster
	// removal. The chart creates the namespace and scopes CWE's Secret grant to
	// it, so this is a constant rather than a config knob.
	SecretsNamespace = "nke-system"

	resyncInterval = 3 * time.Minute
)

// KubeconfigSecretName returns the Secret name that holds the per-cluster
// kubeconfig published into SecretsNamespace.
func KubeconfigSecretName(clusterName string) string { return clusterName + "-kubeconfig" }

// TenantManagerSet stops the per-cluster controller manager when its cluster
// enters deletion.
type TenantManagerSet interface {
	RemoveCluster(clusterID uuid.UUID)
}

// Controller publishes one cluster's access material into the tenant-access
// Secret plane for each cluster-keyed reconciliation.
type Controller struct {
	cfg             config.Config
	logger          *slog.Logger
	db              *dbwrapper.DB
	k8sClient       ctrlclient.Client
	nodeAgentClient agent.Client
	tenantManagers  TenantManagerSet

	// tenantClientFor builds a controller-runtime client for a tenant cluster
	// from its admin.conf bytes. Defaulted in NewController; a test seam.
	tenantClientFor func(adminConf []byte) (ctrlclient.Client, error)
}

// NewController creates a new TenantAccessController.
func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
	tenantManagers TenantManagerSet,
) *Controller {
	return &Controller{
		cfg:             cfg,
		logger:          logger.With("controller", "tenant-access"),
		db:              dbInstance,
		k8sClient:       k8sClient,
		nodeAgentClient: nodeAgentClient,
		tenantManagers:  tenantManagers,
		tenantClientFor: newTenantClient,
	}
}

// SetupWithManager registers the cluster-scoped tenant-access reconciler. A
// joined control-plane node write is the low-latency bootstrap trigger, while
// the cluster source performs startup and periodic scans for eventual repair.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"tenant-access",
		c,
		c.db.ClusterSource(db.ClusterFilters{}, resyncInterval),
		c.db.ClusterForNodeSource(db.NodeFilters{
			ControlPlane: ptr.To(true),
			Joined:       ptr.To(true),
			Deleting:     ptr.To(false),
		}),
	)
}

// newTenantClient builds a controller-runtime client for a tenant cluster from
// its admin.conf.
//
// The REST timeout is set explicitly because agent serves admin.conf
// verbatim without rewriting its server URL, so the endpoint it names is not
// guaranteed to be routable from CWE. Both client construction (which performs
// discovery) and every subsequent apply therefore need a bounded request time.
func newTenantClient(adminConf []byte) (ctrlclient.Client, error) {
	restConfig, err := kube.NewRESTConfigFromKubeconfig(adminConf)
	if err != nil {
		return nil, err
	}
	restConfig.Timeout = tenantRequestTimeout

	return kube.NewClientFromRESTConfig(restConfig)
}

// Reconcile publishes access material for one cluster. The cluster row is
// loaded at execution time so queued events are hints, never state snapshots.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	cluster, err := c.db.GetCluster(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting cluster %s: %w", key.ID, err)
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.tenantManagers.RemoveCluster(cluster.ID)
		// Temporal owns Secret deletion after it no longer needs tenant access.
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, c.reconcileCluster(ctx, cluster)
}

// reconcileCluster fetches a control-plane node's admin kubeconfig and publishes
// it into the tenant-access Secret plane.
func (c *Controller) reconcileCluster(ctx context.Context, cluster db.Cluster) error {
	adminConf, ready, err := c.fetchAdminKubeconfig(ctx, cluster)
	if err != nil {
		return err
	}
	if !ready {
		// Not-ready gate: no joined control-plane node can serve a kubeconfig yet.
		// A matching node update normally re-enqueues immediately; the periodic
		// cluster resync provides bounded recovery when that notification is lost.
		c.logger.Debug("No ready control plane node for kubeconfig extraction; skipping", "clusterID", cluster.ID, "clusterName", cluster.Name)
		return nil
	}

	if cluster.OIDCAuthEnabled {
		return c.reconcileOIDC(ctx, cluster, adminConf)
	}

	// Non-OIDC: publish the verbatim admin kubeconfig.
	return c.upsertSecret(ctx, cluster, KubeconfigSecretName(cluster.Name), KubeconfigSecretKey, adminConf)
}

// reconcileOIDC applies the NKE tenant RBAC first, then publishes the SPIRE
// exec-auth kubeconfig and the tenant CA trust bundle. RBAC lands before the
// kubeconfig Secret because the Secret is only useful once NKE has access to the
// tenant cluster.
func (c *Controller) reconcileOIDC(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	manifest, err := c.readRBACManifest(ctx)
	if err != nil {
		return err
	}

	tenantClient, err := c.tenantClientFor(adminConf)
	if err != nil {
		return fmt.Errorf("building tenant client for cluster %s: %w", cluster.Name, err)
	}
	if err := applyManifestSSA(ctx, tenantClient, manifest, rbacFieldManager); err != nil {
		return fmt.Errorf("applying NKE RBAC to cluster %s: %w", cluster.Name, err)
	}

	parsedAdmin, err := clientcmd.Load(adminConf)
	if err != nil {
		return fmt.Errorf("parsing kubeconfig for cluster %s: %w", cluster.Name, err)
	}
	ca, err := extractCAFromKubeconfig(parsedAdmin)
	if err != nil {
		return err
	}
	server, err := extractServerFromKubeconfig(parsedAdmin)
	if err != nil {
		return err
	}

	kubeconfig, err := buildSpireKubeconfig(cluster.Name, server, ca, c.cfg.SpiffeEndpointSocket)
	if err != nil {
		return fmt.Errorf("building SPIRE kubeconfig for cluster %s: %w", cluster.Name, err)
	}

	if err := c.upsertSecret(ctx, cluster, KubeconfigSecretName(cluster.Name), KubeconfigSecretKey, kubeconfig); err != nil {
		return err
	}
	if err := c.upsertSecret(ctx, cluster, TrustBundleSecretName(cluster.Name), TrustBundleSecretKey, ca); err != nil {
		return err
	}

	return nil
}
