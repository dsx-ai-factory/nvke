// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/ipam"
)

const (
	scopeTypeSubnet    = "subnet"
	scopeTypeVPCPrefix = "vpc_prefix"
)

type InterfaceRequest struct {
	SubnetID    *string
	VPCPrefixID *string
}

// CreateInstanceInput contains common and backend-specific instance state.
type CreateInstanceInput struct {
	Backend                SiteBackend
	SiteID                 string
	Name                   string
	TenantID               string
	InstanceTypeID         string
	VPCID                  string
	MachineID              string
	IpxeScript             string
	UserData               string
	Labels                 map[string]string
	SSHKeyGroupIDs         []string
	Interfaces             []InterfaceRequest
	InfiniBandPartitionIDs []string
	PhoneHomeEnabled       bool
	VM                     *VMState
	Container              *ContainerState
}

type UpdateInstanceInput struct {
	Labels        map[string]string
	TriggerReboot bool
	IpxeScript    *string
	UserData      *string
}

type ListInstancesFilter struct {
	Backend  SiteBackend
	SiteID   string
	TenantID string
	Name     string
	VPCID    string
}

func selectInstances(db bun.IDB, model any) *bun.SelectQuery {
	return db.NewSelect().
		Model(model).
		Relation("VM").
		Relation("Container").
		Relation("Interfaces", func(query *bun.SelectQuery) *bun.SelectQuery {
			return query.Order("created_at ASC")
		})
}

func validateHydratedInstances(instances []instanceRow) error {
	for _, instance := range instances {
		if err := validateHydratedInstance(instance); err != nil {
			return err
		}
	}
	return nil
}

func validateHydratedInstance(instance instanceRow) error {
	switch instance.Backend {
	case SiteBackendVM:
		if instance.VM == nil || instance.Container != nil {
			return fmt.Errorf("%w: VM instance %s has invalid backend state", ErrOwnershipConflict, instance.ID)
		}
	case SiteBackendContainer:
		if instance.Container == nil || instance.VM != nil {
			return fmt.Errorf("%w: container instance %s has invalid backend state", ErrOwnershipConflict, instance.ID)
		}
	default:
		return fmt.Errorf("%w: instance %s has unsupported backend %q", ErrOwnershipConflict, instance.ID, instance.Backend)
	}
	return nil
}

// ListInstances returns one globally ordered page across all backends.
func (s *Store) ListInstances(ctx context.Context, filter ListInstancesFilter, page Page) ([]Instance, error) {
	page = normalizePage(page)
	var instances []instanceRow
	query := selectInstances(s.db, &instances).
		Order("instance.created_at ASC").
		Order("instance.id ASC").
		Limit(int64(page.Size)).
		Offset(int64((page.Number - 1) * page.Size))
	if filter.Backend != "" {
		query = query.Where("instance.backend = ?", filter.Backend)
	}
	if filter.SiteID != "" {
		query = query.Where("instance.site_id = ?", filter.SiteID)
	}
	if filter.TenantID != "" {
		query = query.Where("instance.tenant_id = ?", filter.TenantID)
	}
	if filter.Name != "" {
		query = query.Where("instance.name = ?", filter.Name)
	}
	if filter.VPCID != "" {
		query = query.Where("instance.vpc_id = ?", filter.VPCID)
	}
	if err := query.Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	if err := validateHydratedInstances(instances); err != nil {
		return nil, err
	}
	return instances, nil
}

// GetInstance returns a fully hydrated instance.
func (s *Store) GetInstance(ctx context.Context, id string) (Instance, error) {
	var instance instanceRow
	if err := selectInstances(s.db, &instance).Where("instance.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, ErrNotFound
		}
		return Instance{}, fmt.Errorf("failed to get instance: %w", err)
	}
	if err := validateHydratedInstance(instance); err != nil {
		return Instance{}, err
	}
	return instance, nil
}

