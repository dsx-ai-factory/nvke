// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	capiclient "github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	nicoclient "github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/naming"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
)

const (
	clusterIDConst  = "clusterID"
	databaseConst   = "database"
	failedValue     = "Failed"
	healthz         = "/healthz"
	readyz          = "/readyz"
	metrics         = "/metrics"
	nodePoolIDConst = "nodePoolID"
	okValue         = "ok"
	temporal        = "temporal"
)

var (
	errVersionSkewViolation                     = errors.New("version skew policy violation")
	errUpgradeInProgress                        = errors.New("cannot update Kubernetes version while an upgrade is already in progress")
	errEtcdEncryptionDisableUnsupported         = errors.New("disabling etcd encryption is not supported")
	errEtcdEncryptionRequiresProvisionedCluster = errors.New("cannot enable etcd encryption until the cluster is provisioned")
	errEtcdEncryptionDuringDeletion             = errors.New("cannot enable etcd encryption while cluster deletion is in progress")
	errEtcdEncryptionWithKubernetesUpgrade      = errors.New("cannot enable etcd encryption and update the Kubernetes version in the same request")
	errOIDCDisableUnsupported                   = errors.New("disabling OIDC authentication is not supported")
	errOIDCRequiresProvisionedCluster           = errors.New("cannot enable OIDC authentication until the cluster is provisioned")
	errOIDCDuringDeletion                       = errors.New("cannot enable OIDC authentication while cluster deletion is in progress")
	errOIDCWithKubernetesUpgrade                = errors.New("cannot enable OIDC authentication and update the Kubernetes version in the same request")
	errOIDCWithEtcdEncryption                   = errors.New("cannot enable OIDC authentication and etcd encryption in the same request")
	errClusterMaxSurgeTooHigh                   = errors.New("maxSurge should be max 1 for cluster-level control plane config")
	cniVersionPattern                           = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
)

// updateInProgressError signals that a mutually-exclusive cluster operation
// (etcd encryption, k8s upgrade, CP node recycling) is already running and a
// new UpdateCluster mutation must be rejected with 409. The caller converts
// this to the API response.
type updateInProgressError struct {
	op        db.OperationType
	startedAt time.Time
	message   string
}

func (e *updateInProgressError) Error() string {
	msg := fmt.Sprintf("cluster update rejected: %s in progress since %s", e.op, e.startedAt.Format(time.RFC3339))
	if e.message != "" {
		msg += " (" + e.message + ")"
	}
	if e.op == db.OperationTypeCertRenewal {
		msg += "; a failed certificate renewal can retain the lock until an operator clears it (see controller/docs/certificate-renewal.md)"
	}
	if e.op == db.OperationTypeEtcdKeyRotation {
		msg += "; a rotation that fails after prepare keeps the lock until its workflow is reset and completes " +
			"(see controller/docs/etcd-encryption-key-rotation.md#phase-aware-recovery)"
	}
	return msg
}

type clusterService struct {
	logger                           *slog.Logger
	db                               db.DB
	nodePoolService                  *nodePoolService
	teleportCfg                      config.TeleportConfig
	oidcAuthEnabled                  bool
	fleetOIDC                        *controllerconfig.OIDCConfig
	providers                        providerSupport
	allowLegacyCalicoClusterCreation bool
	nodeAgentUpdateMethod            string
	allowlistedLabelKeys             []string
	machineAllowlistedLabelKeys      []string
	machineAllowlistedAnnotationKeys []string
	// nodeAgentProfile carries the node-agent settings nodes.PinAgentProfile
	// stamps on every node this deployment creates.
	nodeAgentProfile db.Node
	// nodeAgentClient reads live node state; nil reports nodes unreachable.
	nodeAgentClient             agent.Client
	allowControlPlaneWithoutVIP bool
}

func NewClusterService(
	logger *slog.Logger,
	database db.DB,
	nodePoolService *nodePoolService,
	teleportCfg config.TeleportConfig,
	oidcAuthEnabled bool,
	supportedInfraProviders []string,
	allowLegacyCalicoClusterCreation bool,
	updateMethod string,
) *clusterService {
	return &clusterService{
		logger:                           logger,
		db:                               database,
		nodePoolService:                  nodePoolService,
		teleportCfg:                      teleportCfg,
		oidcAuthEnabled:                  oidcAuthEnabled,
		providers:                        newProviderSupport(supportedInfraProviders),
		allowLegacyCalicoClusterCreation: allowLegacyCalicoClusterCreation,
		nodeAgentUpdateMethod:            updateMethod,
	}
}

// applyClusterDefaults fills in server-side defaults for omitted cluster fields.
func (s *clusterService) applyClusterDefaults(req *api.CreateClusterRequest, cluster *db.Cluster) error {
	cluster.OIDCAuthEnabled = s.oidcAuthEnabled
	s.pinFleetOIDCProviders(cluster)

	if req.TeleportEnabled == nil {
		cluster.TeleportEnabled = s.teleportCfg.DefaultClusterEnabled
	}

	return nil
}

// pinFleetOIDCProviders pins none before Kubernetes 1.34, which cannot read the
// AuthenticationConfiguration NVKE renders.
func (s *clusterService) pinFleetOIDCProviders(cluster *db.Cluster) {
	if s.fleetOIDC == nil {
		return
	}
	providers := s.fleetOIDC.Providers
	if len(providers) > 0 && !kubeadmconfig.SupportsAuthenticationConfiguration(cluster.KubernetesVersion) {
		s.logger.Warn("Creating cluster without the fleet-wide OIDC providers: its Kubernetes version cannot read the AuthenticationConfiguration NVKE renders",
			"cluster", cluster.Name, "kubernetesVersion", cluster.KubernetesVersion, "providers", len(providers))
		providers = nil
	}
	cluster.OIDCProviders = db.ClusterOIDCProvidersFromConfig(providers)
}

// applyNodePoolDefaults fills in server-side defaults for omitted node pool fields
// based on the pool's role (control plane vs worker) and infrastructure type.
func (s *clusterService) applyNodePoolDefaults(req *api.CreateNodePoolRequest, cluster db.Cluster) {
	if req.TeleportEnabled == nil {
		v := s.teleportCfg.NodePoolTeleportDefault(req.ControlPlane)
		req.TeleportEnabled = &v
	}
	// The stored pool is built from the nested payload, so the default has to land
	// there; validation has already required nico for a Forge pool.
	if !clusterIsCAPIBacked(cluster) && req.Nico != nil && req.Nico.DiskSelectionStrategy == "" {
		req.Nico.DiskSelectionStrategy = api.DiskSelectionStrategyCreateSmallest
	}
}

func (s *clusterService) ListClusters(ctx context.Context, req api.ListClustersRequestObject) (api.ListClustersResponseObject, error) {
	filters := db.ClusterFilters{
		Name: req.Params.Name,
	}

	dbClusters, err := s.db.ListClusters(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}
	apiClusters := make([]api.Cluster, len(dbClusters))
	for i := range dbClusters {
		apiClusters[i] = APIClusterFromDB(&dbClusters[i])
	}
	return api.ListClusters200JSONResponse{Clusters: apiClusters}, nil
}

