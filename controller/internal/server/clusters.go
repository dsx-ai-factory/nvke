// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/naming"
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
	errControlPlaneConfigNotEnabled             = errors.New("cluster-level controlPlaneConfig requires control-plane controller provisioning to be enabled; configure the control plane via a control-plane node pool")
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
	return msg
}

type clusterService struct {
	logger          *slog.Logger
	db              db.DB
	nodePoolService *nodePoolService
	teleportCfg     config.TeleportConfig
	oidcAuthEnabled bool
	providers       providerSupport
	// useControlPlaneController mirrors the CWE feature flag
	// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER. When true, the
	// cluster-level controlPlaneConfig is authoritative: it is required on
	// create, control-plane node pools are rejected, and the config is persisted
	// as cluster-level desiredCount + a control plane config snapshot. When
	// false, the control plane is provisioned from a control-plane node pool and
	// supplying controlPlaneConfig.desiredCount/spec is rejected.
	useControlPlaneController bool
}

func NewClusterService(
	logger *slog.Logger,
	database db.DB,
	nodePoolService *nodePoolService,
	teleportCfg config.TeleportConfig,
	oidcAuthEnabled bool,
	supportedInfraProviders []string,
	useControlPlaneController bool,
) *clusterService {
	return &clusterService{
		logger:                    logger,
		db:                        database,
		nodePoolService:           nodePoolService,
		teleportCfg:               teleportCfg,
		oidcAuthEnabled:           oidcAuthEnabled,
		providers:                 newProviderSupport(supportedInfraProviders),
		useControlPlaneController: useControlPlaneController,
	}
}