// CreateInstance persists common and backend-specific state in one transaction.
func (s *Store) CreateInstance(ctx context.Context, input CreateInstanceInput) (Instance, bool, error) {
	if err := validateCreateInstanceInput(input); err != nil {
		return Instance{}, false, err
	}
	var output Instance
	created := false
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := validateInstanceRecordOwnershipTx(ctx, tx, input); err != nil {
			return err
		}
		existing, err := getInstanceBySiteAndNameTx(ctx, tx, input.SiteID, input.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			existingLabels, labelsErr := existing.Labels()
			if labelsErr != nil {
				return labelsErr
			}
			if existing.Backend == input.Backend && sameLogicalOwner(existingLabels, input.Labels) {
				output = existing
				return nil
			}
			return fmt.Errorf("%w: an instance with specified name already exists for site", ErrConflict)
		}
		output, err = insertInstanceTx(ctx, tx, input, s.nowFn().UTC())
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Instance{}, false, err
	}
	return output, created, nil
}

func validateCreateInstanceInput(input CreateInstanceInput) error {
	if input.SiteID == "" || input.Name == "" || input.TenantID == "" || input.InstanceTypeID == "" || input.VPCID == "" {
		return fmt.Errorf("%w: siteId, name, tenantId, instanceTypeId, and vpcId are required", ErrBadRequest)
	}
	if len(input.Interfaces) == 0 {
		return fmt.Errorf("%w: at least one interface is required", ErrBadRequest)
	}
	switch input.Backend {
	case SiteBackendVM:
		if input.VM == nil || input.Container != nil {
			return fmt.Errorf("%w: VM instances require only VM state", ErrBadRequest)
		}
	case SiteBackendContainer:
		if input.Container == nil || input.VM != nil {
			return fmt.Errorf("%w: container instances require only container state", ErrBadRequest)
		}
	default:
		return fmt.Errorf("%w: unsupported instance backend %q", ErrBadRequest, input.Backend)
	}
	return nil
}

// validateInstanceRecordOwnershipTx checks database relationships immediately
// before inserting the instance record.
func validateInstanceRecordOwnershipTx(ctx context.Context, tx bun.Tx, input CreateInstanceInput) error {
	var vpc VPCRow
	if err := tx.NewSelect().Model(&vpc).Where("id = ?", input.VPCID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: vpc %s not found", ErrBadRequest, input.VPCID)
		}
		return fmt.Errorf("failed to resolve vpc: %w", err)
	}
	if vpc.TenantID != input.TenantID || vpc.SiteID != input.SiteID {
		return fmt.Errorf("%w: vpc %s does not belong to tenant %s and site %s", ErrBadRequest, input.VPCID, input.TenantID, input.SiteID)
	}
	var site SiteRow
	if err := tx.NewSelect().Model(&site).Where("id = ?", input.SiteID).Scan(ctx); err != nil {
		return fmt.Errorf("failed to resolve site %s: %w", input.SiteID, err)
	}
	if site.TenantID != input.TenantID || site.Backend != input.Backend {
		return fmt.Errorf("%w: site %s does not match tenant %s and backend %s", ErrBadRequest, input.SiteID, input.TenantID, input.Backend)
	}

	var instanceType InstanceTypeRow
	if err := tx.NewSelect().Model(&instanceType).Where("id = ?", input.InstanceTypeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: instance type %s not found", ErrBadRequest, input.InstanceTypeID)
		}
		return fmt.Errorf("failed to resolve instance type: %w", err)
	}
	if instanceType.SiteID != input.SiteID {
		return fmt.Errorf("%w: instance type %s does not belong to site %s", ErrBadRequest, input.InstanceTypeID, input.SiteID)
	}

	for i, iface := range input.Interfaces {
		switch {
		case iface.SubnetID != nil && iface.VPCPrefixID != nil:
			return fmt.Errorf("%w: interface %d must not set both subnetId and vpcPrefixId", ErrBadRequest, i)
		case iface.SubnetID != nil:
			var subnet SubnetRow
			if err := tx.NewSelect().Model(&subnet).Where("id = ?", *iface.SubnetID).Scan(ctx); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: subnet %s not found", ErrBadRequest, *iface.SubnetID)
				}
				return fmt.Errorf("failed to resolve subnet: %w", err)
			}
			if subnet.SiteID != input.SiteID || subnet.VPCID != input.VPCID {
				return fmt.Errorf("%w: subnet %s does not belong to vpc %s and site %s", ErrBadRequest, subnet.ID, input.VPCID, input.SiteID)
			}
		case iface.VPCPrefixID != nil:
			var prefix VPCPrefixRow
			if err := tx.NewSelect().Model(&prefix).Where("id = ?", *iface.VPCPrefixID).Scan(ctx); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: vpc prefix %s not found", ErrBadRequest, *iface.VPCPrefixID)
				}
				return fmt.Errorf("failed to resolve vpc prefix: %w", err)
			}
			if prefix.SiteID != input.SiteID || prefix.VPCID != input.VPCID {
				return fmt.Errorf("%w: vpc prefix %s does not belong to vpc %s and site %s", ErrBadRequest, prefix.ID, input.VPCID, input.SiteID)
			}
		default:
			return fmt.Errorf("%w: interface %d must set subnetId or vpcPrefixId", ErrBadRequest, i)
		}
	}

	for _, groupID := range input.SSHKeyGroupIDs {
		var group SSHKeyGroupRow
		if err := tx.NewSelect().Model(&group).Where("id = ?", groupID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: ssh key group %s not found", ErrBadRequest, groupID)
			}
			return fmt.Errorf("failed to resolve ssh key group: %w", err)
		}
		siteIDs, err := group.SiteIDs()
		if err != nil {
			return err
		}
		if !slices.Contains(siteIDs, input.SiteID) {
			return fmt.Errorf("%w: ssh key group %s does not belong to site %s", ErrBadRequest, groupID, input.SiteID)
		}
	}

	for _, partitionID := range input.InfiniBandPartitionIDs {
		var partition InfiniBandPartitionRow
		if err := tx.NewSelect().Model(&partition).Where("id = ?", partitionID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: infiniband partition %s not found", ErrBadRequest, partitionID)
			}
			return fmt.Errorf("failed to resolve infiniband partition: %w", err)
		}
		if partition.SiteID != input.SiteID {
			return fmt.Errorf("%w: infiniband partition %s does not belong to site %s", ErrBadRequest, partitionID, input.SiteID)
		}
	}
	return nil
}

