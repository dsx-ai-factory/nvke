// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/cwe"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
)

const openVSwitchMetricName = "nke_controller_open_vswitch_reconciliation_total"

func validateOpenVSwitchPostCreate(test *tenantTest) {
	It("configures Open vSwitch through the typed node-agent RPC", func(ctx context.Context) {
		restConfig, err := kube.RESTConfig(test.cfg.Kube.Kubeconfig, test.cfg.Kube.Context)
		Expect(err).NotTo(HaveOccurred(), "read management-cluster kubeconfig")
		metrics, err := kube.SharedServiceForwarder(restConfig, cwe.Namespace(), cwe.Service(), nkeControllerMetricsPort)
		Expect(err).NotTo(HaveOccurred(), "build a port-forward to NKE controller metrics")
		expectedNodes := totalExpectedNodes(expectedNodeCounts(test.spec))

		// Node readiness above is the behavioral assertion: OVN cannot converge
		// without the host bridge. This metric assertion identifies which of the
		// two implementations produced that behavior during a mixed-agent rollout.
		Eventually(func(ctx context.Context) error {
			statusCode, body, getErr := metrics.Get(ctx, metricsPath)
			if getErr != nil {
				return getErr
			}
			if statusCode != http.StatusOK {
				return fmt.Errorf("GET %s returned %d: %s", metricsPath, statusCode, body)
			}
			rpcSuccess, fallback, parseErr := openVSwitchPathCounts(string(body), test.cluster.Name)
			if parseErr != nil {
				return parseErr
			}
			if fallback != 0 {
				return fmt.Errorf("legacy Open vSwitch fallback ran %d times", fallback)
			}
			if rpcSuccess < expectedNodes {
				return fmt.Errorf("typed Open vSwitch RPC has %d successful node reconciliations, want at least %d", rpcSuccess, expectedNodes)
			}
			return nil
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	}, Label("openvswitch"), SpecTimeout(2*time.Minute))
}

func openVSwitchPathCounts(exposition, clusterName string) (int, int, error) {
	clusterLabel := `tenant_cluster="` + clusterName + `"`
	rpcNodes := map[string]struct{}{}
	fallback := 0
	for _, line := range strings.Split(exposition, "\n") {
		openIdx := strings.Index(line, "{")
		closeIdx := strings.LastIndex(line, "}")
		if openIdx < 0 || closeIdx < openIdx || line[:openIdx] != openVSwitchMetricName {
			continue
		}
		labels := line[openIdx+1 : closeIdx]
		if !strings.Contains(labels, clusterLabel) {
			continue
		}
		fields := strings.Fields(line[closeIdx+1:])
		if len(fields) == 0 {
			return 0, 0, fmt.Errorf("malformed exposition line %q", line)
		}
		value, parseErr := strconv.ParseFloat(fields[0], 64)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("parse value from %q: %w", line, parseErr)
		}
		switch {
		case strings.Contains(labels, `path="rpc"`) && strings.Contains(labels, `result="success"`):
			nodeName, found := openVSwitchMetricLabel(labels, "node_name")
			if !found {
				return 0, 0, fmt.Errorf("Open vSwitch RPC metric has no node_name label: %q", line)
			}
			rpcNodes[nodeName] = struct{}{}
		case strings.Contains(labels, `path="legacy_runbook"`):
			fallback += int(value)
		}
	}
	return len(rpcNodes), fallback, nil
}

func openVSwitchMetricLabel(labels, name string) (string, bool) {
	prefix := name + `="`
	start := strings.Index(labels, prefix)
	if start < 0 {
		return "", false
	}
	value := labels[start+len(prefix):]
	end := strings.Index(value, `"`)
	if end < 0 {
		return "", false
	}
	return value[:end], true
}

func TestOpenVSwitchPathCounts(t *testing.T) {
	exposition := `# TYPE nke_controller_open_vswitch_reconciliation_total counter
nke_controller_open_vswitch_reconciliation_total{node_name="cp-0",path="rpc",result="success",tenant_cluster="e2e-nico-vms"} 2
nke_controller_open_vswitch_reconciliation_total{node_name="cp-1",path="rpc",result="success",tenant_cluster="e2e-nico-vms"} 1
nke_controller_open_vswitch_reconciliation_total{node_name="other",path="legacy_runbook",result="success",tenant_cluster="another-cluster"} 7
nke_controller_open_vswitch_reconciliation_total{node_name="cp-2",path="legacy_runbook",result="failure",tenant_cluster="e2e-nico-vms"} 1
`

	rpcSuccess, fallback, err := openVSwitchPathCounts(exposition, "e2e-nico-vms")
	if err != nil {
		t.Fatalf("openVSwitchPathCounts: %v", err)
	}
	if rpcSuccess != 2 || fallback != 1 {
		t.Fatalf("openVSwitchPathCounts = (%d, %d), want (2, 1)", rpcSuccess, fallback)
	}
}
