// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var imageVerificationCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvke_controller_image_verification_total",
		Help: "Node-image verification pre-flight outcomes, by mode (warn/enforce) and result (pass/fail). " +
			"Mode off runs no pre-flight and is not counted. The warn-to-enforce graduation question " +
			"is the fail rate at mode=\"warn\": zero over a representative window is the evidence " +
			"enforce will not refuse anything warn did not already name.",
	},
	[]string{"mode", resultLabel},
)

func init() {
	prometheus.MustRegister(imageVerificationCounter)
}

// RecordImageVerification counts one evaluated image-verification pre-flight.
// result is "pass" or "fail"; mode is the configured mode that evaluated it.
func RecordImageVerification(mode, result string) {
	imageVerificationCounter.WithLabelValues(mode, result).Inc()
}