// applyClusterDefaults fills in server-side defaults for omitted cluster fields.
func (s *clusterService) applyClusterDefaults(req *api.CreateClusterRequest, cluster *db.Cluster) error {
	cluster.OIDCAuthEnabled = s.oidcAuthEnabled

	if req.TeleportEnabled == nil {
		cluster.TeleportEnabled = s.teleportCfg.DefaultClusterEnabled
	}

	if cluster.SiteID == uuid.Nil || !capi.IsEmptyManifest(cluster.CapiInfraCluster) {
		return nil
	}

	manifest, err := forgeCAPINicoClusterManifest(cluster.SiteID)
	if err != nil {
		return fmt.Errorf("failed to build default Forge CAPI NicoCluster manifest: %w", err)
	}
	cluster.CapiInfraCluster = manifest
	return nil
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

		// In control-plane controller provisioning mode the cluster-level control
		// plane config is authoritative. Persist the desired count and max surge as
		// columns up front; the config snapshot row is created after the cluster row
		// exists (it needs the cluster ID).
		if s.useControlPlaneController && req.Body.ControlPlaneConfig != nil {
			dbCluster.ControlPlaneConfig.DesiredCount = req.Body.ControlPlaneConfig.DesiredCount
			if req.Body.ControlPlaneConfig.MaxSurge != nil {
				dbCluster.ControlPlaneConfig.MaxSurge = *req.Body.ControlPlaneConfig.MaxSurge
			}
		}

		createdCluster, err = tx.CreateCluster(ctx, &dbCluster)
		if err != nil {
			return fmt.Errorf("failed to create cluster: %w", err)
		}

		if s.useControlPlaneController && req.Body.ControlPlaneConfig != nil {
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
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "ProvisionCluster", workflowID, map[string]interface{}{
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
			return api.CreateCluster400JSONResponse{
				Message: fmt.Sprintf("A cluster already exists with the name %q", req.Body.Name),
			}, nil
		}
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
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
		return resolvedClusterNetwork{}, newBadRequestErrorf("%s", err)
	}
	desiredNodes := 0
	if req.NodePools != nil {
		for _, nodePool := range *req.NodePools {
			desiredNodes += nodePool.DesiredNodeCount
		}
	}
	cpSurge := 0
	// Include the cluster-level control plane node count (and its surge) in the
	// pod-CIDR capacity check when the control plane is provisioned from the
	// cluster-level controlPlaneConfig rather than a node pool.
	if s.useControlPlaneController && req.ControlPlaneConfig != nil {
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
	if s.useControlPlaneController && req.ControlPlaneConfig != nil && req.Nico != nil {
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

func forgeCAPINicoClusterManifest(siteID uuid.UUID) ([]byte, error) {
	nicoCluster := nicov1.NicoCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoCluster",
		},
		Spec: nicov1.NicoClusterSpec{
			SiteID: siteID.String(),
		},
	}

	return json.Marshal(nicoCluster)
}

func (s *clusterService) ValidateCreateClusterRequestFields(req *api.CreateClusterRequest) error {
	if err := naming.ValidateClusterNameForNodeNaming(req.Name); err != nil {
		return err
	}
	if len(req.Name) > capi.ClusterNameMaxLength {
		return fmt.Errorf(
			"cluster name must be %d characters or fewer so it can be used as a cluster namespace name",
			capi.ClusterNameMaxLength,
		)
	}

	var nodePools []api.NodePoolCreateConfig
	if req.NodePools != nil {
		nodePools = *req.NodePools
	}

	var providerType db.ProviderType
	switch {
	case req.Infra != nil && req.Nico != nil:
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
	default:
		return fmt.Errorf("exactly one provider must be set")
	}

	if s.useControlPlaneController {
		// Cluster-level control plane config is authoritative: control-plane node
		// pools are rejected and the control plane is described by controlPlaneConfig.
		if err := validateCreateClusterLevelControlPlane(req, nodePools, providerType); err != nil {
			return err
		}
	} else {
		// Legacy mode: the control plane is provided via a control-plane node pool,
		// and a cluster-level controlPlaneConfig is not accepted.
		if req.ControlPlaneConfig != nil {
			return errControlPlaneConfigNotEnabled
		}
		controlPlaneNodeCount := 0
		for _, nodePool := range nodePools {
			if nodePool.ControlPlane {
				controlPlaneNodeCount += nodePool.DesiredNodeCount
			}
		}
		if controlPlaneNodeCount == 0 {
			return fmt.Errorf("cluster must have at least one control plane node")
		}
	}

	dbCniPlugin := db.CniPlugin(req.CniPlugin)
	if !slices.Contains(db.ListCNIPlugins(), dbCniPlugin) {
		return fmt.Errorf("invalid CNI plugin: %s, valid plugins: %v", req.CniPlugin, db.ListCNIPlugins())
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
	if err := validateCAPIInfrastructureManifest(req.Infra.Cluster); err != nil {
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
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "ProvisionNodePool", nodePoolWorkflowID, map[string]interface{}{
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
		// Reject a controlPlaneConfig provider block that doesn't match the cluster before
		// any field is touched.
		if cpConfig := req.Body.ControlPlaneConfig; cpConfig != nil {
			if dbCluster.ProviderType == db.ProviderTypeNico && cpConfig.Infra != nil {
				return newBadRequestErrorf("controlPlaneConfig.infra is not supported, please provide controlPlaneConfig.nico")
			}
			if dbCluster.ProviderType == db.ProviderTypeCAPI && cpConfig.Nico != nil {
				return newBadRequestErrorf("controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
			}
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
				if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.EnableEtcdEncryptionWorkflowType, workflowID, map[string]interface{}{
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
		var uipErr *updateInProgressError
		if errors.As(err, &uipErr) {
			return api.UpdateCluster409JSONResponse{Message: uipErr.Error()}, nil
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
		errors.Is(err, errClusterMaxSurgeTooHigh) ||
		errors.Is(err, errControlPlaneConfigNotEnabled)
}

func hasNonEtcdEncryptionMutation(req *api.UpdateClusterRequest) bool {
	return req.EnableCPNodeRecycling != nil ||
		req.TeleportEnabled != nil ||
		(req.ControlPlaneKubernetesVersion != nil && *req.ControlPlaneKubernetesVersion != "") ||
		req.ControlPlaneConfig != nil
}

// podCIDRNodePoolsWithClusterCP returns nodePools augmented, when control-plane
// controller provisioning is enabled, with a synthetic control-plane pool that
// carries the cluster-level ControlPlaneConfig.DesiredCount. In that mode the
// control plane is not a node pool, so this is how its nodes enter the pod-CIDR
// capacity accounting. CP MaxSurge is supplied separately as the clusterMaxSurge
// argument to the capacity helpers, so the synthetic pool leaves MaxSurge zero to
// avoid double-counting.
func podCIDRNodePoolsWithClusterCP(useControlPlaneController bool, cluster db.Cluster, nodePools []db.NodePool) []db.NodePool {
	if !useControlPlaneController {
		return nodePools
	}
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
	currentNodePools := podCIDRNodePoolsWithClusterCP(s.useControlPlaneController, *cluster, nodePools)
	previousNodePools := podCIDRNodePoolsWithClusterCP(s.useControlPlaneController, previousCluster, nodePools)
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

	controlPlaneConfigSpec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
	// KubernetesVersion lives in the spec row and should track the current
	// cluster control-plane version selected in this PATCH.
	controlPlaneConfigSpec.KubernetesVersion = cluster.KubernetesVersion
	if req.ProvisioningMonitorDisabled != nil {
		controlPlaneConfigSpec.ProvisioningMonitorDisabled = *req.ProvisioningMonitorDisabled
	}
	hasInstanceTypePreferences := cluster.ProviderType == db.ProviderTypeNico && req.Nico != nil && req.Nico.InstanceTypePreference != nil
	if hasInstanceTypePreferences {
		if err := validateInstanceTypePreferenceIDs(req.Nico.InstanceTypePreference); err != nil {
			return newBadRequestErrorf("%s", err)
		}
		nico, err := controlPlaneConfigSpec.NicoProvider()
		if err != nil {
			return err
		}
		nico.InstanceTypePreference = dedupeUUIDsPreserveOrder(
			uuidSliceFromAPI(req.Nico.InstanceTypePreference),
		)
		controlPlaneConfigSpec.SetNico(nico)
	}

	// desiredCount and the full control-plane spec are only honored in
	// control-plane controller provisioning mode. In legacy mode the control
	// plane is defined by control-plane node pools, so these updates are rejected.
	if hasClusterLevelControlPlaneUpdate(req) {
		if !s.useControlPlaneController {
			return errControlPlaneConfigNotEnabled
		}
		if err := s.applyClusterLevelControlPlaneSpecUpdate(ctx, cluster, req, &controlPlaneConfigSpec); err != nil {
			return err
		}
	}

	// A PATCH must not clear a required field or persist a spec that create would
	// reject (Forge instanceTypePreference/osProfile/configProfile or the CAPI
	// capiInfraMachineTemplate). instanceTypePreference is merged above and is not
	// covered by hasClusterLevelControlPlaneUpdate, so gate on it explicitly;
	// maxSurge- and k8s-version-only updates leave these fields untouched.
	if s.useControlPlaneController &&
		(hasClusterLevelControlPlaneUpdate(req) || hasInstanceTypePreferences) {
		if err := validateMergedClusterControlPlaneSpec(*cluster, controlPlaneConfigSpec); err != nil {
			return err
		}
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
// cluster-level control plane fields that require control-plane controller
// provisioning (i.e. everything beyond the legacy maxSurge /
// provisioningMonitorDisabled knobs).
//
// Provider fields are read directly from the nested payloads.
func hasClusterLevelControlPlaneUpdate(req *api.UpdateClusterControlPlaneConfig) bool {
	if req.DesiredCount != nil ||
		req.SystemReservedMemory != nil ||
		req.NodeLabels != nil ||
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
		n.OSProfile != nil ||
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
		cluster.ControlPlaneConfig.DesiredCount = *req.DesiredCount
	}

	if req.SystemReservedMemory != nil {
		spec.SystemReservedMemory = *req.SystemReservedMemory
	}
	if req.NodeLabels != nil {
		spec.NodeLabels = append([]string(nil), *req.NodeLabels...)
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
		if req.Nico.OSProfile != nil {
			nico.OSProfile = *req.Nico.OSProfile
		}
		if req.Nico.ConfigProfile != nil {
			nico.ConfigProfile = *req.Nico.ConfigProfile
		}
		if req.Nico.SSHKeyGroupIDs != nil {
			nico.SSHKeyGroupIDs = uuidSliceDedupe(uuidSliceFromAPI(req.Nico.SSHKeyGroupIDs))
		}
		spec.SetNico(nico)

		if req.Nico.SubnetIDs != nil || req.Nico.VpcPrefixIDs != nil {
			if err := validateControlPlaneNicoNetworking(spec.SubnetIDs, spec.VpcPrefixIDs); err != nil {
				return err
			}
		}
	case req.Infra != nil:
		capi, err := spec.CAPIProvider()
		if err != nil {
			return err
		}
		if req.Infra.SpireAttestationMethod != nil {
			capi.SpireAttestationMethod = db.SpireAttestationMethod(*req.Infra.SpireAttestationMethod)
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
				if err := validateCAPIInfrastructureManifest(req.Infra.MachineTemplate); err != nil {
					return newBadRequestErrorf("invalid infra.machineTemplate: %s", err)
				}
				prefs, err := infraMachineTemplatePrefsFromAPI(req.Infra.MachineTemplate)
				if err != nil {
					return newBadRequestErrorf("failed to convert infra.machineTemplate: %v", err)
				}
				capi.InfraMachineTemplatePreferences = prefs
			}
		}
		spec.SetCapi(capi)
	}
	return nil
}

func uuidSliceFromAPI(ids []openapi_types.UUID) []uuid.UUID {
	return append([]uuid.UUID(nil), ids...)
}

// validateInstanceTypePreferenceIDs rejects a nil (all-zero) UUID in the
// instanceTypePreference list. A nil ID is never a valid instance type and would
// otherwise be silently dropped by dedupeUUIDsPreserveOrder, leaving the control
// plane with fewer instance types than requested (or none at all).
func validateInstanceTypePreferenceIDs(ids []openapi_types.UUID) error {
	for _, id := range ids {
		if id == uuid.Nil {
			return fmt.Errorf("controlPlaneConfig.instanceTypePreference contains an invalid nil instance type ID")
		}
	}
	return nil
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
	provisioningMonitorDisabled := cfg.ProvisioningMonitorDisabled
	teleportEnabled := cfg.TeleportEnabled
	return &api.ControlPlaneConfig{
		ConfigurationID:             cfg.ConfigurationID,
		DesiredCount:                cfg.DesiredCount,
		MaxSurge:                    &maxSurge,
		ProvisioningMonitorDisabled: &provisioningMonitorDisabled,
		SystemReservedMemory:        cfg.SystemReservedMemory,
		NodeLabels:                  append([]string(nil), cfg.NodeLabels...),
		TeleportEnabled:             &teleportEnabled,
		Nico:                        apiNicoControlPlaneProviderFromDB(cfg.Nico),
		Infra:                       apiInfraControlPlaneProviderFromDB(cfg.Capi),
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
	if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.EnableOIDCWorkflowType, workflowID, map[string]interface{}{
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
		dbCluster, err := tx.GetCluster(ctx, req.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		dbCluster.Conditions.Deleting = db.NewCondition(true, "Cluster deletion is pending")

		if _, err := tx.UpdateCluster(ctx, &dbCluster); err != nil {
			return fmt.Errorf("failed to update cluster: %w", err)
		}

		workflowID := constants.TeardownClusterWorkflowPrefix + dbCluster.ID.String()
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "TeardownCluster", workflowID, map[string]interface{}{
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
		CreatedAt: dbCluster.CreatedAt,

		Nico:  apiNicoClusterProviderFromDB(dbCluster.Nico),
		Infra: apiInfraClusterProviderFromDB(dbCluster.Capi),

		Conditions: conditions,
	}
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
	cniPlugin := db.CniPlugin(req.CniPlugin)
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
	}

	return cluster, nil
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
