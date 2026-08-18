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

	"github.com/NVIDIA/nke/controller/test/e2e/internal/config"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
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

	return healthConfig{
		Namespace:       envOrDefault("CWE_NAMESPACE", "cloud-workflow-engine"),
		Service:         envOrDefault("CWE_SERVICE", "cloud-workflow-engine"),
		ServicePort:     envOrDefault("CWE_SERVICE_PORT", "8080"),
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

var _ = Describe("CWE management-plane health", Label("health"), func() {
	var (
		hc    healthConfig
		proxy *kube.ServiceProxy
	)

	BeforeEach(func() {
		var err error
		hc, err = loadHealthConfig()
		Expect(err).NotTo(HaveOccurred(), "load health configuration")

		cfg, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred(), "load e2e configuration")

		restConfig, err := kube.RESTConfigWithInClusterFallback(cfg.Kube.Kubeconfig, cfg.Kube.Context)
		Expect(err).NotTo(HaveOccurred(), "health specs require KUBECONFIG or in-cluster configuration")

		proxy, err = kube.NewServiceProxy(restConfig, hc.Namespace, hc.Service, hc.ServicePort)
		Expect(err).NotTo(HaveOccurred(), "build service-proxy client")
	})

	It("serves /healthz", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		statusCode, body, err := proxy.Get(tctx, "/healthz")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)
	})

	It("serves /readyz", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		statusCode, body, err := proxy.Get(tctx, "/readyz")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)
	})

	It("reports the expected version", func(ctx SpecContext) {
		tctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		defer cancel()

		if hc.ExpectedVersion == "" {
			Skip("CWE_EXPECTED_VERSION is not set")
		}

		statusCode, body, err := proxy.Get(tctx, "/api/version")
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(Equal(200), "response body: %s", body)

		var response struct {
			Version string `json:"version"`
		}
		Expect(json.Unmarshal(body, &response)).To(Succeed())
		Expect(response.Version).To(Equal(hc.ExpectedVersion))
	})
})
