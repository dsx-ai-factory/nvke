// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/api"
)

// Backend identifies the cluster infrastructure backend.
type Backend string

const (
	// BackendCAPD is the Cluster API Docker provider backend.
	BackendCAPD Backend = "capd"
	// BackendNicoContainers is the local CAPNico backend backed by Docker containers.
	BackendNicoContainers Backend = "nico-containers"
	// BackendNicoVMS is the local nico-vms Forge emulator backend.
	BackendNicoVMS Backend = "nico-vms"
	// BackendNicoForge is CAPNico against real Forge infrastructure. It builds
	// the same CAPI manifests as nico-containers, since CAPNico reconciles
	// both, and differs in the identifiers it names and in the two payloads
	// that turn a provisioned machine into an NKE node.
	BackendNicoForge Backend = "nico-forge"

	capiSpecKey          = "spec"
	nodeRoleWorker       = "worker"
	nodeRoleControlPlane = "control-plane"
)

// NodePoolSpec describes a single node pool to create.
type NodePoolSpec struct {
	Name, NodeNamePrefix string
	ControlPlane         bool
	DesiredNodeCount     int
	SystemReservedMemory string // optional, e.g. "1Gi"
	KubeReservedMemory   string // optional, e.g. "250Mi"

	// Infrastructure-specific pool fields. Nico containers use InstanceTypeID,
	// SubnetIDs, and SSHKeyGroupIDs; NICo VMs use InstanceTypeID,
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

	// Config, when non-nil, provisions the control plane through the cluster-level
	// control-plane controller (CreateClusterControlPlaneConfig) with no
	// control-plane node pool. It is mutually exclusive with Pools and requires
	// CWE with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER on (its default).
	Config *ControlPlaneConfigSpec
}

// ControlPlaneConfigSpec describes a cluster-level control-plane configuration
// used by control-plane-controller provisioning (no control-plane node pool).
// CAPD supplies the CAPI machine template from an embedded skeleton. NICo VMs
// fill the Forge fields below instead.
type ControlPlaneConfigSpec struct {
	DesiredCount int
	MaxSurge     *int

	InstanceTypeID       string
	OSProfile            string
	ConfigProfile        string
	VpcPrefixIDs         []string
	SubnetIDs            []string
	SSHKeyGroupIDs       []string
	SystemReservedMemory string
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
	// VpcID inside their CAPI manifests; NICo VMs use all three as API fields.
	OrgID  string
	SiteID string
	VpcID  string

	// NodePayloads is required by nico-forge and ignored by everything else.
	NodePayloads *RolePayloads
}

// NodePayloads are the two things that turn a bare machine into an NKE node:
// the iPXE script that boots the OS image, and the commands that install the
// NKE packages before kubeadm runs.
//
// The simulator backends stub both, because a container boots no image and a
// seeded emulator needs no packages. Real hardware needs both to be right, and
// both name component versions, which is why a nico-forge run has to be told
// what it is qualifying.
type NodePayloads struct {
	IPXEScript         string
	PreKubeadmCommands []string
}

// RolePayloads is one set per role, because a cluster's control plane and its
// workers need not be the same architecture: a GB200 tenant puts arm64 workers
// behind an amd64 control plane, and each boots the OS image and installs the
// packages built for its own machine.
//
// Both are required rather than one defaulting to the other. A single-
// architecture cluster passes the same value twice, which is a line of code,
// whereas a silent fallback would let a mixed cluster boot half its nodes on
// the wrong image and say nothing.
type RolePayloads struct {
	ControlPlane NodePayloads
	Worker       NodePayloads
}

var (
	//go:embed capd_cluster.json
	capdClusterJSON []byte
	//go:embed capd_nodepool.json
	capdNodePoolJSON []byte
	//go:embed capd_control_plane_config.json
	capdControlPlaneConfigJSON []byte
)

