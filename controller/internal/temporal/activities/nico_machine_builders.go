// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/teleport"
	"github.com/dsx-ai-factory/nke/controller/internal/utils"
)

type templateLessForgeCAPINodeConfig struct {
	osProfile                string
	diskSelectionStrategy    db.DiskSelectionStrategy
	kubernetesVersion        string
	teleportEnabled          bool
	instanceTypeID           uuid.UUID
	sshKeyGroupIDs           []uuid.UUID
	subnetIDs                []uuid.UUID
	vpcPrefixIDs             []uuid.UUID
	nodePoolID               uuid.UUID
	isControlPlaneRole       bool
	infinibandPartitionID    *uuid.UUID
	nvLinkLogicalPartitionID *uuid.UUID
}

func (a *Activities) buildNicoMachineForNode(createCtx *capiMachineCreateContext) (*nicov1.NicoMachine, error) {
	provisioningConfig, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	if err != nil {
		return nil, err
	}
	nicoProvider, err := createCtx.cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)
	ipxeScript, err := getIpxeScript(osProfileDir, string(provisioningConfig.diskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to get iPXE script for OS profile %s: %w", provisioningConfig.osProfile, err)
	}

	instanceLabels := buildNicoInstanceLabels(createCtx.node, createCtx.cluster, provisioningConfig.nodePoolID)

	nicoMachine, err := buildNicoMachine(
		createCtx.node,
		createCtx.cluster,
		nicoProvider,
		createCtx.namespace,
		createCtx.nodePool.ControlPlane,
		instanceLabels,
		provisioningConfig.instanceTypeID,
		provisioningConfig.sshKeyGroupIDs,
		provisioningConfig.subnetIDs,
		provisioningConfig.vpcPrefixIDs,
		provisioningConfig.infinibandPartitionID,
		provisioningConfig.nvLinkLogicalPartitionID,
		ipxeScript,
	)
	if err != nil {
		return nil, err
	}

	return nicoMachine, nil
}

func (a *Activities) renderTemplateLessForgeCAPIBootstrapUserData(ctx context.Context, createCtx *capiMachineCreateContext) (string, error) {
	provisioningConfig, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	if err != nil {
		return "", err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)
	teleportCfg, err := a.nicoMachineTeleportConfig(createCtx, provisioningConfig.teleportEnabled, provisioningConfig.nodePoolID, provisioningConfig.isControlPlaneRole)
	if err != nil {
		return "", err
	}
	userData, err := a.getUserData(ctx, osProfileDir, createCtx.node, provisioningConfig.diskSelectionStrategy, provisioningConfig.kubernetesVersion, teleportCfg, createCtx.cluster)
	if err != nil {
		return "", fmt.Errorf("failed to get user data for OS profile %s: %w", provisioningConfig.osProfile, err)
	}

	return userData, nil
}

func resolveTemplateLessForgeCAPINodeConfig(createCtx *capiMachineCreateContext) (templateLessForgeCAPINodeConfig, error) {
	return resolveTemplateLessForgeNodeConfig(createCtx.nodeSet, createCtx.nodePool)
}

func resolveTemplateLessForgeNodeConfig(nodeSet db.NodeSet, nodePool db.NodePool) (templateLessForgeCAPINodeConfig, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}
	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	if err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}
	if err := validateResolvedTemplateLessForgeNodeContext(nodeSet, nicoNodeSetProvider, nicoNodePoolProvider); err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}

	provisioningConfig := templateLessForgeCAPINodeConfig{
		osProfile:                nicoNodeSetProvider.OSProfile,
		diskSelectionStrategy:    nicoNodeSetProvider.DiskSelectionStrategy,
		kubernetesVersion:        nodeSet.KubernetesVersion,
		teleportEnabled:          nodeSet.TeleportEnabled,
		instanceTypeID:           nicoNodePoolProvider.InstanceTypeID,
		sshKeyGroupIDs:           nicoNodePoolProvider.SSHKeyGroupIDs,
		subnetIDs:                nicoNodePoolProvider.SubnetIDs,
		vpcPrefixIDs:             nicoNodePoolProvider.VpcPrefixIDs,
		nodePoolID:               nodePool.ID,
		isControlPlaneRole:       nodePool.ControlPlane,
		infinibandPartitionID:    nicoNodePoolProvider.InfinibandPartitionID,
		nvLinkLogicalPartitionID: nicoNodePoolProvider.NvLinkLogicalPartitionID,
	}

	if provisioningConfig.osProfile == "" {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("os profile is empty")
	}
	if provisioningConfig.instanceTypeID == uuid.Nil {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("instance type is empty")
	}
	if len(provisioningConfig.subnetIDs) == 0 && len(provisioningConfig.vpcPrefixIDs) == 0 {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
	}

	return provisioningConfig, nil
}