func (s *clusterService) CreateCluster(ctx context.Context, req api.CreateClusterRequestObject) (api.CreateClusterResponseObject, error) {
	network, err := s.preflightCheckCreateClusterRequest(ctx, req.Body)
	if err != nil {
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
			recordRequestRejectionReason(ctx, badRequestErr.rejectionReason)
			return api.CreateCluster400JSONResponse{Message: badRequestErr.Error()}, nil
		}
		return nil, err
	}
	if err := network.validateCNI(db.CniPlugin(req.Body.CniPlugin)); err != nil {
		return api.CreateCluster400JSONResponse{Message: err.Error()}, nil
	}

	var createdCluster db.Cluster
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		dbCluster, err := DBClusterFromAPI(req.Body)
		if err != nil {
			return fmt.Errorf("failed to convert cluster request: %w", err)
		}
		if err := s.applyClusterDefaults(req.Body, &dbCluster); err != nil {
			return err
		}
		dbCluster.PodCIDR = network.podCIDR
		dbCluster.ServiceCIDR = network.serviceCIDR.String()

		// Persist the desired count and max surge as columns up front; the config
		// snapshot row is created after the cluster row exists (it needs the cluster ID).
		if req.Body.ControlPlaneConfig != nil {
			dbCluster.ControlPlaneConfig.DesiredCount = req.Body.ControlPlaneConfig.DesiredCount
			if req.Body.ControlPlaneConfig.MaxSurge != nil {
				dbCluster.ControlPlaneConfig.MaxSurge = *req.Body.ControlPlaneConfig.MaxSurge
			}
			if req.Body.ControlPlaneConfig.AutoScaleDownDisabled != nil {
				dbCluster.ControlPlaneConfig.AutoScaleDownDisabled = *req.Body.ControlPlaneConfig.AutoScaleDownDisabled
			}
		}

		createdCluster, err = tx.CreateCluster(ctx, &dbCluster)
		if err != nil {
			return fmt.Errorf("failed to create cluster: %w", err)
		}

		if req.Body.ControlPlaneConfig != nil {
			spec := controlPlaneConfigSpecFromCreateAPI(
				req.Body.ControlPlaneConfig,
				createdCluster.KubernetesVersion,
				s.teleportCfg.NodePoolTeleportDefault(true),
				createdCluster.ProviderType,
			)
			if err := db.CreateControlPlaneConfigRowIfChanged(ctx, tx, &createdCluster, spec, db.CreateControlPlaneConfigRowOpts{
				ChangeReason: "cluster create controlPlaneConfig",
			}); err != nil {
				return fmt.Errorf("failed to create initial control plane configuration: %w", err)
			}
			if _, err := tx.UpdateCluster(ctx, &createdCluster); err != nil {
				return fmt.Errorf("failed to persist cluster control plane configuration: %w", err)
			}
		}

		workflowID := constants.ProvisionClusterWorkflowPrefix + createdCluster.ID.String()
		err = enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
			WorkflowType: "ProvisionCluster",
			WorkflowID:   workflowID,
			ClusterID:    createdCluster.ID,
			ClusterName:  createdCluster.Name,
		}, map[string]interface{}{
			clusterIDConst: createdCluster.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry for cluster: %w", err)
		}

		var initialNodePools []api.NodePoolCreateConfig
		if req.Body.NodePools != nil {
			initialNodePools = *req.Body.NodePools
		}
		if err := s.createInitialNodePools(ctx, tx, createdCluster, initialNodePools); err != nil {
			return fmt.Errorf("failed to create initial node pools and nodes: %w", err)
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, db.ErrClusterNameNotUnique) {
			recordRequestRejectionReason(ctx, rejectionReasonClusterNameAlreadyExists)
			return api.CreateCluster400JSONResponse{
				Message: fmt.Sprintf("A cluster already exists with the name %q", req.Body.Name),
			}, nil
		}
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
			recordRequestRejectionReason(ctx, badRequestErr.rejectionReason)
			return api.CreateCluster400JSONResponse{Message: badRequestErr.Error()}, nil
		}
		return nil, err
	}

	return api.CreateCluster201JSONResponse(APIClusterFromDB(&createdCluster)), nil
}

func (s *clusterService) preflightCheckCreateClusterRequest(
	_ context.Context,
	req *api.CreateClusterRequest,
) (resolvedClusterNetwork, error) {
	if err := s.ValidateCreateClusterRequestFields(req); err != nil {
		return resolvedClusterNetwork{}, newBadRequestErrorf("%s", err)
	}
	network, err := resolveClusterNetwork(req.Network)
	if err != nil {
		return resolvedClusterNetwork{}, newClassifiedBadRequestErrorf(rejectionReasonInvalidClusterNetwork, "%s", err)
	}
	desiredNodes := 0
	if req.NodePools != nil {
		for _, nodePool := range *req.NodePools {
			desiredNodes += nodePool.DesiredNodeCount
		}
	}
	cpSurge := 0
	// Include the cluster-level control plane node count (and its surge) in the
	// pod-CIDR capacity check.
	if req.ControlPlaneConfig != nil {
		desiredNodes += req.ControlPlaneConfig.DesiredCount
		if req.ControlPlaneConfig.MaxSurge != nil {
			cpSurge = *req.ControlPlaneConfig.MaxSurge
		}
	}
	if err := validatePodCIDRCapacity(network.podCIDRPrefixes, desiredNodes, cpSurge); err != nil {
		return resolvedClusterNetwork{}, newBadRequestErrorf("%s", err)
	}
	controlPlaneVIP := strings.TrimSpace(req.ControlPlaneVIP)
	if controlPlaneVIP != "" {
		if err := network.validateControlPlaneVIP(controlPlaneVIP); err != nil {
			return resolvedClusterNetwork{}, newBadRequestErrorf("%s", err)
		}
	}

	// When the control plane is configured at the cluster level for a Forge
	// cluster, enforce the same subnet/VPC-prefix native-networking rules used
	if req.ControlPlaneConfig != nil && req.Nico != nil {
		if req.ControlPlaneConfig.Nico == nil {
			return resolvedClusterNetwork{}, newBadRequestErrorf(
				"controlPlaneConfig.nico is required for a nico cluster control plane",
			)
		}
		if err := validateControlPlaneNicoNetworking(
			req.ControlPlaneConfig.Nico.SubnetIDs,
			req.ControlPlaneConfig.Nico.VpcPrefixIDs,
		); err != nil {
			return resolvedClusterNetwork{}, err
		}
	}

	return network, nil
}

// validateNodeAgentProfile rejects new nodes of nodePool that could never
// join under this deployment's node-agent settings, profile.
func validateNodeAgentProfile(profile db.Node, cluster db.Cluster, nodePool db.NodePool) error {
	validator, err := clients.GetNodeAgentProfileValidator(cluster)
	if err != nil {
		return err
	}
	if err := validator.ValidateNodeAgentProfile(profile, nodePool); err != nil {
		return fmt.Errorf("this deployment's node-agent settings do not support the %s provider: %w", cluster.ProviderType, err)
	}
	return nil
}

// validateControlPlaneNodeAgentProfile is validateNodeAgentProfile for the
// control plane, whose nodes belong to the node pool the provider builds from
// spec.
func validateControlPlaneNodeAgentProfile(profile db.Node, cluster db.Cluster, spec db.ControlPlaneConfigSpec) error {
	nodeProvider, err := clients.GetControlPlaneConfigNodeProvider(cluster)
	if err != nil {
		return err
	}
	var nodeSet db.NodeSet
	var nodePool db.NodePool
	nodeProvider.ApplyControlPlaneConfigPayloads(db.Node{}, spec, &nodeSet, &nodePool)
	return validateNodeAgentProfile(profile, cluster, nodePool)
}

func (s *clusterService) ValidateCreateClusterRequestFields(req *api.CreateClusterRequest) error {
	if err := naming.ValidateClusterNameForNodeNaming(req.Name); err != nil {
		return err
	}
	if len(req.Name) > capi.ClusterNameMaxLength {
		return fmt.Errorf("cluster name must be %d characters or fewer so Kubernetes object names derived from it stay within limits", capi.ClusterNameMaxLength)
	}

	var nodePools []api.NodePoolCreateConfig
	if req.NodePools != nil {
		nodePools = *req.NodePools
	}

	var providerType db.ProviderType
	switch {
	case req.Infra != nil && req.Nico != nil, req.Infra != nil && req.LaunchLayer != nil, req.Nico != nil && req.LaunchLayer != nil:
		return fmt.Errorf("exactly one provider must be set")
	case req.Infra != nil:
		if err := s.providers.require(config.InfraProviderInfra); err != nil {
			return err
		}
		if err := validateCreateCAPIClusterProvider(req); err != nil {
			return err
		}
		providerType = db.ProviderTypeCAPI
	case req.Nico != nil:
		if err := s.providers.require(config.InfraProviderNico); err != nil {
			return err
		}
		if err := validateCreateNicoClusterProvider(req); err != nil {
			return err
		}
		providerType = db.ProviderTypeNico
	case req.LaunchLayer != nil:
		if err := s.providers.require(config.InfraProviderLaunchLayer); err != nil {
			return err
		}
		if err := validateCreateLaunchLayerClusterProvider(req); err != nil {
			return err
		}
		providerType = db.ProviderTypeLaunchLayer
	default:
		return fmt.Errorf("exactly one provider must be set")
	}

	if req.ControlPlaneConfig != nil && req.ControlPlaneConfig.NodeAgentConfig != nil && req.ControlPlaneConfig.NodeAgentConfig.Version != nil {
		version, err := canonicalNodeAgentVersion(*req.ControlPlaneConfig.NodeAgentConfig.Version)
		if err != nil {
			return err
		}
		if version != "" && s.nodeAgentUpdateMethod == "" {
			return fmt.Errorf("nodeAgent.updateMethod is required for agent convergence")
		}
		req.ControlPlaneConfig.NodeAgentConfig.Version = &version
	}
	if err := validateCreateClusterLevelControlPlane(req, nodePools, providerType, s.allowlistedLabelKeys); err != nil {
		return err
	}
	controlPlaneSpec := controlPlaneConfigSpecFromCreateAPI(req.ControlPlaneConfig, req.ControlPlaneKubernetesVersion, false, providerType)
	if err := validateControlPlaneNodeAgentProfile(s.nodeAgentProfile, db.Cluster{ProviderType: providerType}, controlPlaneSpec); err != nil {
		return err
	}
	if req.ControlPlaneConfig == nil {
		return fmt.Errorf("controlPlaneConfig is required")
	}
	if err := validateMachineMetadata(req.ControlPlaneConfig.MachineAnnotations, req.ControlPlaneConfig.MachineLabels, s.machineAllowlistedAnnotationKeys, s.machineAllowlistedLabelKeys); err != nil {
		return fmt.Errorf("controlPlaneConfig: %w", err)
	}
	if err := s.validateControlPlaneCapacityChange(req.ControlPlaneVIP, 0, req.ControlPlaneConfig.DesiredCount); err != nil {
		return err
	}

	dbCniPlugin := db.EffectiveCNIPlugin(db.CniPlugin(req.CniPlugin))
	supportedCNIPlugins := db.ListCNIPlugins()
	if !s.allowLegacyCalicoClusterCreation {
		supportedCNIPlugins = slices.DeleteFunc(supportedCNIPlugins, func(p db.CniPlugin) bool { return p == db.CNICalico })
	}
	if !slices.Contains(supportedCNIPlugins, dbCniPlugin) {
		if dbCniPlugin == db.CNICalico {
			return errors.New("calico cluster creation is disabled by this controller deployment; use ovn-kubernetes")
		}
		return fmt.Errorf("invalid CNI plugin: %s, valid plugins: %v", req.CniPlugin, supportedCNIPlugins)
	}
	if req.CniVersion != nil {
		cniVersion := strings.TrimSpace(*req.CniVersion)
		if cniVersion != "" && !cniVersionPattern.MatchString(cniVersion) {
			return fmt.Errorf("invalid CNI version: %s, expected semver version core X.Y.Z without leading v", *req.CniVersion)
		}
	}

	for _, nodePool := range nodePools {
		if err := naming.ValidateNodeNamePrefix(req.Name, nodePool.NodeNamePrefix); err != nil {
			return fmt.Errorf("invalid node pool %q: %w", nodePool.Name, err)
		}
		if err := validateNodeLabels(nodePool.NodeLabels, s.allowlistedLabelKeys); err != nil {
			return fmt.Errorf("invalid node pool %q: %w", nodePool.Name, err)
		}
		if err := validateNodeTaints(nodePool.NodeTaints, nodePool.ControlPlane); err != nil {
			return fmt.Errorf("invalid node pool %q: %w", nodePool.Name, err)
		}
	}

	return nil
}

