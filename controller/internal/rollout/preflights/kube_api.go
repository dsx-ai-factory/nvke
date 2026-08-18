// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights

import (
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

const (
	PreflightNameKubeAPIHealthy = "cwe.kube-api-healthy"
)

// NewKubeAPIHealthyFunc probes /readyz?verbose on every CP and gates
// on healthyRequired(N). In the default-on set.
func NewKubeAPIHealthyFunc(dialer nodeagent.Dialer) rollout.ClusterPreflightFunc {
	return func(ctx context.Context, cluster rollout.Cluster, _ map[string]string) error {
		cps := cluster.ControlPlaneNodes()
		n := len(cps)
		if n == 0 {
			return fmt.Errorf("kube-api-healthy: cluster %s has 0 CPs", cluster.Name())
		}
		threshold := healthyRequired(n)
		healthy := 0
		var failed []string
		for _, cp := range cps {
			detail, ok := probeKubeReadyz(ctx, dialer, cp)
			if ok {
				healthy++
				continue
			}
			failed = append(failed, fmt.Sprintf("%s (%s)", cp.Name(), detail))
		}
		if healthy < threshold {
			return fmt.Errorf("kube-api-healthy: %d/%d CPs healthy (need >= %d for safe rollout); failing=[%s]",
				healthy, n, threshold, strings.Join(failed, ", "))
		}
		return nil
	}
}

// probeKubeReadyz probes one CP's /readyz?verbose. Returns
// (detail, healthy); fail-closed on indeterminate output.
func probeKubeReadyz(
	ctx context.Context, dialer nodeagent.Dialer, cp rollout.Node,
) (string, bool) {
	code, stdout, stderr, err := execScriptOnNode(ctx, dialer, cp,
		"preflight-kube-readyz", kubectlRawLocal("/readyz?verbose"))
	if err != nil {
		return fmt.Sprintf("probe err: %v", err), false
	}
	failing, sawAny := parseReadyzVerbose(stdout)
	if code != 0 {
		// --raw bypasses kubectl error formatting, so failing checks
		// can still appear in stdout when code != 0.
		if len(failing) > 0 {
			return fmt.Sprintf("failing checks: %v", failing), false
		}
		return fmt.Sprintf("exit=%d stderr=%q", code, strings.TrimSpace(stderr)), false
	}
	if !sawAny {
		return fmt.Sprintf("no readyz markers in %d-byte response", len(stdout)), false
	}
	if len(failing) > 0 {
		return fmt.Sprintf("failing checks: %v", failing), false
	}
	return "", true
}
