// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/spiffe"
)

const (
	// orphanSweepInterval is how often managed entries are checked for a vanished Machine.
	orphanSweepInterval = 10 * time.Minute
	// orphanEntryGracePeriod keeps the sweep off entries young enough to race their Machine into the cache.
	orphanEntryGracePeriod = time.Hour
	// legacyCWEManagedBy is the managed-by value the pre-import CWE wrote on host entries; its GC went with it.
	legacyCWEManagedBy = "cloud-workflow-engine"
)

type MachineSpireEntryController struct {
	cfg         config.Config
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

func NewMachineSpireEntryController(cfg config.Config, k8sClient client.Client, logger *slog.Logger, trustDomain string) *MachineSpireEntryController {
	return &MachineSpireEntryController{
		cfg:         cfg,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

func (c *MachineSpireEntryController) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(manager.RunnableFunc(c.runOrphanSweep)); err != nil {
		return fmt.Errorf("failed to add orphan ClusterStaticEntry sweep: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}).
		Named("machine-spire-entry").
		Complete(c)
}

func (c *MachineSpireEntryController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("machineNamespace", req.Namespace, "machineName", req.Name)

	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get machine: %w", err)
	}

	hostID := keys.MachineHostID.Value(machine.GetAnnotations())
	if !machine.DeletionTimestamp.IsZero() {
		if hostID == "" {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, c.deleteClusterStaticEntry(ctx, hostID)
	}

	if hostID == "" {
		logger.Debug("Machine has no host-id annotation, skipping ClusterStaticEntry reconciliation")
		return ctrl.Result{}, nil
	}

	spireAgentIDPath := keys.MachineSpireAgentSpiffeIDPath.Value(machine.GetAnnotations())
	if spireAgentIDPath == "" {
		logger.Info("Waiting for spire agent ID path to be set on Machine", "hostID", hostID)
		return ctrl.Result{}, nil
	}
	parentID := spiffeIDFromPath(c.trustDomain, spireAgentIDPath)
	nodeID, err := uuid.Parse(hostID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid host ID %q: %w", hostID, err)
	}
	agentConnection, ok := keys.MachineAgentConnection.Get(machine.GetAnnotations())
	if !ok {
		logger.Info("Waiting for agent-connection annotation to be set on Machine", "hostID", hostID)
		return ctrl.Result{}, nil
	}

	logger.Info("Applying ClusterStaticEntry for machine", "hostID", hostID, "parentID", parentID)
	cseName := machineSpireEntryName(hostID)
	workloadSpiffeID := spiffe.NodeAgentID(
		c.trustDomain,
		c.cfg.NodeAgent.TLSJWT.SubjectTenant,
		c.cfg.NodeAgent.TLSJWT.SubjectService,
		nodeID,
		strings.HasSuffix(agentConnection, keys.MachineAgentConnectionJWTSuffix),
	)
	if err := c.applyClusterStaticEntry(ctx, cseName, workloadSpiffeID, parentID); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("ClusterStaticEntry applied for machine", "hostID", hostID, "parentID", parentID)

	// A re-provisioned host keeps its TPM parent ID, so a leaked entry for the old host ID would compete for the SVID.
	if err := c.deleteOrphanedSiblings(ctx, cseName, parentID); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// deleteOrphanedSiblings deletes managed entries sharing parentID with keepName whose host ID has no live Machine.
func (c *MachineSpireEntryController) deleteOrphanedSiblings(ctx context.Context, keepName, parentID string) error {
	entries, err := c.listManagedEntries(ctx)
	if err != nil {
		return err
	}
	var siblings []*spirev1alpha1.ClusterStaticEntry
	for i := range entries {
		if entries[i].Name != keepName && entries[i].Spec.ParentID == parentID {
			siblings = append(siblings, &entries[i])
		}
	}
	if len(siblings) == 0 {
		return nil
	}

	live, err := c.machineEntryNames(ctx, false)
	if err != nil {
		return err
	}
	for _, entry := range siblings {
		if _, ok := live[entry.Name]; ok {
			continue
		}
		if err := c.deleteEntry(ctx, entry, "sibling entry has no live Machine"); err != nil {
			return err
		}
	}
	return nil
}

func (c *MachineSpireEntryController) runOrphanSweep(ctx context.Context) error {
	ticker := time.NewTicker(orphanSweepInterval)
	defer ticker.Stop()
	for {
		if err := c.sweepOrphanedEntries(ctx, time.Now()); err != nil {
			c.logger.Error("Failed to sweep orphaned ClusterStaticEntries", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// sweepOrphanedEntries deletes managed entries older than the grace period whose host ID matches no Machine.
func (c *MachineSpireEntryController) sweepOrphanedEntries(ctx context.Context, now time.Time) error {
	entries, err := c.listManagedEntries(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	existing, err := c.machineEntryNames(ctx, true)
	if err != nil {
		return err
	}
	for i := range entries {
		entry := &entries[i]
		if _, ok := existing[entry.Name]; ok {
			continue
		}
		if now.Sub(entry.CreationTimestamp.Time) < orphanEntryGracePeriod {
			continue
		}
		if err := c.deleteEntry(ctx, entry, "entry has no Machine"); err != nil {
			return err
		}
	}
	return nil
}

// listManagedEntries returns the host ClusterStaticEntries this controller, or the pre-import CWE, labeled.
func (c *MachineSpireEntryController) listManagedEntries(ctx context.Context) ([]spirev1alpha1.ClusterStaticEntry, error) {
	list := &spirev1alpha1.ClusterStaticEntryList{}
	if err := c.k8sClient.List(ctx, list); err != nil {
		return nil, fmt.Errorf("failed to list cluster static entries: %w", err)
	}
	managed := make([]spirev1alpha1.ClusterStaticEntry, 0, len(list.Items))
	for _, entry := range list.Items {
		if entry.DeletionTimestamp.IsZero() && isManagedHostEntry(&entry) {
			managed = append(managed, entry)
		}
	}
	return managed, nil
}

func isManagedHostEntry(entry *spirev1alpha1.ClusterStaticEntry) bool {
	labels := entry.GetLabels()
	if keys.ManagedBy.Value(labels) == constants.FieldManagerMachineSpireEntry {
		return true
	}
	return labels[keys.ManagedBy.Legacy()] == legacyCWEManagedBy && strings.HasPrefix(entry.Name, machineSpireEntryName(""))
}

// machineEntryNames returns the entry names of Machines in all namespaces, skipping deleting ones unless includeDeleting.
func (c *MachineSpireEntryController) machineEntryNames(ctx context.Context, includeDeleting bool) (map[string]struct{}, error) {
	machines := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, machines); err != nil {
		return nil, fmt.Errorf("failed to list machines: %w", err)
	}
	names := make(map[string]struct{}, len(machines.Items))
	for i := range machines.Items {
		machine := &machines.Items[i]
		if !includeDeleting && !machine.DeletionTimestamp.IsZero() {
			continue
		}
		if hostID := keys.MachineHostID.Value(machine.GetAnnotations()); hostID != "" {
			names[machineSpireEntryName(hostID)] = struct{}{}
		}
	}
	return names, nil
}

func (c *MachineSpireEntryController) deleteEntry(ctx context.Context, entry *spirev1alpha1.ClusterStaticEntry, reason string) error {
	if err := c.k8sClient.Delete(ctx, entry, client.Preconditions{UID: &entry.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster static entry %s: %w", entry.Name, err)
	}
	c.logger.Info("Deleted orphaned ClusterStaticEntry",
		"name", entry.Name, "parentID", entry.Spec.ParentID, "spiffeID", entry.Spec.SPIFFEID, "reason", reason)
	return nil
}

func (c *MachineSpireEntryController) applyClusterStaticEntry(ctx context.Context, name, workloadSpiffeID, parentID string) error {
	cse := &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       "ClusterStaticEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				keys.ManagedBy.String(): constants.FieldManagerMachineSpireEntry,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: c.cfg.SpireClassName,
			ParentID:  parentID,
			Selectors: []string{"unix:uid:0"},
			SPIFFEID:  workloadSpiffeID,
		},
	}

	if err := c.k8sClient.Patch(
		ctx,
		cse,
		client.Apply,
		client.FieldOwner(constants.FieldManagerMachineSpireEntry),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to apply cluster static entry %s: %w", name, err)
	}

	return nil
}

func (c *MachineSpireEntryController) deleteClusterStaticEntry(ctx context.Context, hostID string) error {
	cse := &spirev1alpha1.ClusterStaticEntry{}
	cse.SetName(machineSpireEntryName(hostID))
	if err := c.k8sClient.Delete(ctx, cse); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster static entry %s: %w", cse.GetName(), err)
	}
	return nil
}

func machineSpireEntryName(hostID string) string {
	return fmt.Sprintf("host-%s", hostID)
}

func spiffeIDFromPath(trustDomain, path string) string {
	if strings.HasPrefix(path, "/") {
		return fmt.Sprintf("spiffe://%s%s", trustDomain, path)
	}
	return fmt.Sprintf("spiffe://%s/%s", trustDomain, path)
}
