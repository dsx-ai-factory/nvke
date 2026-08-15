// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ConstraintClustersNameUnique = "clusters_name_unique"
)

var (
	ErrClusterNameNotUnique   = errors.New("cluster name not unique")
	ErrMissingProviderPayload = errors.New("missing provider payload")
)

type ProviderType string

const (
	ProviderTypeNico ProviderType = "nico"
	ProviderTypeCAPI ProviderType = "capi"
)

type NicoClusterProvider struct {
	OrgID  string    `json:"org_id,omitempty"`
	SiteID uuid.UUID `json:"site_id,omitempty"`
	VpcID  uuid.UUID `json:"vpc_id,omitempty"`
}

type CAPIClusterProvider struct {
	InfraCluster json.RawMessage `json:"infra_cluster,omitempty"`
}

type Cluster struct {
	ID                    uuid.UUID `bun:"type:uuid,pk,default:gen_random_uuid()"`
	Name                  string    `bun:"type:text,notnull"`
	OrgID                 string    `bun:"type:text,notnull"`
	SiteID                uuid.UUID `bun:"type:uuid,notnull"`
	VpcID                 uuid.UUID `bun:"type:uuid,notnull"`
	CniPlugin             CniPlugin `bun:"type:text"`
	CniVersion            string    `bun:"type:text"`
	EtcdEncrypted         bool      `bun:"type:boolean,notnull,default:false"`
	KubernetesVersion     string    `bun:"type:text,notnull"`
	ControlPlaneVIP       string    `bun:"type:text"`
	EnableCPNodeRecycling bool      `bun:"type:boolean,notnull,default:false"`
	TeleportEnabled       bool      `bun:"type:boolean,notnull,default:false"`
	PodCIDR               string    `bun:"pod_cidr,type:text,notnull,default:'192.168.0.0/16'"`
	ServiceCIDR           string    `bun:"service_cidr,type:text,notnull,default:'10.96.0.0/12'"`
	OIDCAuthEnabled       bool      `bun:"type:boolean,notnull,default:false"`

	CapiInfraCluster json.RawMessage      `bun:"type:jsonb,notnull,default:'{}'"`
	ProviderType     ProviderType         `bun:"provider_type,type:text,notnull"`
	Nico             *NicoClusterProvider `bun:"nico,type:jsonb"`
	Capi             *CAPIClusterProvider `bun:"capi,type:jsonb"`

	// ControlPlaneConfig keeps the API/business shape while persisting these
	// fields as real DB columns on clusters (configuration_id, cp_desired_count,
	// cp_max_surge) via bun embed mapping.
	ControlPlaneConfig ControlPlaneConfig `bun:",embed"`

	Conditions ClusterConditions `bun:"type:jsonb"` // JSONB field keys use camelCase across all sub-structs for consistency (see ClusterConditions below)
	CreatedAt  time.Time         `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt  time.Time         `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

func (c *Cluster) NicoProvider() (NicoClusterProvider, error) {
	if c.ProviderType != ProviderTypeNico {
		return NicoClusterProvider{}, fmt.Errorf("cluster %s provider type is %q, not %q", c.ID, c.ProviderType, ProviderTypeNico)
	}
	if c.Nico == nil {
		return NicoClusterProvider{}, fmt.Errorf("%w: cluster %s nico", ErrMissingProviderPayload, c.ID)
	}
	return *c.Nico, nil
}

func (c *Cluster) CAPIProvider() (CAPIClusterProvider, error) {
	if c.ProviderType != ProviderTypeCAPI {
		return CAPIClusterProvider{}, fmt.Errorf("cluster %s provider type is %q, not %q", c.ID, c.ProviderType, ProviderTypeCAPI)
	}
	if c.Capi == nil {
		return CAPIClusterProvider{}, fmt.Errorf("%w: cluster %s capi", ErrMissingProviderPayload, c.ID)
	}
	return CAPIClusterProvider{
		InfraCluster: cloneRawMessage(c.Capi.InfraCluster),
	}, nil
}

func (c *Cluster) SetNicoProvider(provider NicoClusterProvider) {
	c.ProviderType = ProviderTypeNico
	c.Nico = &provider
	c.Capi = nil
	c.OrgID = provider.OrgID
	c.SiteID = provider.SiteID
	c.VpcID = provider.VpcID
}

func (c *Cluster) SetCAPIProvider(provider CAPIClusterProvider) {
	c.ProviderType = ProviderTypeCAPI
	c.Nico = nil
	c.Capi = &CAPIClusterProvider{
		InfraCluster: cloneRawMessage(provider.InfraCluster),
	}
	c.CapiInfraCluster = cloneRawMessage(provider.InfraCluster)
}

func (c *Cluster) SyncProviderFields() {
	c.Nico = nil
	c.Capi = nil
	if c.SiteID == uuid.Nil {
		c.ProviderType = ProviderTypeCAPI
		c.Capi = &CAPIClusterProvider{
			InfraCluster: cloneRawMessage(c.CapiInfraCluster),
		}
		return
	}

	c.ProviderType = ProviderTypeNico
	c.Nico = &NicoClusterProvider{
		OrgID:  c.OrgID,
		SiteID: c.SiteID,
		VpcID:  c.VpcID,
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// ControlPlaneConfigFromNodePool copies a control-plane node pool into ControlPlaneConfig.
// Provider payloads (Nico/CAPI) are stored in the spec via SetNico/SetCapi, which also
// keeps the legacy flat columns in sync.
// Returns nil if np is nil or not a control-plane pool.
func ControlPlaneConfigFromNodePool(np *NodePool) *ControlPlaneConfig {
	if np == nil || !np.ControlPlane {
		return nil
	}

	labels := make([]string, len(np.NodeLabels))
	copy(labels, np.NodeLabels)

	cfg := &ControlPlaneConfig{
		DesiredCount: np.DesiredCount,
		ControlPlaneConfigSpec: ControlPlaneConfigSpec{
			NodeLabels:                  labels,
			SystemReservedMemory:        np.SystemReservedMemory,
			KubeReservedMemory:          np.KubeReservedMemory,
			TeleportEnabled:             np.TeleportEnabled,
			ProvisioningMonitorDisabled: np.ProvisioningMonitorDisabled,
			KubernetesVersion:           np.KubernetesVersion,
		},
	}

	if np.Nico != nil {
		nicoNodePoolProvider, err := np.NicoProvider()
		if err != nil {
			return nil
		}
		cfg.SetNico(NicoControlPlaneProvider{
			InstanceTypePreference: orderedInstanceTypePreferences([]NodePool{*np}),
			SubnetIDs:              nicoNodePoolProvider.SubnetIDs,
			VpcPrefixIDs:           nicoNodePoolProvider.VpcPrefixIDs,
			OSProfile:              nicoNodePoolProvider.OSProfile,
			DiskSelectionStrategy:  nicoNodePoolProvider.DiskSelectionStrategy,
			ConfigProfile:          nicoNodePoolProvider.ConfigProfile,
			SSHKeyGroupIDs:         nicoNodePoolProvider.SSHKeyGroupIDs,
		})
	}

	if np.Capi != nil {
		capiNodePoolProvider, err := np.CAPIProvider()
		if err != nil {
			return nil
		}
		infraPrefs := []json.RawMessage{}
		if len(capiNodePoolProvider.InfraMachineTemplate) > 0 {
			infraPrefs = append(infraPrefs, append(json.RawMessage(nil), capiNodePoolProvider.InfraMachineTemplate...))
		}
		cfg.SetCapi(CAPIControlPlaneProvider{
			InfraMachineTemplatePreferences: infraPrefs,
			SpireAttestationMethod:          capiNodePoolProvider.SpireAttestationMethod,
			BootstrapFiles:                  capiNodePoolProvider.BootstrapFiles,
			PreKubeadmCommands:              capiNodePoolProvider.PreKubeadmCommands,
		})
	}

	return cfg
}

// MergeControlPlaneConfigFromNodePools builds one cluster-level control_plane_config from every
// control-plane node pool in the cluster. The function sorts pools by CreatedAt descending
// (newest first); the newest control-plane pool supplies scalar fields like OS/config profile
// and CAPI settings, while counts and list-like fields aggregate across all pools.
func MergeControlPlaneConfigFromNodePools(pools []NodePool) *ControlPlaneConfig {
	if len(pools) == 0 {
		return nil
	}
	sortedPools := append([]NodePool(nil), pools...)
	sort.SliceStable(sortedPools, func(i, j int) bool {
		return sortedPools[i].CreatedAt.After(sortedPools[j].CreatedAt)
	})
	primary := &sortedPools[0]

	cfg := ControlPlaneConfigFromNodePool(primary)
	if cfg == nil {
		return nil
	}

	desired := 0
	tele := false
	provisioningMonitorDisabled := false
	instPref := orderedInstanceTypePreferences(sortedPools)
	infraSeen := make(map[string]struct{})
	var infraPref []json.RawMessage
	subSeen := make(map[uuid.UUID]struct{})
	var subnets []uuid.UUID
	pfxSeen := make(map[uuid.UUID]struct{})
	var vpcPrefixes []uuid.UUID
	keySeen := make(map[uuid.UUID]struct{})
	var sshKeys []uuid.UUID
	var labels []string
	labelSeen := make(map[string]struct{})
	k8sVer := ""

	for i := range sortedPools {
		p := &sortedPools[i]
		if !p.ControlPlane {
			continue
		}
		desired += p.DesiredCount
		tele = tele || p.TeleportEnabled
		provisioningMonitorDisabled = provisioningMonitorDisabled || p.ProvisioningMonitorDisabled

		var infraTemplate json.RawMessage
		if p.Capi != nil {
			if capiNodePoolProvider, err := p.CAPIProvider(); err == nil {
				infraTemplate = capiNodePoolProvider.InfraMachineTemplate
			}
		}
		infraPref = appendUniqueManifests(infraPref, infraSeen, infraTemplate)

		var subnetIDs, vpcPrefixIDs, sshKeyGroupIDs []uuid.UUID
		if p.Nico != nil {
			if nicoNodePoolProvider, err := p.NicoProvider(); err == nil {
				subnetIDs = nicoNodePoolProvider.SubnetIDs
				vpcPrefixIDs = nicoNodePoolProvider.VpcPrefixIDs
				sshKeyGroupIDs = nicoNodePoolProvider.SSHKeyGroupIDs
			}
		}
		subnets = appendUniqueUUIDs(subnets, subSeen, subnetIDs)
		vpcPrefixes = appendUniqueUUIDs(vpcPrefixes, pfxSeen, vpcPrefixIDs)
		sshKeys = appendUniqueUUIDs(sshKeys, keySeen, sshKeyGroupIDs)
		for _, l := range p.NodeLabels {
			if _, ok := labelSeen[l]; !ok {
				labelSeen[l] = struct{}{}
				labels = append(labels, l)
			}
		}
		if p.KubernetesVersion != "" {
			if k8sVer == "" {
				k8sVer = p.KubernetesVersion
			}
		}
	}

	cfg.DesiredCount = desired
	cfg.NodeLabels = labels
	cfg.TeleportEnabled = tele
	cfg.ProvisioningMonitorDisabled = provisioningMonitorDisabled
	if k8sVer != "" {
		cfg.KubernetesVersion = k8sVer
	}

	// Rebuild provider payloads with aggregated list fields, preserving scalar
	// fields (OSProfile, ConfigProfile, etc.) already set from the primary pool.
	if cfg.Nico != nil {
		cfg.SetNico(NicoControlPlaneProvider{
			InstanceTypePreference: instPref,
			SubnetIDs:              subnets,
			VpcPrefixIDs:           vpcPrefixes,
			SSHKeyGroupIDs:         sshKeys,
			OSProfile:              cfg.Nico.OSProfile,
			DiskSelectionStrategy:  cfg.Nico.DiskSelectionStrategy,
			ConfigProfile:          cfg.Nico.ConfigProfile,
		})
	}
	if cfg.Capi != nil {
		cfg.SetCapi(CAPIControlPlaneProvider{
			InfraMachineTemplatePreferences: infraPref,
			SpireAttestationMethod:          cfg.Capi.SpireAttestationMethod,
			BootstrapFiles:                  cfg.Capi.BootstrapFiles,
			PreKubeadmCommands:              cfg.Capi.PreKubeadmCommands,
		})
	}

	return cfg
}

func appendUniqueUUIDs(dst []uuid.UUID, seen map[uuid.UUID]struct{}, add []uuid.UUID) []uuid.UUID {
	for _, u := range add {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		dst = append(dst, u)
	}
	return dst
}

func appendUniqueManifests(dst []json.RawMessage, seen map[string]struct{}, raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return dst
	}
	key := string(raw)
	if _, ok := seen[key]; ok {
		return dst
	}
	seen[key] = struct{}{}
	return append(dst, append(json.RawMessage(nil), raw...))
}

func orderedInstanceTypePreferences(pools []NodePool) []uuid.UUID {
	// Pools are passed in newest-first order. Traverse in reverse so the oldest
	// control-plane pool has the highest instance type preference.
	// Only Nico pools carry an InstanceTypeID; CAPI pools are skipped.
	seen := make(map[uuid.UUID]struct{})
	instPref := make([]uuid.UUID, 0, len(pools))
	for i := len(pools) - 1; i >= 0; i-- {
		p := pools[i]
		if !p.ControlPlane || p.Nico == nil {
			continue
		}
		nicoNodePoolProvider, err := p.NicoProvider()
		if err != nil || nicoNodePoolProvider.InstanceTypeID == uuid.Nil {
			continue
		}
		if _, ok := seen[nicoNodePoolProvider.InstanceTypeID]; ok {
			continue
		}
		seen[nicoNodePoolProvider.InstanceTypeID] = struct{}{}
		instPref = append(instPref, nicoNodePoolProvider.InstanceTypeID)
	}
	return instPref
}

func mergeInstanceTypePreferences(existing, derived []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(existing)+len(derived))
	out := make([]uuid.UUID, 0, len(existing)+len(derived))
	for _, id := range append(slices.Clone(existing), derived...) {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func preserveClusterControlPlaneOverrides(existing Cluster, merged *ControlPlaneConfig) {
	if merged == nil {
		return
	}
	// MaxSurge is cluster-level only; sync must not derive it from node pools.
	merged.MaxSurge = existing.ControlPlaneConfig.MaxSurge
	merged.ProvisioningMonitorDisabled = existing.ControlPlaneConfig.ProvisioningMonitorDisabled
	merged.InstanceTypePreference = mergeInstanceTypePreferences(
		existing.ControlPlaneConfig.InstanceTypePreference,
		merged.InstanceTypePreference,
	)
}

type ClusterConditions struct {
	Provisioning          Condition `json:"provisioning"`
	Provisioned           Condition `json:"provisioned"`
	Deleting              Condition `json:"deleting"`
	ControlPlaneUpgrading Condition `json:"controlPlaneUpgrading"`
	CPNodesRecycling      Condition `json:"cpNodesRecycling"`
	// UpdateInProgress marks a mutually-exclusive cluster operation started by
	// UpdateCluster; legacy operation flags may still be populated by controllers.
	UpdateInProgress *UpdateInProgress `json:"updateInProgress,omitempty"`
	UpdateFailed     *Condition        `json:"updateFailed,omitempty"`
}

func (c *Cluster) BeforeUpdate() error {
	c.UpdatedAt = time.Now()
	return nil
}

type ClusterFilters struct {
	Name   *string
	SiteID *uuid.UUID
}

type CniPlugin string

const (
	CNICalico                      CniPlugin = "calico"
	CNIOvnKubernetes               CniPlugin = "ovn-kubernetes"
	DefaultCalicoCNIVersion                  = "3.30.1"
	DefaultOvnKubernetesCNIVersion           = "1.4.0"
)

func ListCNIPlugins() []CniPlugin {
	return []CniPlugin{CNICalico, CNIOvnKubernetes}
}

func DefaultCNIVersion(plugin CniPlugin) (string, bool) {
	switch plugin {
	case CNICalico:
		return DefaultCalicoCNIVersion, true
	case CNIOvnKubernetes:
		return DefaultOvnKubernetesCNIVersion, true
	default:
		return "", false
	}
}

func EffectiveCNIVersion(plugin CniPlugin, version string) string {
	if version = strings.TrimSpace(version); version != "" {
		return version
	}
	if defaultVersion, ok := DefaultCNIVersion(plugin); ok {
		return defaultVersion
	}
	return ""
}

type ClusterQueries interface {
	ListClusters(ctx context.Context, filters ClusterFilters) ([]Cluster, error)
	GetCluster(ctx context.Context, id uuid.UUID) (Cluster, error)
	// GetClusterForUpdate locks the cluster row with SELECT ... FOR UPDATE, blocking
	// concurrent mutators until the enclosing tx commits or rolls back. Use only
	// inside RunInTx on write paths that need mutual exclusion (e.g., UpdateCluster
	// paths that set UpdateInProgress).
	GetClusterForUpdate(ctx context.Context, id uuid.UUID) (Cluster, error)
	CreateCluster(ctx context.Context, cluster *Cluster) (Cluster, error)
	UpdateCluster(ctx context.Context, cluster *Cluster) (Cluster, error)
	DeleteCluster(ctx context.Context, id uuid.UUID) error
	// SyncClusterControlPlaneConfigFromCPNodePools sets control_plane_config from all control-plane
	// node pools for the cluster that are not marked deleting.
	SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error
}

func (q *queriesImpl) ListClusters(ctx context.Context, filters ClusterFilters) ([]Cluster, error) {
	var clusters []Cluster
	query := q.db.NewSelect().Model(&clusters)

	if filters.Name != nil {
		query = query.Where("name = ?", *filters.Name)
	}

	if filters.SiteID != nil {
		query = query.Where("site_id = ?", *filters.SiteID)
	}

	err := query.Scan(ctx)
	if err != nil {
		return nil, err
	}
	if err := LoadClustersControlPlaneConfig(ctx, q, clusters); err != nil {
		return nil, err
	}
	return clusters, nil
}

func (q *queriesImpl) GetCluster(ctx context.Context, id uuid.UUID) (Cluster, error) {
	var cluster Cluster
	err := q.db.NewSelect().Model(&cluster).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, ErrNotFound
		}
		return Cluster{}, err
	}
	if err := LoadClusterControlPlaneConfig(ctx, q, &cluster); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

func (q *queriesImpl) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (Cluster, error) {
	var cluster Cluster
	err := q.db.NewSelect().Model(&cluster).Where("id = ?", id).For("UPDATE").Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, ErrNotFound
		}
		return Cluster{}, err
	}
	if err := LoadClusterControlPlaneConfig(ctx, q, &cluster); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

func (q *queriesImpl) CreateCluster(ctx context.Context, cluster *Cluster) (Cluster, error) {
	cluster.SyncProviderFields()
	_, err := q.db.NewInsert().Model(cluster).Exec(ctx)
	if err != nil {
		if isViolationOfConstraint(err, ConstraintClustersNameUnique) {
			return Cluster{}, ErrClusterNameNotUnique
		}
		return Cluster{}, err
	}
	return *cluster, nil
}

func (q *queriesImpl) UpdateCluster(ctx context.Context, cluster *Cluster) (Cluster, error) {
	cluster.SyncProviderFields()
	res, err := q.db.NewUpdate().Model(cluster).WherePK().Exec(ctx)
	if err != nil {
		return Cluster{}, err
	}
	return *cluster, notFoundErrIfNoRowsAffected(res)
}

func (q *queriesImpl) DeleteCluster(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.NewDelete().Model(&Cluster{}).Where("id = ?", id).Exec(ctx)
	return err
}

func (q *queriesImpl) providerTypeForCluster(ctx context.Context, clusterID uuid.UUID) (ProviderType, error) {
	var cluster Cluster
	err := q.db.NewSelect().
		Model(&cluster).
		Column("provider_type", "site_id").
		Where("id = ?", clusterID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if cluster.ProviderType != "" {
		return cluster.ProviderType, nil
	}
	if cluster.SiteID == uuid.Nil {
		return ProviderTypeCAPI, nil
	}
	return ProviderTypeNico, nil
}

func (q *queriesImpl) SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error {
	cluster, err := q.GetCluster(ctx, clusterID)
	if err != nil {
		return err
	}

	cp := true
	pools, err := q.ListNodePools(ctx, NodePoolFilters{ClusterID: &clusterID, ControlPlane: &cp})
	if err != nil {
		return fmt.Errorf("list control plane node pools: %w", err)
	}

	active := make([]NodePool, 0, len(pools))
	for i := range pools {
		if pools[i].Conditions.Deleting.Status == ConditionStatusTrue {
			continue
		}
		active = append(active, pools[i])
	}
	cfg := MergeControlPlaneConfigFromNodePools(active)
	if cfg == nil {
		if cluster.Conditions.Deleting.Status == ConditionStatusTrue {
			return nil
		}
		return fmt.Errorf("expected at least one active control-plane node pool for cluster %s", clusterID)
	}
	preserveClusterControlPlaneOverrides(cluster, cfg)

	cluster.ControlPlaneConfig.DesiredCount = cfg.DesiredCount
	snapshotCfg := configSpecFromMergedPools(cfg)
	if err := createControlPlaneConfigRowIfChanged(ctx, q, &cluster, snapshotCfg, CreateControlPlaneConfigRowOpts{
		ChangeReason: "sync from control plane node pools",
	}); err != nil {
		return fmt.Errorf("create control plane configuration row: %w", err)
	}

	res, err := q.db.NewUpdate().
		Model(&Cluster{
			ID: clusterID,
			ControlPlaneConfig: ControlPlaneConfig{
				ConfigurationID: cluster.ControlPlaneConfig.ConfigurationID,
				DesiredCount:    cluster.ControlPlaneConfig.DesiredCount,
			},
		}).
		Column("configuration_id", "cp_desired_count").
		WherePK().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("update cluster control plane configuration: %w", err)
	}
	return notFoundErrIfNoRowsAffected(res)
}

// SetUpdateInProgress marks the cluster as having a mutually-exclusive op in
// flight. Callers are responsible for gating on the current state inside a
// RunInTx that used GetClusterForUpdate, and for persisting the cluster via
// UpdateCluster after calling this. SetUpdateInProgress unconditionally
// overwrites any prior UpdateInProgress entry — the mutual-exclusion check
// lives in the caller, not here.
func SetUpdateInProgress(c *Cluster, op OperationType, message string, startedAt time.Time) {
	c.Conditions.UpdateInProgress = &UpdateInProgress{
		Type:      op,
		Message:   message,
		StartedAt: startedAt,
	}
}

// ClearUpdateInProgress removes any in-flight op marker. Idempotent.
func ClearUpdateInProgress(c *Cluster) {
	c.Conditions.UpdateInProgress = nil
}

func SetUpdateFailed(c *Cluster, message string) {
	condition := NewCondition(true, message)
	c.Conditions.UpdateFailed = &condition
}

func ClearUpdateFailed(c *Cluster) {
	c.Conditions.UpdateFailed = nil
}