func validateCreateCAPIClusterProvider(req *api.CreateClusterRequest) error {
	if req.Nico != nil {
		return fmt.Errorf("exactly one provider must be set")
	}
	if isEmptyManifestObject(req.Infra.Cluster) {
		return fmt.Errorf("infra.cluster is required")
	}
	infraCluster, err := rawManifestFromObject(req.Infra.Cluster)
	if err != nil {
		return fmt.Errorf("invalid infra.cluster: %w", err)
	}
	if err := capiclient.ValidateInfrastructureManifestBytes(infraCluster); err != nil {
		return fmt.Errorf("invalid infra.cluster: %w", err)
	}
	return nil
}

func validateCreateNicoClusterProvider(req *api.CreateClusterRequest) error {
	if req.Nico.SiteID == uuid.Nil {
		return fmt.Errorf("nico.siteID is required to create a nico cluster")
	}
	if req.Nico.OrgID == "" {
		return fmt.Errorf("nico.orgID is required to create a nico cluster")
	}
	if req.Nico.VpcID == uuid.Nil {
		return fmt.Errorf("nico.vpcID is required to create a nico cluster")
	}
	return nil
}

func validateCreateLaunchLayerClusterProvider(req *api.CreateClusterRequest) error {
	if req.LaunchLayer.VpcID == "" && req.LaunchLayer.OrgID == "" {
		return fmt.Errorf("exactly one of launchLayer.vpcID or launchLayer.orgID must be provided")
	}
	if req.LaunchLayer.VpcID != "" && req.LaunchLayer.OrgID != "" {
		return fmt.Errorf("exactly one of launchLayer.vpcID or launchLayer.orgID must be provided")
	}
	if req.LaunchLayer.SiteID == "" {
		return fmt.Errorf("launchLayer.siteID is required to create a launchLayer cluster")
	}
	if req.LaunchLayer.CloudProviderEnabled {
		return fmt.Errorf("launchLayer.cloudProviderEnabled must not be true: NVKE runs no cloud controller manager")
	}
	return validateLaunchLayerSecurityGroups(req.LaunchLayer)
}

// validateLaunchLayerSecurityGroups applies the security-group rules CAPLL's
// CRD enforces, so a bad group gets 400 rather than a cluster whose
// LaunchLayerCluster apply is retried forever.
func validateLaunchLayerSecurityGroups(provider *api.LaunchLayerClusterProvider) error {
	if provider.SecurityGroups == nil {
		return nil
	}
	names := make(map[string]struct{}, len(*provider.SecurityGroups))
	for _, group := range *provider.SecurityGroups {
		if _, exists := names[group.Name]; exists {
			return fmt.Errorf("duplicate launchLayer.securityGroups name %q", group.Name)
		}
		names[group.Name] = struct{}{}
		virtualized := group.Type == api.LaunchLayerSecurityGroupTypeVIRTUALIZED
		if virtualized != (group.NetworkType != nil) {
			return fmt.Errorf("launchLayer.securityGroups %q: networkType is required for a VIRTUALIZED group and forbidden for a BARE_METAL group", group.Name)
		}
		// CAPLL treats an unset vpcType as VIRTUALIZED.
		if virtualized && provider.VpcType != nil && *provider.VpcType != api.LaunchLayerClusterProviderVpcTypeVIRTUALIZED {
			return fmt.Errorf("launchLayer.securityGroups %q: a VIRTUALIZED group requires a VIRTUALIZED vpcType", group.Name)
		}
	}
	return nil
}

func (s *clusterService) createInitialNodePools(ctx context.Context, tx db.Tx, cluster db.Cluster, nodePools []api.NodePoolCreateConfig) error {
	for _, nodePoolConfig := range nodePools {
		//exhaustruct:enforce
		nodePoolReq := api.CreateNodePoolRequest{
			ClusterID:                   cluster.ID,
			Name:                        nodePoolConfig.Name,
			NodeNamePrefix:              nodePoolConfig.NodeNamePrefix,
			SystemReservedMemory:        nodePoolConfig.SystemReservedMemory,
			KubeReservedMemory:          nodePoolConfig.KubeReservedMemory,
			NodeLabels:                  nodePoolConfig.NodeLabels,
			NodeTaints:                  nodePoolConfig.NodeTaints,
			ControlPlane:                nodePoolConfig.ControlPlane,
			DesiredNodeCount:            nodePoolConfig.DesiredNodeCount,
			Description:                 nodePoolConfig.Description,
			AutoProvisionDisabled:       nodePoolConfig.AutoProvisionDisabled,
			AutoModeDisabled:            nodePoolConfig.AutoModeDisabled,
			ProvisioningMonitorDisabled: nodePoolConfig.ProvisioningMonitorDisabled,
			RecycleBudget:               nodePoolConfig.RecycleBudget,
			TeleportEnabled:             nodePoolConfig.TeleportEnabled,
			Nico:                        nodePoolConfig.Nico,
			Infra:                       nodePoolConfig.Infra,
			MachineAnnotations:          nodePoolConfig.MachineAnnotations,
			MachineLabels:               nodePoolConfig.MachineLabels,
			LaunchLayer:                 nodePoolConfig.LaunchLayer,
		}

		if err := s.nodePoolService.validateCreateNodePoolRequest(cluster, &nodePoolReq); err != nil {
			return fmt.Errorf("invalid node pool %q: %w", nodePoolConfig.Name, err)
		}

		s.applyNodePoolDefaults(&nodePoolReq, cluster)
		dbNodePool, err := dbNodePoolFromAPI(&nodePoolReq, cluster.KubernetesVersion)
		if err != nil {
			return fmt.Errorf("failed to convert node pool request: %w", err)
		}

		createdNodePool, err := tx.CreateNodePool(ctx, &dbNodePool)
		if err != nil {
			return fmt.Errorf("failed to create node pool: %w", err)
		}

		nodePoolWorkflowID := constants.ProvisionNodePoolWorkflowPrefix + createdNodePool.ID.String()
		err = enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
			WorkflowType: "ProvisionNodePool",
			WorkflowID:   nodePoolWorkflowID,
			ClusterID:    cluster.ID,
			ClusterName:  cluster.Name,
			NodePoolID:   createdNodePool.ID,
			NodePoolName: createdNodePool.Name,
		}, map[string]interface{}{
			nodePoolIDConst: createdNodePool.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry for node pool: %w", err)
		}
	}

	return nil
}