func containerCAPIReadyCommands() []string {
	var commands []string
	// Cloud-init replaces the agent arguments baked into the container node image.
	// Restore the E2E bundle source before kubeadm init needs it for CNI installation.
	bundleServerURL := strings.TrimRight(strings.TrimSpace(os.Getenv("NKE_E2E_BUNDLE_SERVER_URL")), "/")
	if bundleServerURL != "" {
		commands = append(commands, fmt.Sprintf(
			`sh -ec 'agent_env=/etc/default/nke-agent; sed_url=$(printf "%%s" "$1" | sed "s/[\\\\&|]/\\\\&/g"); sed -E -i.bak "/^AGENT_ARGS=/ { s/ --(bundle-server-url|index-file-url)(=[^ \" ]+| [^ \" ]+)//g; s/^AGENT_ARGS=\"--disable-mlx5-rescan ?/AGENT_ARGS=\"/; s/ --disable-mlx5-rescan//g; s|\"$| --disable-mlx5-rescan --bundle-server-url ${sed_url}/ --index-file-url ${sed_url}/index.json\"|; }" "$agent_env"; rm -f "$agent_env.bak"; systemctl restart nke-agent.service' -- %s`,
			shellQuote(bundleServerURL),
		))
	}
	return append(commands,
		"mkdir -p /run/cluster-api",
		"echo success > /run/cluster-api/bootstrap-success.complete",
	)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// BuildCreateRequest dispatches to the backend-specific request builder.
//
// Exported for internal/nkectl, which writes the same request to a file and
// creates it through the CLI. Sharing the builder is what keeps the CLI-driven
// table entry differing from its REST counterpart in transport alone.
func BuildCreateRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	switch spec.Backend {
	case BackendCAPD:
		return buildCAPDRequest(spec)
	case BackendNicoContainers:
		return buildNicoRequest(spec)
	case BackendNicoVMS:
		return buildNicoVMSRequest(spec)
	case BackendNicoForge:
		return buildNicoForgeRequest(spec)
	default:
		return api.CreateClusterRequest{}, fmt.Errorf("unsupported backend %q", spec.Backend)
	}
}

// buildNicoRequest builds a CAPI request whose infrastructure manifests are
// consumed by CAPNico. The fixed mock identifiers are supplied by the caller
// through spec so this builder remains independent of a particular test seed.
func buildNicoRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	return buildCAPNicoRequest(spec, containerNodePayloads)
}

// buildNicoForgeRequest builds the same CAPI request against real Forge. The
// manifests are identical to nico-containers, which is the point: CAPNico is
// what reconciles them either way, so running on hardware exercises the same
// request path rather than a parallel one.
//
// What differs is that the payloads have to be real. A container reaches
// kubeadm whatever the iPXE script says; a Forge instance boots the script it
// is given and comes up with only the packages it was told to install.
func buildNicoForgeRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	if spec.NodePayloads == nil {
		return api.CreateClusterRequest{}, fmt.Errorf("nico-forge requires node payloads: a real machine boots an image and installs packages")
	}
	payloads := *spec.NodePayloads
	for _, role := range []struct {
		name     string
		payloads NodePayloads
	}{
		{"control plane", payloads.ControlPlane},
		{"worker", payloads.Worker},
	} {
		if role.payloads.IPXEScript == "" {
			return api.CreateClusterRequest{}, fmt.Errorf("nico-forge requires a %s iPXE script: without one the instance boots nothing", role.name)
		}
		if len(role.payloads.PreKubeadmCommands) == 0 {
			return api.CreateClusterRequest{}, fmt.Errorf("nico-forge requires %s pre-kubeadm commands: without them the node has no nke-agent", role.name)
		}
	}

	return buildCAPNicoRequest(spec, func(controlPlane bool) NodePayloads {
		if controlPlane {
			return payloads.ControlPlane
		}
		return payloads.Worker
	})
}

// containerNodePayloads stubs both payloads for the container backend. The
// iPXE script is never booted and the marker file is what stands in for an
// agent that a container does not run.
func containerNodePayloads(controlPlane bool) NodePayloads {
	return NodePayloads{
		IPXEScript:         fmt.Sprintf("#!ipxe\necho local-nico-%s\n", nodeRole(controlPlane)),
		PreKubeadmCommands: containerCAPIReadyCommands(),
	}
}

func nodeRole(controlPlane bool) string {
	if controlPlane {
		return nodeRoleControlPlane
	}
	return nodeRoleWorker
}

