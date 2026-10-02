// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbmetrics

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"

	prometheus "github.com/prometheus/client_golang/prometheus"
)

const (
	Timeout           = 10 * time.Second
	LabelValueUnknown = "unknown"
)

// clusterInfoContext holds cluster-scoped IDs (org, site, vpc) for use in info metrics.
type clusterInfoContext struct {
	Name   string
	OrgID  string
	SiteID string
	VpcID  string
}

// nodeSetParentContext holds parent identifiers for a node set (cluster and node pool names).
type nodeSetParentContext struct {
	ClusterName  string
	NodePoolName string
}

var _ prometheus.Collector = &Collector{}

// Expose metrics derived from records in the database. Queried synchronously when metrics are scraped.
type Collector struct {
	logger *slog.Logger
	db     db.DB

	nodeConditionDesc            *prometheus.Desc
	nodePoolDesiredCountDesc     *prometheus.Desc
	nodePoolActualCountDesc      *prometheus.Desc
	nodePoolPendingCountDesc     *prometheus.Desc
	nodeSetDeprecatedCountDesc   *prometheus.Desc
	nodeInfoDesc                 *prometheus.Desc
	clusterInfoDesc              *prometheus.Desc
	nodePoolInfoDesc             *prometheus.Desc
	nodeSetInfoDesc              *prometheus.Desc
	nodeLifecycleElapsedSecsDesc *prometheus.Desc
}

// Prometheus label names, shared across the metric descriptors below.
const (
	labelTenantCluster     = "tenant_cluster"
	labelNode              = "node"
	labelStatus            = "status"
	labelNodePoolID        = "node_pool_id"
	labelNodePoolName      = "node_pool_name"
	labelOSProfile         = "os_profile"
	labelConfigProfile     = "config_profile"
	labelKubernetesVersion = "kubernetes_version"
	labelTeleportEnabled   = "teleport_enabled"
	labelCreatedAt         = "created_at"
	labelUpdatedAt         = "updated_at"
)

