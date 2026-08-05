// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nke

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
)

// Backend identifies the cluster infrastructure backend.
type Backend string

const (
	// BackendCAPD is the Cluster API Docker provider backend.
	BackendCAPD Backend = "capd"
	// BackendForgeVMS is the local forge-vms Forge emulator backend.
	BackendForgeVMS Backend = "forge-vms"
)

// NodePoolSpec describes a single node pool to create.
type NodePoolSpec struct {
	Name, NodeNamePrefix string
	ControlPlane         bool
	DesiredNodeCount     int
	SystemReservedMemory string // optional, e.g. "1Gi"

	// Forge-backed pool fields (BackendForgeVMS). UUIDs are parsed from these
	// strings; OSProfile/ConfigProfile/DiskSelectionStrategy are passed through.
	InstanceTypeID        string
	VpcPrefixIDs          []string
	SSHKeyGroupIDs        []string
	OSProfile             string
	ConfigProfile         string
	DiskSelectionStrategy string // "smallest" | "largest"
}

// ControlPlane describes the control-plane pools plus cluster-wide knobs.
type ControlPlane struct {
	Pools          []NodePoolSpec
	EtcdEncrypted  bool
	DisableKubeVip bool
}

// ClusterNetworkSpec describes the non-default network configuration to pass
// to cluster creation. ExpectedPodCIDRs is test-only metadata used by e2e
// validations and is not sent to the CWE API.
type ClusterNetworkSpec struct {
	PodCIDR          string
	ServiceCIDR      string
	ExpectedPodCIDRs []string
}

// ClusterSpec is the backend-parameterized, N-pool description of a tenant
// cluster to create through the CWE API.
type ClusterSpec struct {
	Name              string // base name; CreateCluster appends a timestamp for uniqueness
	Backend           Backend
	KubernetesVersion string // e.g. "1.34.2"
	CNIPlugin         string // e.g. "calico"
	CAPDNodeImage     string // CAPD only: capd customImage, e.g. "cwe-capd-kindest-node:local"
	ControlPlane      ControlPlane
	Workers           []NodePoolSpec
	Network           *ClusterNetworkSpec

	// Forge-backed cluster fields (BackendForgeVMS).
	OrgID  string
	SiteID string // registered (non-nil) Forge site
	VpcID  string
}

var (
	//go:embed capd_cluster.json
	capdClusterJSON []byte
	//go:embed capd_nodepool.json
	capdNodePoolJSON []byte
)

// buildCreateRequest dispatches to the backend-specific request builder.
func buildCreateRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	switch spec.Backend {
	case BackendCAPD:
		return buildCAPDRequest(spec)
	case BackendForgeVMS:
		return buildForgeRequest(spec)
	default:
		return api.CreateClusterRequest{}, fmt.Errorf("unsupported backend %q", spec.Backend)
	}
}

// buildCAPDRequest stamps the embedded CAPD skeletons with the values from spec
// and returns the generated-API request.
func buildCAPDRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	var req api.CreateClusterRequest
	if err := json.Unmarshal(capdClusterJSON, &req); err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("unmarshaling cluster skeleton: %w", err)
	}

	req.Name = timestampedName(spec.Name)
	req.ControlPlaneKubernetesVersion = spec.KubernetesVersion
	req.CniPlugin = spec.CNIPlugin
	req.EtcdEncrypted = spec.ControlPlane.EtcdEncrypted
	req.DisableKubeVip = spec.ControlPlane.DisableKubeVip
	applyNetworkConfig(&req, spec.Network)
	if err := setMapPath(req.CapiInfraCluster, req.Name, "metadata", "name"); err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("setting infra cluster name: %w", err)
	}

	for _, ps := range orderedPools(spec) {
		pool, err := buildCAPDNodePool(ps, spec.CAPDNodeImage)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building node pool %q: %w", ps.Name, err)
		}
		req.NodePools = append(req.NodePools, pool)
	}

	return req, nil
}

func buildCAPDNodePool(spec NodePoolSpec, nodeImage string) (api.NodePoolCreateConfig, error) {
	var pool api.NodePoolCreateConfig
	if err := json.Unmarshal(capdNodePoolJSON, &pool); err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("unmarshaling node pool skeleton: %w", err)
	}

	pool.Name = spec.Name
	pool.NodeNamePrefix = spec.NodeNamePrefix
	pool.ControlPlane = spec.ControlPlane
	pool.DesiredNodeCount = spec.DesiredNodeCount
	if spec.SystemReservedMemory != "" {
		pool.SystemReservedMemory = spec.SystemReservedMemory
	}
	if err := setMapPath(pool.CapiInfraMachineTemplate, nodeImage, "spec", "template", "spec", "customImage"); err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("setting custom image: %w", err)
	}

	return pool, nil
}

