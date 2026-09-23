// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/cwe"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
)

type healthConfig struct {
	Namespace       string
	Service         string
	ServicePort     string
	ExpectedVersion string
	Timeout         time.Duration
}

func loadHealthConfig() (healthConfig, error) {
	timeout, err := time.ParseDuration(envOrDefault("CWE_HEALTH_TIMEOUT", "60s"))
	if err != nil {
		return healthConfig{}, fmt.Errorf("parsing CWE_HEALTH_TIMEOUT: %w", err)
	}

	// Where NKE is, from internal/cwe, which reads the same CWE_NAMESPACE /
	// CWE_SERVICE / CWE_SERVICE_PORT everything else looking for NKE reads.
	return healthConfig{
		Namespace:       cwe.Namespace(),
		Service:         cwe.Service(),
		ServicePort:     cwe.Port(),
		ExpectedVersion: envOrDefault("CWE_EXPECTED_VERSION", ""),
		Timeout:         timeout,
	}, nil
}

func envOrDefault(key, def string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return def
}

// These specs talk to the forwarder directly rather than through nke.Client, so
// they carry no NKE credential and need none: all three paths are on the
// anonymous surface in internal/auth/routes.go. If /api/version ever stops
// being anonymous, this file goes red before anything else does.
var _ = Describe("CWE management-plane health", Label("health"), func() {
	var (
		hc        healthConfig
		forwarder *kube.ServiceForwarder
	)

	BeforeEach(func() {
		var err error
		hc, err = loadHealthConfig()
		Expect(err).NotTo(HaveOccurred(), "load health configuration")

		cfg, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred(), "load e2e configuration")

		restConfig, err := kube.RESTConfigWithInClusterFallback(cfg.Kube.Kubeconfig, cfg.Kube.Context)
		Expect(err).NotTo(HaveOccurred(), "health specs require KUBECONFIG or in-cluster configuration")

		forwarder, err = kube.SharedServiceForwarder(restConfig, hc.Namespace, hc.Service, hc.ServicePort)
		Expect(err).NotTo(HaveOccurred(), "build port-forward client")
	})

	It("serves /healthz", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		statusCode, body, err := forwarder.Get(tctx, "/healthz")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)
	})

	It("serves /readyz", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		statusCode, body, err := forwarder.Get(tctx, "/readyz")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)
	})

	It("reports the expected version", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		if hc.ExpectedVersion == "" {
			Skip("CWE_EXPECTED_VERSION is not set")
		}

		statusCode, body, err := forwarder.Get(tctx, "/api/version")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)

		var response struct {
			Version string `json:"version"`
		}
		Expect(json.Unmarshal(body, &response)).To(Succeed())
		Expect(response.Version).To(Equal(hc.ExpectedVersion))
	})
})
