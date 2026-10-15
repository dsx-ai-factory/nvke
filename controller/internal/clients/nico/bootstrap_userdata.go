// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodecontract"
	"github.com/dsx-ai-factory/nvke/controller/internal/teleport"
	"github.com/dsx-ai-factory/nvke/controller/internal/utils"
)

// renderBootstrapUserData renders the template-less Forge/NICo node's
// cloud-init user data from the first-boot template of its release.
func (*Client) renderBootstrapUserData(
	ctx context.Context,
	deps shared.MachineCreateDeps,
	userDataTemplate string,
	node db.Node,
	nodeSet db.NodeSet,
	nodePool db.NodePool,
	cluster db.Cluster,
) (string, error) {
	provisioningConfig, err := ResolveNodeConfig(nodeSet, nodePool)
	if err != nil {
		return "", err
	}

	teleportCfg, err := nicoMachineTeleportConfig(deps.Config, node, cluster, provisioningConfig.TeleportEnabled, provisioningConfig.NodePoolID, provisioningConfig.IsControlPlaneRole)
	if err != nil {
		return "", err
	}
	userData, err := renderUserData(ctx, deps, userDataTemplate, node, provisioningConfig.DiskSelectionStrategy, provisioningConfig.KubernetesVersion, teleportCfg, cluster)
	if err != nil {
		return "", fmt.Errorf("failed to render user data for release %s: %w", provisioningConfig.Release, err)
	}

	return userData, nil
}

func nicoMachineTeleportConfig(cfg config.Config, node db.Node, cluster db.Cluster, teleportEnabled bool, nodePoolID uuid.UUID, isControlPlaneRole bool) (*TeleportUserDataConfig, error) {
	if !cfg.Teleport.Enabled || !cluster.TeleportEnabled || !teleportEnabled {
		return &TeleportUserDataConfig{}, nil
	}
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	return &TeleportUserDataConfig{
		Enabled:      true,
		TokenName:    teleport.TPMTokenName(cfg.Teleport.TokenNamePrefix, node.ID),
		ProxyAddress: cfg.Teleport.ProxyAddress,
		JoinMethod:   string(teleport.JoinMethodTPM),
		Labels:       buildTeleportLabels(node, cluster, nicoProvider, nodePoolID, isControlPlaneRole, cfg.NVKESiteControllerName, cfg.Teleport.Environment, cfg.Teleport.ManagedByLabelValue, cfg.Teleport.ResourceGroupLabelValue),
	}, nil
}

// TeleportUserDataConfig holds Teleport configuration for userData templates.
type TeleportUserDataConfig struct {
	Enabled      bool
	TokenName    string
	ProxyAddress string
	JoinMethod   string
	Labels       string
}

func renderUserData(
	ctx context.Context,
	deps shared.MachineCreateDeps,
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
		trustDomain, trustBundle, err = deps.GetTrustDomainBundle(ctx, deps.Config)
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
	cniPrefix := nodecontract.ForAgentVersion(node.AgentVersion).CNIPackagePrefix
	cniImagePackage, err := cniImagePackageName(cniPrefix, cluster.CniPlugin, cniVersion)
	if err != nil {
		return "", err
	}
	cniChartsPackage, err := cniChartsPackageName(cniPrefix, cluster.CniPlugin, cniVersion)
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

	files, err := deps.NodeAgentBootstrapFiles(node)
	if err != nil {
		return "", err
	}

	if err := validateDgxcTenantCredentials(deps.Config); err != nil {
		return "", err
	}
	hashedPassword, err := sha512_crypt.New().Generate([]byte(deps.Config.DgxcTenantPassword), nil)
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
		NodeAgentPort:         deps.Config.NodeAgent.Port,
		OvnEnabled:            ovnEnabled,
		SpiffeEnabled:         node.SpiffeEnabled,
		SpireServerAddress:    deps.Config.SpireServerAddress,
		SpireServerPort:       deps.Config.SpireServerPort,
		TrustBundle:           trustBundle,
		TrustDomain:           trustDomain,
		Username:              deps.Config.DgxcTenantUser,
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

func validateDgxcTenantCredentials(cfg config.Config) error {
	if strings.TrimSpace(cfg.DgxcTenantUser) == "" {
		return fmt.Errorf("DGXC_TENANT_USER is required for Forge release user-data rendering")
	}
	if strings.TrimSpace(cfg.DgxcTenantPassword) == "" {
		return fmt.Errorf("DGXC_TENANT_PASSWORD is required for Forge release user-data rendering")
	}
	return nil
}

func cniImagePackageName(prefix string, plugin db.CniPlugin, version string) (string, error) {
	switch plugin {
	case db.CNICalico:
		return prefix + "calico-images-" + version, nil
	case db.CNIOvnKubernetes:
		return prefix + "ovn-images-" + version, nil
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
// These names must match the CHARTS role of nvkeCniAptSpec in agent/internal/agent/cni.go, which is
// what the agent looks up. Note Calico's package is named "manifests" and OVN's is named "charts".
func cniChartsPackageName(prefix string, plugin db.CniPlugin, version string) (string, error) {
	switch plugin {
	case db.CNICalico:
		return prefix + "calico-manifests-" + version, nil
	case db.CNIOvnKubernetes:
		return prefix + "ovn-charts-" + version, nil
	default:
		return "", fmt.Errorf("unsupported CNI plugin: %s", plugin)
	}
}

// buildTeleportLabels constructs YAML-formatted label lines for the Teleport config in userData.
// Output is "key: value" per line (no leading indent); the template applies indentation.
func buildTeleportLabels(node db.Node, cluster db.Cluster, nicoProvider db.NicoClusterProvider, nodePoolID uuid.UUID, controlPlane bool, siteName, environment, managedByLabelValue, resourceGroupLabelValue string) string {
	labels := map[string]string{
		teleport.LabelNodeID:                 node.ID.String(),
		teleport.LabelNodeName:               node.Name,
		teleport.LabelNodeRole:               teleport.NodeRoleLabel(controlPlane),
		teleport.LabelSite:                   nicoProvider.SiteID.String(),
		teleport.LabelNVKESiteControllerName: siteName,
		teleport.LabelCluster:                cluster.ID.String(),
		teleport.LabelClusterName:            cluster.Name,
		teleport.LabelResourceGroup:          resourceGroupLabelValue,
		teleport.LabelManagedBy:              managedByLabelValue,
		teleport.LabelEnvironment:            environment,
	}
	if nodePoolID != uuid.Nil {
		labels[teleport.LabelNodePool] = nodePoolID.String()
	}
	return teleport.BuildLabelsYAML(labels)
}
