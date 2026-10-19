// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

type healthConfig struct {
	Namespace       string
	Service         string
	ServicePort     string
	ExpectedVersion string
	Timeout         time.Duration
}

func loadHealthConfig() (healthConfig, error) {
	// NKE_CONTROLLER_HEALTH_TIMEOUT, NKE_CONTROLLER_EXPECTED_VERSION, CWE_HEALTH_TIMEOUT and
	// CWE_EXPECTED_VERSION are deprecated aliases for the NVKE_CONTROLLER_ names; #1399 removes them.
	timeout, err := time.ParseDuration(envOrDefaultLegacy("60s", "NVKE_CONTROLLER_HEALTH_TIMEOUT", "NKE_CONTROLLER_HEALTH_TIMEOUT", "CWE_HEALTH_TIMEOUT"))
	if err != nil {
		return healthConfig{}, fmt.Errorf("parsing NVKE_CONTROLLER_HEALTH_TIMEOUT: %w", err)
	}

	// Where the controller is, from internal/controller, which reads the same
	// NVKE_CONTROLLER_NAMESPACE / NVKE_CONTROLLER_SERVICE / NVKE_CONTROLLER_SERVICE_PORT
	// everything else looking for the controller reads.
	return healthConfig{
		Namespace:       controller.Namespace(),
		Service:         controller.Service(),
		ServicePort:     controller.Port(),
		ExpectedVersion: envOrDefaultLegacy("", "NVKE_CONTROLLER_EXPECTED_VERSION", "NKE_CONTROLLER_EXPECTED_VERSION", "CWE_EXPECTED_VERSION"),
		Timeout:         timeout,
	}, nil
}

// envOrDefaultLegacy reads key, then each of legacyKeys (deprecated spellings
// of key, still honored, in order), then def. An empty value counts as unset,
// so an empty key does not hide a set legacy key.
func envOrDefaultLegacy(def, key string, legacyKeys ...string) string {
	value := os.Getenv(key)
	for _, legacyKey := range legacyKeys {
		value = cmp.Or(value, os.Getenv(legacyKey))
	}
	return cmp.Or(value, def)
}

func TestUnit_EnvOrDefaultLegacy(t *testing.T) {
	const key, previousKey, cweKey = "NKE_E2E_TEST_NEW", "NKE_E2E_TEST_NKE", "NKE_E2E_TEST_OLD"
	for _, tc := range []struct {
		name, value, previous, legacy, want string
	}{
		{name: "default when none is set", want: "def"},
		{name: "legacy name when only it is set", legacy: "old", want: "old"},
		{name: "NKE_CONTROLLER name when only it is set", previous: "nke", want: "nke"},
		{name: "new name wins over legacy names", value: "new", previous: "nke", legacy: "old", want: "new"},
		{name: "NKE_CONTROLLER name wins over CWE name", previous: "nke", legacy: "old", want: "nke"},
		{name: "empty new name does not hide legacy names", value: "", previous: "", legacy: "old", want: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.value)
			t.Setenv(previousKey, tc.previous)
			t.Setenv(cweKey, tc.legacy)
			if got := envOrDefaultLegacy("def", key, previousKey, cweKey); got != tc.want {
				t.Errorf("envOrDefaultLegacy = %q, want %q", got, tc.want)
			}
		})
	}
}

func envOrDefault(key, def string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return def
}

// These specs talk to the forwarder directly rather than through nvke.Client, so
// they carry no NVKE credential and need none: all three paths are on the
// anonymous surface in internal/auth/routes.go. If /api/version ever stops
// being anonymous, this file goes red before anything else does.
var _ = Describe("controller management-plane health", Label("health"), func() {
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
			Skip("NVKE_CONTROLLER_EXPECTED_VERSION is not set")
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
