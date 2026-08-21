// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	capiAdoptionNicoClusters = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cwe_capi_adoption_nico_clusters",
			Help: "Clusters still needing CAPI adoption: leftover cluster-<name> namespace, or nico-shaped clusters whose eligible nodes are not yet adopted",
		},
		[]string{siteIDLabel},
	)
	capiAdoptionOrphanedLegacyNamespaces = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cwe_capi_adoption_orphaned_legacy_namespaces",
			Help: "cluster-* namespaces with no matching cluster row on any site, left if teardown ran before adoption drained the legacy namespace",
		},
		[]string{siteIDLabel},
	)
)

func init() {
	prometheus.MustRegister(capiAdoptionNicoClusters)
	prometheus.MustRegister(capiAdoptionOrphanedLegacyNamespaces)
}

func RecordCAPIAdoptionRemaining(siteID string, remaining float64) {
	capiAdoptionNicoClusters.WithLabelValues(siteID).Set(remaining)
}

func RecordCAPIAdoptionOrphanedLegacyNamespaces(siteID string, count float64) {
	capiAdoptionOrphanedLegacyNamespaces.WithLabelValues(siteID).Set(count)
}
