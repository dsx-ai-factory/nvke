// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/spiffe"
	"github.com/NVIDIA/nke/controller/internal/teleport"
	"github.com/NVIDIA/nke/controller/internal/utils"
)

type CreateForgeInstanceInput struct {
	NodeID uuid.UUID
}

func (a *Activities) CreateForgeInstance(ctx context.Context, input CreateForgeInstanceInput) (*forgesdk.Instance, error) {
	err := a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
		NodeID: input.NodeID,
		Conditions: NodeConditions{
			Pending: &ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Creating Forge instance",
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update node conditions: %w", err)
	}

	res, err := a.createForgeInstance(ctx, input)
	if err != nil {
		// Best effort to update the node pending condition to indicate the create forge instance failed
		if err := a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: input.NodeID,
			Conditions: NodeConditions{
				Pending: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: fmt.Sprintf("Failed to create Forge instance: %s", err),
				},
			},
		}); err != nil {
			activity.GetLogger(ctx).Error("failed to update node conditions", "error", err)
		}

		return nil, fmt.Errorf("failed to create Forge instance: %w", err)
	}

	return res, nil
}

func (a *Activities) createForgeInstance(ctx context.Context, input CreateForgeInstanceInput) (*forgesdk.Instance, error) {
	logger := activity.GetLogger(ctx)

	var createInstanceResponse *forgesdk.Instance

	// We need to hold the advisory lock during the entire forge instance creation
	// to prevent the NodePoolController from deleting this pending node while we
	// are creating the forge instance, resulting in zombie forge instances.
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Acquire the lock first to prevent the auto-provisioner from deleting this node
		// If we fail to acquire the lock (likely because the NodePoolController is deleting the pending node),
		// just fail and return early.
		acquired, err := tx.TryAcquireNodeProvisioningLock(ctx, input.NodeID)
		if err != nil {
			return fmt.Errorf("failed to try to acquire advisory lock for node: %w", err)
		}
		if !acquired {
			return fmt.Errorf("failed to acquire advisory lock for node")
		}

		logger.Debug("Acquired advisory lock for node provisioning", "nodeID", input.NodeID)

		node, err := tx.GetNode(ctx, input.NodeID)
		if err != nil {
			// Note: The NodePoolController will delete pending node records from non-current node sets,
			// or delete pending nodes when the count is above the desired count. Which may cause a not found error here.
			// It may be worthwhile to detect this and cancel or just return early if these workflow failures become noisy.
			return fmt.Errorf("failed to get node: %w", err)
		}

		// This needs to run in the same transaction as the lock and the forge instance creation.
		// Because we only want to move from Pending to Provisioning if the forge instance creation succeeds.
		err = a.UpdateNodeConditionsWithTx(ctx, tx, NodeConditionsUpdate{
			NodeID: node.ID,
			Conditions: NodeConditions{
				// Setting NodeCondition.Provisioning to true implies that Pending is now false.
				Pending: &ConditionUpdate{
					Status:  db.ConditionStatusFalse,
					Message: "",
				},
				Provisioning: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: "Creating Forge instance",
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to update node conditions: %w", err)
		}

		nodeSet, nodePool, err := nodes.GetNodeContext(ctx, tx, node)
		if err != nil {
			return fmt.Errorf("failed to get node context: %w", err)
		}
		cluster, err := a.GetClusterForNode(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if node.IsManagedByNodePool() {
			if !nodePool.IsCurrentNodeSet(nodeSet.ID) {
				return fmt.Errorf("node set is not the current node set for the node pool")
			}
		}

		// Now create the forge instance while holding the lock
		// This ensures the node won't be deleted by the NodePoolController during forge instance creation
		createInstanceResponse, err = a.createForgeInstanceWithLockHeld(
			ctx,
			input,
			node,
			nodeSet,
			nodePool,
			cluster,
			a.cfg.NKESiteControllerName,
		)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return createInstanceResponse, nil
}

// createForgeInstanceWithLockHeld does the actual forge instance creation
// This function assumes the advisory lock is already held
func (a *Activities) createForgeInstanceWithLockHeld(
	ctx context.Context,
	input CreateForgeInstanceInput,
	node db.Node,
	nodeSet db.NodeSet,
	nodePool db.NodePool,
	cluster db.Cluster,
	siteName string,
) (*forgesdk.Instance, error) {
	// Setup a few variables we'll need multiple times
	newInstance := forgesdk.InstanceCreateRequest{}
	provisioningConfig, err := resolveTemplateLessForgeNodeConfig(nodeSet, nodePool)
	if err != nil {
		return nil, err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)

	// Setup the tenant ID
	tenantID, err := a.getTenantID(cluster.OrgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant ID: %w", err)
	}
	newInstance.SetTenantId(tenantID.String())

	var instanceType GetInstanceTypeOutput
	if provisioningConfig.infinibandPartitionID != nil || provisioningConfig.nvLinkLogicalPartitionID != nil {
		instanceType, err = a.getInstanceType(ctx, cluster.OrgID, cluster.SiteID.String(), provisioningConfig.instanceTypeID)
		if err != nil {
			return nil, fmt.Errorf("failed to get instance type: %w", err)
		}
	}

	// Setup InfiniBand interfaces (if needed)
	if provisioningConfig.infinibandPartitionID != nil {
		if instanceType.InfiniBandDeviceName == "" {
			return nil, fmt.Errorf("instance type %s does not support InfiniBand", provisioningConfig.instanceTypeID)
		}

		if len(instanceType.InfiniBandActiveDeviceIDs) > 0 {
			for _, deviceID := range instanceType.InfiniBandActiveDeviceIDs {
				infinibandInterface := forgesdk.InfiniBandInterfaceCreateRequest{
					PartitionId:    forgesdk.PtrString(provisioningConfig.infinibandPartitionID.String()),
					Device:         forgesdk.PtrString(instanceType.InfiniBandDeviceName),
					DeviceInstance: forgesdk.PtrInt32(deviceID),
					IsPhysical:     forgesdk.PtrBool(true),
				}

				newInstance.InfinibandInterfaces = append(newInstance.InfinibandInterfaces, infinibandInterface)
			}
		} else {
			return nil, fmt.Errorf("instance type %s does not support InfiniBand", provisioningConfig.instanceTypeID)
		}
	}

	// Setup NVLink interfaces (if needed)
	if provisioningConfig.nvLinkLogicalPartitionID != nil {
		if instanceType.NVLinkInterfaceCount == 0 {
			return nil, fmt.Errorf("instance type %s does not support NVLink Interface", provisioningConfig.instanceTypeID)
		}

		activeDeviceIDs := getNVLinkActiveDeviceIDs(instanceType)
		for _, deviceID := range activeDeviceIDs {
			nvLinkInterface := forgesdk.NVLinkInterfaceCreateRequest{
				NvLinklogicalPartitionId: forgesdk.PtrString(provisioningConfig.nvLinkLogicalPartitionID.String()),
				DeviceInstance:           forgesdk.PtrInt32(deviceID),
			}

			newInstance.NvLinkInterfaces = append(newInstance.NvLinkInterfaces, nvLinkInterface)
		}
	}

	// Setup IPXE script
	ipxeScript, err := getIpxeScript(osProfileDir, string(provisioningConfig.diskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to get iPXE script for OS profile %s: %w", provisioningConfig.osProfile, err)
	}
	newInstance.SetIpxeScript(ipxeScript)

	var teleportCfg *TeleportUserDataConfig
	if a.cfg.Teleport.Enabled && cluster.TeleportEnabled && provisioningConfig.teleportEnabled {
		teleportCfg = &TeleportUserDataConfig{
			Enabled:      true,
			TokenName:    teleport.TPMTokenName(node.ID),
			ProxyAddress: a.cfg.Teleport.ProxyAddress,
			JoinMethod:   string(teleport.JoinMethodTPM),
			Labels:       buildTeleportLabels(node, cluster, provisioningConfig.nodePoolID, provisioningConfig.isControlPlaneRole, siteName, a.cfg.Teleport.Environment),
		}
	}
	userData, err := a.getUserData(ctx, osProfileDir, node, provisioningConfig.diskSelectionStrategy, provisioningConfig.kubernetesVersion, teleportCfg, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get user data for OS profile %s: %w", provisioningConfig.osProfile, err)
	}
	newInstance.SetUserData(userData)

	// Setup SSH Key Group IDs
	sshKeyGroupIDsStr := []string{}
	for _, sshKeyGroupID := range provisioningConfig.sshKeyGroupIDs {
		sshKeyGroupIDsStr = append(sshKeyGroupIDsStr, sshKeyGroupID.String())
	}
	newInstance.SetSshKeyGroupIds(sshKeyGroupIDsStr)

	// Setup other required fields that don't change on a per-subnet/per-vpc-prefix basis
	newInstance.SetName(node.Name)
	newInstance.SetDescription("Managed by NKE")
	newInstance.SetInstanceTypeId(provisioningConfig.instanceTypeID.String())
	newInstance.SetVpcId(cluster.VpcID.String())
	forgeLabels := map[string]string{
		constants.LabelKeyNodeID:    node.ID.String(),
		constants.LabelKeyClusterID: cluster.ID.String(),
	}
	if provisioningConfig.nodePoolID != uuid.Nil {
		forgeLabels[constants.LabelKeyNodePoolID] = provisioningConfig.nodePoolID.String()
	}
	newInstance.SetLabels(forgeLabels)

	// Use VPC prefix IDs if present, otherwise use subnet IDs
	if len(provisioningConfig.vpcPrefixIDs) > 0 {
		return a.tryCreateInstanceWithVpcPrefixes(ctx, newInstance, input, node, provisioningConfig.nodePoolID, provisioningConfig.vpcPrefixIDs, cluster)
	}

	return a.tryCreateInstanceWithSubnets(ctx, newInstance, input, node, provisioningConfig.nodePoolID, provisioningConfig.subnetIDs, cluster)
}

func getNVLinkActiveDeviceIDs(instanceType GetInstanceTypeOutput) []int32 {
	if len(instanceType.NVLinkActiveDeviceIDs) > 0 {
		return instanceType.NVLinkActiveDeviceIDs
	}

	activeDeviceIDs := make([]int32, 0, instanceType.NVLinkInterfaceCount)
	for deviceID := range instanceType.NVLinkInterfaceCount {
		activeDeviceIDs = append(activeDeviceIDs, deviceID)
	}

	return activeDeviceIDs
}

// tryCreateInstanceWithVpcPrefixes attempts to create a Forge instance by trying each VPC prefix in the node pool
func (a *Activities) tryCreateInstanceWithVpcPrefixes(
	ctx context.Context,
	newInstance forgesdk.InstanceCreateRequest,
	input CreateForgeInstanceInput,
	node db.Node,
	nodePoolID uuid.UUID,
	vpcPrefixIDs []uuid.UUID,
	cluster db.Cluster,
) (*forgesdk.Instance, error) {
	logger := activity.GetLogger(ctx)
	var lastErr error

	for i, vpcPrefixID := range vpcPrefixIDs {
		vpcPrefixIDStr := vpcPrefixID.String()
		logger.Info("Attempting to create Forge instance for node",
			"clusterID", cluster.ID,
			"nodePoolID", nodePoolID,
			"nodeID", input.NodeID,
			"nodeName", node.Name,
			"vpcPrefixID", vpcPrefixIDStr,
		)

		newInstance.SetInterfaces([]forgesdk.InterfaceCreateRequest{
			{VpcPrefixId: forgesdk.PtrString(vpcPrefixIDStr)},
		})

		createInstanceResponse, err := a.forgeProvider.CreateInstance(ctx, cluster.OrgID, newInstance)
		if err != nil {
			lastErr = err

			// Handle scenario where instance gets created in forge, but response doesn't make it back to CWE.
			if errors.Is(err, forge.ErrInstanceAlreadyExists) {
				return a.findInstance(ctx, cluster.OrgID, node.Name, cluster.SiteID.String(), node.ID)
			}

			// Handle scenario where the VPC prefix has no available IP addresses by continuing to the next VPC prefix.
			if errors.Is(err, forge.ErrVpcPrefixExhausted) {
				logger.Warn("VPC prefix exhausted, trying next VPC prefix",
					"vpcPrefixID", vpcPrefixIDStr,
					"error", err.Error(),
					"remainingVpcPrefixes", len(vpcPrefixIDs)-i-1,
				)

				// Try the next VPC prefix
				continue
			}

			// Fail immediately on any other error type
			logger.Error("Failed to create Forge instance", "error", err)
			return nil, err
		}

		logger.Info("Forge instance created successfully",
			"vpcPrefixID", vpcPrefixIDStr,
			"instanceID", createInstanceResponse.GetId(),
			"forgeMachineID", createInstanceResponse.GetMachineId(),
		)

		return createInstanceResponse, nil
	}

	// If we've exhausted all VPC prefixes, return the last error
	return nil, fmt.Errorf("all %d VPC prefixes are exhausted, error from the last create instance attempt: %w", len(vpcPrefixIDs), lastErr)
}

// tryCreateInstanceWithSubnets attempts to create a Forge instance by trying each subnet in the node pool
func (a *Activities) tryCreateInstanceWithSubnets(
	ctx context.Context,
	newInstance forgesdk.InstanceCreateRequest,
	input CreateForgeInstanceInput,
	node db.Node,
	nodePoolID uuid.UUID,
	subnetIDs []uuid.UUID,
	cluster db.Cluster,
) (*forgesdk.Instance, error) {
	logger := activity.GetLogger(ctx)
	var lastErr error

	for i, subnetID := range subnetIDs {
		subnetIDStr := subnetID.String()
		logger.Info("Attempting to create Forge instance for node",
			"clusterID", cluster.ID,
			"nodePoolID", nodePoolID,
			"nodeID", input.NodeID,
			"nodeName", node.Name,
			"subnetID", subnetIDStr,
		)

		newInstance.SetInterfaces([]forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString(subnetIDStr)},
		})

		createInstanceResponse, err := a.forgeProvider.CreateInstance(ctx, cluster.OrgID, newInstance)
		if err != nil {
			lastErr = err

			// Handle scenario where instance gets created in forge, but response doesn't make it back to CWE.
			if errors.Is(err, forge.ErrInstanceAlreadyExists) {
				return a.findInstance(ctx, cluster.OrgID, node.Name, cluster.SiteID.String(), node.ID)
			}

			// Handle scenario where the subnet has no available IP addresses by continuing to the next subnet.
			if errors.Is(err, forge.ErrSubnetExhausted) {
				logger.Warn("Subnet exhausted, trying next subnet",
					"subnetID", subnetIDStr,
					"error", err.Error(),
					"remainingSubnets", len(subnetIDs)-i-1,
				)

				// Try the next subnet
				continue
			}

			// Fail immediately on any other error type
			logger.Error("Failed to create Forge instance", "error", err)
			return nil, err
		}

		logger.Info("Forge instance created successfully",
			"subnetID", subnetIDStr,
			"instanceID", createInstanceResponse.GetId(),
			"forgeMachineID", createInstanceResponse.GetMachineId(),
		)

		return createInstanceResponse, nil
	}

	// If we've exhausted all subnets, return the last error
	return nil, fmt.Errorf("all %d subnets are exhausted, error from the last create instance attempt: %w", len(subnetIDs), lastErr)
}

type IpxeData struct {
	DiskSelectionStrategy string
}

func getIpxeScript(osProfileDir, diskSelectionStrategy string) (string, error) {
	ipxeScriptBytes, err := os.ReadFile(path.Join(osProfileDir, "ipxeScript"))
	if err != nil {
		return "", fmt.Errorf("failed to read ipxeScript file: %w", err)
	}

	tmpl, err := template.New("ipxeScript").Parse(string(ipxeScriptBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse ipxeScript template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, IpxeData{DiskSelectionStrategy: diskSelectionStrategy}); err != nil {
		return "", fmt.Errorf("failed to execute ipxeScript template: %w", err)
	}

	return buf.String(), nil
}

// getTrustDomainAndBundle is a wrapper around spiffe.GetTrustDomainAndBundle exposed for testing
//
//nolint:gochecknoglobals // both variables are exposed for test overrides
var (
	// computeHashedPassword generates a SHA-512 crypt hash ($6$...) from a plaintext password.
	// Tests override this to produce a deterministic hash.
	computeHashedPassword = func(password string) (string, error) {
		return sha512_crypt.New().Generate([]byte(password), nil)
	}

	getTrustDomainAndBundle = spiffe.GetTrustDomainAndBundle
)

// TeleportUserDataConfig holds Teleport configuration for userData templates.
type TeleportUserDataConfig struct {
	Enabled      bool
	TokenName    string
	ProxyAddress string
	JoinMethod   string
	Labels       string
}

func (a *Activities) getUserData(
	ctx context.Context,
	osProfileDir string,
	node db.Node,
	diskSelectionStrategy db.DiskSelectionStrategy,
	kubernetesVersion string,
	teleportCfg *TeleportUserDataConfig,
	cluster db.Cluster,
) (string, error) {
	userDataTemplateBytes, err := os.ReadFile(path.Join(osProfileDir, "userData.tmpl"))
	if err != nil {
		return "", fmt.Errorf("failed to read user data template file: %w", err)
	}

	userDataTemplate, err := template.New("userData").Funcs(
		template.FuncMap{
			"indent":     utils.Indent,
			"trimPrefix": utils.TrimPrefix,
		},
	).Parse(string(userDataTemplateBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse userData template: %w", err)
	}

	var trustDomain string
	var trustBundle string
	if node.SpiffeEnabled {
		trustDomain, trustBundle, err = getTrustDomainAndBundle(ctx, a.cfg)
		if err != nil {
			return "", fmt.Errorf("failed to get trust domain and bundle: %w", err)
		}
	}

	// Exactly one CNI Plugin must be enabled
	calicoEnabled := false
	ovnEnabled := false

	switch cluster.CniPlugin {
	case db.CNICalico:
		calicoEnabled = true
	case db.CNIOvnKubernetes:
		ovnEnabled = true
	default:
		return "", fmt.Errorf("unsupported CNI plugin: %s", cluster.CniPlugin)
	}
	cniVersion := db.EffectiveCNIVersion(cluster.CniPlugin, strings.TrimSpace(cluster.CniVersion))
	cniImagePackage, err := cniImagePackageName(cluster.CniPlugin, cniVersion)
	if err != nil {
		return "", err
	}

	type UserData struct {
		CalicoEnabled         bool
		CNIImagePackage       string
		DiskSelectionStrategy string
		HashedPassword        string
		Hostname              string
		KubernetesVersion     string
		NodeAgentPort         string
		OvnEnabled            bool
		SpiffeEnabled         bool
		SpireServerAddress    string
		SpireServerPort       string
		TrustBundle           string
		TrustDomain           string
		TeleportEnabled       bool
		TeleportTokenName     string
		TeleportProxyAddress  string
		TeleportJoinMethod    string
		TeleportLabels        string
		Username              string
	}

	if err := a.validateDgxcTenantCredentials(); err != nil {
		return "", err
	}
	hashedPassword, err := computeHashedPassword(a.cfg.DgxcTenantPassword)
	if err != nil {
		return "", fmt.Errorf("failed to hash DGXC tenant password: %w", err)
	}

	// Note: Adding the trust bundle into the user data template does not present a security risk.
	// Only public certs are included in the trust bundle, and this is how zero-trust systems are intended to bootstrap.
	userDataTemplateData := UserData{
		CalicoEnabled:         calicoEnabled,
		CNIImagePackage:       cniImagePackage,
		DiskSelectionStrategy: string(diskSelectionStrategy),
		HashedPassword:        hashedPassword,
		Hostname:              node.Name,
		KubernetesVersion:     kubernetesVersion,
		NodeAgentPort:         a.cfg.NodeAgentPort,
		OvnEnabled:            ovnEnabled,
		SpiffeEnabled:         node.SpiffeEnabled,
		SpireServerAddress:    a.cfg.SpireServerAddress,
		SpireServerPort:       a.cfg.SpireServerPort,
		TrustBundle:           trustBundle,
		TrustDomain:           trustDomain,
		Username:              a.cfg.DgxcTenantUser,
	}

	if teleportCfg != nil && teleportCfg.Enabled {
		userDataTemplateData.TeleportEnabled = true
		userDataTemplateData.TeleportTokenName = teleportCfg.TokenName
		userDataTemplateData.TeleportProxyAddress = teleportCfg.ProxyAddress
		userDataTemplateData.TeleportJoinMethod = teleportCfg.JoinMethod
		userDataTemplateData.TeleportLabels = teleportCfg.Labels
	}

	userDataByteBuffer := new(bytes.Buffer)
	if err := userDataTemplate.Execute(userDataByteBuffer, userDataTemplateData); err != nil {
		return "", fmt.Errorf("failed to execute userData template: %w", err)
	}

	return userDataByteBuffer.String(), nil
}

func (a *Activities) validateDgxcTenantCredentials() error {
	if strings.TrimSpace(a.cfg.DgxcTenantUser) == "" {
		return fmt.Errorf("DGXC_TENANT_USER is required for Forge OS profile user-data rendering")
	}
	if strings.TrimSpace(a.cfg.DgxcTenantPassword) == "" {
		return fmt.Errorf("DGXC_TENANT_PASSWORD is required for Forge OS profile user-data rendering")
	}
	return nil
}

func cniImagePackageName(plugin db.CniPlugin, version string) (string, error) {
	switch plugin {
	case db.CNICalico:
		return "nke-cni-calico-images-" + version, nil
	case db.CNIOvnKubernetes:
		return "nke-cni-ovn-images-" + version, nil
	default:
		return "", fmt.Errorf("unsupported CNI plugin: %s", plugin)
	}
}

// buildTeleportLabels constructs YAML-formatted label lines for the Teleport config in userData.
// Output is "key: value" per line (no leading indent); the template applies indentation.
func buildTeleportLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID, controlPlane bool, siteName, environment string) string {
	labels := map[string]string{
		teleport.LabelNodeID:                node.ID.String(),
		teleport.LabelNodeName:              node.Name,
		teleport.LabelNodeRole:              teleport.NodeRoleLabel(controlPlane),
		teleport.LabelSite:                  cluster.SiteID.String(),
		teleport.LabelNKESiteControllerName: siteName,
		teleport.LabelCluster:               cluster.ID.String(),
		teleport.LabelClusterName:           cluster.Name,
		teleport.LabelResourceGroup:         teleport.LabelResourceGroupNKEValue,
		teleport.LabelManagedBy:             teleport.LabelManagedByValue,
		teleport.LabelEnvironment:           environment,
	}
	if nodePoolID != uuid.Nil {
		labels[teleport.LabelNodePool] = nodePoolID.String()
	}
	return teleport.BuildLabelsYAML(labels)
}

// List forge instances filtered by name and check for a label with a matching NKE node-id. If such an instance exists, return it.
func (a *Activities) findInstance(ctx context.Context, orgID, name, siteID string, nodeID uuid.UUID) (*forgesdk.Instance, error) {
	logger := activity.GetLogger(ctx)

	cachedInstances, err := a.forgeProvider.GetInstances(ctx, orgID, siteID)
	if err != nil {
		return nil, fmt.Errorf("failed to list existing instances: %w", err)
	}

	var foundInstance *forgesdk.Instance

	for _, instance := range cachedInstances {
		if instance.GetName() == name {
			foundInstance = instance
		}
	}

	if foundInstance == nil {
		return nil, fmt.Errorf("failed to find existing instance with name conflict %s", name)
	}

	nodeIDLabel := ""
	if foundInstance.Labels != nil {
		nodeIDLabel = foundInstance.Labels[constants.LabelKeyNodeID]
	}

	if nodeIDLabel != nodeID.String() {
		logger.Error("Name collision. Existing instance has a node-id label that doesn't match.",
			"nodeName", name,
			"nodeID", nodeID.String(),
			"nodeIDLabel", nodeIDLabel,
		)

		return nil, fmt.Errorf("forge instance with name %s already belongs to node %s", name, nodeIDLabel)
	}

	return foundInstance, nil
}