func (s *clusterService) GetCluster(ctx context.Context, req api.GetClusterRequestObject) (api.GetClusterResponseObject, error) {
	dbCluster, err := s.db.GetCluster(ctx, req.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetCluster404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	return api.GetCluster200JSONResponse(APIClusterFromDB(&dbCluster)), nil
}

func (s *clusterService) UpdateCluster(ctx context.Context, req api.UpdateClusterRequestObject) (api.UpdateClusterResponseObject, error) {
	if req.Body == nil {
		return api.UpdateCluster400JSONResponse{Message: "request body is required"}, nil
	}

	if req.Body.Network != nil {
		return api.UpdateCluster400JSONResponse{Message: "network is immutable after cluster creation"}, nil
	}
	if req.Body.EnableEtcdEncryption != nil && !*req.Body.EnableEtcdEncryption {
		return api.UpdateCluster400JSONResponse{Message: errEtcdEncryptionDisableUnsupported.Error()}, nil
	}
	if err := validateOIDCUpdateRequest(req.Body); err != nil {
		return api.UpdateCluster400JSONResponse{Message: err.Error()}, nil
	}

	var updatedCluster db.Cluster
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// GetClusterForUpdate acquires SELECT FOR UPDATE on the cluster row so the
		// UpdateInProgress conflict check below is atomic with the row lock. This
		// closes the TOCTOU window where two concurrent PATCH requests could both
		// observe UpdateInProgress=nil and both proceed.
		dbCluster, err := tx.GetClusterForUpdate(ctx, req.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}
		// Reject PATCH mutations while another cluster update is running.
		// UpdateInProgress is the shared lock for these updates. Any API or controller
		// that can add, remove, replace, or reconfigure a control-plane node must check
		// this field first so the control-plane node list cannot change during an update.
		// Legacy control-plane operations still use their existing condition flags, so
		// those are checked in the operation-specific validation below.
		if uip := dbCluster.Conditions.UpdateInProgress; uip != nil {
			return &updateInProgressError{
				op:        uip.Type,
				startedAt: uip.StartedAt,
				message:   uip.Message,
			}
		}
		if agentUpgradeBlocksMaintenance(dbCluster, req.Body) {
			return errAgentUpgradeInProgress
		}
		// Reject a controlPlaneConfig provider block that doesn't match the cluster before
		// any field is touched.
		if err := validateUpdateControlPlaneConfigProviderMatch(dbCluster.ProviderType, req.Body.ControlPlaneConfig); err != nil {
			return err
		}
		// Update fields from request
		if req.Body.EnableCPNodeRecycling != nil {
			dbCluster.EnableCPNodeRecycling = *req.Body.EnableCPNodeRecycling
		}
		if req.Body.TeleportEnabled != nil {
			dbCluster.TeleportEnabled = *req.Body.TeleportEnabled
		}
		if req.Body.ControlPlaneKubernetesVersion != nil && *req.Body.ControlPlaneKubernetesVersion != "" {
			if req.Body.EnableEtcdEncryption != nil && *req.Body.EnableEtcdEncryption {
				return errEtcdEncryptionWithKubernetesUpgrade
			}
			if req.Body.EnableOidc != nil && *req.Body.EnableOidc {
				return errOIDCWithKubernetesUpgrade
			}
			if err := ValidateKubernetesVersionSkewWithControlPlane(dbCluster.KubernetesVersion, *req.Body.ControlPlaneKubernetesVersion); err != nil {
				return err
			}
			if err := s.validateControlPlaneUpgrade(dbCluster.ControlPlaneVIP); err != nil {
				return err
			}

			if err := ValidateKubernetesVersionSkewWithWorkerNodes(ctx, tx, req.ClusterID, *req.Body.ControlPlaneKubernetesVersion); err != nil {
				return err
			}

			// Reject upgrade request if an upgrade is already in progress
			if dbCluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
				return errUpgradeInProgress
			}

			dbCluster.KubernetesVersion = *req.Body.ControlPlaneKubernetesVersion
		}

		if req.Body.EnableEtcdEncryption != nil && *req.Body.EnableEtcdEncryption {
			if dbCluster.EtcdEncrypted {
				if !hasNonEtcdEncryptionMutation(req.Body) {
					updatedCluster = dbCluster
					return nil
				}
			} else {
				if err := validateEtcdEncryptionCanStart(dbCluster); err != nil {
					return err
				}

				db.ClearUpdateFailed(&dbCluster)
				db.SetUpdateInProgress(&dbCluster, db.OperationTypeEtcdEncryption, "enabling etcd encryption", time.Now().UTC())
				workflowID := constants.EnableEtcdEncryptionWorkflowPrefix + dbCluster.ID.String()
				if err := enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
					WorkflowType: constants.EnableEtcdEncryptionWorkflowType,
					WorkflowID:   workflowID,
					ClusterID:    dbCluster.ID,
					ClusterName:  dbCluster.Name,
				}, map[string]interface{}{
					clusterIDConst: dbCluster.ID,
				}); err != nil {
					return fmt.Errorf("failed to create workflow outbox entry for etcd encryption: %w", err)
				}
			}
		}

		if err := enqueueOIDCEnablement(ctx, tx, req.Body, &dbCluster); err != nil {
			return err
		}

		if req.Body.ControlPlaneConfig != nil ||
			(req.Body.ControlPlaneKubernetesVersion != nil && *req.Body.ControlPlaneKubernetesVersion != "") {
			if err := s.applyControlPlaneConfigUpdateWithCIDRCheck(ctx, tx, &dbCluster, req.Body.ControlPlaneConfig); err != nil {
				return err
			}
		}

		updatedCluster, err = tx.UpdateCluster(ctx, &dbCluster)
		if err != nil {
			return fmt.Errorf("failed to update cluster: %w", err)
		}
		if err := db.LoadClusterControlPlaneConfig(ctx, tx, &updatedCluster); err != nil {
			return fmt.Errorf("load updated cluster control plane config: %w", err)
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.UpdateCluster404Response{}, nil
		}
		// Return 409 when another mutually-exclusive operation is already running.
		if response := updateClusterConflictResponse(err); response != nil {
			return response, nil
		}
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
			return api.UpdateCluster400JSONResponse{Message: badRequestErr.Error()}, nil
		}
		// Return 400 for client validation errors
		if isUpdateClusterClientError(err) {
			return api.UpdateCluster400JSONResponse{Message: err.Error()}, nil
		}
		return nil, fmt.Errorf("failed to update cluster: %w", err)
	}
	response := api.UpdateCluster200JSONResponse(APIClusterFromDB(&updatedCluster))
	s.logger.Info("UpdateCluster response",
		clusterIDConst, req.ClusterID,
		"response", response,
	)
	return response, nil
}

// validateUpdateControlPlaneConfigProviderMatch rejects a controlPlaneConfig
// provider block that doesn't match providerType, before any field is
// touched.
func validateUpdateControlPlaneConfigProviderMatch(providerType db.ProviderType, cpConfig *api.UpdateClusterControlPlaneConfig) error {
	if cpConfig == nil {
		return nil
	}
	var providerField, providerName string
	switch providerType {
	case db.ProviderTypeNico:
		providerField, providerName = "nico", "controlPlaneConfig.nico"
	case db.ProviderTypeCAPI:
		providerField, providerName = "infra", "controlPlaneConfig.infra"
	case db.ProviderTypeLaunchLayer:
		providerField, providerName = "launchLayer", "controlPlaneConfig.launchLayer"
	default:
		return newBadRequestErrorf("unsupported cluster provider %q", providerType)
	}
	if providerField != "nico" && cpConfig.Nico != nil {
		return newBadRequestErrorf("controlPlaneConfig.nico is not supported, please provide %s", providerName)
	}
	if providerField != "infra" && cpConfig.Infra != nil {
		return newBadRequestErrorf("controlPlaneConfig.infra is not supported, please provide %s", providerName)
	}
	if providerField != "launchLayer" && cpConfig.LaunchLayer != nil {
		return newBadRequestErrorf("controlPlaneConfig.launchLayer is not supported, please provide %s", providerName)
	}
	return nil
}

// validateOIDCUpdateRequest rejects unsupported OIDC-toggle requests up front:
// disabling OIDC is not supported, and OIDC enablement cannot be combined with
// etcd encryption enablement (both are one-way control-plane rollouts that take
// the single UpdateInProgress lock).
func validateOIDCUpdateRequest(body *api.UpdateClusterRequest) error {
	if body.EnableOidc == nil || !*body.EnableOidc {
		if body.EnableOidc != nil {
			return errOIDCDisableUnsupported
		}
		return nil
	}
	if body.EnableEtcdEncryption != nil && *body.EnableEtcdEncryption {
		return errOIDCWithEtcdEncryption
	}
	return nil
}

// isUpdateClusterClientError reports whether err is a client-side validation
// failure from UpdateCluster that should map to a 400 rather than a 500.
func isUpdateClusterClientError(err error) bool {
	return errors.Is(err, errVersionSkewViolation) ||
		errors.Is(err, errUpgradeInProgress) ||
		errors.Is(err, errEtcdEncryptionDisableUnsupported) ||
		errors.Is(err, errEtcdEncryptionRequiresProvisionedCluster) ||
		errors.Is(err, errEtcdEncryptionDuringDeletion) ||
		errors.Is(err, errEtcdEncryptionWithKubernetesUpgrade) ||
		errors.Is(err, errOIDCDisableUnsupported) ||
		errors.Is(err, errOIDCRequiresProvisionedCluster) ||
		errors.Is(err, errOIDCDuringDeletion) ||
		errors.Is(err, errOIDCWithKubernetesUpgrade) ||
		errors.Is(err, errOIDCWithEtcdEncryption) ||
		errors.Is(err, errClusterMaxSurgeTooHigh)
}

