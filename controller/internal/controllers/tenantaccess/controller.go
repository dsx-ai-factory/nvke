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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
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
	SecretsNamespace = constants.NamespaceNKESystem

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
// it into the tenant-access Secret plane (admin.conf, or SPIRE when OIDC is on).
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

	return c.publish(ctx, cluster, adminConf)
}

// publish writes the kubeconfig Secret for one cluster. Split from
// reconcileCluster so the publish decision can be exercised without standing up
// a node-agent to serve admin.conf.
func (c *Controller) publish(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	// Re-read the cluster before choosing a mode: Reconcile's initial row can
	// become stale during the agent probe, so an OIDC flag change during the probe
	// must be honored.
	//
	// This refresh selects the mode; it does not guard the writes. upsertSecret
	// re-reads again immediately before each write, because the OIDC path applies
	// RBAC into the tenant cluster in between and that window is long enough for a
	// teardown to land.
	live, skip, err := c.refreshCluster(ctx, cluster)
	if err != nil || skip {
		return err
	}

	if live.OIDCAuthEnabled {
		return c.publishSPIRE(ctx, live, adminConf)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(live.Name),
			Namespace: SecretsNamespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: live.Name,
			},
		},
		// Opaque, not clusterv1.ClusterSecretType. Nothing reads the type -- CAPI
		// resolves a kubeconfig by name and data key -- and Opaque is CAPI's marker
		// for a Secret its core components did not create, which is what a
		// TenantAccess-published kubeconfig is. It also matches what the retired SWE
		// controller left on the fleet, so taking those Secrets over is an in-place
		// update rather than a collision on an immutable field.
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretKey: adminConf},
	}
	capi.SetSecretOwnership(secret, live, constants.LabelValueKubeconfigModeAdmin)
	return c.upsertSecret(ctx, live, secret)
}

// refreshCluster returns the live DB row. skip means do not publish because the
// cluster is gone or has started deleting.
func (c *Controller) refreshCluster(ctx context.Context, cluster db.Cluster) (db.Cluster, bool, error) {
	live, err := c.db.GetCluster(ctx, cluster.ID)
	if errors.Is(err, db.ErrNotFound) {
		c.logger.Debug("Cluster no longer exists; skipping tenant-access publish", "clusterID", cluster.ID)
		return db.Cluster{}, true, nil
	}
	if err != nil {
		return db.Cluster{}, false, fmt.Errorf("re-reading cluster %s before publish: %w", cluster.ID, err)
	}
	if live.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.logger.Debug("Cluster started deleting; skipping tenant-access publish", "clusterID", cluster.ID)
		return db.Cluster{}, true, nil
	}

	return live, false, nil
}

// publishSPIRE applies the NKE tenant RBAC first, then publishes the SPIRE
// exec-auth kubeconfig and the tenant CA trust bundle. RBAC lands before the
// kubeconfig Secret because the Secret is only useful once NKE has access to the
// tenant cluster.
func (c *Controller) publishSPIRE(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	if err := c.applyRBAC(ctx, cluster, adminConf); err != nil {
		return err
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

	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretKey: kubeconfig},
	}
	capi.SetSecretOwnership(kubeconfigSecret, cluster, constants.LabelValueKubeconfigModeSPIRE)
	if err := c.upsertSecret(ctx, cluster, kubeconfigSecret); err != nil {
		return err
	}

	trustBundleSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TrustBundleSecretName(cluster.Name),
			Namespace: SecretsNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{TrustBundleSecretKey: ca},
	}
	capi.SetSecretOwnership(trustBundleSecret, cluster, constants.LabelValueKubeconfigModeSPIRE)
	return c.upsertSecret(ctx, cluster, trustBundleSecret)
}

// applyRBAC server-side applies the operator-managed nke-rbac manifest into the
// tenant cluster.
func (c *Controller) applyRBAC(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	manifest, err := c.readRBACManifest(ctx)
	if apierrors.IsNotFound(err) {
		// Do not publish a SPIRE kubeconfig that cannot authenticate as nke:cwe /
		// CAPI SPIFFE IDs. The chart renders this ConfigMap when spiffe.trustDomain
		// is set, independent of the live OIDC flag.
		return fmt.Errorf("nke-rbac ConfigMap not found; refusing to publish SPIRE kubeconfig for cluster %s: %w", cluster.Name, err)
	}
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

	return nil
}

// newTenantClient builds a controller-runtime client for a tenant cluster from
// its admin.conf.
//
// The REST timeout is set explicitly because node-agent serves admin.conf
// verbatim without rewriting its server URL, so the endpoint it names is not
// guaranteed to be routable from CWE. Both client construction (which performs
// discovery) and every subsequent apply would otherwise block on the
// long-lived reconcile context.
func newTenantClient(adminConf []byte) (ctrlclient.Client, error) {
	restConfig, err := kube.NewRESTConfigFromKubeconfig(adminConf)
	if err != nil {
		return nil, err
	}
	restConfig.Timeout = tenantRequestTimeout

	return kube.NewClientFromRESTConfig(restConfig)
}
