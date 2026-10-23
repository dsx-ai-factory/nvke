//go:build integration_test

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

type environment struct {
	host              client.Client
	state             client.Client
	nvkeController    *api.ClientWithResponses
	temporal          string
	temporalAddress   string
	temporalNamespace string
	resultsDir        string
}

func TestNVKEControllerIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Management Engine NVKE Controller Integration Suite")
}

func loadEnvironment() environment {
	kubeconfig := requiredEnv("KUBECONFIG")
	hostContext, stateContext := requiredEnv("HOST_CONTEXT"), requiredEnv("STATE_CONTEXT")
	Expect(hostContext).NotTo(Equal(stateContext), "host and state clusters must be separate")
	endpoint := requiredEnv("NVKE_CONTROLLER_API_URL")
	endpointURL, err := url.Parse(endpoint)
	Expect(err).NotTo(HaveOccurred())
	Expect(endpointURL.Scheme).To(Equal("http"), "use the local NVKE controller port-forward")
	Expect(isLoopback(endpointURL.Hostname())).To(BeTrue(), "The NVKE controller must use a loopback port-forward")
	tokenFile := requiredEnv("NVKE_CONTROLLER_TOKEN_FILE")
	_, err = os.Stat(tokenFile)
	Expect(err).NotTo(HaveOccurred())
	nvkeController, err := api.NewClientWithResponses(endpoint, api.WithHTTPClient(&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}), api.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		token, err := os.ReadFile(tokenFile)
		if err != nil {
			return fmt.Errorf("read NVKE controller token file: %w", err)
		}
		if strings.TrimSpace(string(token)) == "" {
			return fmt.Errorf("NVKE controller token file is empty")
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		return nil
	}))
	Expect(err).NotTo(HaveOccurred())
	temporal, err := exec.LookPath("temporal")
	Expect(err).NotTo(HaveOccurred(), "activate the repository toolchain for the Temporal CLI")
	address := envOr("TEMPORAL_ADDRESS", "127.0.0.1:7233")
	host, _, err := net.SplitHostPort(address)
	Expect(err).NotTo(HaveOccurred())
	Expect(isLoopback(host)).To(BeTrue(), "Temporal must use a loopback port-forward")
	return environment{
		host: newLocalClient(kubeconfig, hostContext), state: newLocalClient(kubeconfig, stateContext),
		nvkeController: nvkeController, temporal: temporal, temporalAddress: address,
		temporalNamespace: envOr("TEMPORAL_NAMESPACE", "nvke-controller"),
		resultsDir:        requiredEnv("NVKE_CONTROLLER_RESULTS_DIR"),
	}
}

func newLocalClient(kubeconfig, contextName string) client.Client {
	Expect(strings.HasPrefix(contextName, "kind-")).To(BeTrue(), "integration tests require explicit Kind contexts")
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	server, err := url.Parse(config.Host)
	Expect(err).NotTo(HaveOccurred())
	Expect(isLoopback(server.Hostname())).To(BeTrue(), "Kind API servers must be local")
	config.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(internalv1.AddToScheme(scheme)).To(Succeed())
	Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	return kubeClient
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	Expect(value).NotTo(BeEmpty(), "%s is required; use make test-nvke-controller-integration", name)
	return value
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func isLoopback(host string) bool {
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}