func hasNonEtcdEncryptionMutation(req *api.UpdateClusterRequest) bool {
	return req.EnableCPNodeRecycling != nil ||
		req.TeleportEnabled != nil ||
		(req.ControlPlaneKubernetesVersion != nil && *req.ControlPlaneKubernetesVersion != "") ||
		req.ControlPlaneConfig != nil
}

// podCIDRNodePoolsWithClusterCP returns nodePools augmented with a synthetic
// control-plane pool that carries the cluster-level ControlPlaneConfig.DesiredCount.
// The control plane is not a node pool, so this is how its nodes enter the pod-CIDR
// capacity accounting. CP MaxSurge is supplied separately as the clusterMaxSurge
// argument to the capacity helpers, so the synthetic pool leaves MaxSurge zero to
// avoid double-counting.
func podCIDRNodePoolsWithClusterCP(cluster db.Cluster, nodePools []db.NodePool) []db.NodePool {
	return append(append([]db.NodePool(nil), nodePools...), db.NodePool{
		ControlPlane: true,
		DesiredCount: cluster.ControlPlaneConfig.DesiredCount,
	})
}

// applyControlPlaneConfigUpdateWithCIDRCheck applies the control-plane config
// update and, when the caller supplied a controlPlaneConfig, re-runs pod-CIDR
// capacity validation if the control-plane surge or cluster-level desired count
// grew.
func (s *clusterService) applyControlPlaneConfigUpdateWithCIDRCheck(
	ctx context.Context,
	tx db.Tx,
	cluster *db.Cluster,
	cpConfig *api.UpdateClusterControlPlaneConfig,
) error {
	// Snapshot the pre-update cluster so we can compare capacity before vs after.
	// Safe as a shallow copy: we only read the scalar MaxSurge / DesiredCount.
	previousCluster := *cluster
	if err := s.applyClusterControlPlaneConfigUpdate(ctx, tx, cluster, cpConfig); err != nil {
		return err
	}
	if cpConfig == nil {
		return nil
	}

	if cluster.ControlPlaneConfig.DesiredCount == previousCluster.ControlPlaneConfig.DesiredCount &&
		cluster.ControlPlaneConfig.MaxSurge == previousCluster.ControlPlaneConfig.MaxSurge {
		return nil
	}

	clusterID := cluster.ID
	nodePools, err := tx.ListNodePools(ctx, db.NodePoolFilters{ClusterID: &clusterID})
	if err != nil {
		return fmt.Errorf("failed to list node pools for pod CIDR capacity validation: %w", err)
	}
	currentNodePools := podCIDRNodePoolsWithClusterCP(*cluster, nodePools)
	previousNodePools := podCIDRNodePoolsWithClusterCP(previousCluster, nodePools)
	if requiredPodCIDRNodeBlocks(currentNodePools, cluster.ControlPlaneConfig.MaxSurge) >
		requiredPodCIDRNodeBlocks(previousNodePools, previousCluster.ControlPlaneConfig.MaxSurge) {
		if err := validateStoredPodCIDRCapacity(*cluster, currentNodePools); err != nil {
			return newBadRequestErrorf("%s", err)
		}
	}
	return nil
}

func (s *clusterService) applyClusterControlPlaneConfigUpdate(ctx context.Context, tx db.Tx, cluster *db.Cluster, req *api.UpdateClusterControlPlaneConfig) error {
	if req == nil {
		req = &api.UpdateClusterControlPlaneConfig{}
	}
	if req.Nico != nil {
		if err := s.providers.require(config.InfraProviderNico); err != nil {
			return newBadRequestErrorf("%s", err)
		}
	}
	if req.Infra != nil {
		if err := s.providers.require(config.InfraProviderInfra); err != nil {
			return newBadRequestErrorf("%s", err)
		}
	}

	// MaxSurge is cluster-level only; it is not part of ControlPlaneConfigSpec revisions.
	if req.MaxSurge != nil {
		if *req.MaxSurge > 1 {
			return errClusterMaxSurgeTooHigh
		}
		cluster.ControlPlaneConfig.MaxSurge = *req.MaxSurge
	}
	// AutoScaleDownDisabled is cluster-level only, same as MaxSurge.
	if req.AutoScaleDownDisabled != nil {
		cluster.ControlPlaneConfig.AutoScaleDownDisabled = *req.AutoScaleDownDisabled
	}
	controlPlaneConfigSpec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
	// KubernetesVersion lives in the spec row and should track the current
	// cluster control-plane version selected in this PATCH.
	controlPlaneConfigSpec.KubernetesVersion = cluster.KubernetesVersion
	if req.ProvisioningMonitorDisabled != nil {
		controlPlaneConfigSpec.ProvisioningMonitorDisabled = *req.ProvisioningMonitorDisabled
	}
	hasInstanceTypePreferences := cluster.ProviderType == db.ProviderTypeNico && req.Nico != nil && req.Nico.InstanceTypePreference != nil
	if hasInstanceTypePreferences {
		if err := nicoclient.ValidateInstanceTypePreferenceIDs(req.Nico.InstanceTypePreference); err != nil {
			return newBadRequestErrorf("%s", err)
		}
		nicoProvider, err := controlPlaneConfigSpec.NicoProvider()
		if err != nil {
			return err
		}
		nicoProvider.InstanceTypePreference = dedupeUUIDsPreserveOrder(
			uuidSliceFromAPI(req.Nico.InstanceTypePreference),
		)
		controlPlaneConfigSpec.SetNico(nicoProvider)
	}

	if hasClusterLevelControlPlaneUpdate(req) {
		if err := s.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, req, &controlPlaneConfigSpec); err != nil {
			return err
		}
	}

	// A PATCH must not clear a required field or persist a spec that create would
	// reject (Forge instanceTypePreference/release or the CAPI
	// capiInfraMachineTemplate). instanceTypePreference is merged above and is not
	// covered by hasClusterLevelControlPlaneUpdate, so gate on it explicitly;
	// maxSurge- and k8s-version-only updates leave these fields untouched.
	if hasClusterLevelControlPlaneUpdate(req) || hasInstanceTypePreferences {
		if err := validateMergedClusterControlPlaneSpec(*cluster, controlPlaneConfigSpec); err != nil {
			return err
		}
	}
	if err := s.validateControlPlaneRollout(
		cluster.ControlPlaneVIP,
		db.IsControlPlaneRolloutRequired(cluster.ControlPlaneConfig.ControlPlaneConfigSpec, controlPlaneConfigSpec),
	); err != nil {
		return err
	}

	// Diff the entire spec against the current row and create a new row only
	// when the effective control-plane spec actually changed.
	if err := db.CreateControlPlaneConfigRowIfChanged(ctx, tx, cluster, controlPlaneConfigSpec, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "cluster PATCH controlPlaneConfig",
	}); err != nil {
		return fmt.Errorf("create control plane configuration spec row: %w", err)
	}
	return nil
}

// hasClusterLevelControlPlaneUpdate reports whether the update touches any of the
// cluster-level control plane fields beyond the maxSurge /
// provisioningMonitorDisabled knobs.
//
// Provider fields are read directly from the nested payloads.
func hasClusterLevelControlPlaneUpdate(req *api.UpdateClusterControlPlaneConfig) bool {
	if req.NodeAgentConfig != nil ||
		req.DesiredCount != nil ||
		req.SystemReservedMemory != nil ||
		req.TeleportEnabled != nil ||
		req.Infra != nil {
		return true
	}
	return hasNestedNicoControlPlanePatch(req.Nico)
}

func hasNestedNicoControlPlanePatch(n *api.NicoControlPlaneProviderUpdate) bool {
	if n == nil {
		return false
	}
	// instanceTypePreference is applied in the parent and does not require
	// control-plane-controller mode by itself.
	return n.SubnetIDs != nil ||
		n.VpcPrefixIDs != nil ||
		n.SSHKeyGroupIDs != nil ||
		n.Release != nil ||
		n.ConfigProfile != nil
}