// buildCAPNicoRequest is the CAPNico request both Nico CAPI backends build.
// payloads is a function of the node's role because the container stub names
// the role in its iPXE line; a real backend ignores the argument.
func buildCAPNicoRequest(spec ClusterSpec, payloads func(controlPlane bool) NodePayloads) (api.CreateClusterRequest, error) {
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
		ControlPlaneKubernetesVersion: spec.KubernetesVersion,
		CniPlugin:                     spec.CNIPlugin,
		EtcdEncrypted:                 spec.ControlPlane.EtcdEncrypted,
		ControlPlaneVIP:               spec.ControlPlane.ControlPlaneVIP,
		Infra: &api.InfraClusterProvider{
			Cluster: map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
				"kind":       "NicoCluster",
				"metadata": map[string]any{
					"name": name,
				},
				capiSpecKey: map[string]any{
					"siteID": siteID.String(),
				},
			},
		},
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool, err := buildNicoNodePool(req.Name, vpcID, ps, payloads(ps.ControlPlane))
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building Nico node pool %q: %w", ps.Name, err)
		}
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
	}

	if spec.ControlPlane.Config != nil {
		cfg, err := buildNicoControlPlaneConfig(req.Name, vpcID, spec, payloads(true))
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building control plane config: %w", err)
		}
		req.ControlPlaneConfig = cfg
	}

	return req, nil
}

// buildNicoControlPlaneConfig builds the cluster-level control-plane config for
// control-plane-controller provisioning, where the control plane has no owning
// node pool. A NICo control-plane node is shaped exactly like a NICo pool node,
// so this borrows buildNicoNodePool's machine template rather than repeating it.
func buildNicoControlPlaneConfig(clusterName string, vpcID uuid.UUID, spec ClusterSpec, payloads NodePayloads) (*api.CreateClusterControlPlaneConfig, error) {
	cp := spec.ControlPlane.Config
	pool, err := buildNicoNodePool(clusterName, vpcID, NodePoolSpec{
		Name:                 nodeRoleControlPlane,
		NodeNamePrefix:       "cp",
		ControlPlane:         true,
		DesiredNodeCount:     cp.DesiredCount,
		SystemReservedMemory: cp.SystemReservedMemory,
		InstanceTypeID:       cp.InstanceTypeID,
		SubnetIDs:            cp.SubnetIDs,
		SSHKeyGroupIDs:       cp.SSHKeyGroupIDs,
	}, payloads)
	if err != nil {
		return nil, err
	}
	//exhaustruct:ignore
	return &api.CreateClusterControlPlaneConfig{
		DesiredCount:         cp.DesiredCount,
		MaxSurge:             cp.MaxSurge,
		SystemReservedMemory: pool.SystemReservedMemory,
		Infra: &api.InfraControlPlaneProvider{
			PreKubeadmCommands: pool.Infra.PreKubeadmCommands,
			MachineTemplate:    pool.Infra.MachineTemplate,
		},
	}, nil
}

