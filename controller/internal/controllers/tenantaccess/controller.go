// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tenantaccess publishes each cluster's admin kubeconfig into the
// management-cluster tenant-access Secret plane so downstream consumers can
// reach tenant clusters. This is a port of the site-workflow-engine cluster
// controller's kubeconfig-publish path onto CWE's DB-site-poller
// ControllerManager.
package tenantaccess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
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
)

// KubeconfigSecretName returns the Secret name that holds the per-cluster
// kubeconfig published into SecretsNamespace.
func KubeconfigSecretName(clusterName string) string { return clusterName + "-kubeconfig" }

// Controller publishes per-cluster admin kubeconfigs into the tenant-access
// Secret plane for each site it reconciles.
type Controller struct {
	cfg             config.Config
	logger          *slog.Logger
	db              db.DB
	k8sClient       ctrlclient.Client
	nodeAgentClient agent.Client

	// tenantClientFor builds a controller-runtime client for a tenant cluster
	// from its admin.conf bytes. Defaulted in NewController; a test seam.
	tenantClientFor func(adminConf []byte) (ctrlclient.Client, error)
}

// NewController creates a new TenantAccessController.
func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
) *Controller {
	return &Controller{
		cfg:             cfg,
		logger:          logger.With("controller", "tenant-access"),
		db:              dbInstance,
		k8sClient:       k8sClient,
		nodeAgentClient: nodeAgentClient,
		tenantClientFor: newTenantClient,
	}
}

// newTenantClient builds a controller-runtime client for a tenant cluster from
// its admin.conf.
//
// The REST timeout is set explicitly because agent serves admin.conf
// verbatim without rewriting its server URL, so the endpoint it names is not
// guaranteed to be routable from CWE. Both client construction (which performs
// discovery) and every subsequent apply would otherwise block on the reconcile
// context, which the controller-manager shares across all sites.
func newTenantClient(adminConf []byte) (ctrlclient.Client, error) {
	restConfig, err := kube.NewRESTConfigFromKubeconfig(adminConf)
	if err != nil {
		return nil, err
	}
	restConfig.Timeout = tenantRequestTimeout

	return kube.NewClientFromRESTConfig(restConfig)
}

// Reconcile publishes the kubeconfig Secret for every non-deleting cluster on
// the site. A per-cluster failure is collected but does not stop the other
// clusters from being reconciled.
func (c *Controller) Reconcile(ctx context.Context, site db.Site) error {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	var reconcileErrs []error
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			c.logger.Debug("Cluster is deleting, skipping tenant-access reconciliation", "clusterID", cluster.ID, "clusterName", cluster.Name)
			continue
		}

		if err := c.reconcileCluster(ctx, cluster); err != nil {
			c.logger.Error("Failed to reconcile tenant access", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	return errors.Join(reconcileErrs...)
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
		// The poller re-enqueues the site, so we simply skip this pass.
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