// applyClusterLevelControlPlaneSpecUpdate merges the supplied cluster-level control
// plane fields onto spec and desiredCount onto the cluster. desiredCount may go up
// or down; node teardown on scale-down is the control-plane controller's job.
//
// The caller (UpdateCluster) has already rejected a req.Nico/req.Capi that doesn't
// match cluster.ProviderType, so this can assume req carries only the provider the
// cluster actually has.
func (s *clusterService) applyClusterLevelControlPlaneSpecUpdate(
	_ context.Context,
	cluster *db.Cluster,
	req *api.UpdateClusterControlPlaneConfig,
	spec *db.ControlPlaneConfigSpec,
) error {
	if req.DesiredCount != nil {
		if *req.DesiredCount < 1 {
			return newBadRequestErrorf("controlPlaneConfig.desiredCount must be at least 1")
		}
		if err := s.validateControlPlaneCapacityChange(
			cluster.ControlPlaneVIP,
			cluster.ControlPlaneConfig.DesiredCount,
			*req.DesiredCount,
		); err != nil {
			return err
		}
		if *req.DesiredCount > cluster.ControlPlaneConfig.DesiredCount {
			if err := validateControlPlaneNodeAgentProfile(s.nodeAgentProfile, *cluster, *spec); err != nil {
				return newBadRequestErrorf("%s", err)
			}
		}
		cluster.ControlPlaneConfig.DesiredCount = *req.DesiredCount
	}

	if req.NodeAgentConfig != nil && req.NodeAgentConfig.Version != nil {
		version, err := canonicalNodeAgentVersion(*req.NodeAgentConfig.Version)
		if err != nil {
			return newBadRequestErrorf("%s", err)
		}
		if version != "" && s.nodeAgentUpdateMethod == "" {
			return newBadRequestErrorf("nodeAgent.updateMethod is required for agent convergence")
		}
		spec.NodeAgentConfig.Version = version
	}
	if req.SystemReservedMemory != nil {
		spec.SystemReservedMemory = *req.SystemReservedMemory
	}
	if req.TeleportEnabled != nil {
		spec.TeleportEnabled = *req.TeleportEnabled
	}

	switch {
	case req.Nico != nil:
		nico, err := spec.NicoProvider()
		if err != nil {
			return err
		}
		// Preference is also applied in the parent before this helper runs; re-apply
		// here so cloning an existing Nico JSONB does not restore a cleared list.
		if req.Nico.InstanceTypePreference != nil {
			nico.InstanceTypePreference = dedupeUUIDsPreserveOrder(
				uuidSliceFromAPI(req.Nico.InstanceTypePreference),
			)
		}
		if req.Nico.SubnetIDs != nil {
			nico.SubnetIDs = uuidSliceDedupe(uuidSliceFromAPI(req.Nico.SubnetIDs))
		}
		if req.Nico.VpcPrefixIDs != nil {
			nico.VpcPrefixIDs = uuidSliceDedupe(uuidSliceFromAPI(req.Nico.VpcPrefixIDs))
		}
		if req.Nico.Release != nil {
			nico.Release = *req.Nico.Release
		}
		if req.Nico.ConfigProfile != nil {
			nico.ConfigProfile = *req.Nico.ConfigProfile
		}
		if req.Nico.SSHKeyGroupIDs != nil {
			nico.SSHKeyGroupIDs = uuidSliceDedupe(uuidSliceFromAPI(req.Nico.SSHKeyGroupIDs))
		}
		spec.SetNico(nico)

		if req.Nico.SubnetIDs != nil || req.Nico.VpcPrefixIDs != nil {
			if err := validateControlPlaneNicoNetworking(nico.SubnetIDs, nico.VpcPrefixIDs); err != nil {
				return err
			}
		}
	case req.Infra != nil:
		capi, err := spec.CAPIProvider()
		if err != nil {
			return err
		}
		if req.Infra.BootstrapFiles != nil {
			capi.BootstrapFiles = dbCloudInitFilesFromAPI(req.Infra.BootstrapFiles)
		}
		if req.Infra.PreKubeadmCommands != nil {
			capi.PreKubeadmCommands = append([]string(nil), req.Infra.PreKubeadmCommands...)
		}
		if req.Infra.MachineTemplate != nil {
			if isEmptyManifestObject(req.Infra.MachineTemplate) {
				capi.InfraMachineTemplatePreferences = nil
			} else {
				prefs, err := infraMachineTemplatePrefsFromAPI(req.Infra.MachineTemplate)
				if err != nil {
					return newBadRequestErrorf("failed to convert infra.machineTemplate: %v", err)
				}
				if err := capiclient.ValidateInfrastructureManifestBytes(prefs[0]); err != nil {
					return newBadRequestErrorf("invalid infra.machineTemplate: %s", err)
				}
				capi.InfraMachineTemplatePreferences = prefs
			}
		}
		spec.SetCapi(capi)
	case req.LaunchLayer != nil:
		// No-op: every LaunchLayer control-plane field is fixed at creation,
		// so launchLayer has no updatable fields (see
		// LaunchLayerControlPlaneProviderUpdate in openapi.yaml) —
		// req.LaunchLayer being non-nil just means the caller sent an empty
		// launchLayer object, which changes nothing.
	}
	return nil
}

func uuidSliceFromAPI(ids []openapi_types.UUID) []uuid.UUID {
	return append([]uuid.UUID(nil), ids...)
}

// dedupeUUIDsPreserveOrder removes duplicate UUIDs while preserving first-seen order, skipping uuid.Nil.
// Used for instanceTypePreference priority list on cluster update.
func dedupeUUIDsPreserveOrder(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
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

func apiControlPlaneConfigFromDB(cfg db.ControlPlaneConfig) *api.ControlPlaneConfig {
	maxSurge := cfg.MaxSurge
	autoScaleDownDisabled := cfg.AutoScaleDownDisabled
	provisioningMonitorDisabled := cfg.ProvisioningMonitorDisabled
	teleportEnabled := cfg.TeleportEnabled
	return &api.ControlPlaneConfig{
		ConfigurationID:             cfg.ConfigurationID,
		DesiredCount:                cfg.DesiredCount,
		MaxSurge:                    &maxSurge,
		AutoScaleDownDisabled:       &autoScaleDownDisabled,
		ProvisioningMonitorDisabled: &provisioningMonitorDisabled,
		SystemReservedMemory:        cfg.SystemReservedMemory,
		NodeLabels:                  maps.Clone(cfg.NodeLabels),
		TeleportEnabled:             &teleportEnabled,
		Nico:                        apiNicoControlPlaneProviderFromDB(cfg.Nico),
		Infra:                       apiInfraControlPlaneProviderFromDB(cfg.Capi),
		NodeAgentConfig:             apiNodeAgentConfigFromDB(cfg.NodeAgentConfig),
		MachineAnnotations:          maps.Clone(cfg.MachineAnnotations),
		MachineLabels:               maps.Clone(cfg.MachineLabels),
		LaunchLayer:                 apiLaunchLayerControlPlaneProviderFromDB(cfg.LaunchLayer),
	}
}

func validateEtcdEncryptionCanStart(cluster db.Cluster) error {
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return errEtcdEncryptionDuringDeletion
	}
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
		return errEtcdEncryptionRequiresProvisionedCluster
	}
	if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeK8sUpgrade,
			startedAt: cluster.Conditions.ControlPlaneUpgrading.LastTransitionTime,
			message:   cluster.Conditions.ControlPlaneUpgrading.Message,
		}
	}
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeCPNodesRecycling,
			startedAt: cluster.Conditions.CPNodesRecycling.LastTransitionTime,
			message:   cluster.Conditions.CPNodesRecycling.Message,
		}
	}
	return nil
}

// enqueueOIDCEnablement handles the enableOidc toggle inside the UpdateCluster
// transaction: one-way, provisioned-only, serialized through the UpdateInProgress
// lock. When OIDC is already enabled the toggle is a no-op; any other fields in
// the same request are still persisted by the caller's tx.UpdateCluster. The
// workflow's final MarkOIDCEnabled step flips OIDCAuthEnabled, which the
// tenant-access controller reconciles into the published kubeconfig + RBAC.
func enqueueOIDCEnablement(ctx context.Context, tx db.Tx, req *api.UpdateClusterRequest, cluster *db.Cluster) error {
	if req.EnableOidc == nil || !*req.EnableOidc {
		return nil
	}
	if cluster.OIDCAuthEnabled {
		return nil
	}
	if err := validateOIDCEnableCanStart(*cluster); err != nil {
		return err
	}

	db.ClearUpdateFailed(cluster)
	db.SetUpdateInProgress(cluster, db.OperationTypeOIDCEnable, "enabling OIDC authentication", time.Now().UTC())
	workflowID := constants.EnableOIDCWorkflowPrefix + cluster.ID.String()
	if err := enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
		WorkflowType: constants.EnableOIDCWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
	}, map[string]interface{}{
		clusterIDConst: cluster.ID,
	}); err != nil {
		return fmt.Errorf("failed to create workflow outbox entry for OIDC enablement: %w", err)
	}
	return nil
}