func insertInstanceTx(
	ctx context.Context,
	tx bun.Tx,
	input CreateInstanceInput,
	now time.Time,
) (Instance, error) {
	labelsJSON, err := json.Marshal(input.Labels)
	if err != nil {
		return Instance{}, fmt.Errorf("failed to marshal labels: %w", err)
	}
	sshKeyGroupIDsJSON, err := json.Marshal(input.SSHKeyGroupIDs)
	if err != nil {
		return Instance{}, fmt.Errorf("failed to marshal ssh key group ids: %w", err)
	}
	instance := instanceRow{
		ID:                 uuid.NewString(),
		Name:               input.Name,
		Backend:            input.Backend,
		TenantID:           input.TenantID,
		SiteID:             input.SiteID,
		InstanceTypeID:     input.InstanceTypeID,
		VPCID:              input.VPCID,
		MachineID:          input.MachineID,
		IpxeScript:         input.IpxeScript,
		UserData:           input.UserData,
		LabelsJSON:         string(labelsJSON),
		SSHKeyGroupIDsJSON: string(sshKeyGroupIDsJSON),
		Status:             "provisioning",
		PhoneHomeEnabled:   input.PhoneHomeEnabled,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if _, err := tx.NewInsert().Model(&instance).Exec(ctx); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Instance{}, fmt.Errorf("%w: an instance with specified name already exists for site", ErrConflict)
		}
		return Instance{}, fmt.Errorf("failed to insert instance record: %w", err)
	}
	if input.VM != nil {
		vm := *input.VM
		vm.InstanceID = instance.ID
		if _, err := tx.NewInsert().Model(&vm).Exec(ctx); err != nil {
			return Instance{}, fmt.Errorf("failed to insert VM record: %w", err)
		}
		instance.VM = &vm
	}
	if input.Container != nil {
		container := *input.Container
		container.InstanceID = instance.ID
		if _, err := tx.NewInsert().Model(&container).Exec(ctx); err != nil {
			return Instance{}, fmt.Errorf("failed to insert container record: %w", err)
		}
		instance.Container = &container
	}
	interfaces, err := insertInstanceInterfacesTx(ctx, tx, instance.ID, input.Interfaces, now)
	if err != nil {
		return Instance{}, err
	}
	instance.Interfaces = interfaces
	return instance, nil
}