func buildNicoNodePool(clusterName string, vpcID uuid.UUID, spec NodePoolSpec, payloads NodePayloads) (api.NodePoolCreateConfig, error) {
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
	pool := api.NodePoolCreateConfig{
		Name:                 spec.Name,
		NodeNamePrefix:       spec.NodeNamePrefix,
		ControlPlane:         spec.ControlPlane,
		DesiredNodeCount:     spec.DesiredNodeCount,
		SystemReservedMemory: spec.SystemReservedMemory,
		Infra: &api.InfraNodePoolProvider{
			PreKubeadmCommands: payloads.PreKubeadmCommands,
			MachineTemplate: map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
				"kind":       "NicoMachineTemplate",
				"metadata": map[string]any{
					"name": fmt.Sprintf("%s-%s-template", clusterName, spec.Name),
				},
				capiSpecKey: map[string]any{
					"template": map[string]any{
						capiSpecKey: map[string]any{
							"vpcID":                   vpcID.String(),
							"instanceTypeID":          instanceTypeID.String(),
							"cloudInitInjectHostname": true,
							"ipxeScript":              payloads.IPXEScript,
							"sshKeyGroupIDs":          sshKeyGroups,
							"interfaces":              interfaces,
						},
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
	if req.Infra == nil {
		return api.CreateClusterRequest{}, fmt.Errorf("CAPD cluster skeleton is missing infra.cluster")
	}
	if err := setMapPath(req.Infra.Cluster, req.Name, "metadata", "name"); err != nil {
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

	if spec.ControlPlane.Config != nil {
		cfg, err := buildCAPDControlPlaneConfig(spec)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building control plane config: %w", err)
		}
		req.ControlPlaneConfig = cfg
	}

	return req, nil
}

// buildCAPDControlPlaneConfig stamps the embedded control-plane-config skeleton
// (a DockerMachineTemplate carrying the CAPD node image) with the caller's
// desired count, surge and system-reserved memory. It is used for
// control-plane-controller provisioning, where the control plane has no owning
// node pool.
func buildCAPDControlPlaneConfig(spec ClusterSpec) (*api.CreateClusterControlPlaneConfig, error) {
	var cfg api.CreateClusterControlPlaneConfig
	if err := json.Unmarshal(capdControlPlaneConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling control plane config skeleton: %w", err)
	}
	cfg.DesiredCount = spec.ControlPlane.Config.DesiredCount
	cfg.MaxSurge = spec.ControlPlane.Config.MaxSurge
	if spec.ControlPlane.Config.SystemReservedMemory != "" {
		cfg.SystemReservedMemory = spec.ControlPlane.Config.SystemReservedMemory
	}
	if cfg.Infra == nil {
		return nil, fmt.Errorf("CAPD control plane config skeleton is missing infra.machineTemplate")
	}
	cfg.Infra.PreKubeadmCommands = containerCAPIReadyCommands()
	if err := setMapPath(cfg.Infra.MachineTemplate, spec.CAPDNodeImage, capiSpecKey, "template", capiSpecKey, "customImage"); err != nil {
		return nil, fmt.Errorf("setting control plane custom image: %w", err)
	}
	return &cfg, nil
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
	if pool.Infra == nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("CAPD node pool skeleton is missing infra.machineTemplate")
	}
	pool.Infra.PreKubeadmCommands = containerCAPIReadyCommands()
	if err := setMapPath(pool.Infra.MachineTemplate, nodeImage, capiSpecKey, "template", capiSpecKey, "customImage"); err != nil {
		return api.NodePoolCreateConfig{}, fmt.Errorf("setting custom image: %w", err)
	}

	return pool, nil
}

// CAPDNodePoolRequest builds a standalone create-node-pool request for a CAPD
// cluster from spec.
func CAPDNodePoolRequest(clusterID uuid.UUID, spec NodePoolSpec, nodeImage string) (api.CreateNodePoolRequest, error) {
	pool, err := buildCAPDNodePool(spec, nodeImage)
	if err != nil {
		return api.CreateNodePoolRequest{}, err
	}
	return nodePoolRequest(clusterID, pool), nil
}

// buildNicoVMSRequest builds a Forge-backed create request (no CAPI manifests).
// The Forge identifiers come from spec; see test/e2e/oidc_test.go for the
// nico-vms seed values.
func buildNicoVMSRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
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
		ControlPlaneKubernetesVersion: spec.KubernetesVersion,
		CniPlugin:                     spec.CNIPlugin,
		EtcdEncrypted:                 spec.ControlPlane.EtcdEncrypted,
		ControlPlaneVIP:               spec.ControlPlane.ControlPlaneVIP,
		TeleportEnabled:               &teleportDisabled,
		Nico: &api.NicoClusterProvider{
			OrgID:  spec.OrgID,
			SiteID: siteID,
			VpcID:  vpcID,
		},
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool, err := buildNicoVMSNodePool(ps)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building node pool %q: %w", ps.Name, err)
		}
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
	}

	if spec.ControlPlane.Config != nil {
		cfg, err := buildNicoVMSControlPlaneConfig(spec)
		if err != nil {
			return api.CreateClusterRequest{}, fmt.Errorf("building control plane config: %w", err)
		}
		req.ControlPlaneConfig = cfg
	}

	return req, nil
}

func buildNicoVMSControlPlaneConfig(spec ClusterSpec) (*api.CreateClusterControlPlaneConfig, error) {
	cp := spec.ControlPlane.Config
	if cp.InstanceTypeID == "" {
		return nil, fmt.Errorf("control plane config requires instanceTypeID")
	}
	instanceTypeID, err := uuid.Parse(cp.InstanceTypeID)
	if err != nil {
		return nil, fmt.Errorf("parsing instance type ID %q: %w", cp.InstanceTypeID, err)
	}
	vpcPrefixIDs, err := parseUUIDs(cp.VpcPrefixIDs)
	if err != nil {
		return nil, fmt.Errorf("parsing VPC prefix IDs: %w", err)
	}
	sshKeyGroupIDs, err := parseUUIDs(cp.SSHKeyGroupIDs)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH key group IDs: %w", err)
	}
	if len(vpcPrefixIDs) == 0 {
		return nil, fmt.Errorf("control plane config requires vpcPrefixIDs")
	}
	if cp.OSProfile == "" || cp.ConfigProfile == "" {
		return nil, fmt.Errorf("control plane config requires osProfile and configProfile")
	}

	teleportDisabled := false
	systemReserved := cp.SystemReservedMemory
	if systemReserved == "" {
		systemReserved = "1Gi"
	}
	return &api.CreateClusterControlPlaneConfig{
		DesiredCount: cp.DesiredCount,
		MaxSurge:     cp.MaxSurge,
		Nico: &api.NicoControlPlaneProviderCreate{
			InstanceTypePreference: []uuid.UUID{instanceTypeID},
			VpcPrefixIDs:           vpcPrefixIDs,
			SSHKeyGroupIDs:         sshKeyGroupIDs,
			OSProfile:              cp.OSProfile,
			ConfigProfile:          cp.ConfigProfile,
		},
		SystemReservedMemory: systemReserved,
		TeleportEnabled:      &teleportDisabled,
	}, nil
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

