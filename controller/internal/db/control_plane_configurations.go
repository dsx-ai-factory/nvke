// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

// NodeAgentConfig stores the exact desired agent release.
type NodeAgentConfig struct {
	Version string `json:"version,omitempty"`
}

// ControlPlaneConfigSpec is the snapshot payload stored in
// control_plane_configurations.config.
//
// These fields are intentionally marked bun:"-" because they are serialized
// inside ControlPlaneConfiguration.Config JSONB rather than mapped to columns
// on the clusters table.
type ControlPlaneConfigSpec struct {
	SystemReservedMemory        string                           `json:"system_reserved_memory,omitempty" bun:"-"`
	KubeReservedMemory          string                           `json:"kube_reserved_memory,omitempty" bun:"-"`
	NodeLabels                  map[string]string                `json:"node_labels,omitempty" bun:"-"`
	TeleportEnabled             bool                             `json:"teleport_enabled,omitempty" bun:"-"`
	ProvisioningMonitorDisabled bool                             `json:"provisioning_monitor_disabled,omitempty" bun:"-"`
	KubernetesVersion           string                           `json:"kubernetes_version,omitempty" bun:"-"`
	Nico                        *NicoControlPlaneProvider        `json:"nico,omitempty" bun:"-"`
	Capi                        *CAPIControlPlaneProvider        `json:"capi,omitempty" bun:"-"`
	NodeAgentConfig             NodeAgentConfig                  `json:"node_agent_config,omitempty" bun:"-"`
	MachineAnnotations          map[string]string                `json:"machine_annotations,omitempty" bun:"-"`
	MachineLabels               map[string]string                `json:"machine_labels,omitempty" bun:"-"`
	LaunchLayer                 *LaunchLayerControlPlaneProvider `json:"launch_layer,omitempty" bun:"-"`
}

type NicoControlPlaneProvider struct {
	InstanceTypePreference []uuid.UUID           `json:"instance_type_preference,omitempty"`
	SubnetIDs              []uuid.UUID           `json:"subnet_ids,omitempty"`
	VpcPrefixIDs           []uuid.UUID           `json:"vpc_prefix_ids,omitempty"`
	Release                string                `json:"release,omitempty"`
	DiskSelectionStrategy  DiskSelectionStrategy `json:"disk_selection_strategy,omitempty"`
	ConfigProfile          string                `json:"config_profile,omitempty"`
	SSHKeyGroupIDs         []uuid.UUID           `json:"ssh_key_group_ids,omitempty"`
}

type CAPIControlPlaneProvider struct {
	InfraMachineTemplatePreferences []json.RawMessage      `json:"infra_machine_template_preferences,omitempty"`
	SpireAttestationMethod          SpireAttestationMethod `json:"spire_attestation_method,omitempty"`
	BootstrapFiles                  []CloudInitFile        `json:"bootstrap_files,omitempty"`
	PreKubeadmCommands              []string               `json:"pre_kubeadm_commands,omitempty"`
}

// LaunchLayerControlPlaneProvider is the LaunchLayer-specific control-plane
// payload. Same field set as LaunchLayerNodePoolProvider — control-plane
// nodes are still LaunchLayer machines.
type LaunchLayerControlPlaneProvider struct {
	InstanceType        string                       `json:"instance_type,omitempty"`
	HostOSProfile       string                       `json:"host_os_profile,omitempty"`
	VMProfile           string                       `json:"vm_profile,omitempty"`
	FulfillmentStrategy string                       `json:"fulfillment_strategy,omitempty"`
	SecurityGroupRefs   LaunchLayerSecurityGroupRefs `json:"security_group_refs,omitempty"`
	SecurityGroups      LaunchLayerSecurityGroups    `json:"security_groups,omitempty"`
}

func (c *ControlPlaneConfigSpec) SetNico(provider NicoControlPlaneProvider) {
	c.Nico = cloneNicoControlPlaneProvider(&provider)
	c.Capi = nil
	c.LaunchLayer = nil
}

func (c *ControlPlaneConfigSpec) SetCapi(provider CAPIControlPlaneProvider) {
	c.Capi = cloneCAPIControlPlaneProvider(&provider)
	c.Nico = nil
	c.LaunchLayer = nil
}

func (c *ControlPlaneConfigSpec) SetLaunchLayer(provider LaunchLayerControlPlaneProvider) {
	c.LaunchLayer = cloneLaunchLayerControlPlaneProvider(&provider)
	c.Nico = nil
	c.Capi = nil
}

func (c ControlPlaneConfigSpec) NicoProvider() (NicoControlPlaneProvider, error) {
	if c.Nico == nil {
		return NicoControlPlaneProvider{}, fmt.Errorf("%w: control plane config nico", ErrMissingProviderPayload)
	}
	return *cloneNicoControlPlaneProvider(c.Nico), nil
}