func validateResolvedTemplateLessForgeNodeContext(nodeSet db.NodeSet, nicoNodeSetProvider, nicoProvider db.NicoNodePoolProvider) error {
	missingFields := make([]string, 0, 4)

	if nicoNodeSetProvider.OSProfile == "" {
		missingFields = append(missingFields, "nodeSet.osProfile")
	}
	if nodeSet.KubernetesVersion == "" {
		missingFields = append(missingFields, "nodeSet.kubernetesVersion")
	}
	if nicoProvider.InstanceTypeID == uuid.Nil {
		missingFields = append(missingFields, "nodePool.instanceTypeID")
	}
	if len(nicoProvider.SubnetIDs) == 0 && len(nicoProvider.VpcPrefixIDs) == 0 {
		missingFields = append(missingFields, "nodePool.subnetIDs|vpcPrefixIDs")
	}

	if len(missingFields) > 0 {
		return fmt.Errorf("resolved node context is incomplete: missing %s", strings.Join(missingFields, ", "))
	}

	return nil
}

func (a *Activities) nicoMachineTeleportConfig(createCtx *capiMachineCreateContext, teleportEnabled bool, nodePoolID uuid.UUID, isControlPlaneRole bool) (*TeleportUserDataConfig, error) {
	if !a.cfg.Teleport.Enabled || !createCtx.cluster.TeleportEnabled || !teleportEnabled {
		return &TeleportUserDataConfig{}, nil
	}
	nicoProvider, err := createCtx.cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	return &TeleportUserDataConfig{
		Enabled:      true,
		TokenName:    teleport.TPMTokenName(a.cfg.Teleport.TokenNamePrefix, createCtx.node.ID),
		ProxyAddress: a.cfg.Teleport.ProxyAddress,
		JoinMethod:   string(teleport.JoinMethodTPM),
		Labels:       buildTeleportLabels(createCtx.node, createCtx.cluster, nicoProvider, nodePoolID, isControlPlaneRole, a.cfg.NKESiteControllerName, a.cfg.Teleport.Environment, a.cfg.Teleport.ManagedByLabelValue),
	}, nil
}

func buildNicoMachine(
	node db.Node,
	cluster db.Cluster,
	nicoProvider db.NicoClusterProvider,
	namespace string,
	controlPlane bool,
	instanceLabels map[string]string,
	instanceTypeID uuid.UUID,
	sshKeyGroupIDs []uuid.UUID,
	subnetIDs []uuid.UUID,
	vpcPrefixIDs []uuid.UUID,
	infinibandPartitionID *uuid.UUID,
	nvLinkLogicalPartitionID *uuid.UUID,
	ipxeScript string,
) (*nicov1.NicoMachine, error) {
	interfaces, err := buildNicoMachineInterfaces(subnetIDs, vpcPrefixIDs)
	if err != nil {
		return nil, err
	}

	infinibandPartition := ""
	if infinibandPartitionID != nil {
		infinibandPartition = infinibandPartitionID.String()
	}
	nvLinkPartition := ""
	if nvLinkLogicalPartitionID != nil {
		nvLinkPartition = nvLinkLogicalPartitionID.String()
	}

	labels := capiMachineLabels(cluster.Name, controlPlane)
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: nicov1.NicoMachineSpec{
			VPCID:                    nicoProvider.VpcID.String(),
			InstanceTypeID:           instanceTypeID.String(),
			Interfaces:               interfaces,
			InfinibandPartitionID:    infinibandPartition,
			NVLinkLogicalPartitionID: nvLinkPartition,
			SSHKeyGroupIDs:           uuidStrings(sshKeyGroupIDs),
			IpxeScript:               ipxeScript,
			// Identity labels CWE knows before create. CAPNICo owns machine-id,
			// site-id, site-name, vpc-id, vpc-name, and ip-address.
			Labels: instanceLabels,
		},
	}, nil
}

// buildNicoInstanceLabels is the subset of instance labels CWE still writes on
// the CAPI path. CAPNICo fills in topology and machine identity after create.
func buildNicoInstanceLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID) map[string]string {
	labels := map[string]string{
		constants.LabelKeyNodeID:    node.ID.String(),
		constants.LabelKeyClusterID: cluster.ID.String(),
	}
	if nodePoolID != uuid.Nil {
		labels[constants.LabelKeyNodePoolID] = nodePoolID.String()
	}
	return labels
}

func buildNicoMachineInterfaces(subnetIDs, vpcPrefixIDs []uuid.UUID) ([]nicov1.NicoMachineInterface, error) {
	if len(vpcPrefixIDs) > 0 {
		return []nicov1.NicoMachineInterface{{VPCPrefixID: vpcPrefixIDs[0].String()}}, nil
	}
	if len(subnetIDs) > 0 {
		return []nicov1.NicoMachineInterface{{SubnetID: subnetIDs[0].String()}}, nil
	}

	return nil, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
}

func uuidStrings(ids []uuid.UUID) []string {
	strings := make([]string, 0, len(ids))
	for _, id := range ids {
		strings = append(strings, id.String())
	}
	return strings
}

type IpxeData struct {
	DiskSelectionStrategy string
}