// AllocateInterfaceIPs resolves the CIDR for the given InterfaceRequest and
// allocates IPs from the matching subnet or VPC prefix within the supplied
// transaction. Exported so HTTP handlers or other callers can invoke it
// directly when they need to pre-allocate IPs before creating a VM instance.
func (s *Store) AllocateInterfaceIPs(
	ctx context.Context,
	tx bun.Tx,
	ifaceReq InterfaceRequest,
	instanceID, interfaceID string,
) ([]string, error) {
	var (
		cidr      string
		scopeType string
		scopeID   string
		ipCount   int
	)

	if ifaceReq.SubnetID != nil {
		scopeType = scopeTypeSubnet
		scopeID = *ifaceReq.SubnetID
		ipCount = 1

		var subnet SubnetRow
		if err := tx.NewSelect().Model(&subnet).Where("id = ?", *ifaceReq.SubnetID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: subnet %s not found", ErrBadRequest, *ifaceReq.SubnetID)
			}
			return nil, fmt.Errorf("failed to get subnet %s: %w", *ifaceReq.SubnetID, err)
		}
		cidr = subnet.CIDR
	}

	if ifaceReq.VPCPrefixID != nil {
		scopeType = scopeTypeVPCPrefix
		scopeID = *ifaceReq.VPCPrefixID
		ipCount = 2

		var pfx VPCPrefixRow
		if err := tx.NewSelect().Model(&pfx).Where("id = ?", *ifaceReq.VPCPrefixID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: vpcPrefix %s not found", ErrBadRequest, *ifaceReq.VPCPrefixID)
			}
			return nil, fmt.Errorf("failed to get vpc prefix %s: %w", *ifaceReq.VPCPrefixID, err)
		}
		cidr = pfx.Prefix
	}

	return s.allocateIPsTx(ctx, tx, scopeType, scopeID, cidr, ipCount, instanceID, interfaceID)
}

// UpdateInstance updates common fields and, when requested, VM reboot state.
func (s *Store) UpdateInstance(ctx context.Context, id string, input UpdateInstanceInput) (Instance, error) {
	var out Instance
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		instance, err := getInstanceTx(ctx, tx, id)
		if err != nil {
			return err
		}
		labels, err := instance.Labels()
		if err != nil {
			return err
		}
		for k, v := range input.Labels {
			labels[k] = v
		}
		labelsJSON, err := json.Marshal(labels)
		if err != nil {
			return fmt.Errorf("failed to marshal merged labels: %w", err)
		}
		instance.LabelsJSON = string(labelsJSON)

		if input.TriggerReboot {
			if instance.VM == nil {
				return fmt.Errorf("%w: instance %s is not VM-backed", ErrBadRequest, id)
			}
			instance.VM.RebootRequested = true
			if instance.VM.ObservedState == ObservedStateReady {
				instance.VM.ObservedState = ObservedStateRebooting
			}
			instance.Status = "rebooting"
		}
		if input.IpxeScript != nil {
			instance.IpxeScript = *input.IpxeScript
		}
		if input.UserData != nil {
			instance.UserData = *input.UserData
		}
		instance.UpdatedAt = s.nowFn().UTC()

		if _, err := tx.NewUpdate().
			Model(&instance).
			Where("id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to update instance %s: %w", id, err)
		}
		if instance.VM != nil {
			if _, err := tx.NewUpdate().Model(instance.VM).WherePK().Exec(ctx); err != nil {
				return fmt.Errorf("failed to update VM %s: %w", id, err)
			}
		}
		out = instance
		return nil
	})
	if err != nil {
		return Instance{}, err
	}
	return out, nil
}

// MarkVMInstanceForDelete requests asynchronous deletion by the VM reconciler.
func (s *Store) MarkVMInstanceForDelete(ctx context.Context, id string) (bool, error) {
	found := false
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().
			Model((*VMState)(nil)).
			Set("desired_state = ?", DesiredStateAbsent).
			Set("observed_state = CASE WHEN observed_state = ? THEN ? ELSE observed_state END", ObservedStateReady, ObservedStateDeleting).
			Where("instance_id = ?", id).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to mark VM for delete: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed getting rows affected: %w", err)
		}
		found = affected > 0
		if !found {
			return nil
		}
		if _, err := tx.NewUpdate().
			Model((*instanceRow)(nil)).
			Set("status = ?", "deleting").
			Set("updated_at = ?", s.nowFn().UTC()).
			Where("id = ?", id).
			Where("backend = ?", SiteBackendVM).
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to mark instance for delete: %w", err)
		}
		return nil
	})
	return found, err
}

