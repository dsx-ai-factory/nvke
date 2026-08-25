// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"
	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
)

const (
	capiAdoptionTimeout = 2 * time.Minute
)

type capiMachineAdopter interface {
	LookupForgeInstanceForAdoption(ctx context.Context, cluster db.Cluster, node db.Node) (*forgesdk.Instance, error)
	ApplyAdoptedForgeCAPIMachine(ctx context.Context, cluster db.Cluster, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, instance *forgesdk.Instance) error
}

type ForgeCAPIMigrationController struct {
	logger         *slog.Logger
	db             db.DB
	k8sClient      ctrlclient.Client
	machineAdopter capiMachineAdopter
}

func NewForgeCAPIMigrationController(
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	machineAdopter capiMachineAdopter,
) *ForgeCAPIMigrationController {
	return &ForgeCAPIMigrationController{
		logger:         logger,
		db:             dbInstance,
		k8sClient:      k8sClient,
		machineAdopter: machineAdopter,
	}
}

func (c *ForgeCAPIMigrationController) Reconcile(ctx context.Context, site db.Site) error {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	defer func() {
		if err := c.recordCoverage(ctx, site, clusters); err != nil {
			c.logger.Error("Failed to record CAPI adoption coverage", "error", err, "siteID", site.ID)
		}
	}()

	var reconcileErrs []error
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			c.logger.Debug("Cluster is deleting, skipping Forge CAPI migration", "clusterID", cluster.ID, "clusterName", cluster.Name)
			continue
		}

		manifest, err := capi.ClusterScaffoldManifest(cluster)
		if err != nil {
			c.logger.Error("Failed to resolve CAPI cluster scaffold manifest", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		if capi.IsEmptyManifest(manifest) {
			continue
		}

		// Forge lookups stay outside the row lock so cache misses do not burn
		// the teardown mutex or the k8s apply deadline.
		var prefetchedInstances map[uuid.UUID]*forgesdk.Instance
		if cluster.ProviderType == db.ProviderTypeNico {
			var prefetchErr error
			prefetchedInstances, prefetchErr = c.prefetchAdoptedForgeInstances(ctx, cluster)
			if prefetchErr != nil {
				c.logger.Error("Failed to prefetch Forge instances for CAPI adoption", "error", prefetchErr, "clusterID", cluster.ID, "clusterName", cluster.Name)
				reconcileErrs = append(reconcileErrs, prefetchErr)
			}
		}

		// Create a timeout for the migration.
		// The cluster row lock is the coarse mutex against teardown, scale, and live deletes.
		// Without a deadline, a hung apiserver would leave the transaction open (idle_in_transaction) and block those paths indefinitely.
		reconcileCtx, cancel := context.WithTimeout(ctx, capiAdoptionTimeout)

		err = c.db.RunInTx(reconcileCtx, func(txCtx context.Context, tx db.Tx) error {
			lockedCluster, err := tx.GetClusterForUpdate(txCtx, cluster.ID)
			if err != nil {
				return fmt.Errorf("lock cluster for CAPI adoption: %w", err)
			}
			return c.reconcileCluster(txCtx, tx, lockedCluster, prefetchedInstances)
		})
		cancel()
		if err != nil {
			c.logger.Error("Failed to reconcile Forge CAPI migration", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	return errors.Join(reconcileErrs...)
}

func (c *ForgeCAPIMigrationController) reconcileCluster(ctx context.Context, tx db.Tx, cluster db.Cluster, prefetchedInstances map[uuid.UUID]*forgesdk.Instance) error {
	// First migrate any legacy CAPI namespace objects from cluster-<name> to nke-system.
	migrated, err := migrateLegacyCAPINamespaceObjects(ctx, c.k8sClient, cluster.Name)
	if err != nil {
		return fmt.Errorf("migrate legacy CAPI namespace objects: %w", err)
	}
	if migrated {
		// Pause/copy/delete finished this pass. Don't unpause/adopt until the
		// next pass, when the legacy namespace is gone.
		return nil
	}

	// Then unpause caller-authored CAPI clusters, or adopt Forge/nico clusters
	// into CAPI objects in nke-system.
	// After this every cluster in NKE will be CAPI managed
	// InfraMode = CAPI not providerType = CAPI, don't confuse the two.
	// Once this completes across the fleet, we can remove legacy Forge direct management as all infra is managed through capi providers.
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return c.reconcileCAPICluster(ctx, cluster)

	case db.ProviderTypeNico:
		return c.reconcileNicoCluster(ctx, tx, cluster, prefetchedInstances)

	default:
		return nil
	}
}

// reconcileCAPICluster unpauses the Cluster, infrastructure Cluster, and
// Machines if they were paused by migrateLegacyCAPINamespaceObjects.
//
// Machines and NicoMachines are never created here. Provision workflows already
// applied them either into cluster-<name>, then moved, or already into nke-system.
func (c *ForgeCAPIMigrationController) reconcileCAPICluster(ctx context.Context, cluster db.Cluster) error {
	clusterObj := &clusterv1.Cluster{}
	key := ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: cluster.Name}
	if err := c.k8sClient.Get(ctx, key, clusterObj); err != nil {
		return fmt.Errorf("get migrated Cluster %s: %w", cluster.Name, err)
	}

	if err := c.unpauseCluster(ctx, clusterObj); err != nil {
		return err
	}
	return c.unpauseMigratedMachines(ctx, cluster.Name)
}

// reconcileNicoCluster must ensure that for this cluster:
// Cluster, NicoCluster, Machines and NicoMachines exist in nke-system.
// This is what "adopt" a nico cluster into CAPI objects in nke-system.
func (c *ForgeCAPIMigrationController) reconcileNicoCluster(ctx context.Context, tx db.Tx, cluster db.Cluster, prefetchedInstances map[uuid.UUID]*forgesdk.Instance) error {
	nodes, err := tx.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
	if err != nil {
		return fmt.Errorf("list nodes for CAPI adoption: %w", err)
	}

	// Filter nodes to only those that are eligible for CAPI adoption.
	eligibleNodes := make([]db.Node, 0, len(nodes))
	for _, node := range nodes {
		if eligibleForCAPIAdoption(node) {
			eligibleNodes = append(eligibleNodes, node)
		}
	}

	// Of the eligible nodes we should adopt, which ones still lack CAPI objects (Machine/NicoMachine).
	missingNodes, err := c.missingAdoptedMachineObjects(ctx, eligibleNodes)
	if err != nil {
		return err
	}

	clusterObj := &clusterv1.Cluster{}
	key := ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: cluster.Name}
	err = c.k8sClient.Get(ctx, key, clusterObj)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get adopted Cluster %s: %w", cluster.Name, err)
	}
	clusterMissing := apierrors.IsNotFound(err)

	if len(missingNodes) > 0 {
		// Cluster and NicoCluster must exist before Machines: CAPNICo waits
		// for Cluster.InfrastructureRef, and Machine admission needs the
		// Cluster. Keep them paused until the next pass, when Get can see
		// the Machines we just applied. Do not re-apply the sparse scaffold
		// over an existing Cluster; that SSA-drops copied spec fields.
		if clusterMissing {
			if err := capi.ApplyClusterScaffold(ctx, c.k8sClient, cluster, true); err != nil {
				return fmt.Errorf("failed to apply CAPI cluster scaffold: %w", err)
			}
		}

		if err := c.adoptMissingNodes(ctx, tx, cluster, missingNodes, prefetchedInstances); err != nil {
			return err
		}

		// Node migrations finished this pass. Don't unpause until the next pass, when the Machines are created.
		return nil
	}

	// Apply the full Cluster and infrastructure Cluster scaffold if it doesn't exist.
	if clusterMissing {
		if err := capi.ApplyClusterScaffold(ctx, c.k8sClient, cluster, false); err != nil {
			return fmt.Errorf("failed to apply CAPI cluster scaffold: %w", err)
		}
		return c.unpauseMigratedMachines(ctx, cluster.Name)
	}

	// All Nodes and Cluster objects have been adopted. Unpause the Cluster and Machines.
	if err := c.unpauseCluster(ctx, clusterObj); err != nil {
		return err
	}
	if err := c.unpauseMigratedMachines(ctx, cluster.Name); err != nil {
		return err
	}

	return nil
}