func NewCollector(logger *slog.Logger, db db.DB) *Collector {
	return &Collector{
		logger: logger,
		db:     db,
		nodeConditionDesc: prometheus.NewDesc(
			"nke_controller_node_condition",
			"Status of a node condition",
			[]string{
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				labelNode,
				"condition",
				labelStatus,
			},
			nil, // no const labels
		),
		nodePoolDesiredCountDesc: prometheus.NewDesc(
			"nke_controller_node_pool_desired_count",
			"Desired count of nodes in the node pool",
			[]string{
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				labelNodePoolID,
				labelNodePoolName,
			},
			nil,
		),
		nodePoolActualCountDesc: prometheus.NewDesc(
			"nke_controller_node_pool_actual_count",
			"Actual count of nodes in the node pool",
			[]string{
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				labelNodePoolID,
				labelNodePoolName,
			},
			nil,
		),
		nodePoolPendingCountDesc: prometheus.NewDesc(
			"nke_controller_node_pool_pending_count",
			"Count of pending nodes in the node pool",
			[]string{
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				labelNodePoolID,
				labelNodePoolName,
			},
			nil,
		),
		nodeSetDeprecatedCountDesc: prometheus.NewDesc(
			"nke_controller_node_pool_deprecated_count",
			"Count of nodes in deprecated node sets",
			[]string{
				labelTenantCluster,
				labelNodePoolID,
				labelNodePoolName,
			},
			nil,
		),
		nodeInfoDesc: prometheus.NewDesc(
			"nke_controller_node_info",
			"Information about a node",
			[]string{
				labelTenantCluster,
				"node_pool",
				"node_set_id",
				labelNode,
				"ip",
				"agent_version",
				"spiffe_enabled",
				"forge_machine_id",
				labelOSProfile,
				labelConfigProfile,
			},
			nil,
		),
		clusterInfoDesc: prometheus.NewDesc(
			"nke_controller_cluster_info",
			"Information about a cluster",
			[]string{
				"name",
				"org_id",
				"site_id",
				"vpc_id",
				"cni_plugin",
				"etcd_encrypted",
				labelKubernetesVersion,
				"control_plane_vip",
				"enable_cp_node_recycling",
				labelTeleportEnabled,
				labelStatus,
				labelCreatedAt,
				labelUpdatedAt,
			},
			nil,
		),
		nodePoolInfoDesc: prometheus.NewDesc(
			"nke_controller_nodepool_info",
			"Information about a node pool",
			[]string{
				"id",
				"cluster_id",
				"name",
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				"org_id",
				"site_id",
				"vpc_id",
				"node_name_prefix",
				"subnet_ids",
				"vpc_prefix_ids",
				"instance_type_id",
				"description",
				"infiniband_partition_id",
				"nvlink_logical_partition_id",
				"ssh_key_group_ids",
				labelOSProfile,
				labelConfigProfile,
				"system_reserved_memory",
				"kube_reserved_memory",
				"control_plane",
				"node_labels",
				labelStatus,
				labelCreatedAt,
				labelUpdatedAt,
				labelKubernetesVersion,
				"desired_count",
				"auto_provision_disabled",
				"max_surge",
				"current_node_set_id",
				"auto_mode_disabled",
				"recycle_budget",
				labelTeleportEnabled,
				"disk_selection_strategy",
			},
			nil,
		),
		nodeSetInfoDesc: prometheus.NewDesc(
			"nke_controller_nodeset_info",
			"Information about a node set",
			[]string{
				// "tenant_cluster" instead of "cluster" to avoid label collision with the site controller cluster where we're running
				labelTenantCluster,
				labelNodePoolName,
				"id",
				labelNodePoolID,
				"desired_count",
				labelOSProfile,
				labelConfigProfile,
				labelKubernetesVersion,
				"system_reserved_memory",
				"kube_reserved_memory",
				"subnet_ids",
				"vpc_prefix_ids",
				labelCreatedAt,
				labelUpdatedAt,
				labelTeleportEnabled,
				"disk_selection_strategy",
			},
			nil,
		),
		nodeLifecycleElapsedSecsDesc: prometheus.NewDesc(
			"nke_controller_node_lifecycle_elapsed_seconds",
			"Seconds elapsed since a node entered its current lifecycle state (pending, provisioning, deleting, or markedForRecycling)",
			[]string{
				labelTenantCluster,
				labelNode,
				"node_type",
				"state",
			},
			nil,
		),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodeConditionDesc
	ch <- c.nodePoolDesiredCountDesc
	ch <- c.nodePoolActualCountDesc
	ch <- c.nodePoolPendingCountDesc
	ch <- c.nodeSetDeprecatedCountDesc
	ch <- c.nodeInfoDesc
	ch <- c.clusterInfoDesc
	ch <- c.nodePoolInfoDesc
	ch <- c.nodeSetInfoDesc
	ch <- c.nodeLifecycleElapsedSecsDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.logger.Debug("Collecting DB metrics")

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer func() {
		cancel()
		if ctx.Err() == context.DeadlineExceeded {
			c.logger.Error("DB metrics collection timed out", "duration", Timeout)
		}
	}()

	c.collectNodeConditions(ctx, ch)
	c.collectNodePoolMetrics(ctx, ch)
	c.collectNodeInfo(ctx, ch)
	c.collectClusterNodePoolAndNodeSetInfo(ctx, ch)
	c.collectNodeLifecycleElapsed(ctx, ch)
}

func (c *Collector) collectNodeConditions(ctx context.Context, ch chan<- prometheus.Metric) {
	conditions, err := c.db.ListNodeConditions(ctx)
	if err != nil {
		c.logger.Error("failed to get node conditions", "error", err)
		return
	}

	for _, condition := range conditions {
		ch <- prometheus.MustNewConstMetric(
			c.nodeConditionDesc,
			prometheus.GaugeValue,
			1, // Value always set to 1, mirroring behavior of kube-state-metrics kube_node_status_condition
			condition.ClusterName,
			condition.NodeName,
			condition.ConditionType,
			string(condition.ConditionStatus),
		)
	}
}

func (c *Collector) collectNodePoolMetrics(ctx context.Context, ch chan<- prometheus.Metric) {
	metrics, err := c.db.ListNodePoolMetrics(ctx)
	if err != nil {
		c.logger.Error("failed to get node pool metrics", "error", err)
		return
	}

	for _, metric := range metrics {
		labels := []string{
			metric.ClusterName,
			metric.NodePoolID.String(),
			metric.NodePoolName,
		}

		ch <- prometheus.MustNewConstMetric(
			c.nodePoolDesiredCountDesc,
			prometheus.GaugeValue,
			float64(metric.DesiredCount),
			labels...,
		)

		ch <- prometheus.MustNewConstMetric(
			c.nodePoolActualCountDesc,
			prometheus.GaugeValue,
			float64(metric.ActualCount),
			labels...,
		)

		ch <- prometheus.MustNewConstMetric(
			c.nodePoolPendingCountDesc,
			prometheus.GaugeValue,
			float64(metric.PendingCount),
			labels...,
		)

		ch <- prometheus.MustNewConstMetric(
			c.nodeSetDeprecatedCountDesc,
			prometheus.GaugeValue,
			float64(metric.DeprecatedCount),
			labels...,
		)
	}
}

func (c *Collector) collectNodeInfo(ctx context.Context, ch chan<- prometheus.Metric) {
	nodeInfo, err := c.db.ListNodeInfo(ctx)
	if err != nil {
		c.logger.Error("failed to get node info", "error", err)
		return
	}

	for _, info := range nodeInfo {
		ch <- prometheus.MustNewConstMetric(
			c.nodeInfoDesc,
			prometheus.GaugeValue,
			1,
			info.ClusterName,
			info.NodePoolName,
			info.NodeSetID.String(),
			info.NodeName,
			info.IP,
			info.AgentVersion,
			strconv.FormatBool(info.SpiffeEnabled),
			info.ForgeMachineID,
			info.OSProfile,
			info.ConfigProfile,
		)
	}
}

func (c *Collector) collectClusterNodePoolAndNodeSetInfo(ctx context.Context, ch chan<- prometheus.Metric) {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		c.logger.Error("failed to list clusters for info metrics", "error", err)
		return
	}

	clusterContexts := make(map[uuid.UUID]clusterInfoContext, len(clusters))
	for _, cluster := range clusters {
		clusterCtx := clusterInfoContext{
			Name: cluster.Name,
		}
		if cluster.ProviderType == db.ProviderTypeNico {
			provider, err := cluster.NicoProvider()
			if err != nil {
				c.logger.Error("failed to resolve NICo provider for cluster info metrics", "error", err, "cluster", cluster.Name)
				continue
			}
			clusterCtx.OrgID = provider.OrgID
			clusterCtx.SiteID = provider.SiteID.String()
			clusterCtx.VpcID = provider.VpcID.String()
		}

		c.emitClusterInfo(ch, &clusterCtx, cluster)
		clusterContexts[cluster.ID] = clusterCtx
	}

	if len(clusterContexts) == 0 {
		return
	}

	nodePools, err := c.db.ListNodePools(ctx, db.NodePoolFilters{})
	if err != nil {
		c.logger.Error("failed to list node pools for info metrics", "error", err)
		return
	}

	nodeSetContexts := make(map[uuid.UUID]nodeSetParentContext, len(nodePools))
	for _, nodePool := range nodePools {
		clusterCtx, ok := clusterContexts[nodePool.ClusterID]
		if !ok {
			continue
		}

		c.emitNodePoolInfo(ch, &clusterCtx, nodePool)
		nodeSetContexts[nodePool.ID] = nodeSetParentContext{ClusterName: clusterCtx.Name, NodePoolName: nodePool.Name}
	}

	if len(nodeSetContexts) == 0 {
		return
	}

	nodeSets, err := c.db.ListNodeSets(ctx, db.NodeSetFilters{})
	if err != nil {
		c.logger.Error("failed to list node sets for info metrics", "error", err)
		return
	}

	for _, nodeSet := range nodeSets {
		nodeSetCtx, ok := nodeSetContexts[nodeSet.NodePoolID]
		if !ok {
			continue
		}

		c.emitNodeSetInfo(ch, &nodeSetCtx, nodeSet)
	}
}