// DeleteInstance removes an instance and all persisted backend state.
func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := getInstanceTx(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*IPAllocationRow)(nil)).Where("instance_id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete ip allocations for %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*instanceRow)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete instance %s: %w", id, err)
		}
		return nil
	})
}

type VMInstanceStatePatch struct {
	MachineID        *string
	Status           *string
	ObservedState    *string
	ErrorReason      *string
	RebootRequested  *bool
	TpmEkCertificate *string
}

// PatchVMInstanceState updates fields owned by the VM reconciler.
func (s *Store) PatchVMInstanceState(ctx context.Context, id string, patch VMInstanceStatePatch) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		instance, err := getInstanceTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if instance.VM == nil {
			return ErrNotFound
		}

		if patch.MachineID != nil {
			instance.MachineID = *patch.MachineID
		}
		if patch.Status != nil {
			instance.Status = *patch.Status
		}
		if patch.ObservedState != nil {
			instance.VM.ObservedState = *patch.ObservedState
		}
		if patch.ErrorReason != nil {
			instance.VM.ErrorReason = *patch.ErrorReason
		}
		if patch.RebootRequested != nil {
			instance.VM.RebootRequested = *patch.RebootRequested
		}
		if patch.TpmEkCertificate != nil {
			instance.TpmEkCertificate = *patch.TpmEkCertificate
		}
		instance.UpdatedAt = s.nowFn().UTC()

		if _, err := tx.NewUpdate().Model(&instance).Where("id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to patch instance %s: %w", id, err)
		}
		if _, err := tx.NewUpdate().Model(instance.VM).WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("failed to patch VM %s: %w", id, err)
		}
		return nil
	})
}

// ListActiveVMMachineIDs returns all machine_id values for VM instances that are
// not deleted and have a non-empty machine_id. Used at startup to compare
// against the hypervisor's running VMs to detect orphans.
func (s *Store) ListActiveVMMachineIDs(ctx context.Context) (map[string]struct{}, error) {
	type row struct {
		MachineID string `bun:"machine_id"`
	}
	var rows []row
	if err := s.db.NewSelect().
		TableExpr("instances AS instance").
		ColumnExpr("instance.machine_id").
		Join("JOIN vms AS vm ON vm.instance_id = instance.id").
		Where("instance.backend = ?", SiteBackendVM).
		Where("instance.machine_id != ''").
		Where("vm.observed_state != ?", ObservedStateDeleted).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list active machine ids: %w", err)
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r.MachineID] = struct{}{}
	}
	return out, nil
}

// ListVMReconcileCandidates returns IDs that need VM reconciliation.
func (s *Store) ListVMReconcileCandidates(ctx context.Context) ([]string, error) {
	type idRow struct {
		ID string `bun:"id"`
	}
	var rows []idRow

	if err := s.db.NewSelect().
		TableExpr("vms AS vm").
		ColumnExpr("vm.instance_id AS id").
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.WhereOr("(vm.desired_state = ? AND vm.observed_state IN (?, ?, ?, ?, ?) )",
				DesiredStatePresent,
				ObservedStateNew,
				ObservedStateProvisioning,
				ObservedStateRunning,
				ObservedStateRebooting,
				ObservedStateReady,
			).WhereOr("(vm.desired_state = ? AND vm.observed_state != ?)", DesiredStateAbsent, ObservedStateDeleted).
				WhereOr("(vm.desired_state = ? AND vm.reboot_requested = 1)", DesiredStatePresent)
		}).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list reconcile candidates: %w", err)
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

func getInstanceTx(ctx context.Context, tx bun.Tx, id string) (instanceRow, error) {
	var row instanceRow
	if err := selectInstances(tx, &row).Where("instance.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return instanceRow{}, ErrNotFound
		}
		return instanceRow{}, fmt.Errorf("failed to get instance %s: %w", id, err)
	}
	if err := validateHydratedInstance(row); err != nil {
		return instanceRow{}, err
	}
	return row, nil
}

func getInstanceBySiteAndNameTx(ctx context.Context, tx bun.Tx, siteID, name string) (instanceRow, error) {
	var row instanceRow
	if err := selectInstances(tx, &row).
		Where("instance.site_id = ?", siteID).
		Where("instance.name = ?", name).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return instanceRow{}, ErrNotFound
		}
		return instanceRow{}, fmt.Errorf("failed to get instance by site/name: %w", err)
	}
	if err := validateHydratedInstance(row); err != nil {
		return instanceRow{}, err
	}
	return row, nil
}