// buildForgeRequest builds a Forge-backed create request (no CAPI manifests).
// The Forge identifiers come from spec; see test/e2e/oidc_test.go for the
// forge-vms seed values.
func buildForgeRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	siteID, err := uuid.Parse(spec.SiteID)
	if err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("parsing site ID %q: %w", spec.SiteID, err)
	}
	vpcID, err := uuid.Parse(spec.VpcID)
	if err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("parsing VPC ID %q: %w", spec.VpcID, err)
	}

	teleportDisabled := false
	req := api.CreateClusterRequest{
		Name:                          timestampedName(spec.Name),
		OrgID:                         spec.OrgID,
		SiteID:                        siteID,
		VpcID:                         vpcID,
		ControlPlaneKubernetesVersion: spec.KubernetesVersion,
		CniPlugin:                     spec.CNIPlugin,
		EtcdEncrypted:                 spec.ControlPlane.EtcdEncrypted,
		TeleportEnabled:               &teleportDisabled,
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool, err := buildForgeNodePool(ps)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building node pool %q: %w", ps.Name, err)
		}
		req.NodePools = append(req.NodePools, pool)
	}

	return req, nil
}

func applyNetworkConfig(req *api.CreateClusterRequest, spec *ClusterNetworkSpec) {
	if spec == nil || (strings.TrimSpace(spec.PodCIDR) == "" && strings.TrimSpace(spec.ServiceCIDR) == "") {
		return
	}
	req.Network = &api.ClusterNetworkCreateConfig{
		PodCIDR:     spec.PodCIDR,
		ServiceCIDR: spec.ServiceCIDR,
	}
}

func buildForgeNodePool(spec NodePoolSpec) (api.NodePoolCreateConfig, error) {
	instanceTypeID, err := uuid.Parse(spec.InstanceTypeID)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing instance type ID %q: %w", spec.InstanceTypeID, err)
	}
	vpcPrefixIDs, err := parseUUIDs(spec.VpcPrefixIDs)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing VPC prefix IDs: %w", err)
	}
	sshKeyGroupIDs, err := parseUUIDs(spec.SSHKeyGroupIDs)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing SSH key group IDs: %w", err)
	}

	pool := api.NodePoolCreateConfig{
		Name:                  spec.Name,
		NodeNamePrefix:        spec.NodeNamePrefix,
		ControlPlane:          spec.ControlPlane,
		DesiredNodeCount:      spec.DesiredNodeCount,
		InstanceTypeID:        instanceTypeID,
		VpcPrefixIDs:          vpcPrefixIDs,
		SSHKeyGroupIDs:        &sshKeyGroupIDs,
		OSProfile:             spec.OSProfile,
		ConfigProfile:         spec.ConfigProfile,
		DiskSelectionStrategy: api.DiskSelectionStrategyCreate(spec.DiskSelectionStrategy),
	}
	if spec.SystemReservedMemory != "" {
		pool.SystemReservedMemory = spec.SystemReservedMemory
	}

	return pool, nil
}

// ForgeNodePoolRequest builds a standalone create-node-pool request for a
// forge-vms cluster from spec. It delegates the UUID parsing and field shaping
// to buildForgeNodePool so the two stay in sync, adding only the owning cluster
// ID that the CreateNodePool endpoint requires (unlike the cluster-create path,
// which nests the pool inside the cluster request).
func ForgeNodePoolRequest(clusterID uuid.UUID, spec NodePoolSpec) (api.CreateNodePoolRequest, error) {
	pool, err := buildForgeNodePool(spec)
	if err != nil {
		return api.CreateNodePoolRequest{}, err
	}
	return api.CreateNodePoolRequest{
		ClusterID:             clusterID,
		Name:                  pool.Name,
		NodeNamePrefix:        pool.NodeNamePrefix,
		ControlPlane:          pool.ControlPlane,
		DesiredNodeCount:      pool.DesiredNodeCount,
		InstanceTypeID:        pool.InstanceTypeID,
		VpcPrefixIDs:          pool.VpcPrefixIDs,
		SSHKeyGroupIDs:        pool.SSHKeyGroupIDs,
		OSProfile:             pool.OSProfile,
		ConfigProfile:         pool.ConfigProfile,
		DiskSelectionStrategy: pool.DiskSelectionStrategy,
		SystemReservedMemory:  pool.SystemReservedMemory,
	}, nil
}

// orderedPools returns the control-plane pools (forced controlPlane=true) first,
// followed by the worker pools (forced controlPlane=false).
func orderedPools(spec ClusterSpec) []NodePoolSpec {
	pools := make([]NodePoolSpec, 0, len(spec.ControlPlane.Pools)+len(spec.Workers))
	for _, cpPool := range spec.ControlPlane.Pools {
		cpPool.ControlPlane = true
		pools = append(pools, cpPool)
	}
	for _, workerPool := range spec.Workers {
		workerPool.ControlPlane = false
		pools = append(pools, workerPool)
	}
	return pools
}

func timestampedName(base string) string {
	return fmt.Sprintf("%s-%s", base, time.Now().Format("20060102-150405"))
}

func parseUUIDs(ss []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("parsing UUID %q: %w", s, err)
		}
		out = append(out, u)
	}
	return out, nil
}

// setMapPath walks the nested map[string]interface{} along keys (asserting each
// intermediate level) and assigns value at the final key.
func setMapPath(root map[string]any, value string, keys ...string) error {
	if len(keys) == 0 {
		return fmt.Errorf("no keys provided")
	}
	cur := root
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return fmt.Errorf("key %q is not a nested object", k)
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = value
	return nil
}