func getIpxeScript(osProfileDir, diskSelectionStrategy string) (string, error) {
	ipxeScriptBytes, err := os.ReadFile(path.Join(osProfileDir, "ipxeScript"))
	if err != nil {
		return "", fmt.Errorf("failed to read ipxeScript file: %w", err)
	}

	return renderIpxeScript(string(ipxeScriptBytes), diskSelectionStrategy)
}

func renderIpxeScript(ipxeScriptTemplate, diskSelectionStrategy string) (string, error) {
	tmpl, err := template.New("ipxeScript").Parse(ipxeScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse ipxeScript template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, IpxeData{DiskSelectionStrategy: diskSelectionStrategy}); err != nil {
		return "", fmt.Errorf("failed to execute ipxeScript template: %w", err)
	}

	return buf.String(), nil
}

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

	return a.renderUserData(ctx, string(userDataTemplateBytes), node, diskSelectionStrategy, kubernetesVersion, teleportCfg, cluster)
}

func (a *Activities) renderUserData(
	ctx context.Context,
	userDataTemplateText string,
	node db.Node,
	diskSelectionStrategy db.DiskSelectionStrategy,
	kubernetesVersion string,
	teleportCfg *TeleportUserDataConfig,
	cluster db.Cluster,
) (string, error) {
	userDataTemplate, err := template.New("userData").Funcs(
		template.FuncMap{
			"indent":     utils.Indent,
			"quote":      utils.Quote,
			"trimPrefix": utils.TrimPrefix,
		},
	).Parse(userDataTemplateText)
	if err != nil {
		return "", fmt.Errorf("failed to parse userData template: %w", err)
	}

	var trustDomain string
	var trustBundle string
	if node.SpiffeEnabled {
		trustDomain, trustBundle, err = a.getTrustDomainBundle(ctx, a.cfg)
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
	cniChartsPackage, err := cniChartsPackageName(cluster.CniPlugin, cniVersion)
	if err != nil {
		return "", err
	}

	type UserData struct {
		CalicoEnabled         bool
		CNIChartsPackage      string
		CNIImagePackage       string
		DiskSelectionStrategy string
		Files                 []db.CloudInitFile
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

	files, err := a.nodeAgentBootstrapFiles()
	if err != nil {
		return "", err
	}

	if err := a.validateDgxcTenantCredentials(); err != nil {
		return "", err
	}
	hashedPassword, err := sha512_crypt.New().Generate([]byte(a.cfg.DgxcTenantPassword), nil)
	if err != nil {
		return "", fmt.Errorf("failed to hash DGXC tenant password: %w", err)
	}

	// Note: Adding the trust bundle into the user data template does not present a security risk.
	// Only public certs are included in the trust bundle, and this is how zero-trust systems are intended to bootstrap.
	userDataTemplateData := UserData{
		CalicoEnabled:         calicoEnabled,
		CNIChartsPackage:      cniChartsPackage,
		CNIImagePackage:       cniImagePackage,
		DiskSelectionStrategy: string(diskSelectionStrategy),
		Files:                 files,
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

// cniChartsPackageName returns the Debian package carrying the CNI Helm charts.
//
// InstallCNI refuses to run until this package is registered in dpkg, and it reads the chart
// directory out of that package's own file list. The images package carries no charts, so a node
// that installs only that one fails before helm runs.
//
// These names must match the CHARTS role of nkeCniAptSpec in agent/internal/agent/cni.go, which is
// what the agent looks up. Note Calico's package is named "manifests" and OVN's is named "charts".
func cniChartsPackageName(plugin db.CniPlugin, version string) (string, error) {
	switch plugin {
	case db.CNICalico:
		return "nke-cni-calico-manifests-" + version, nil
	case db.CNIOvnKubernetes:
		return "nke-cni-ovn-charts-" + version, nil
	default:
		return "", fmt.Errorf("unsupported CNI plugin: %s", plugin)
	}
}

// buildTeleportLabels constructs YAML-formatted label lines for the Teleport config in userData.
// Output is "key: value" per line (no leading indent); the template applies indentation.
func buildTeleportLabels(node db.Node, cluster db.Cluster, nicoProvider db.NicoClusterProvider, nodePoolID uuid.UUID, controlPlane bool, siteName, environment, managedByLabelValue string) string {
	labels := map[string]string{
		teleport.LabelNodeID:                node.ID.String(),
		teleport.LabelNodeName:              node.Name,
		teleport.LabelNodeRole:              teleport.NodeRoleLabel(controlPlane),
		teleport.LabelSite:                  nicoProvider.SiteID.String(),
		teleport.LabelNKESiteControllerName: siteName,
		teleport.LabelCluster:               cluster.ID.String(),
		teleport.LabelClusterName:           cluster.Name,
		teleport.LabelResourceGroup:         teleport.LabelResourceGroupNKEValue,
		teleport.LabelManagedBy:             managedByLabelValue,
		teleport.LabelEnvironment:           environment,
	}
	if nodePoolID != uuid.Nil {
		labels[teleport.LabelNodePool] = nodePoolID.String()
	}
	return teleport.BuildLabelsYAML(labels)
}