// eligibleForCAPIAdoption reports whether CAPI objects may be applied for this
// node now. Adoption waits for the node to be provisioned because
// WaitForForgeInstanceReady writes the instance IP label while provisioning is
// still running, and Forge label updates replace the whole label map rather
// than merging: CAPNICo's own topology label write would race it and one set of
// labels would be lost.
//
// A node pool and node set are required because ApplyAdoptedForgeCAPIMachine
// looks them up. No-nodepool control-plane nodes (null pool/set) still count as
// remaining work via expectsCAPIAdoption.
func eligibleForCAPIAdoption(node db.Node) bool {
	return expectsCAPIAdoption(node) &&
		node.NodePoolID != uuid.Nil &&
		node.NodeSetID != uuid.Nil &&
		node.Conditions.Provisioned.Status == db.ConditionStatusTrue
}

// expectsCAPIAdoption reports whether the node should ever gain CAPI objects,
// ignoring whether it is ready for them yet. A deleting node is excluded
// permanently: TeardownNode deletes the Machine before this controller would
// create it, so adopting one leaves a Machine and NicoMachine that no teardown
// will revisit once the node row is gone.
func expectsCAPIAdoption(node db.Node) bool {
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return false
	}
	return node.Nico != nil && node.Nico.ForgeInstanceID != ""
}