func buildNicoVMSNodePool(spec NodePoolSpec) (api.NodePoolCreateConfig, error) {
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
		Name:             spec.Name,
		NodeNamePrefix:   spec.NodeNamePrefix,
		ControlPlane:     spec.ControlPlane,
		DesiredNodeCount: spec.DesiredNodeCount,
		Nico: &api.NicoNodePoolProvider{
			InstanceTypeID:        instanceTypeID,
			VpcPrefixIDs:          vpcPrefixIDs,
			SSHKeyGroupIDs:        &sshKeyGroupIDs,
			OSProfile:             spec.OSProfile,
			ConfigProfile:         spec.ConfigProfile,
			DiskSelectionStrategy: api.DiskSelectionStrategyCreate(spec.DiskSelectionStrategy),
		},
	}
	if spec.SystemReservedMemory != "" {
		pool.SystemReservedMemory = spec.SystemReservedMemory
	}
	if spec.KubeReservedMemory != "" {
		pool.KubeReservedMemory = spec.KubeReservedMemory
	}

	return pool, nil
}

// NicoVMSNodePoolRequest builds a standalone create-node-pool request for a
// nico-vms cluster from spec. It delegates the UUID parsing and field shaping
// to buildNicoVMSNodePool so the two stay in sync, adding only the owning cluster
// ID that the CreateNodePool endpoint requires (unlike the cluster-create path,
// which nests the pool inside the cluster request).
func NicoVMSNodePoolRequest(clusterID uuid.UUID, spec NodePoolSpec) (api.CreateNodePoolRequest, error) {
	pool, err := buildNicoVMSNodePool(spec)
	if err != nil {
		return api.CreateNodePoolRequest{}, err
	}
	return nodePoolRequest(clusterID, pool), nil
}

// NicoNodePoolRequest is NicoVMSNodePoolRequest for the container-backed NICo
// topology. buildNicoNodePool names the machine template after the cluster and
// reads the VPC from the spec, so both are arguments here where the VMS builder
// needs neither.
func NicoNodePoolRequest(clusterID uuid.UUID, clusterName, vpcID string, spec NodePoolSpec) (api.CreateNodePoolRequest, error) {
	vpc, err := uuid.Parse(vpcID)
	if err != nil {
		return api.CreateNodePoolRequest{}, fmt.Errorf("parsing VPC ID %q: %w", vpcID, err)
	}
	pool, err := buildNicoNodePool(clusterName, vpc, spec, containerNodePayloads(spec.ControlPlane))
	if err != nil {
		return api.CreateNodePoolRequest{}, err
	}
	return nodePoolRequest(clusterID, pool), nil
}

func nodePoolRequest(clusterID uuid.UUID, pool api.NodePoolCreateConfig) api.CreateNodePoolRequest {
	//exhaustruct:enforce
	return api.CreateNodePoolRequest{
		ClusterID:                   clusterID,
		Name:                        pool.Name,
		SystemReservedMemory:        pool.SystemReservedMemory,
		KubeReservedMemory:          pool.KubeReservedMemory,
		ControlPlane:                pool.ControlPlane,
		NodeLabels:                  pool.NodeLabels,
		DesiredNodeCount:            pool.DesiredNodeCount,
		Infra:                       pool.Infra,
		Description:                 pool.Description,
		Nico:                        pool.Nico,
		NodeNamePrefix:              pool.NodeNamePrefix,
		AutoProvisionDisabled:       pool.AutoProvisionDisabled,
		AutoModeDisabled:            pool.AutoModeDisabled,
		ProvisioningMonitorDisabled: pool.ProvisioningMonitorDisabled,
		RecycleBudget:               pool.RecycleBudget,
		TeleportEnabled:             pool.TeleportEnabled,
	}
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
