// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package envtest starts a Kubernetes API server with a rendered OIDC config.
package envtest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	controllerruntimeenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dsx-ai-factory/nvke/controller/test/oidctest/probe"
)

const (
	binaryAssetsEnv         = "KUBEBUILDER_ASSETS"
	maximumDiagnosticsBytes = 32 << 10
)

// Config provides the rendered AuthenticationConfiguration.
type Config struct {
	AuthenticationConfig []byte
}

// Runner owns an envtest control plane and its setup-only admin client.
type Runner struct {
	adminConfig *rest.Config
	adminClient kubernetes.Interface
	environment *controllerruntimeenvtest.Environment
	diagnostics lockedBuffer
	closeOnce   sync.Once
	closeErr    error
}

// Start writes the auth config, starts envtest, and registers cleanup first.
func Start(tb testing.TB, config Config) (*Runner, error) {
	tb.Helper()
	runner := &Runner{}
	tb.Cleanup(func() {
		if err := runner.Close(); err != nil {
			tb.Errorf("stop OIDC envtest: %v", err)
		}
		if tb.Failed() && runner.Diagnostics() != "" {
			tb.Logf("OIDC envtest diagnostics:\n%s", runner.Diagnostics())
		}
	})
	if len(config.AuthenticationConfig) == 0 {
		return nil, fmt.Errorf("authentication configuration is required")
	}
	assets := os.Getenv(binaryAssetsEnv)
	if assets == "" {
		return nil, fmt.Errorf("%s is required; run make test-oidc-reference with the versions in make/tool-versions.mk", binaryAssetsEnv)
	}
	// #nosec G703 -- the test runner supplies this read-only binary directory.
	if info, err := os.Stat(assets); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s must name an existing directory", binaryAssetsEnv)
	}
	directory := tb.TempDir()
	configPath := filepath.Join(directory, "authentication-config.yaml")
	if err := os.WriteFile(configPath, config.AuthenticationConfig, 0o600); err != nil {
		return nil, fmt.Errorf("write authentication configuration: %w", err)
	}

	runner.environment = &controllerruntimeenvtest.Environment{
		BinaryAssetsDirectory: assets, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 45 * time.Second,
	}
	apiServer := runner.environment.ControlPlane.GetAPIServer()
	apiServer.Out, apiServer.Err = &runner.diagnostics, &runner.diagnostics
	apiServer.Configure().Append("authentication-config", configPath)
	adminConfig, err := runner.environment.Start()
	if err != nil {
		return nil, fmt.Errorf("start OIDC envtest: %w; diagnostics: %s", err, runner.Diagnostics())
	}
	runner.adminConfig = rest.CopyConfig(adminConfig)
	runner.adminClient, err = kubernetes.NewForConfig(runner.adminConfig)
	if err != nil {
		return nil, fmt.Errorf("create envtest admin client: %w", err)
	}
	return runner, nil
}

// AdminClient returns the certificate-authenticated setup client.
func (r *Runner) AdminClient() kubernetes.Interface { return r.adminClient }

// ProbeRequest copies only the endpoint and CA from the admin configuration.
func (r *Runner) ProbeRequest(token string, allowed, denied probe.ResourceRequest) probe.Request {
	return probe.Request{
		Endpoint: r.adminConfig.Host, CAPEM: append([]byte(nil), r.adminConfig.CAData...), Token: token,
		Allowed: allowed, Denied: denied,
	}
}

// Diagnostics returns bounded control-plane output with bearer headers removed.
func (r *Runner) Diagnostics() string {
	value := r.diagnostics.String()
	if len(value) > maximumDiagnosticsBytes {
		value = value[len(value)-maximumDiagnosticsBytes:]
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if start := strings.Index(strings.ToLower(line), "authorization: bearer "); start >= 0 {
			lines[index] = line[:start] + "Authorization: Bearer REDACTED"
		}
	}
	return strings.Join(lines, "\n")
}

// Close stops the control plane. It is safe to call more than once.
func (r *Runner) Close() error {
	r.closeOnce.Do(func() {
		if r.environment != nil {
			r.closeErr = r.environment.Stop()
		}
	})
	return r.closeErr
}

type lockedBuffer struct {
	mutex sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.Buffer.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.Buffer.String()
}
