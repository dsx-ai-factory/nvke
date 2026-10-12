// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	teleportCollectorTimeout = 10 * time.Second
)

var _ prometheus.Collector = &TeleportCollector{}

// TeleportCollector exposes Teleport rollout state as Prometheus metrics,
// queried from the DB at scrape time (pull model).
type TeleportCollector struct {
	logger *slog.Logger
	db     db.DB

	clusterEnabledDesc  *prometheus.Desc
	nodePoolEnabledDesc *prometheus.Desc
	nodesEligibleDesc   *prometheus.Desc
	nodesRegisteredDesc *prometheus.Desc
	nodesPendingDesc    *prometheus.Desc
}

func NewTeleportCollector(logger *slog.Logger, db db.DB) *TeleportCollector {
	return &TeleportCollector{
		logger: logger,
		db:     db,
		clusterEnabledDesc: prometheus.NewDesc(
			"nvke_controller_teleport_cluster_enabled",
			"Whether Teleport is enabled on the cluster (1=enabled, 0=disabled)",
			[]string{siteIDLabel, clusterIDLabel, "cluster_name"},
			nil,
		),
		nodePoolEnabledDesc: prometheus.NewDesc(
			"nvke_controller_teleport_node_pool_enabled",
			"Whether Teleport is enabled on the node pool (1=enabled, 0=disabled)",
			[]string{siteIDLabel, clusterIDLabel, nodePoolIDLabel, "node_pool_name"},
			nil,
		),
		nodesEligibleDesc: prometheus.NewDesc(
			"nvke_controller_teleport_nodes_eligible",
			"Count of nodes eligible for Teleport registration",
			[]string{siteIDLabel, clusterIDLabel, nodePoolIDLabel},
			nil,
		),
		nodesRegisteredDesc: prometheus.NewDesc(
			"nvke_controller_teleport_nodes_registered",
			"Count of nodes registered with Teleport",
			[]string{siteIDLabel, clusterIDLabel, nodePoolIDLabel},
			nil,
		),
		nodesPendingDesc: prometheus.NewDesc(
			"nvke_controller_teleport_nodes_pending",
			"Count of eligible nodes not yet registered with Teleport",
			[]string{siteIDLabel, clusterIDLabel, nodePoolIDLabel},
			nil,
		),
	}
}

func (c *TeleportCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.clusterEnabledDesc
	ch <- c.nodePoolEnabledDesc
	ch <- c.nodesEligibleDesc
	ch <- c.nodesRegisteredDesc
	ch <- c.nodesPendingDesc
}

func (c *TeleportCollector) Collect(ch chan<- prometheus.Metric) {
	c.logger.Debug("Collecting Teleport rollout metrics")

	ctx, cancel := context.WithTimeout(context.Background(), teleportCollectorTimeout)
	defer func() {
		cancel()
		if ctx.Err() == context.DeadlineExceeded {
			c.logger.Error("Teleport metrics collection timed out", "duration", teleportCollectorTimeout)
		}
	}()

	rows, err := c.db.ListTeleportRolloutStats(ctx)
	if err != nil {
		c.logger.Error("Failed to query Teleport rollout stats", "error", err)
		return
	}

	// Deduplicate cluster-level metrics since the same cluster appears for each node pool
	seenClusters := make(map[string]struct{})

	for _, row := range rows {
		siteID := row.SiteID.String()
		clusterID := row.ClusterID.String()
		nodePoolID := row.NodePoolID.String()

		// Cluster-level: emit once per cluster
		if _, seen := seenClusters[clusterID]; !seen {
			seenClusters[clusterID] = struct{}{}
			ch <- prometheus.MustNewConstMetric(
				c.clusterEnabledDesc,
				prometheus.GaugeValue,
				boolToFloat64(row.ClusterEnabled),
				siteID, clusterID, row.ClusterName,
			)
		}

		// Node-pool-level
		ch <- prometheus.MustNewConstMetric(
			c.nodePoolEnabledDesc,
			prometheus.GaugeValue,
			boolToFloat64(row.NodePoolEnabled),
			siteID, clusterID, nodePoolID, row.NodePoolName,
		)

		// Node aggregates (only meaningful for teleport-enabled node pools)
		ch <- prometheus.MustNewConstMetric(
			c.nodesEligibleDesc,
			prometheus.GaugeValue,
			float64(row.NodesEligible),
			siteID, clusterID, nodePoolID,
		)

		ch <- prometheus.MustNewConstMetric(
			c.nodesRegisteredDesc,
			prometheus.GaugeValue,
			float64(row.NodesRegistered),
			siteID, clusterID, nodePoolID,
		)

		ch <- prometheus.MustNewConstMetric(
			c.nodesPendingDesc,
			prometheus.GaugeValue,
			float64(row.NodesEligible-row.NodesRegistered),
			siteID, clusterID, nodePoolID,
		)
	}
}

func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
