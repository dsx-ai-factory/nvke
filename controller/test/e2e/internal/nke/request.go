// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	// BackendNicoContainers is the local CAPNico backend backed by Docker containers.
	BackendNicoContainers Backend = "nico-containers"
	// BackendForgeVMS is the local forge-vms Forge emulator backend.
	BackendForgeVMS Backend = "forge-vms"
)

// NodePoolSpec describes a single node pool to create.
type NodePoolSpec struct {
	Name, NodeNamePrefix string
	ControlPlane         bool
	DesiredNodeCount     int
	SystemReservedMemory string // optional, e.g. "1Gi"
	KubeReservedMemory   string // optional, e.g. "250Mi"

	// Infrastructure-specific pool fields. Nico containers use InstanceTypeID,
	// SubnetIDs, and SSHKeyGroupIDs; Forge VMs use InstanceTypeID,
	// VpcPrefixIDs, SSHKeyGroupIDs, and the profile/disk fields.
	InstanceTypeID        string
	SubnetIDs             []string
	VpcPrefixIDs          []string
	SSHKeyGroupIDs        []string
	OSProfile             string
	ConfigProfile         string
	DiskSelectionStrategy string // "smallest" | "largest"
}

// ControlPlane describes the control-plane pools plus cluster-wide knobs.
type ControlPlane struct {
	Pools           []NodePoolSpec
	EtcdEncrypted   bool
	ControlPlaneVIP string
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

	// Infrastructure-specific cluster fields. Nico containers use SiteID and
	// VpcID inside their CAPI manifests; Forge VMs use all three as API fields.
	OrgID  string
	SiteID string
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
	case BackendNicoContainers:
		return buildNicoRequest(spec)
	case BackendForgeVMS:
		return buildForgeRequest(spec)
	default:
		return api.CreateClusterRequest{}, fmt.Errorf("unsupported backend %q", spec.Backend)
	}
}

// buildNicoRequest builds a CAPI request whose infrastructure manifests are
// consumed by CAPNico. The fixed mock identifiers are supplied by the caller
// through spec so this builder remains independent of a particular test seed.
func buildNicoRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	siteID, err := uuid.Parse(spec.SiteID)
	if err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("parsing Nico site ID %q: %w", spec.SiteID, err)
	}
	vpcID, err := uuid.Parse(spec.VpcID)
	if err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("parsing Nico VPC ID %q: %w", spec.VpcID, err)
	}
	name := timestampedName(spec.Name)

	req := api.CreateClusterRequest{
		Name:                          name,
		SiteID:                        uuid.Nil,
		ControlPlaneKubernetesVersion: spec.KubernetesVersion,
		CniPlugin:                     spec.CNIPlugin,
		EtcdEncrypted:                 spec.ControlPlane.EtcdEncrypted,
		ControlPlaneVIP:               spec.ControlPlane.ControlPlaneVIP,
		CapiInfraCluster: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
			"kind":       "NicoCluster",
			"metadata": map[string]any{
				"name": name,
			},
			"spec": map[string]any{
				"siteID": siteID.String(),
			},
		},
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool, err := buildNicoNodePool(req.Name, vpcID, ps)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building Nico node pool %q: %w", ps.Name, err)
		}
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
	}

	return req, nil
}

func buildNicoNodePool(clusterName string, vpcID uuid.UUID, spec NodePoolSpec) (api.NodePoolCreateConfig, error) {
	instanceTypeID, err := uuid.Parse(spec.InstanceTypeID)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing instance type ID %q: %w", spec.InstanceTypeID, err)
	}
	subnetIDs, err := parseUUIDs(spec.SubnetIDs)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing subnet IDs: %w", err)
	}
	if len(subnetIDs) == 0 {
		return api.NodePoolCreateConfig{}, fmt.Errorf("at least one subnet ID is required")
	}
	sshKeyGroupIDs, err := parseUUIDs(spec.SSHKeyGroupIDs)
	if err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("parsing SSH key group IDs: %w", err)
	}

	interfaces := make([]map[string]any, 0, len(subnetIDs))
	for _, subnetID := range subnetIDs {
		interfaces = append(interfaces, map[string]any{"subnetID": subnetID.String()})
	}
	sshKeyGroups := make([]string, 0, len(sshKeyGroupIDs))
	for _, sshKeyGroupID := range sshKeyGroupIDs {
		sshKeyGroups = append(sshKeyGroups, sshKeyGroupID.String())
	}
	role := "worker"
	if spec.ControlPlane {
		role = "control-plane"
	}

	pool := api.NodePoolCreateConfig{
		Name:                       spec.Name,
		NodeNamePrefix:             spec.NodeNamePrefix,
		ControlPlane:               spec.ControlPlane,
		DesiredNodeCount:           spec.DesiredNodeCount,
		SystemReservedMemory:       spec.SystemReservedMemory,
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken,
		CapiPreKubeadmCommands:     []string{},
		CapiInfraMachineTemplate: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
			"kind":       "NicoMachineTemplate",
			"metadata": map[string]any{
				"name": fmt.Sprintf("%s-%s-template", clusterName, spec.Name),
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"vpcID":                   vpcID.String(),
						"instanceTypeID":          instanceTypeID.String(),
						"cloudInitInjectHostname": true,
						"ipxeScript":              fmt.Sprintf("#!ipxe\necho local-nico-%s\n", role),
						"sshKeyGroupIDs":          sshKeyGroups,
						"interfaces":              interfaces,
					},
				},
			},
		},
	}

	return pool, nil
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
	req.ControlPlaneVIP = spec.ControlPlane.ControlPlaneVIP
	applyNetworkConfig(&req, spec.Network)
	if err := setMapPath(req.CapiInfraCluster, req.Name, "metadata", "name"); err != nil {
		return api.CreateClusterRequest{}, fmt.Errorf("setting infra cluster name: %w", err)
	}

	for _, ps := range orderedPools(spec) {
		pool, err := buildCAPDNodePool(ps, spec.CAPDNodeImage)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building node pool %q: %w", ps.Name, err)
		}
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
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
	if spec.KubeReservedMemory != "" {
		pool.KubeReservedMemory = spec.KubeReservedMemory
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
		ControlPlaneVIP:               spec.ControlPlane.ControlPlaneVIP,
		TeleportEnabled:               &teleportDisabled,
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool, err := buildForgeNodePool(ps)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building node pool %q: %w", ps.Name, err)
		}
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
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
	if spec.KubeReservedMemory != "" {
		pool.KubeReservedMemory = spec.KubeReservedMemory
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
		KubeReservedMemory:    pool.KubeReservedMemory,
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
