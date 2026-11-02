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
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
)

const (
	// KubeconfigSecretKey is the data key inside the per-cluster kubeconfig Secret.
	KubeconfigSecretKey = "value"
)

// KubeconfigSecretName returns the Secret name that holds the per-cluster
// kubeconfig the controller consumes to access tenant clusters.
func KubeconfigSecretName(clusterName string) string { return clusterName + "-kubeconfig" }

// AdminKubeconfigSecretName returns the Secret name that holds the
// nvke:breakglass admin kubeconfig.
func AdminKubeconfigSecretName(clusterName string) string {
	return clusterName + "-admin-kubeconfig"
}

// IsControllerKubeconfigSecretName reports whether name is the controller-consumed kubeconfig secret.
func IsControllerKubeconfigSecretName(name string) bool {
	return strings.HasSuffix(name, "-kubeconfig") && !strings.HasSuffix(name, "-admin-kubeconfig")
}

// TenantManagerSet stops the per-cluster controller manager when its cluster
// enters deletion.
type TenantManagerSet interface {
	RemoveCluster(clusterID uuid.UUID)
}

// Controller publishes one cluster's access material into the tenant-access
// Secret plane for each cluster-keyed reconciliation.
type Controller struct {
	cfg                  config.Config
	logger               *slog.Logger
	db                   *dbwrapper.DB
	k8sClient            ctrlclient.Client
	nodeAgentClient      agent.Client
	tenantManagers       TenantManagerSet
	tenantClientFor      func(adminConf []byte) (ctrlclient.Client, error)
	issueAdminKubeconfig func(ctx context.Context, adminConf []byte, cluster db.Cluster, existing []byte) ([]byte, error)
	scheme               *runtime.Scheme
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
	c := &Controller{
		cfg:             cfg,
		logger:          logger.With("controller", "tenant-access"),
		db:              dbInstance,
		k8sClient:       k8sClient,
		nodeAgentClient: nodeAgentClient,
		tenantManagers:  tenantManagers,
		tenantClientFor: newTenantClient,
	}
	c.issueAdminKubeconfig = c.issueAdminBreakglass
	return c
}

// secretsNamespace is the management-cluster namespace that holds the
// per-cluster Secrets this controller owns (kubeconfig + trust bundle) and the
// RBAC ConfigMap it reads. The chart creates the namespace and scopes the
// controller's Secret grant to it, so the two must be configured together.
func (c *Controller) secretsNamespace() string {
	return c.cfg.ManagementNamespaceOrDefault()
}

// SetupWithManager registers the CAPI-Cluster-scoped tenant-access
// reconciler. Existence of the CAPI Cluster is the publish gate; a joined
// control-plane node write and a DB cluster row change (e.g. EnableOIDC) are
// low-latency triggers mapped onto the same Cluster's request.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	c.scheme = mgr.GetScheme()

	nodeSource := dbwrapper.NewMappedNodeSource(c.db, dbfilters.Node{
		ControlPlane: ptr.To(true),
		Joined:       ptr.To(true),
		Deleting:     ptr.To(false),
	}, c.nodeClusterRequest)

	clusterSource := dbwrapper.NewMappedClusterSource(c.db, dbfilters.Cluster{}, c.dbClusterRequest)

	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}).
		WatchesRawSource(nodeSource).
		WatchesRawSource(clusterSource).
		Named("tenant-access").
		Complete(c)
}

// nodeClusterRequest maps a control-plane node write to its cluster's
// tenant-access request, looking up the cluster name the request is keyed on.
func (c *Controller) nodeClusterRequest(ctx context.Context, node db.Node) ([]reconcile.Request, error) {
	if node.ClusterID == uuid.Nil {
		return nil, nil
	}
	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting cluster %s for node %s: %w", node.ClusterID, node.ID, err)
	}
	return []reconcile.Request{c.clusterRequest(cluster.Name)}, nil
}

