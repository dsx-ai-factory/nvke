// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kubebench"
)

func TestCISScanReport(t *testing.T) {
	out := kubebench.Outcome{Cluster: kubebench.ClusterTenant}
	writeErr := fmt.Errorf("%w: creating /out/kube-bench: no space left on device", kubebench.ErrArtifacts)

	for _, tc := range []struct {
		name    string
		out     kubebench.Outcome
		err     error
		want    []string
		notWant string
	}{
		{"a scan that ran reports its summary", out, nil, []string{out.Summary()}, "did not"},
		{"a scan whose results were not written still reports its summary", out, writeErr, []string{out.Summary(), "results were not saved: " + writeErr.Error()}, "did not run"},
		{"a scan that did not run says why", kubebench.Outcome{}, errors.New("no control-plane node"), []string{"the scan did not run: no control-plane node"}, "CIS benchmark scan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cisScanReport(tc.out, tc.err)
			for _, want := range tc.want {
				assert.Contains(t, got, want)
			}
			assert.NotContains(t, got, tc.notWant)
		})
	}
}

// TestTenantAdminConfigGivesUpAfterItsTimeout checks that a stalled read of the
// tenant's kubeconfig returns an error once its timeout passes, rather than
// holding the spec until its NodeTimeout.
func TestTenantAdminConfigGivesUpAfterItsTimeout(t *testing.T) {
	release := make(chan struct{})
	management := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(management.Close)
	t.Cleanup(func() { close(release) })

	clients, err := kube.BuildTenantClientFactory(&rest.Config{Host: management.URL})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := tenantAdminConfig(t.Context(), clients, "tenant", 100*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "getting the tenant's admin config")
	case <-time.After(10 * time.Second):
		t.Fatal("tenantAdminConfig did not give up after its timeout")
	}
}
