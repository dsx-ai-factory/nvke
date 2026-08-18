// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights

import (
	"context"
	"fmt"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

const (
	PreflightNameNodeAgentReachable = "cwe.agent-reachable"
	PreflightNameNodeAgentReady     = "cwe.agent-ready"
	PreflightNameNodeAgentVersion   = "cwe.agent-version"

	// ParamMinVersion is the NodeAgentVersion params key; semver-ish
	// like "v0.1.21" ("v" optional). Required; fails closed otherwise.
	ParamMinVersion = "min_version"
)

// NewNodeAgentReachableFunc dials the agent on every CP. Runs
// FIRST in the default set so an unreachable agent surfaces as itself
// rather than as a misleading apiserver-health failure later.
func NewNodeAgentReachableFunc(dialer nodeagent.Dialer) rollout.ClusterPreflightFunc {
	return func(ctx context.Context, cluster rollout.Cluster, _ map[string]string) error {
		cps := cluster.ControlPlaneNodes()
		if len(cps) == 0 {
			return fmt.Errorf("agent-reachable: cluster %s has 0 CPs", cluster.Name())
		}
		var unreachable []string
		var firstErr error
		for _, cp := range cps {
			if _, err := dialAndStatus(ctx, dialer, cp); err != nil {
				unreachable = append(unreachable, cp.Name())
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(unreachable) > 0 {
			return fmt.Errorf("agent-reachable: %d/%d CPs reachable; unreachable=%v; first err: %w",
				len(cps)-len(unreachable), len(cps), unreachable, firstErr)
		}
		return nil
	}
}

// NewNodeAgentReadyFunc passes only when the kubeadm status is
// "ready". Opt-in; use it before operations that mutate
// kubeadm-managed state.
func NewNodeAgentReadyFunc(dialer nodeagent.Dialer) rollout.NodePreflightFunc {
	return func(ctx context.Context, _ rollout.Cluster, node rollout.Node, _ map[string]string) error {
		st, err := dialAndStatus(ctx, dialer, node)
		if err != nil {
			return fmt.Errorf("agent on %s: %w", node.Name(), err)
		}
		if st.GetKubeadmStatus() != nodeagent.KubeadmStatusReady {
			return fmt.Errorf("agent on %s not ready: status=%q phase=%q message=%q",
				node.Name(), st.GetKubeadmStatus(), st.GetKubeadmPhase(), st.GetKubeadmCurrentMessage())
		}
		return nil
	}
}

// NewNodeAgentVersionFunc gates on params["min_version"]. Opt-in; use
// it for operations that need an agent API older binaries lack.
// Missing/unparseable min_version fails closed.
func NewNodeAgentVersionFunc(dialer nodeagent.Dialer) rollout.NodePreflightFunc {
	return func(ctx context.Context, _ rollout.Cluster, node rollout.Node, params map[string]string) error {
		minimum := params[ParamMinVersion]
		if minimum == "" {
			return fmt.Errorf("preflight %s requires param %q on the NodePreflightRef",
				PreflightNameNodeAgentVersion, ParamMinVersion)
		}
		st, err := dialAndStatus(ctx, dialer, node)
		if err != nil {
			return fmt.Errorf("agent on %s: %w", node.Name(), err)
		}
		if !versionAtLeast(st.GetAgentVersion(), minimum) {
			return fmt.Errorf("agent on %s is %q; operation requires >= %q",
				node.Name(), st.GetAgentVersion(), minimum)
		}
		return nil
	}
}