func (c *ForgeCAPIMigrationController) prefetchAdoptedForgeInstances(ctx context.Context, cluster db.Cluster) (map[uuid.UUID]*forgesdk.Instance, error) {
	nodes, err := c.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
	if err != nil {
		return nil, fmt.Errorf("list nodes for Forge instance prefetch: %w", err)
	}

	eligibleNodes := make([]db.Node, 0, len(nodes))
	for _, node := range nodes {
		if eligibleForCAPIAdoption(node) {
			eligibleNodes = append(eligibleNodes, node)
		}
	}

	missingNodes, err := c.missingAdoptedMachineObjects(ctx, eligibleNodes)
	if err != nil {
		return nil, err
	}

	instances := make(map[uuid.UUID]*forgesdk.Instance, len(missingNodes))
	var lookupErrs []error
	for i := range missingNodes {
		node := missingNodes[i]
		instance, err := c.machineAdopter.LookupForgeInstanceForAdoption(ctx, cluster, node)
		if err != nil {
			lookupErrs = append(lookupErrs, fmt.Errorf("lookup Forge instance for node %s: %w", node.Name, err))
			continue
		}
		instances[node.ID] = instance
	}
	return instances, errors.Join(lookupErrs...)
}

func (c *ForgeCAPIMigrationController) adoptMissingNodes(
	ctx context.Context,
	tx db.Tx,
	cluster db.Cluster,
	missingNodes []db.Node,
	prefetchedInstances map[uuid.UUID]*forgesdk.Instance,
) error {
	var adoptErrs []error
	for i := range missingNodes {
		node := missingNodes[i]
		instance, ok := prefetchedInstances[node.ID]
		if !ok {
			adoptErrs = append(adoptErrs, fmt.Errorf("no prefetched Forge instance for node %s", node.Name))
			continue
		}

		nodePool, err := tx.GetNodePool(ctx, node.NodePoolID)
		if err != nil {
			adoptErrs = append(adoptErrs, fmt.Errorf("get node pool %s for CAPI adoption: %w", node.NodePoolID, err))
			continue
		}
		nodeSet, err := tx.GetNodeSet(ctx, node.NodeSetID)
		if err != nil {
			adoptErrs = append(adoptErrs, fmt.Errorf("get node set %s for CAPI adoption: %w", node.NodeSetID, err))
			continue
		}
		if err := c.machineAdopter.ApplyAdoptedForgeCAPIMachine(ctx, cluster, node, nodeSet, nodePool, instance); err != nil {
			adoptErrs = append(adoptErrs, fmt.Errorf("adopt node %s into CAPI: %w", node.Name, err))
		}
	}
	return errors.Join(adoptErrs...)
}