func (c *Collector) emitClusterInfo(ch chan<- prometheus.Metric, ctx *clusterInfoContext, cluster db.Cluster) {
	ch <- prometheus.MustNewConstMetric(
		c.clusterInfoDesc,
		prometheus.GaugeValue,
		1,
		ctx.Name,
		ctx.OrgID,
		ctx.SiteID,
		ctx.VpcID,
		string(cluster.CniPlugin),
		strconv.FormatBool(cluster.EtcdEncrypted),
		cluster.KubernetesVersion,
		cluster.ControlPlaneVIP,
		strconv.FormatBool(cluster.EnableCPNodeRecycling),
		strconv.FormatBool(cluster.TeleportEnabled),
		getClusterStatus(cluster),
		cluster.CreatedAt.UTC().Format(time.RFC3339),
		cluster.UpdatedAt.UTC().Format(time.RFC3339),
	)
}

func (c *Collector) emitNodePoolInfo(ch chan<- prometheus.Metric, ctx *clusterInfoContext, nodePool db.NodePool) {
	subnetIDs := ""
	vpcPrefixIDs := ""
	instanceTypeID := uuid.Nil.String()
	infinibandPartitionID := ""
	nvLinkLogicalPartitionID := ""
	sshKeyGroupIDs := ""
	osProfile := ""
	configProfile := ""
	diskSelectionStrategy := ""

	if nicoNodePoolProvider, err := nodePool.NicoProvider(); err == nil {
		subnetIDs = uuidSliceToString(nicoNodePoolProvider.SubnetIDs)
		vpcPrefixIDs = uuidSliceToString(nicoNodePoolProvider.VpcPrefixIDs)
		instanceTypeID = nicoNodePoolProvider.InstanceTypeID.String()
		infinibandPartitionID = optionalUUID(nicoNodePoolProvider.InfinibandPartitionID)
		nvLinkLogicalPartitionID = optionalUUID(nicoNodePoolProvider.NvLinkLogicalPartitionID)
		sshKeyGroupIDs = uuidSliceToString(nicoNodePoolProvider.SSHKeyGroupIDs)
		osProfile = nicoNodePoolProvider.OSProfile
		configProfile = nicoNodePoolProvider.ConfigProfile
		diskSelectionStrategy = string(nicoNodePoolProvider.DiskSelectionStrategy)
	}

	ch <- prometheus.MustNewConstMetric(
		c.nodePoolInfoDesc,
		prometheus.GaugeValue,
		1,
		nodePool.ID.String(),
		nodePool.ClusterID.String(),
		nodePool.Name,
		ctx.Name,
		ctx.OrgID,
		ctx.SiteID,
		ctx.VpcID,
		nodePool.NodeNamePrefix,
		subnetIDs,
		vpcPrefixIDs,
		instanceTypeID,
		optionalString(nodePool.Description),
		infinibandPartitionID,
		nvLinkLogicalPartitionID,
		sshKeyGroupIDs,
		osProfile,
		configProfile,
		nodePool.SystemReservedMemory,
		nodePool.KubeReservedMemory,
		strconv.FormatBool(nodePool.ControlPlane),
		strings.Join(nodePool.NodeLabels, ","),
		getNodePoolStatus(nodePool),
		nodePool.CreatedAt.UTC().Format(time.RFC3339),
		nodePool.UpdatedAt.UTC().Format(time.RFC3339),
		nodePool.KubernetesVersion,
		strconv.Itoa(nodePool.DesiredCount),
		strconv.FormatBool(nodePool.AutoProvisionDisabled),
		strconv.Itoa(nodePool.MaxSurge),
		optionalUUID(nodePool.CurrentNodeSetID),
		strconv.FormatBool(nodePool.AutoModeDisabled),
		strconv.Itoa(nodePool.RecycleBudget),
		strconv.FormatBool(nodePool.TeleportEnabled),
		diskSelectionStrategy,
	)
}