func insertInstanceInterfacesTx(
	ctx context.Context,
	tx bun.Tx,
	instanceID string,
	requests []InterfaceRequest,
	now time.Time,
) ([]InstanceInterfaceRow, error) {
	interfaces := make([]InstanceInterfaceRow, 0, len(requests))
	for _, request := range requests {
		if (request.SubnetID == nil && request.VPCPrefixID == nil) ||
			(request.SubnetID != nil && request.VPCPrefixID != nil) {
			return nil, fmt.Errorf("%w: exactly one of subnetId or vpcPrefixId must be provided", ErrBadRequest)
		}
		row := InstanceInterfaceRow{
			ID: uuid.NewString(), InstanceID: instanceID, IPAddressesJSON: "[]",
			Status: "provisioning", CreatedAt: now, UpdatedAt: now,
		}
		if request.SubnetID != nil {
			row.SubnetID = *request.SubnetID
		}
		if request.VPCPrefixID != nil {
			row.VPCPrefixID = *request.VPCPrefixID
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return nil, fmt.Errorf("failed to insert instance interface: %w", err)
		}
		interfaces = append(interfaces, row)
	}
	return interfaces, nil
}

func (s *Store) allocateIPsTx(
	ctx context.Context,
	tx bun.Tx,
	scopeType, scopeID, cidr string,
	count int,
	instanceID, interfaceID string,
) ([]string, error) {
	var existing []IPAllocationRow
	if err := tx.NewSelect().
		Model(&existing).
		Where("scope_type = ?", scopeType).
		Where("scope_id = ?", scopeID).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed reading existing allocations: %w", err)
	}

	used := map[string]struct{}{}
	for _, alloc := range existing {
		used[alloc.IPAddress] = struct{}{}
	}

	ips, err := ipam.FirstFreeIPv4(cidr, count, used)
	if err != nil {
		if scopeType == scopeTypeSubnet {
			return nil, fmt.Errorf("%w: Prefix: %s has exhausted all address space", ErrSubnetExhausted, cidr)
		}
		return nil, fmt.Errorf("%w: Internal error: IP address exhausted: %s", ErrVpcPfxExhausted, cidr)
	}

	now := s.nowFn().UTC()
	for _, ip := range ips {
		row := IPAllocationRow{
			ScopeType:   scopeType,
			ScopeID:     scopeID,
			IPAddress:   ip,
			InstanceID:  instanceID,
			InterfaceID: interfaceID,
			CreatedAt:   now,
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			if scopeType == scopeTypeSubnet {
				return nil, fmt.Errorf("%w: Prefix: %s has exhausted all address space", ErrSubnetExhausted, cidr)
			}
			return nil, fmt.Errorf("%w: Internal error: IP address exhausted: %s", ErrVpcPfxExhausted, cidr)
		}
	}
	return ips, nil
}

// UpdateInterfaceIPs sets the IP addresses for an instance interface.
// Used to confirm the actual IP from a DHCP lease or to clear IPs pending confirmation.
func (s *Store) UpdateInterfaceIPs(ctx context.Context, interfaceID string, ips []string) error {
	if ips == nil {
		ips = []string{}
	}
	ipsJSON, err := json.Marshal(ips)
	if err != nil {
		return fmt.Errorf("marshal ips: %w", err)
	}
	_, err = s.db.NewUpdate().
		Model((*InstanceInterfaceRow)(nil)).
		Set("ip_addresses_json = ?", string(ipsJSON)).
		Set("updated_at = ?", s.nowFn().UTC()).
		Where("id = ?", interfaceID).
		Exec(ctx)
	return err
}