func (c ControlPlaneConfigSpec) CAPIProvider() (CAPIControlPlaneProvider, error) {
	if c.Capi == nil {
		return CAPIControlPlaneProvider{}, fmt.Errorf("%w: control plane config capi", ErrMissingProviderPayload)
	}
	return *cloneCAPIControlPlaneProvider(c.Capi), nil
}

func (c ControlPlaneConfigSpec) LaunchLayerProvider() (LaunchLayerControlPlaneProvider, error) {
	if c.LaunchLayer == nil {
		return LaunchLayerControlPlaneProvider{}, fmt.Errorf("%w: control plane config launch_layer", ErrMissingProviderPayload)
	}
	return *cloneLaunchLayerControlPlaneProvider(c.LaunchLayer), nil
}

// activeProvider returns the single provider set on the spec as an opaque
// value, or nil when none is set. It is the one place that knows the provider
// union; capability accessors like ScaleDownRanker type-assert against it.
func (c ControlPlaneConfigSpec) activeProvider() any {
	switch {
	case c.Nico != nil:
		return *c.Nico
	case c.Capi != nil:
		return *c.Capi
	case c.LaunchLayer != nil:
		return *c.LaunchLayer
	}
	return nil
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
	AutoScaleDownDisabled  bool      `json:"auto_scale_down_disabled,omitempty" bun:"cp_auto_scale_down_disabled,notnull,default:false"`
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
	ListControlPlaneConfigurationsByCluster(ctx context.Context, clusterID uuid.UUID) ([]ControlPlaneConfiguration, error)
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
		AutoScaleDownDisabled:  cluster.ControlPlaneConfig.AutoScaleDownDisabled,
		ControlPlaneConfigSpec: snapshotCfg,
	}
}

func (c ControlPlaneConfigSpec) Equal(other ControlPlaneConfigSpec) bool {
	return reflect.DeepEqual(normalizeConfigSpec(c), normalizeConfigSpec(other))
}

func normalizeConfigSpec(cfg ControlPlaneConfigSpec) ControlPlaneConfigSpec {
	out := cfg
	out.NodeLabels = CloneStringMap(cfg.NodeLabels)
	out.MachineAnnotations = CloneStringMap(cfg.MachineAnnotations)
	out.MachineLabels = CloneStringMap(cfg.MachineLabels)
	out.Nico = cloneNicoControlPlaneProvider(cfg.Nico)
	out.Capi = cloneCAPIControlPlaneProvider(cfg.Capi)
	out.LaunchLayer = cloneLaunchLayerControlPlaneProvider(cfg.LaunchLayer)
	return out
}

func cloneNicoControlPlaneProvider(provider *NicoControlPlaneProvider) *NicoControlPlaneProvider {
	if provider == nil {
		return nil
	}
	return &NicoControlPlaneProvider{
		InstanceTypePreference: append([]uuid.UUID(nil), provider.InstanceTypePreference...),
		SubnetIDs:              append([]uuid.UUID(nil), provider.SubnetIDs...),
		VpcPrefixIDs:           append([]uuid.UUID(nil), provider.VpcPrefixIDs...),
		Release:                provider.Release,
		DiskSelectionStrategy:  provider.DiskSelectionStrategy,
		ConfigProfile:          provider.ConfigProfile,
		SSHKeyGroupIDs:         append([]uuid.UUID(nil), provider.SSHKeyGroupIDs...),
	}
}

func cloneCAPIControlPlaneProvider(provider *CAPIControlPlaneProvider) *CAPIControlPlaneProvider {
	if provider == nil {
		return nil
	}
	return &CAPIControlPlaneProvider{
		InfraMachineTemplatePreferences: cloneRawMessages(provider.InfraMachineTemplatePreferences),
		SpireAttestationMethod:          provider.SpireAttestationMethod,
		BootstrapFiles:                  append([]CloudInitFile(nil), provider.BootstrapFiles...),
		PreKubeadmCommands:              append([]string(nil), provider.PreKubeadmCommands...),
	}
}

