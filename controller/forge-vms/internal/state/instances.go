// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/ipam"
)

const (
	scopeTypeSubnet    = "subnet"
	scopeTypeVPCPrefix = "vpc_prefix"
)

type InterfaceRequest struct {
	SubnetID    *string
	VPCPrefixID *string
}

type CreateInstanceInput struct {
	Name             string
	TenantID         string
	InstanceTypeID   string
	VPCID            string
	IpxeScript       string
	UserData         string
	Labels           map[string]string
	SSHKeyGroupIDs   []string
	Interfaces       []InterfaceRequest
	PhoneHomeEnabled bool
}

type UpdateInstanceInput struct {
	Labels        map[string]string
	TriggerReboot bool
	IpxeScript    *string
	UserData      *string
}

type ListInstancesFilter struct {
	SiteID   string
	TenantID string
}

func (s *Store) ListInstances(ctx context.Context, filter ListInstancesFilter, page Page) ([]InstanceWithInterfaces, error) {
	page = normalizePage(page)

	query := s.db.NewSelect().
		Model((*InstanceRow)(nil)).
		Order("created_at ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size)

	if filter.SiteID != "" {
		query = query.Where("site_id = ?", filter.SiteID)
	}
	if filter.TenantID != "" {
		query = query.Where("tenant_id = ?", filter.TenantID)
	}
	// Deleted instances should not be listed.
	query = query.Where("observed_state != ?", ObservedStateDeleted)

	var rows []InstanceRow
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	return s.withInterfaces(ctx, rows)
}

func (s *Store) GetInstanceByID(ctx context.Context, id string) (InstanceWithInterfaces, error) {
	var row InstanceRow
	if err := s.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InstanceWithInterfaces{}, ErrNotFound
		}
		return InstanceWithInterfaces{}, fmt.Errorf("failed to get instance: %w", err)
	}

	joined, err := s.withInterfaces(ctx, []InstanceRow{row})
	if err != nil {
		return InstanceWithInterfaces{}, err
	}
	if len(joined) == 0 {
		return InstanceWithInterfaces{}, ErrNotFound
	}
	return joined[0], nil
}

func (s *Store) CreateInstance(ctx context.Context, input CreateInstanceInput) (InstanceWithInterfaces, bool, error) {
	if input.Name == "" || input.TenantID == "" || input.InstanceTypeID == "" || input.VPCID == "" {
		return InstanceWithInterfaces{}, false, fmt.Errorf("%w: name, tenantId, instanceTypeId, and vpcId are required", ErrBadRequest)
	}
	if len(input.Interfaces) == 0 {
		return InstanceWithInterfaces{}, false, fmt.Errorf("%w: at least one interface is required", ErrBadRequest)
	}

	var output InstanceWithInterfaces
	created := false

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var vpc VPCRow
		if err := tx.NewSelect().Model(&vpc).Where("id = ?", input.VPCID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: vpc %s not found", ErrBadRequest, input.VPCID)
			}
			return fmt.Errorf("failed to resolve vpc: %w", err)
		}
		if vpc.TenantID != input.TenantID {
			return fmt.Errorf("%w: vpc %s does not belong to tenant %s", ErrBadRequest, input.VPCID, input.TenantID)
		}

		existing, err := getInstanceByTenantAndNameTx(ctx, tx, input.TenantID, input.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			// Safe retry behavior: return existing instance if logical owner matches.
			existingLabels, labelsErr := existing.Labels()
			if labelsErr != nil {
				return labelsErr
			}
			if sameLogicalOwner(existingLabels, input.Labels) {
				output, err = withInterfacesTx(ctx, tx, existing)
				return err
			}
			return fmt.Errorf("%w: An Instance with specified name already exists for Tenant", ErrConflict)
		}

		now := s.nowFn().UTC()
		labelsJSON, err := json.Marshal(input.Labels)
		if err != nil {
			return fmt.Errorf("failed to marshal labels: %w", err)
		}
		sshKeyGroupIDsJSON, err := json.Marshal(input.SSHKeyGroupIDs)
		if err != nil {
			return fmt.Errorf("failed to marshal ssh key group ids: %w", err)
		}

		instance := InstanceRow{
			ID:                 uuid.NewString(),
			Name:               input.Name,
			TenantID:           input.TenantID,
			SiteID:             vpc.SiteID,
			InstanceTypeID:     input.InstanceTypeID,
			VPCID:              input.VPCID,
			IpxeScript:         input.IpxeScript,
			UserData:           input.UserData,
			LabelsJSON:         string(labelsJSON),
			SSHKeyGroupIDsJSON: string(sshKeyGroupIDsJSON),
			DesiredState:       DesiredStatePresent,
			ObservedState:      ObservedStateNew,
			Status:             "provisioning",
			PhoneHomeEnabled:   input.PhoneHomeEnabled,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if _, err := tx.NewInsert().Model(&instance).Exec(ctx); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return fmt.Errorf("%w: An Instance with specified name already exists for Tenant", ErrConflict)
			}
			return fmt.Errorf("failed to insert instance: %w", err)
		}

		// Create interfaces with empty IPs. IP allocation (static IPAM or
		// DHCP lease confirmation) is handled by the engine during provisioning.
		interfaces := make([]InstanceInterfaceRow, 0, len(input.Interfaces))
		for _, ifaceReq := range input.Interfaces {
			if (ifaceReq.SubnetID == nil && ifaceReq.VPCPrefixID == nil) || (ifaceReq.SubnetID != nil && ifaceReq.VPCPrefixID != nil) {
				return fmt.Errorf("%w: exactly one of subnetId or vpcPrefixId must be provided", ErrBadRequest)
			}

			iface := InstanceInterfaceRow{
				ID:              uuid.NewString(),
				InstanceID:      instance.ID,
				IPAddressesJSON: "[]",
				Status:          "provisioning",
				CreatedAt:       now,
				UpdatedAt:       now,
			}

			if ifaceReq.SubnetID != nil {
				iface.SubnetID = *ifaceReq.SubnetID
			}
			if ifaceReq.VPCPrefixID != nil {
				iface.VPCPrefixID = *ifaceReq.VPCPrefixID
			}

			if _, err := tx.NewInsert().Model(&iface).Exec(ctx); err != nil {
				return fmt.Errorf("failed to insert instance interface: %w", err)
			}
			interfaces = append(interfaces, iface)
		}

		output = InstanceWithInterfaces{
			Instance:   instance,
			Interfaces: interfaces,
		}
		created = true
		return nil
	})
	if err != nil {
		return InstanceWithInterfaces{}, false, err
	}

	return output, created, nil
}