func (c *Collector) emitNodeSetInfo(ch chan<- prometheus.Metric, ctx *nodeSetParentContext, nodeSet db.NodeSet) {
	subnetIDs := ""
	vpcPrefixIDs := ""
	osProfile := ""
	configProfile := ""
	diskSelectionStrategy := ""

	if nicoNodeSetProvider, err := nodeSet.NicoProvider(); err == nil {
		subnetIDs = uuidSliceToString(nicoNodeSetProvider.SubnetIDs)
		vpcPrefixIDs = uuidSliceToString(nicoNodeSetProvider.VpcPrefixIDs)
		osProfile = nicoNodeSetProvider.OSProfile
		configProfile = nicoNodeSetProvider.ConfigProfile
		diskSelectionStrategy = string(nicoNodeSetProvider.DiskSelectionStrategy)
	}

	ch <- prometheus.MustNewConstMetric(
		c.nodeSetInfoDesc,
		prometheus.GaugeValue,
		1,
		ctx.ClusterName,
		ctx.NodePoolName,
		nodeSet.ID.String(),
		nodeSet.NodePoolID.String(),
		strconv.Itoa(nodeSet.DesiredCount),
		osProfile,
		configProfile,
		nodeSet.KubernetesVersion,
		nodeSet.SystemReservedMemory,
		nodeSet.KubeReservedMemory,
		subnetIDs,
		vpcPrefixIDs,
		nodeSet.CreatedAt.UTC().Format(time.RFC3339),
		nodeSet.UpdatedAt.UTC().Format(time.RFC3339),
		strconv.FormatBool(nodeSet.TeleportEnabled),
		diskSelectionStrategy,
	)
}