func cloneLaunchLayerControlPlaneProvider(provider *LaunchLayerControlPlaneProvider) *LaunchLayerControlPlaneProvider {
	if provider == nil {
		return nil
	}
	return &LaunchLayerControlPlaneProvider{
		InstanceType:        provider.InstanceType,
		HostOSProfile:       provider.HostOSProfile,
		VMProfile:           provider.VMProfile,
		FulfillmentStrategy: provider.FulfillmentStrategy,
		SecurityGroupRefs: LaunchLayerSecurityGroupRefs{
			PrimaryNetworkInterface: append([]string(nil), provider.SecurityGroupRefs.PrimaryNetworkInterface...),
			GPUNetworkInterface:     append([]string(nil), provider.SecurityGroupRefs.GPUNetworkInterface...),
		},
		SecurityGroups: LaunchLayerSecurityGroups{
			PrimaryNetworkInterface: append([]string(nil), provider.SecurityGroups.PrimaryNetworkInterface...),
			GPUNetworkInterface:     append([]string(nil), provider.SecurityGroups.GPUNetworkInterface...),
		},
	}
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

// ControlPlaneRolloutChange is a single rollout-relevant field that differs
// between two control-plane config snapshots.
type ControlPlaneRolloutChange struct {
	Field string // "kubernetesVersion", "release", or "configProfile"
	From  string
	To    string
}

// ComputeControlPlaneRolloutChanges returns the rollout-triggering fields that
// differ between a node's pinned snapshot and the cluster's desired snapshot (nil
// when none differ). It is the single source of truth for both "does this node
// need rollout" and "what changed" — add a new rollout-relevant field here and
// the rollout loop, the control-plane rollout-required condition, and the API all
// pick it up.
func ComputeControlPlaneRolloutChanges(pinned, desired ControlPlaneConfigSpec) []ControlPlaneRolloutChange {
	var changes []ControlPlaneRolloutChange
	if pinned.KubernetesVersion != desired.KubernetesVersion {
		changes = append(changes, ControlPlaneRolloutChange{Field: "kubernetesVersion", From: pinned.KubernetesVersion, To: desired.KubernetesVersion})
	}
	// A CAPI cluster has no nico payload on either side, so both read as the
	// zero value and the profile comparisons below are a no-op for it.
	pinnedNico, _ := pinned.NicoProvider()
	desiredNico, _ := desired.NicoProvider()
	if pinnedNico.Release != desiredNico.Release {
		changes = append(changes, ControlPlaneRolloutChange{Field: "release", From: pinnedNico.Release, To: desiredNico.Release})
	}
	if pinnedNico.ConfigProfile != desiredNico.ConfigProfile {
		changes = append(changes, ControlPlaneRolloutChange{Field: "configProfile", From: pinnedNico.ConfigProfile, To: desiredNico.ConfigProfile})
	}
	return changes
}

// IsControlPlaneRolloutRequired reports whether two control-plane config snapshots
// differ in a field that requires replacing (recycling) a control-plane node: the
// Kubernetes version, the release, or the config profile. It is a thin
// predicate over ComputeControlPlaneRolloutChanges, so the boolean and the
// reported diff can never disagree. The common (unchanged) case allocates nothing,
// since ComputeControlPlaneRolloutChanges returns a nil slice when the snapshots
// match.
func IsControlPlaneRolloutRequired(a, b ControlPlaneConfigSpec) bool {
	return len(ComputeControlPlaneRolloutChanges(a, b)) > 0
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

// ListControlPlaneConfigurationsByCluster returns every control-plane
// configuration snapshot for a cluster, ordered oldest to newest, so callers can
// build a config-change history.
func (q *queriesImpl) ListControlPlaneConfigurationsByCluster(ctx context.Context, clusterID uuid.UUID) ([]ControlPlaneConfiguration, error) {
	if clusterID == uuid.Nil {
		return nil, nil
	}
	var rows []ControlPlaneConfiguration
	err := q.db.NewSelect().
		Model(&rows).
		Where("cluster_id = ?", clusterID).
		Order("created_at ASC").
		Scan(ctx)
	return rows, err
}

func LoadClusterControlPlaneConfig(ctx context.Context, q Queries, cluster *Cluster) error {
	if cluster == nil {
		return nil
	}
	if cluster.ControlPlaneConfig.ConfigurationID == uuid.Nil {
		cluster.ControlPlaneConfig = ControlPlaneConfig{
			ConfigurationID:       cluster.ControlPlaneConfig.ConfigurationID,
			DesiredCount:          cluster.ControlPlaneConfig.DesiredCount,
			MaxSurge:              cluster.ControlPlaneConfig.MaxSurge,
			AutoScaleDownDisabled: cluster.ControlPlaneConfig.AutoScaleDownDisabled,
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
				ConfigurationID:       clusters[i].ControlPlaneConfig.ConfigurationID,
				DesiredCount:          clusters[i].ControlPlaneConfig.DesiredCount,
				MaxSurge:              clusters[i].ControlPlaneConfig.MaxSurge,
				AutoScaleDownDisabled: clusters[i].ControlPlaneConfig.AutoScaleDownDisabled,
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
		c.AutoScaleDownDisabled == other.AutoScaleDownDisabled &&
		c.ControlPlaneConfigSpec.Equal(other.ControlPlaneConfigSpec)
}