// AllocateInterfaceIPs resolves the CIDR for the given InterfaceRequest and
// allocates IPs from the matching subnet or VPC prefix within the supplied
// transaction. Exported so HTTP handlers or other callers can invoke it
// directly when they need to pre-allocate IPs before calling CreateInstance.
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

func (s *Store) UpdateInstance(ctx context.Context, id string, input UpdateInstanceInput) (InstanceWithInterfaces, error) {
	var out InstanceWithInterfaces
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
			instance.RebootRequested = true
			if instance.ObservedState == ObservedStateReady {
				instance.ObservedState = ObservedStateRebooting
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

		out, err = withInterfacesTx(ctx, tx, instance)
		return err
	})
	if err != nil {
		return InstanceWithInterfaces{}, err
	}
	return out, nil
}

func (s *Store) MarkInstanceForDelete(ctx context.Context, id string) (bool, error) {
	result, err := s.db.NewUpdate().
		Model((*InstanceRow)(nil)).
		Set("desired_state = ?", DesiredStateAbsent).
		Set("status = ?", "deleting").
		Set("observed_state = CASE WHEN observed_state = ? THEN ? ELSE observed_state END", ObservedStateReady, ObservedStateDeleting).
		Set("updated_at = ?", s.nowFn().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to mark instance for delete: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed getting rows affected: %w", err)
	}
	return affected > 0, nil
}

func (s *Store) DeleteInstanceHard(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*IPAllocationRow)(nil)).Where("instance_id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete ip allocations for %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*InstanceInterfaceRow)(nil)).Where("instance_id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete interfaces for %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*InstanceRow)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete instance %s: %w", id, err)
		}
		return nil
	})
}

type InstanceStatePatch struct {
	MachineID        *string
	Status           *string
	ObservedState    *string
	ErrorReason      *string
	RebootRequested  *bool
	TpmEkCertificate *string
}

func (s *Store) PatchInstanceState(ctx context.Context, id string, patch InstanceStatePatch) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		instance, err := getInstanceTx(ctx, tx, id)
		if err != nil {
			return err
		}

		if patch.MachineID != nil {
			instance.MachineID = *patch.MachineID
		}
		if patch.Status != nil {
			instance.Status = *patch.Status
		}
		if patch.ObservedState != nil {
			instance.ObservedState = *patch.ObservedState
		}
		if patch.ErrorReason != nil {
			instance.ErrorReason = *patch.ErrorReason
		}
		if patch.RebootRequested != nil {
			instance.RebootRequested = *patch.RebootRequested
		}
		if patch.TpmEkCertificate != nil {
			instance.TpmEkCertificate = *patch.TpmEkCertificate
		}
		instance.UpdatedAt = s.nowFn().UTC()

		if _, err := tx.NewUpdate().Model(&instance).Where("id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("failed to patch instance %s: %w", id, err)
		}
		return nil
	})
}