// GetAllocatedIPsForInterface returns the IPs originally allocated by IPAM
// for the given interface from the ip_allocations table.
func (s *Store) GetAllocatedIPsForInterface(ctx context.Context, interfaceID string) ([]string, error) {
	var allocations []IPAllocationRow
	err := s.db.NewSelect().
		Model(&allocations).
		Where("interface_id = ?", interfaceID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	ips := make([]string, len(allocations))
	for i, a := range allocations {
		ips[i] = a.IPAddress
	}
	return ips, nil
}

// AllocateAndStoreIPs performs IPAM allocation for an interface and stores
// the allocated IPs. Called by the engine during provisioning for static-IP
// instances. Returns the allocated IPs.
func (s *Store) AllocateAndStoreIPs(ctx context.Context, iface InstanceInterfaceRow, instanceID string) ([]string, error) {
	var ips []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		allocated, allocErr := s.AllocateInterfaceIPs(ctx, tx,
			InterfaceRequest{
				SubnetID:    ptrIfNonEmpty(iface.SubnetID),
				VPCPrefixID: ptrIfNonEmpty(iface.VPCPrefixID),
			},
			instanceID, iface.ID,
		)
		if allocErr != nil {
			return allocErr
		}
		ips = allocated

		ipsJSON, err := json.Marshal(ips)
		if err != nil {
			return fmt.Errorf("marshal ips: %w", err)
		}
		_, err = tx.NewUpdate().
			Model((*InstanceInterfaceRow)(nil)).
			Set("ip_addresses_json = ?", string(ipsJSON)).
			Set("updated_at = ?", s.nowFn().UTC()).
			Where("id = ?", iface.ID).
			Exec(ctx)
		return err
	})
	return ips, err
}

func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// LookupInstanceByIP finds the instance ID that owns the given IP address.
// Scans the ip_addresses_json column in instance_interfaces.
func (s *Store) LookupInstanceByIP(ctx context.Context, ip string) (string, bool) {
	var iface InstanceInterfaceRow
	// SQLite JSON: ip_addresses_json is a JSON array like ["192.168.200.42"].
	// Use LIKE as a simple substring match — sufficient for IP uniqueness.
	err := s.db.NewSelect().
		Model(&iface).
		Where(`ip_addresses_json LIKE ?`, `%"`+ip+`"%`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return "", false
	}
	return iface.InstanceID, true
}

// LookupInstanceByMAC finds the instance ID that owns the given MAC address.
func (s *Store) LookupInstanceByMAC(ctx context.Context, mac string) (string, bool) {
	mac = strings.ToLower(mac)
	var iface InstanceInterfaceRow
	err := s.db.NewSelect().
		Model(&iface).
		Where("LOWER(mac_address) = ?", mac).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return "", false
	}
	return iface.InstanceID, true
}

// UpdateInterfaceMAC sets the MAC address for an interface.
func (s *Store) UpdateInterfaceMAC(ctx context.Context, interfaceID, mac string) error {
	_, err := s.db.NewUpdate().
		Model((*InstanceInterfaceRow)(nil)).
		Set("mac_address = ?", strings.ToLower(mac)).
		Set("updated_at = ?", s.nowFn().UTC()).
		Where("id = ?", interfaceID).
		Exec(ctx)
	return err
}

// MarkVMPhonedHome records the phone_home callback time for a VM instance.
func (s *Store) MarkVMPhonedHome(ctx context.Context, instanceID string) error {
	now := s.nowFn().UTC()
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().Model((*VMState)(nil)).
			Set("phoned_home_at = ?", now).
			Where("instance_id = ?", instanceID).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("mark VM phoned home: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read VM phone-home update result: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		if _, err := tx.NewUpdate().Model((*instanceRow)(nil)).
			Set("updated_at = ?", now).
			Where("id = ?", instanceID).
			Exec(ctx); err != nil {
			return fmt.Errorf("update phoned-home instance timestamp: %w", err)
		}
		return nil
	})
}

func sameLogicalOwner(existingLabels, newLabels map[string]string) bool {
	// Forge retries for create should be treated as idempotent if node owner label matches.
	existingOwner := ownerLabel(existingLabels)
	newOwner := ownerLabel(newLabels)
	if existingOwner != "" && newOwner != "" {
		return existingOwner == newOwner
	}

	// Fallback: exact label map equality.
	if len(existingLabels) != len(newLabels) {
		return false
	}
	for k, v := range existingLabels {
		if newLabels[k] != v {
			return false
		}
	}
	return true
}

func ownerLabel(labels map[string]string) string {
	if v := labels["nvke.nvidia.com/node-id"]; v != "" {
		return v
	}
	// The controller wrote this key before the nvke.nvidia.com rename.
	if v := labels["nke.nvidia.com/node-id"]; v != "" {
		return v
	}
	if v := labels["nvke.node-id"]; v != "" {
		return v
	}
	return ""
}
