// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ControlPlaneConfigSpec is the snapshot payload stored in
// control_plane_configurations.config.
//
// These fields are intentionally marked bun:"-" because they are serialized
// inside ControlPlaneConfiguration.Config JSONB rather than mapped to columns
// on the clusters table.
type ControlPlaneConfigSpec struct {
	InstanceTypePreference          []uuid.UUID            `json:"instance_type_preference" bun:"-"`
	InfraMachineTemplatePreferences []json.RawMessage      `json:"infra_machine_template_preferences,omitempty" bun:"-"`
	SubnetIDs                       []uuid.UUID            `json:"subnet_ids,omitempty" bun:"-"`
	VpcPrefixIDs                    []uuid.UUID            `json:"vpc_prefix_ids,omitempty" bun:"-"`
	OSProfile                       string                 `json:"os_profile" bun:"-"`
	DiskSelectionStrategy           DiskSelectionStrategy  `json:"disk_selection_strategy,omitempty" bun:"-"`
	ConfigProfile                   string                 `json:"config_profile" bun:"-"`
	SystemReservedMemory            string                 `json:"system_reserved_memory,omitempty" bun:"-"`
	SSHKeyGroupIDs                  []uuid.UUID            `json:"ssh_key_group_ids,omitempty" bun:"-"`
	NodeLabels                      []string               `json:"node_labels,omitempty" bun:"-"`
	TeleportEnabled                 bool                   `json:"teleport_enabled,omitempty" bun:"-"`
	ProvisioningMonitorDisabled     bool                   `json:"provisioning_monitor_disabled,omitempty" bun:"-"`
	KubernetesVersion               string                 `json:"kubernetes_version,omitempty" bun:"-"`
	CapiSpireAttestationMethod      SpireAttestationMethod `json:"capi_spire_attestation_method,omitempty" bun:"-"`
	CapiBootstrapFiles              []CloudInitFile        `json:"capi_bootstrap_files,omitempty" bun:"-"`
	CapiPreKubeadmCommands          []string               `json:"capi_pre_kubeadm_commands,omitempty" bun:"-"`
}

// ControlPlaneConfig is the cluster control plane configuration shape used by
// API and controller logic. It contains cluster-level fields (configuration ID,
// desired count, max surge) plus the current snapshot spec loaded from
// control_plane_configurations.config.
type ControlPlaneConfig struct {
	ConfigurationID        uuid.UUID `json:"configuration_id,omitempty" bun:"configuration_id,type:uuid,nullzero"`
	ConfigurationCreatedAt time.Time `json:"configuration_created_at,omitempty" bun:"-"`
	DesiredCount           int       `json:"desired_count" bun:"cp_desired_count,notnull,default:0"`
	MaxSurge               int       `json:"max_surge,omitempty" bun:"cp_max_surge,notnull,default:0"`
	ControlPlaneConfigSpec
}

