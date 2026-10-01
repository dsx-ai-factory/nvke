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

	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/teleport"
	"github.com/dsx-ai-factory/nke/controller/internal/utils"
)

func (a *Activities) renderTemplateLessForgeCAPIBootstrapUserData(ctx context.Context, createCtx *capiMachineCreateContext) (string, error) {
	provisioningConfig, err := nico.ResolveNodeConfig(createCtx.nodeSet, createCtx.nodePool)
	if err != nil {
		return "", err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.OSProfile)
	teleportCfg, err := a.nicoMachineTeleportConfig(createCtx, provisioningConfig.TeleportEnabled, provisioningConfig.NodePoolID, provisioningConfig.IsControlPlaneRole)
	if err != nil {
		return "", err
	}
	userData, err := a.getUserData(ctx, osProfileDir, createCtx.node, provisioningConfig.DiskSelectionStrategy, provisioningConfig.KubernetesVersion, teleportCfg, createCtx.cluster)
	if err != nil {
		return "", fmt.Errorf("failed to get user data for OS profile %s: %w", provisioningConfig.OSProfile, err)
	}

	return userData, nil
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

	files, err := a.nodeAgentBootstrapFiles(node)
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
		NodeAgentPort:         a.cfg.NodeAgent.Port,
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