// validateOIDCEnableCanStart checks whether OIDC enablement can begin. It is a
// control-plane rollout, so it is refused during deletion, before the cluster is
// provisioned, and while another control-plane operation (k8s upgrade or CP-node
// recycling) holds its legacy condition flag.
func validateOIDCEnableCanStart(cluster db.Cluster) error {
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return errOIDCDuringDeletion
	}
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
		return errOIDCRequiresProvisionedCluster
	}
	if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeK8sUpgrade,
			startedAt: cluster.Conditions.ControlPlaneUpgrading.LastTransitionTime,
			message:   cluster.Conditions.ControlPlaneUpgrading.Message,
		}
	}
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeCPNodesRecycling,
			startedAt: cluster.Conditions.CPNodesRecycling.LastTransitionTime,
			message:   cluster.Conditions.CPNodesRecycling.Message,
		}
	}
	return nil
}

func (s *clusterService) DeleteCluster(ctx context.Context, req api.DeleteClusterRequestObject) (api.DeleteClusterResponseObject, error) {
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		dbCluster, err := tx.GetClusterForUpdate(ctx, req.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		// Cluster deletion stops controller-side agent upgrades.
		dbCluster.Conditions.Deleting = db.NewCondition(true, "Cluster deletion is pending")

		if _, err := tx.UpdateCluster(ctx, &dbCluster); err != nil {
			return fmt.Errorf("failed to update cluster: %w", err)
		}

		workflowID := constants.TeardownClusterWorkflowPrefix + dbCluster.ID.String()
		err = enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
			WorkflowType: "TeardownCluster",
			WorkflowID:   workflowID,
			ClusterID:    dbCluster.ID,
			ClusterName:  dbCluster.Name,
		}, map[string]interface{}{
			clusterIDConst: dbCluster.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry: %w", err)
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.DeleteCluster404Response{}, nil
		}
		return nil, err
	}

	return api.DeleteCluster202Response{}, nil
}

func APIClusterFromDB(dbCluster *db.Cluster) api.Cluster {
	var controlPlaneVIP *string
	if dbCluster.ControlPlaneVIP != "" {
		controlPlaneVIP = &dbCluster.ControlPlaneVIP
	}
	cpNodesRecycling := apiConditionFromDB(dbCluster.Conditions.CPNodesRecycling)
	controlPlaneUpgrading := apiConditionFromDB(dbCluster.Conditions.ControlPlaneUpgrading)

	conditions := api.ClusterConditions{
		Provisioning:          apiConditionFromDB(dbCluster.Conditions.Provisioning),
		Provisioned:           apiConditionFromDB(dbCluster.Conditions.Provisioned),
		Deleting:              apiConditionFromDB(dbCluster.Conditions.Deleting),
		ControlPlaneUpgrading: &controlPlaneUpgrading,
		CpNodesRecycling:      &cpNodesRecycling,
	}
	if uip := dbCluster.Conditions.UpdateInProgress; uip != nil {
		apiUIP := api.UpdateInProgress{
			Type:      api.UpdateInProgressType(uip.Type),
			StartedAt: uip.StartedAt,
		}
		if uip.Message != "" {
			msg := uip.Message
			apiUIP.Message = &msg
		}
		conditions.UpdateInProgress = &apiUIP
	}
	if dbCluster.Conditions.UpdateFailed != nil {
		updateFailed := apiConditionFromDB(*dbCluster.Conditions.UpdateFailed)
		conditions.UpdateFailed = &updateFailed
	}
	// Project the recorded cycle separately from UpdateInProgress: a failed cycle
	// can still own the mutation lock. Empty optional diagnostics remain absent;
	// overdue status is derived by consumers from NextCertRenewalDue.
	if status := dbCluster.Conditions.CertificateRenewal; status != nil {
		apiStatus := api.CertificateRenewalStatus{
			CycleID:      status.CycleID,
			State:        api.CertificateRenewalStatusState(status.State),
			Forced:       status.Forced,
			ScheduledFor: status.ScheduledFor,
			StartedAt:    status.StartedAt,
			CompletedAt:  status.CompletedAt,
			FailedNodeID: status.FailedNodeID,
		}
		if status.Outcome != "" {
			outcome := api.CertificateRenewalStatusOutcome(status.Outcome)
			apiStatus.Outcome = &outcome
		}
		if status.Phase != "" {
			phase := status.Phase
			apiStatus.Phase = &phase
		}
		if status.Message != "" {
			message := status.Message
			apiStatus.Message = &message
		}
		conditions.CertificateRenewal = &apiStatus
	}

	if status := dbCluster.Conditions.EtcdKeyRotation; status != nil {
		apiStatus := api.EtcdKeyRotationStatus{
			WorkflowID:     status.WorkflowID,
			State:          api.EtcdKeyRotationStatusState(status.State),
			Phase:          api.EtcdKeyRotationStatusPhase(status.Phase),
			StartedAt:      status.StartedAt,
			LastProgressAt: status.LastProgressAt,
			CompletedAt:    status.CompletedAt,
		}
		if status.FailureReason != "" {
			reason := api.EtcdKeyRotationStatusFailureReason(status.FailureReason)
			apiStatus.FailureReason = &reason
		}
		conditions.EtcdKeyRotation = &apiStatus
	}

	return api.Cluster{
		ID:                            dbCluster.ID,
		Name:                          dbCluster.Name,
		CniPlugin:                     string(dbCluster.CniPlugin),
		CniVersion:                    db.EffectiveCNIVersion(dbCluster.CniPlugin, strings.TrimSpace(dbCluster.CniVersion)),
		EtcdEncrypted:                 dbCluster.EtcdEncrypted,
		ControlPlaneKubernetesVersion: dbCluster.KubernetesVersion,
		ControlPlaneVIP:               controlPlaneVIP,
		EnableCPNodeRecycling:         &dbCluster.EnableCPNodeRecycling,
		TeleportEnabled:               &dbCluster.TeleportEnabled,
		ControlPlaneConfig:            apiControlPlaneConfigFromDB(dbCluster.ControlPlaneConfig),
		Network: api.ClusterNetwork{
			PodCIDR:     dbCluster.PodCIDR,
			ServiceCIDR: dbCluster.ServiceCIDR,
		},
		CreatedAt:          dbCluster.CreatedAt,
		NextCertRenewalDue: dbCluster.NextCertRenewalDue,

		Nico:        apiNicoClusterProviderFromDB(dbCluster.Nico),
		Infra:       apiInfraClusterProviderFromDB(dbCluster.Capi),
		LaunchLayer: apiLaunchLayerClusterProviderFromDB(dbCluster.LaunchLayer),

		Conditions:       conditions,
		NodeAgentUpgrade: apiNodeAgentUpgradeFromDB(dbCluster.NodeAgentUpgrade),
	}
}

func apiLaunchLayerClusterProviderFromDB(provider *db.LaunchLayerClusterProvider) *api.LaunchLayerClusterProvider {
	if provider == nil {
		return nil
	}
	out := &api.LaunchLayerClusterProvider{
		VpcID:                provider.VpcID,
		OrgID:                provider.OrgID,
		SiteID:               provider.SiteID,
		CloudProviderEnabled: provider.CloudProviderEnabled,
	}
	if provider.VpcType != "" {
		vpcType := api.LaunchLayerClusterProviderVpcType(provider.VpcType)
		out.VpcType = &vpcType
	}
	if provider.Owner.UserID != "" || provider.Owner.Username != "" {
		out.Owner = &api.LaunchLayerClusterOwner{
			UserID:   provider.Owner.UserID,
			Username: provider.Owner.Username,
		}
	}
	if len(provider.SecurityGroups) > 0 {
		groups := make([]api.LaunchLayerSecurityGroup, 0, len(provider.SecurityGroups))
		for _, g := range provider.SecurityGroups {
			group := api.LaunchLayerSecurityGroup{Name: g.Name, Type: api.LaunchLayerSecurityGroupType(g.Type)}
			if g.NetworkType != "" {
				networkType := api.LaunchLayerSecurityGroupNetworkType(g.NetworkType)
				group.NetworkType = &networkType
			}
			groups = append(groups, group)
		}
		out.SecurityGroups = &groups
	}
	return out
}

func apiNicoClusterProviderFromDB(nicoClusterProvider *db.NicoClusterProvider) *api.NicoClusterProvider {
	if nicoClusterProvider == nil {
		return nil
	}
	return &api.NicoClusterProvider{
		OrgID:  nicoClusterProvider.OrgID,
		SiteID: nicoClusterProvider.SiteID,
		VpcID:  nicoClusterProvider.VpcID,
	}
}

func apiInfraClusterProviderFromDB(capiClusterProvider *db.CAPIClusterProvider) *api.InfraClusterProvider {
	if capiClusterProvider == nil {
		return nil
	}
	capiCluster, err := objectFromRawManifest(capiClusterProvider.InfraCluster)
	if err != nil {
		capiCluster = nil
	}
	return &api.InfraClusterProvider{Cluster: capiCluster}
}