// dbClusterRequest maps a DB cluster row change to its tenant-access request.
func (c *Controller) dbClusterRequest(_ context.Context, cluster db.Cluster) ([]reconcile.Request, error) {
	if cluster.Name == "" {
		return nil, nil
	}
	return []reconcile.Request{c.clusterRequest(cluster.Name)}, nil
}

func (c *Controller) clusterRequest(clusterName string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.secretsNamespace(), Name: clusterName}}
}

// Reconcile publishes access material for one cluster. Existence of the CAPI
// Cluster named by req is the publish gate: a missing Cluster means no
// unowned Secret is ever created, and Kubernetes GC (via the owner reference
// set on every published Secret) removes them once the Cluster is deleted --
// this controller never deletes a Secret itself.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	capiCluster := &clusterv1.Cluster{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting CAPI cluster %s: %w", req.NamespacedName, err)
	}

	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{Name: &capiCluster.Name})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing clusters named %s: %w", capiCluster.Name, err)
	}
	if len(clusters) == 0 {
		c.logger.Debug("No DB cluster row for CAPI cluster; skipping tenant-access publish", "clusterName", capiCluster.Name)
		return ctrl.Result{}, nil
	}
	if len(clusters) > 1 {
		return ctrl.Result{}, fmt.Errorf("%d DB clusters named %s, want at most 1", len(clusters), capiCluster.Name)
	}
	cluster := clusters[0]

	if !capiCluster.DeletionTimestamp.IsZero() || cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.tenantManagers.RemoveCluster(cluster.ID)
		// No Secret writes: Kubernetes GC removes the owned Secrets once the
		// CAPI Cluster itself finishes deleting.
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, c.reconcileCluster(ctx, cluster)
}

// reconcileCluster fetches a control-plane node's admin kubeconfig and publishes
// tenant access from it. The admin Secret gets an nvke:breakglass kubeconfig.
// The controller Secret gets admin.conf, or a SPIRE kubeconfig when OIDC is on.
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

// publish writes the kubeconfig Secrets for one cluster. Split from
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

	var publishErr error
	if live.OIDCAuthEnabled {
		publishErr = c.publishSPIRE(ctx, live, adminConf)
	} else {
		publishErr = c.upsertKubeconfigSecret(ctx, live, KubeconfigSecretName(live.Name), adminConf, constants.LabelValueKubeconfigModeAdmin)
	}
	if err := errors.Join(publishErr, c.publishBreakglass(ctx, live, adminConf)); err != nil {
		return err
	}

	// Ordered last because a valid Cluster.spec.controlPlaneEndpoint is what makes CAPI's own reconcileKubeconfig
	// reachable, and that path mints <cluster>-kubeconfig by signing with a CA private key NVKE does not hold.
	// It only acts when the Secret is absent, so the endpoint is applied only after the kubeconfig Secret exists.
	if err := c.publishCAPIBootstrapContract(ctx, live, adminConf); err != nil {
		return fmt.Errorf("publishing CAPI bootstrap contract for cluster %s: %w", live.Name, err)
	}

	return nil
}

// publishBreakglass stores the nvke:breakglass kubeconfig in the admin Secret.
func (c *Controller) publishBreakglass(ctx context.Context, live db.Cluster, adminConf []byte) error {
	existing, err := c.currentAdminKubeconfig(ctx, live.Name)
	if err != nil {
		return err
	}
	adminKubeconfig, err := c.issueAdminKubeconfig(ctx, adminConf, live, existing)
	if err != nil {
		return fmt.Errorf("issuing breakglass kubeconfig for cluster %s: %w", live.Name, err)
	}
	return c.upsertKubeconfigSecret(ctx, live, AdminKubeconfigSecretName(live.Name), adminKubeconfig, constants.LabelValueKubeconfigModeAdmin)
}

// currentAdminKubeconfig returns the admin Secret payload, or nil when it
// does not exist yet.
func (c *Controller) currentAdminKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secret := &corev1.Secret{}
	err := c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName(clusterName),
		Namespace: c.secretsNamespace(),
	}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting admin kubeconfig secret for cluster %s: %w", clusterName, err)
	}
	return secret.Data[KubeconfigSecretKey], nil
}

