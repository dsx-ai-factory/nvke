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
	// RequireExpectedVersion fails the version spec, rather than skipping it,
	// when no version is expected. The validation image sets it: there a
	// skipped check reads as a pass.
	RequireExpectedVersion bool
	Timeout                time.Duration
}

// expectedVersionRequireEnv sets healthConfig.RequireExpectedVersion.
const expectedVersionRequireEnv = "NVKE_E2E_REQUIRE_EXPECTED_VERSION"

func loadHealthConfig() (healthConfig, error) {
	timeout, err := time.ParseDuration(cmp.Or(os.Getenv("NVKE_CONTROLLER_HEALTH_TIMEOUT"), "60s"))
	if err != nil {
		return healthConfig{}, fmt.Errorf("parsing NVKE_CONTROLLER_HEALTH_TIMEOUT: %w", err)
	}

	requireVersion, err := parseRequireEnv(expectedVersionRequireEnv, os.Getenv(expectedVersionRequireEnv))
	if err != nil {
		return healthConfig{}, err
	}

	// Where the controller is, from internal/controller, which reads the same
	// NVKE_CONTROLLER_NAMESPACE / NVKE_CONTROLLER_SERVICE / NVKE_CONTROLLER_SERVICE_PORT
	// everything else looking for the controller reads.
	return healthConfig{
		Namespace:              controller.Namespace(),
		Service:                controller.Service(),
		ServicePort:            controller.Port(),
		ExpectedVersion:        os.Getenv("NVKE_CONTROLLER_EXPECTED_VERSION"),
		RequireExpectedVersion: requireVersion,
		Timeout:                timeout,
	}, nil
}

func TestLoadHealthConfigRequireExpectedVersion(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "", want: false},
		{raw: "true", want: true},
		{raw: "false", want: false},
		{raw: "yes", wantErr: true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(expectedVersionRequireEnv, tc.raw)
			hc, err := loadHealthConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadHealthConfig with %s=%q succeeded, want an error", expectedVersionRequireEnv, tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadHealthConfig: %v", err)
			}
			if hc.RequireExpectedVersion != tc.want {
				t.Fatalf("RequireExpectedVersion = %v, want %v", hc.RequireExpectedVersion, tc.want)
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
// anonymous surface in auth/authn/routes.go. If /api/version ever stops
// being anonymous, this file goes red before anything else does.
//
// site-validate marks what the validation image runs against a production
// site: specs that create, change and install nothing, so they can run under a
// read-only identity. The authn specs are not among them because they mint
// tokens for the scaffolding identities in deploy/k8s/nvke-auth-dev.yaml, which
// a site does not install. tenant-validate too: a tenant has no controller of
// its own, so a run for a tenant alone checks the site's.
var _ = Describe("controller management-plane health", Label("health", siteValidateLabel, tenantValidateLabel), func() {
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
			if hc.RequireExpectedVersion {
				Fail(fmt.Sprintf("NVKE_CONTROLLER_EXPECTED_VERSION is not set, and %s requires it", expectedVersionRequireEnv))
			}
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