func DBClusterFromAPI(req *api.CreateClusterRequest) (db.Cluster, error) {
	var teleportEnabled bool
	if req.TeleportEnabled != nil {
		teleportEnabled = *req.TeleportEnabled
	}
	cniVersion := ""
	if req.CniVersion != nil {
		cniVersion = strings.TrimSpace(*req.CniVersion)
	}
	cniPlugin := db.EffectiveCNIPlugin(db.CniPlugin(req.CniPlugin))
	controlPlaneVIP := strings.TrimSpace(req.ControlPlaneVIP)

	cluster := db.Cluster{
		Name:              req.Name,
		CniPlugin:         cniPlugin,
		CniVersion:        db.EffectiveCNIVersion(cniPlugin, cniVersion),
		EtcdEncrypted:     req.EtcdEncrypted,
		KubernetesVersion: req.ControlPlaneKubernetesVersion,
		ControlPlaneVIP:   controlPlaneVIP,
		TeleportEnabled:   teleportEnabled,

		Conditions: db.ClusterConditions{
			Provisioning:          db.NewCondition(true, "Starting provisioning"),
			Provisioned:           db.NewCondition(false, "Not yet provisioned"),
			Deleting:              db.NewCondition(false, "Cluster is not being deleted"),
			ControlPlaneUpgrading: db.NewCondition(false, "No upgrade in progress"),
		},
	}

	switch {
	case req.Nico != nil:
		cluster.SetNicoProvider(db.NicoClusterProvider{
			OrgID:  req.Nico.OrgID,
			SiteID: req.Nico.SiteID,
			VpcID:  req.Nico.VpcID,
		})
	case req.Infra != nil:
		capiCluster, err := rawManifestFromObject(req.Infra.Cluster)
		if err != nil {
			return db.Cluster{}, err
		}
		cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: capiCluster})
	case req.LaunchLayer != nil:
		cluster.SetLaunchLayerProvider(dbLaunchLayerClusterProviderFromAPI(*req.LaunchLayer))
	}

	return cluster, nil
}

func dbLaunchLayerClusterProviderFromAPI(provider api.LaunchLayerClusterProvider) db.LaunchLayerClusterProvider {
	out := db.LaunchLayerClusterProvider{
		VpcID:                provider.VpcID,
		OrgID:                provider.OrgID,
		SiteID:               provider.SiteID,
		CloudProviderEnabled: provider.CloudProviderEnabled,
	}
	if provider.VpcType != nil {
		out.VpcType = string(*provider.VpcType)
	}
	if provider.Owner != nil {
		out.Owner = db.LaunchLayerClusterOwner{
			UserID:   provider.Owner.UserID,
			Username: provider.Owner.Username,
		}
	}
	if provider.SecurityGroups != nil {
		groups := make([]db.LaunchLayerSecurityGroup, 0, len(*provider.SecurityGroups))
		for _, g := range *provider.SecurityGroups {
			group := db.LaunchLayerSecurityGroup{Name: g.Name, Type: string(g.Type)}
			if g.NetworkType != nil {
				group.NetworkType = string(*g.NetworkType)
			}
			groups = append(groups, group)
		}
		out.SecurityGroups = groups
	}
	return out
}

func ValidateKubernetesVersionSkewWithControlPlane(currentVersion, newVersion string) error {
	currentMajor, currentMinor, currentPatch, err := parseKubernetesVersion(currentVersion)
	if err != nil {
		return fmt.Errorf("%w: current version: %w", errVersionSkewViolation, err)
	}
	newMajor, newMinor, newPatch, err := parseKubernetesVersion(newVersion)
	if err != nil {
		return fmt.Errorf("%w: new version: %w", errVersionSkewViolation, err)
	}

	// Don't allow downgrade - check major, minor, and patch versions
	if newMajor < currentMajor {
		return fmt.Errorf("%w: kubernetes version downgrade not supported (current: %s, requested: %s)",
			errVersionSkewViolation, currentVersion, newVersion)
	}
	if newMajor == currentMajor && newMinor < currentMinor {
		return fmt.Errorf("%w: kubernetes version downgrade not supported (current: %s, requested: %s)",
			errVersionSkewViolation, currentVersion, newVersion)
	}
	if newMajor == currentMajor && newMinor == currentMinor && newPatch < currentPatch {
		return fmt.Errorf("%w: kubernetes version downgrade not supported (current: %s, requested: %s)",
			errVersionSkewViolation, currentVersion, newVersion)
	}
	if newMajor == currentMajor && newMinor == currentMinor && newPatch == currentPatch {
		return fmt.Errorf("%w: requested kubernetes version must be greater than the current version (current: %s, requested: %s)",
			errVersionSkewViolation, currentVersion, newVersion)
	}

	// Kubernetes version skew policy: control plane can be at most 1 minor version ahead
	versionDiff := newMinor - currentMinor
	if newMajor == currentMajor && versionDiff > 1 {
		return fmt.Errorf("%w: control plane can only be upgraded 1 minor version at a time (current: %s, requested: %s)",
			errVersionSkewViolation, currentVersion, newVersion)
	}

	return nil
}

// ValidateKubernetesVersionSkewWithWorkerNodes ensures the requested control plane version
// does not violate Kubernetes version skew with existing worker node pools. Kubelet must not
// be more than 3 minor versions behind the control plane (allowed skew: N-3).
func ValidateKubernetesVersionSkewWithWorkerNodes(ctx context.Context, tx db.Tx, clusterID uuid.UUID, newControlPlaneVersion string) error {
	controlPlaneFalse := false
	nodePools, err := tx.ListNodePools(ctx, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlaneFalse,
	})
	if err != nil {
		return fmt.Errorf("listing worker node pools: %w", err)
	}

	newMajor, newMinor, _, err := parseKubernetesVersion(newControlPlaneVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", errVersionSkewViolation, err)
	}

	for _, np := range nodePools {
		if np.KubernetesVersion == "" {
			continue
		}
		poolMajor, poolMinor, _, err := parseKubernetesVersion(np.KubernetesVersion)
		if err != nil {
			return fmt.Errorf("%w: node pool %q has invalid kubernetes_version %q: %w", errVersionSkewViolation, np.Name, np.KubernetesVersion, err)
		}
		// Different major: reject (e.g. CP 2.0 with worker 1.30 is invalid).
		if newMajor != poolMajor {
			return fmt.Errorf("%w: worker node pool %q has kubernetes_version %s (major %d) which does not match requested control plane major version %d",
				errVersionSkewViolation, np.Name, np.KubernetesVersion, poolMajor, newMajor)
		}
		// Same major: worker must not be more than 3 minor versions behind the new control plane (N-3 skew).
		if poolMinor < newMinor-3 {
			return fmt.Errorf("%w: worker node pool %q has kubernetes_version %s which is more than 3 minor versions behind requested control plane version %s (kubelet skew policy N-3)",
				errVersionSkewViolation, np.Name, np.KubernetesVersion, newControlPlaneVersion)
		}
	}
	return nil
}

// parseKubernetesVersion parses a "major.minor.patch" version string (optional "v" prefix).
// Returns major, minor, patch and an error if the format is invalid.
func parseKubernetesVersion(version string) (int, int, int, error) {
	var major, minor, patch int
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid version format (expected major.minor.patch): %s", version)
	}
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, 0, 0, fmt.Errorf("invalid major version: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		return 0, 0, 0, fmt.Errorf("invalid minor version: %w", err)
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &patch); err != nil {
		return 0, 0, 0, fmt.Errorf("invalid patch version: %w", err)
	}
	return major, minor, patch, nil
}

func apiNodeAgentUpgradeFromDB(upgrade *db.NodeAgentUpgrade) *api.NodeAgentUpgradeStatus {
	if upgrade == nil {
		return nil
	}
	phase := api.NodeAgentUpgradeStatusPhase("Running")
	if upgrade.Phase == "Pending" {
		phase = "Pending"
	}
	return &api.NodeAgentUpgradeStatus{NodeID: upgrade.NodeID, TargetVersion: upgrade.TargetVersion, Method: upgrade.Method, Phase: phase, ErrorMessage: upgrade.ErrorMessage}
}

func apiNodeAgentConfigFromDB(config db.NodeAgentConfig) *api.NodeAgentConfig {
	if config.Version == "" {
		return nil
	}
	return &api.NodeAgentConfig{Version: &config.Version}
}

func agentUpgradeBlocksMaintenance(cluster db.Cluster, request *api.UpdateClusterJSONRequestBody) bool {
	return cluster.NodeAgentUpgrade != nil && (request.EnableEtcdEncryption != nil && *request.EnableEtcdEncryption || request.EnableOidc != nil && *request.EnableOidc)
}

func updateClusterConflictResponse(err error) api.UpdateClusterResponseObject {
	if errors.Is(err, errAgentUpgradeInProgress) {
		return api.UpdateCluster409JSONResponse{Message: err.Error()}
	}
	var uipErr *updateInProgressError
	if errors.As(err, &uipErr) {
		return api.UpdateCluster409JSONResponse{Message: uipErr.Error()}
	}
	return nil
}