// upsertKubeconfigSecret upserts one Opaque kubeconfig Secret in c.secretsNamespace().
//
// Opaque, not clusterv1.ClusterSecretType: nothing reads the type — CAPI resolves
// a kubeconfig by name and data key — and Opaque is CAPI's marker for a Secret
// its core components did not create, which is what a TenantAccess-published
// kubeconfig is. It also matches what the retired SWE controller left on the
// fleet, so taking those Secrets over is an in-place update rather than a
// collision on an immutable field.
func (c *Controller) upsertKubeconfigSecret(ctx context.Context, cluster db.Cluster, name string, kubeconfig []byte, mode string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.secretsNamespace(),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretKey: kubeconfig},
	}
	capi.SetSecretOwnership(secret, cluster, mode, c.cfg.ManagedByLabelValue)
	return c.upsertSecret(ctx, cluster, secret)
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

// publishSPIRE applies the NVKE tenant RBAC first, then publishes the SPIRE
// exec-auth kubeconfig and the tenant CA trust bundle. RBAC lands before the
// kubeconfig Secret because the Secret is only useful once NVKE has access to the
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

	kubeconfig, err := buildSpireKubeconfig(cluster.Name, server, ca, c.cfg.SpiffeEndpointSocket, c.cfg.SpiffeJWTExchangeEndpoint)
	if err != nil {
		return fmt.Errorf("building SPIRE kubeconfig for cluster %s: %w", cluster.Name, err)
	}

	if err := c.upsertKubeconfigSecret(ctx, cluster, KubeconfigSecretName(cluster.Name), kubeconfig, constants.LabelValueKubeconfigModeSPIRE); err != nil {
		return err
	}

	trustBundleSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TrustBundleSecretName(cluster.Name),
			Namespace: c.secretsNamespace(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{TrustBundleSecretKey: ca},
	}
	capi.SetSecretOwnership(trustBundleSecret, cluster, constants.LabelValueKubeconfigModeSPIRE, c.cfg.ManagedByLabelValue)
	return c.upsertSecret(ctx, cluster, trustBundleSecret)
}

// applyRBAC server-side applies the operator-managed RBAC manifest into the
// tenant cluster.
func (c *Controller) applyRBAC(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	manifest, err := c.readRBACManifest(ctx)
	if apierrors.IsNotFound(err) {
		// Do not publish a SPIRE kubeconfig that cannot authenticate as nvke:cwe /
		// CAPI SPIFFE IDs. The chart renders this ConfigMap when spiffe.trustDomain
		// is set, independent of the live OIDC flag. The name is per-site
		// configurable, so say which one was looked for.
		src := c.cfg.RBACConfigMapSource()
		return fmt.Errorf("RBAC ConfigMap %s/%s not found; refusing to publish SPIRE kubeconfig for cluster %s: %w", src.Namespace, src.Name, cluster.Name, err)
	}
	if err != nil {
		return err
	}

	tenantClient, err := c.tenantClientFor(adminConf)
	if err != nil {
		return fmt.Errorf("building tenant client for cluster %s: %w", cluster.Name, err)
	}
	if err := applyManifestSSA(ctx, tenantClient, manifest, rbacFieldManager); err != nil {
		return fmt.Errorf("applying NVKE RBAC to cluster %s: %w", cluster.Name, err)
	}
	if err := pruneLegacyRBAC(ctx, tenantClient, manifest); err != nil {
		return fmt.Errorf("pruning legacy NVKE RBAC from cluster %s: %w", cluster.Name, err)
	}

	return nil
}

// newTenantClient builds a controller-runtime client for a tenant cluster from
// its admin.conf.
//
// The REST timeout is set explicitly because node-agent serves admin.conf
// verbatim without rewriting its server URL, so the endpoint it names is not
// guaranteed to be routable from the controller. Both client construction (which performs
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