func getClusterStatus(cluster db.Cluster) string {
	status := LabelValueUnknown
	if db.BoolFromConditionStatus(cluster.Conditions.Provisioning.Status) {
		status = "provisioning"
	}
	if db.BoolFromConditionStatus(cluster.Conditions.Provisioned.Status) {
		status = "provisioned"
	}
	if db.BoolFromConditionStatus(cluster.Conditions.Deleting.Status) {
		status = "deleting"
	}
	if db.BoolFromConditionStatus(cluster.Conditions.ControlPlaneUpgrading.Status) {
		status = "control_plane_upgrading"
	}
	if db.BoolFromConditionStatus(cluster.Conditions.CPNodesRecycling.Status) {
		status = "cp_nodes_recycling"
	}
	return status
}

func getNodePoolStatus(nodePool db.NodePool) string {
	status := LabelValueUnknown
	if db.BoolFromConditionStatus(nodePool.Conditions.Provisioning.Status) {
		status = "provisioning"
	}
	if db.BoolFromConditionStatus(nodePool.Conditions.Provisioned.Status) {
		status = "provisioned"
	}
	if db.BoolFromConditionStatus(nodePool.Conditions.Deleting.Status) {
		status = "deleting"
	}
	return status
}

func uuidSliceToString(ids []uuid.UUID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ",")
}

func optionalUUID(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func optionalString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (c *Collector) collectNodeLifecycleElapsed(ctx context.Context, ch chan<- prometheus.Metric) {
	rows, err := c.db.ListNodeLifecycleElapsed(ctx)
	if err != nil {
		c.logger.Error("failed to get node lifecycle elapsed", "error", err)
		return
	}

	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(
			c.nodeLifecycleElapsedSecsDesc,
			prometheus.GaugeValue,
			row.ElapsedSecs,
			row.ClusterName,
			row.NodeName,
			row.NodeType,
			row.State,
		)
	}
}