// ControlPlaneConfiguration is an append-only control plane config snapshot row.
type ControlPlaneConfiguration struct {
	ID           uuid.UUID              `bun:"type:uuid,pk,default:gen_random_uuid()"`
	ClusterID    uuid.UUID              `bun:"type:uuid,notnull"`
	Config       ControlPlaneConfigSpec `bun:"type:jsonb,notnull"`
	ChangeReason string                 `bun:"type:text"`
	CreatedBy    string                 `bun:"type:text"`
	CreatedAt    time.Time              `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type ControlPlaneConfigurationQueries interface {
	CreateControlPlaneConfiguration(ctx context.Context, row *ControlPlaneConfiguration) (ControlPlaneConfiguration, error)
	GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (ControlPlaneConfiguration, error)
	ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]ControlPlaneConfiguration, error)
}

func ConfigSpecFromFull(cfg ControlPlaneConfig) ControlPlaneConfigSpec {
	return cfg.ControlPlaneConfigSpec
}

func BuildControlPlaneConfig(cluster Cluster, snapshotCfg ControlPlaneConfigSpec, createdAt time.Time) ControlPlaneConfig {
	return ControlPlaneConfig{
		ConfigurationID:        cluster.ControlPlaneConfig.ConfigurationID,
		ConfigurationCreatedAt: createdAt,
		DesiredCount:           cluster.ControlPlaneConfig.DesiredCount,
		MaxSurge:               cluster.ControlPlaneConfig.MaxSurge,
		ControlPlaneConfigSpec: snapshotCfg,
	}
}

func (c ControlPlaneConfigSpec) Equal(other ControlPlaneConfigSpec) bool {
	return reflect.DeepEqual(normalizeConfigSpec(c), normalizeConfigSpec(other))
}

func normalizeConfigSpec(cfg ControlPlaneConfigSpec) ControlPlaneConfigSpec {
	out := cfg
	out.InstanceTypePreference = append([]uuid.UUID(nil), cfg.InstanceTypePreference...)
	out.InfraMachineTemplatePreferences = cloneRawMessages(cfg.InfraMachineTemplatePreferences)
	out.SubnetIDs = append([]uuid.UUID(nil), cfg.SubnetIDs...)
	out.VpcPrefixIDs = append([]uuid.UUID(nil), cfg.VpcPrefixIDs...)
	out.SSHKeyGroupIDs = append([]uuid.UUID(nil), cfg.SSHKeyGroupIDs...)
	out.NodeLabels = append([]string(nil), cfg.NodeLabels...)
	out.CapiBootstrapFiles = append([]CloudInitFile(nil), cfg.CapiBootstrapFiles...)
	out.CapiPreKubeadmCommands = append([]string(nil), cfg.CapiPreKubeadmCommands...)
	return out
}

func cloneRawMessages(in []json.RawMessage) []json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(in))
	for i, raw := range in {
		out[i] = append(json.RawMessage(nil), raw...)
	}
	return out
}

type controlPlaneReplaceTemplate struct {
	OSProfile         string `json:"os_profile"`
	ConfigProfile     string `json:"config_profile"`
	KubernetesVersion string `json:"kubernetes_version"`
}

func ReplaceTemplate(cfg ControlPlaneConfigSpec) controlPlaneReplaceTemplate {
	return controlPlaneReplaceTemplate{
		OSProfile:         cfg.OSProfile,
		ConfigProfile:     cfg.ConfigProfile,
		KubernetesVersion: cfg.KubernetesVersion,
	}
}

func ReplaceTemplateChanged(a, b ControlPlaneConfigSpec) bool {
	return !reflect.DeepEqual(ReplaceTemplate(a), ReplaceTemplate(b))
}

func (q *queriesImpl) CreateControlPlaneConfiguration(ctx context.Context, row *ControlPlaneConfiguration) (ControlPlaneConfiguration, error) {
	_, err := q.db.NewInsert().Model(row).Exec(ctx)
	if err != nil {
		return ControlPlaneConfiguration{}, err
	}
	return *row, nil
}

func (q *queriesImpl) GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (ControlPlaneConfiguration, error) {
	if id == uuid.Nil {
		return ControlPlaneConfiguration{}, ErrNotFound
	}
	var row ControlPlaneConfiguration
	err := q.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ControlPlaneConfiguration{}, ErrNotFound
		}
		return ControlPlaneConfiguration{}, err
	}
	return row, nil
}

func (q *queriesImpl) ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]ControlPlaneConfiguration, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	unique := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	var rows []ControlPlaneConfiguration
	err := q.db.NewSelect().
		Model(&rows).
		Where("id IN (?)", bun.List(unique)).
		Scan(ctx)
	return rows, err
}

func LoadClusterControlPlaneConfig(ctx context.Context, q Queries, cluster *Cluster) error {
	if cluster == nil {
		return nil
	}
	if cluster.ControlPlaneConfig.ConfigurationID == uuid.Nil {
		cluster.ControlPlaneConfig = ControlPlaneConfig{
			ConfigurationID: cluster.ControlPlaneConfig.ConfigurationID,
			DesiredCount:    cluster.ControlPlaneConfig.DesiredCount,
			MaxSurge:        cluster.ControlPlaneConfig.MaxSurge,
		}
		return nil
	}
	row, err := q.GetControlPlaneConfiguration(ctx, cluster.ControlPlaneConfig.ConfigurationID)
	if err != nil {
		return fmt.Errorf("load control plane configuration %s: %w", cluster.ControlPlaneConfig.ConfigurationID, err)
	}
	cluster.ControlPlaneConfig = BuildControlPlaneConfig(*cluster, row.Config, row.CreatedAt)
	return nil
}

func LoadClustersControlPlaneConfig(ctx context.Context, q Queries, clusters []Cluster) error {
	ids := make([]uuid.UUID, 0, len(clusters))
	for i := range clusters {
		if clusters[i].ControlPlaneConfig.ConfigurationID != uuid.Nil {
			ids = append(ids, clusters[i].ControlPlaneConfig.ConfigurationID)
		}
	}
	rows, err := q.ListControlPlaneConfigurationsByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("list control plane configurations: %w", err)
	}
	byID := make(map[uuid.UUID]ControlPlaneConfiguration, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for i := range clusters {
		row, ok := byID[clusters[i].ControlPlaneConfig.ConfigurationID]
		if !ok {
			clusters[i].ControlPlaneConfig = ControlPlaneConfig{
				ConfigurationID: clusters[i].ControlPlaneConfig.ConfigurationID,
				DesiredCount:    clusters[i].ControlPlaneConfig.DesiredCount,
				MaxSurge:        clusters[i].ControlPlaneConfig.MaxSurge,
			}
			continue
		}
		clusters[i].ControlPlaneConfig = BuildControlPlaneConfig(clusters[i], row.Config, row.CreatedAt)
	}
	return nil
}

type CreateControlPlaneConfigRowOpts struct {
	ChangeReason string
	CreatedBy    string
}

// CreateControlPlaneConfigRowIfChanged inserts a new snapshot row and updates
// the cluster configuration pointer when the config payload changes.
func CreateControlPlaneConfigRowIfChanged(
	ctx context.Context,
	q Queries,
	cluster *Cluster,
	snapshotCfg ControlPlaneConfigSpec,
	opts CreateControlPlaneConfigRowOpts,
) error {
	return createControlPlaneConfigRowIfChanged(ctx, q, cluster, snapshotCfg, opts)
}

func createControlPlaneConfigRowIfChanged(
	ctx context.Context,
	q Queries,
	cluster *Cluster,
	snapshotCfg ControlPlaneConfigSpec,
	opts CreateControlPlaneConfigRowOpts,
) error {
	current := ControlPlaneConfigSpec{}
	if cluster.ControlPlaneConfig.ConfigurationID != uuid.Nil {
		row, err := q.GetControlPlaneConfiguration(ctx, cluster.ControlPlaneConfig.ConfigurationID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			current = row.Config
		}
	}
	if current.Equal(snapshotCfg) {
		cluster.ControlPlaneConfig = BuildControlPlaneConfig(*cluster, snapshotCfg, cluster.ControlPlaneConfig.ConfigurationCreatedAt)
		return nil
	}

	row := &ControlPlaneConfiguration{
		ClusterID:    cluster.ID,
		Config:       snapshotCfg,
		ChangeReason: opts.ChangeReason,
		CreatedBy:    opts.CreatedBy,
	}
	created, err := q.CreateControlPlaneConfiguration(ctx, row)
	if err != nil {
		return fmt.Errorf("create control plane configuration: %w", err)
	}
	cluster.ControlPlaneConfig = BuildControlPlaneConfig(*cluster, snapshotCfg, created.CreatedAt)
	cluster.ControlPlaneConfig.ConfigurationID = created.ID
	return nil
}

func configSpecFromMergedPools(merged *ControlPlaneConfig) ControlPlaneConfigSpec {
	if merged == nil {
		return ControlPlaneConfigSpec{}
	}
	return merged.ControlPlaneConfigSpec
}

func parseLegacyControlPlaneConfigJSON(raw json.RawMessage) (ControlPlaneConfig, error) {
	var cfg ControlPlaneConfig
	if len(raw) == 0 || bytes.Equal(raw, []byte("{}")) {
		return ControlPlaneConfig{}, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ControlPlaneConfig{}, err
	}
	return cfg, nil
}

// LegacyControlPlaneConfigFromJSON parses the legacy clusters.control_plane_config JSONB column.
func LegacyControlPlaneConfigFromJSON(raw json.RawMessage) (ControlPlaneConfig, error) {
	return parseLegacyControlPlaneConfigJSON(raw)
}

func (c ControlPlaneConfig) Equal(other ControlPlaneConfig) bool {
	return c.DesiredCount == other.DesiredCount &&
		c.MaxSurge == other.MaxSurge &&
		c.ControlPlaneConfigSpec.Equal(other.ControlPlaneConfigSpec)
}