// ListActiveMachineIDs returns all machine_id values for instances that are
// not deleted and have a non-empty machine_id. Used at startup to compare
// against the hypervisor's running VMs to detect orphans.
func (s *Store) ListActiveMachineIDs(ctx context.Context) (map[string]struct{}, error) {
	type row struct {
		MachineID string `bun:"machine_id"`
	}
	var rows []row
	if err := s.db.NewSelect().
		Table("instances").
		Column("machine_id").
		Where("machine_id != ''").
		Where("observed_state != ?", ObservedStateDeleted).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list active machine ids: %w", err)
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r.MachineID] = struct{}{}
	}
	return out, nil
}

func (s *Store) ListReconcileCandidates(ctx context.Context) ([]string, error) {
	type idRow struct {
		ID string `bun:"id"`
	}
	var rows []idRow

	if err := s.db.NewSelect().
		Table("instances").
		Column("id").
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.WhereOr("(desired_state = ? AND observed_state IN (?, ?, ?, ?, ?) )",
				DesiredStatePresent,
				ObservedStateNew,
				ObservedStateProvisioning,
				ObservedStateRunning,
				ObservedStateRebooting,
				ObservedStateReady,
			).WhereOr("(desired_state = ? AND observed_state != ?)", DesiredStateAbsent, ObservedStateDeleted).
				WhereOr("(desired_state = ? AND reboot_requested = 1)", DesiredStatePresent)
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

func getInstanceTx(ctx context.Context, tx bun.Tx, id string) (InstanceRow, error) {
	var row InstanceRow
	if err := tx.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InstanceRow{}, ErrNotFound
		}
		return InstanceRow{}, fmt.Errorf("failed to get instance %s: %w", id, err)
	}
	return row, nil
}

func getInstanceByTenantAndNameTx(ctx context.Context, tx bun.Tx, tenantID, name string) (InstanceRow, error) {
	var row InstanceRow
	if err := tx.NewSelect().
		Model(&row).
		Where("tenant_id = ?", tenantID).
		Where("name = ?", name).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InstanceRow{}, ErrNotFound
		}
		return InstanceRow{}, fmt.Errorf("failed to get instance by tenant/name: %w", err)
	}
	return row, nil
}

func (s *Store) withInterfaces(ctx context.Context, rows []InstanceRow) ([]InstanceWithInterfaces, error) {
	if len(rows) == 0 {
		return []InstanceWithInterfaces{}, nil
	}

	instanceIDs := make([]string, 0, len(rows))
	indexByID := make(map[string]int, len(rows))
	out := make([]InstanceWithInterfaces, 0, len(rows))
	for i, row := range rows {
		instanceIDs = append(instanceIDs, row.ID)
		indexByID[row.ID] = i
		out = append(out, InstanceWithInterfaces{Instance: row})
	}

	var interfaces []InstanceInterfaceRow
	if err := s.db.NewSelect().
		Model(&interfaces).
		Where("instance_id IN (?)", bun.In(instanceIDs)). //nolint:staticcheck // deprecated by the bun bump from the workspace version unification; migration to bun.List/Tuple tracked separately
		Order("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list instance interfaces: %w", err)
	}

	for _, iface := range interfaces {
		i := indexByID[iface.InstanceID]
		out[i].Interfaces = append(out[i].Interfaces, iface)
	}
	return out, nil
}

func withInterfacesTx(ctx context.Context, tx bun.Tx, row InstanceRow) (InstanceWithInterfaces, error) {
	var interfaces []InstanceInterfaceRow
	if err := tx.NewSelect().
		Model(&interfaces).
		Where("instance_id = ?", row.ID).
		Order("created_at ASC").
		Scan(ctx); err != nil {
		return InstanceWithInterfaces{}, fmt.Errorf("failed to get interfaces for instance %s: %w", row.ID, err)
	}
	return InstanceWithInterfaces{
		Instance:   row,
		Interfaces: interfaces,
	}, nil
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

// MarkPhonedHome records the phone_home callback time for an instance.
func (s *Store) MarkPhonedHome(ctx context.Context, instanceID string) error {
	now := s.nowFn().UTC()
	_, err := s.db.NewUpdate().
		Model((*InstanceRow)(nil)).
		Set("phoned_home_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", instanceID).
		Exec(ctx)
	return err
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
	if v := labels["nke.nvidia.com/node-id"]; v != "" {
		return v
	}
	if v := labels["nke.node-id"]; v != "" {
		return v
	}
	return ""
}