func (c *ForgeCAPIMigrationController) missingAdoptedMachineObjects(ctx context.Context, eligibleNodes []db.Node) ([]db.Node, error) {
	missing := make([]db.Node, 0, len(eligibleNodes))
	for _, eligibleNode := range eligibleNodes {
		machine := &clusterv1.Machine{}
		if err := c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: eligibleNode.Name}, machine); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, eligibleNode)
				continue
			}
			return nil, fmt.Errorf("get adopted Machine %s: %w", eligibleNode.Name, err)
		}

		nicoMachine := &nicov1.NicoMachine{}
		if err := c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: eligibleNode.Name}, nicoMachine); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, eligibleNode)
				continue
			}
			return nil, fmt.Errorf("get adopted NicoMachine %s: %w", eligibleNode.Name, err)
		}
		if failedNicoMachineAdoption(nicoMachine) {
			missing = append(missing, eligibleNode)
		}
	}
	return missing, nil
}

func failedNicoMachineAdoption(nicoMachine *nicov1.NicoMachine) bool {
	if nicoMachine.Spec.ProviderID == "" || nicoMachine.Status.InstanceID != "" {
		return false
	}

	ready := apimeta.FindStatusCondition(nicoMachine.Status.Conditions, clusterv1.ReadyCondition)
	return ready != nil &&
		ready.Status == metav1.ConditionFalse &&
		ready.Reason == clusterv1.InspectionFailedReason
}

func (c *ForgeCAPIMigrationController) unpauseCluster(ctx context.Context, clusterObj *clusterv1.Cluster) error {
	_, hasPausedAnnotation := clusterObj.GetAnnotations()[clusterv1.PausedAnnotation]
	specPaused := clusterObj.Spec.Paused != nil && *clusterObj.Spec.Paused

	if hasPausedAnnotation || specPaused {
		base := clusterObj.DeepCopy()
		capi.SetPausedAnnotation(clusterObj, false)
		clusterObj.Spec.Paused = ptr.To(false)

		if err := c.k8sClient.Patch(ctx, clusterObj, ctrlclient.MergeFrom(base)); err != nil {
			return fmt.Errorf("unpause Cluster %s: %w", clusterObj.Name, err)
		}
	}

	// Unpause the infrastructure cluster.
	if !clusterObj.Spec.InfrastructureRef.IsDefined() {
		return nil
	}
	infraCluster, err := capiexternal.GetObjectFromContractVersionedRef(ctx, c.k8sClient, clusterObj.Spec.InfrastructureRef, constants.NamespaceNKESystem)
	if err != nil {
		return fmt.Errorf("get migrated infrastructure cluster %s: %w", clusterObj.Name, err)
	}
	return unpause(ctx, c.k8sClient, infraCluster)
}

func (c *ForgeCAPIMigrationController) unpauseMigratedMachines(ctx context.Context, clusterName string) error {
	machines := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, machines, ctrlclient.InNamespace(constants.NamespaceNKESystem), ctrlclient.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
		return fmt.Errorf("list migrated Machines: %w", err)
	}

	for _, machine := range machines.Items {
		if err := unpause(ctx, c.k8sClient, &machine); err != nil {
			return err
		}

		ref := machine.Spec.InfrastructureRef
		if !ref.IsDefined() {
			continue
		}
		infraMachine, err := capiexternal.GetObjectFromContractVersionedRef(ctx, c.k8sClient, ref, constants.NamespaceNKESystem)
		if err != nil {
			return fmt.Errorf("get migrated infrastructure machine %s: %w", machine.Name, err)
		}
		if err := unpause(ctx, c.k8sClient, infraMachine); err != nil {
			return err
		}
	}
	return nil
}

