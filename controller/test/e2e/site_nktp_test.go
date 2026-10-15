// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nktp"
)

// siteTelemetry is what NKTP on a site built from deploy/nico delivers: the
// baseline, then what each component wires up. The comments name the file
// under deploy/nico that wires it.
func siteTelemetry(v *nktp.Validator, traceID string) []nktp.Check {
	return append(v.Baseline(),
		// Control plane. kubeadm serves etcd's metrics on 0.0.0.0:2381, where
		// Kind binds 127.0.0.1, and nktp/values.yaml reads the audit log from
		// the path kubeadm writes it to.
		v.ScrapeJobUp("kube-etcd"),
		v.MetricArrives("etcd_server_has_leader"),
		v.AuditLogArrives(),

		// Controller: controller/overlay.yaml.
		v.ScrapeJobUp(controller.Service()),
		v.MetricArrives("nvke_controller_.+"),
		v.PodLogsArrive(controller.Namespace()),
		v.TraceArrives(traceID, "nvke-controller"),

		// Temporal: temporal/overlay.yaml.
		v.ScrapeJobUp("temporal-frontend-headless"),
		v.ScrapeJobUp("temporal-history-headless"),
		v.ScrapeJobUp("temporal-matching-headless"),
		v.ScrapeJobUp("temporal-worker-headless"),
		v.PodLogsArrive("temporal"),

		// SPIRE: spire/overlay.yaml.
		v.ScrapeJobUp("spire-server/spire-server"),
		v.ScrapeJobUp("spire-system/spire-agent"),
		v.PodLogsArrive("spire-server"),
		v.PodLogsArrive("spire-system"),

		// Site-wide: nktp/values.yaml turns off every monitor a site cannot reach.
		v.NoTargetDown(),
	)
}