func unpause(ctx context.Context, k8sClient ctrlclient.Client, obj ctrlclient.Object) error {
	annotations := obj.GetAnnotations()
	if _, ok := annotations[clusterv1.PausedAnnotation]; !ok {
		return nil // No paused annotation to remove
	}

	base, ok := obj.DeepCopyObject().(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("deep copy %T does not implement client.Object", obj)
	}
	capi.SetPausedAnnotation(obj, false)
	if err := k8sClient.Patch(ctx, obj, ctrlclient.MergeFrom(base)); err != nil {
		return fmt.Errorf("unpause %T %s: %w", obj, obj.GetName(), err)
	}

	return nil
}

func (c *ForgeCAPIMigrationController) recordCoverage(ctx context.Context, site db.Site, clusters []db.Cluster) error {
	namespaces := &corev1.NamespaceList{}
	if err := c.k8sClient.List(ctx, namespaces); err != nil {
		return fmt.Errorf("list namespaces for orphaned legacy CAPI namespaces: %w", err)
	}

	var clustersRemainingToMigrate float64
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			continue
		}
		if clusterHasLeftoverLegacyNamespace(namespaces.Items, cluster.Name) {
			clustersRemainingToMigrate++
			continue
		}
		if cluster.ProviderType != db.ProviderTypeNico {
			continue
		}
		nodes, err := c.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
		if err != nil {
			clustersRemainingToMigrate++
			continue
		}
		if c.nicoAdoptionIncomplete(ctx, nodes) {
			clustersRemainingToMigrate++
		}
	}

	allClusters, err := c.db.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		return fmt.Errorf("list all clusters for orphaned legacy CAPI namespaces: %w", err)
	}
	orphanedLegacyNamespaces := countOrphanedLegacyNamespaces(namespaces.Items, allClusters)

	metrics.RecordCAPIAdoptionRemaining(site.ID.String(), clustersRemainingToMigrate)
	metrics.RecordCAPIAdoptionOrphanedLegacyNamespaces(site.ID.String(), float64(orphanedLegacyNamespaces))

	return nil
}

// nicoAdoptionIncomplete treats every node expected to gain CAPI objects as
// remaining work until its Machine is ready and its NicoMachine exists. Nodes
// without the Forge and database identity required for adoption are excluded by
// expectsCAPIAdoption.
func (c *ForgeCAPIMigrationController) nicoAdoptionIncomplete(ctx context.Context, nodes []db.Node) bool {
	for i := range nodes {
		if !expectsCAPIAdoption(nodes[i]) {
			continue
		}
		machine := &clusterv1.Machine{}
		err := c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: nodes[i].Name}, machine)
		if err != nil || !capiconditions.IsTrue(machine, clusterv1.MachineReadyCondition) {
			return true
		}

		nicoMachine := &nicov1.NicoMachine{}
		err = c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: constants.NamespaceNKESystem, Name: nodes[i].Name}, nicoMachine)
		if err != nil {
			return true
		}
	}

	// All nodes are adopted.
	return false
}

func countOrphanedLegacyNamespaces(namespaces []corev1.Namespace, clusters []db.Cluster) int {
	clusterNames := make(map[string]struct{}, len(clusters))
	for i := range clusters {
		clusterNames[clusters[i].Name] = struct{}{}
	}

	var orphanedLegacyNamespaces int
	for _, namespace := range namespaces {
		name := namespace.Name
		if !strings.HasPrefix(name, "cluster-") {
			continue
		}
		if _, exists := clusterNames[strings.TrimPrefix(name, "cluster-")]; exists {
			continue
		}
		orphanedLegacyNamespaces++
	}

	return orphanedLegacyNamespaces
}

func clusterHasLeftoverLegacyNamespace(namespaces []corev1.Namespace, clusterName string) bool {
	legacy := capi.LegacyClusterNamespace(clusterName)
	for i := range namespaces {
		if namespaces[i].Name == legacy {
			return true
		}
	}
	return false
}
