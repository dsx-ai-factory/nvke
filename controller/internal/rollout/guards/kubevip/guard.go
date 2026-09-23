// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

const (
	GuardNameKubeVipDrainRestore = "cwe.kube-vip-drain-restore"
)

type KubeVipDrainRestoreGuard struct {
	dialer nodeAgentKubeVipDialer
}

var (
	_ rollout.NodeGuard                 = (*KubeVipDrainRestoreGuard)(nil)
	_ rollout.NodeGuardPreApplyChecker  = (*KubeVipDrainRestoreGuard)(nil)
	_ rollout.NodeGuardPostApplyChecker = (*KubeVipDrainRestoreGuard)(nil)
)

func NewGuard(dialer nodeagent.Dialer) *KubeVipDrainRestoreGuard {
	return &KubeVipDrainRestoreGuard{
		dialer: adaptDialer(dialer),
	}
}

func (g *KubeVipDrainRestoreGuard) Name() string {
	return GuardNameKubeVipDrainRestore
}

func (g *KubeVipDrainRestoreGuard) PreApply(
	ctx context.Context,
	cluster rollout.Cluster,
	node rollout.Node,
) (rollout.GuardState, error) {
	stateless := rollout.StatelessGuardState(g.Name())
	if cluster == nil {
		return stateless, errors.New("cluster is nil")
	}
	if node == nil {
		return stateless, errors.New("node is nil")
	}
	if node.Role() != rollout.NodeRoleControlPlane {
		return stateless, fmt.Errorf("node %s has role %q; kube-vip drain only supports control-plane nodes",
			node.Name(), node.Role())
	}

	state := g.newState(ctx, cluster, node)
	guardState, err := state.toGuardState(g.Name())
	if err != nil {
		return stateless, err
	}
	// Persist the operation IDs in workflow history before dispatching the
	// drain. CheckPreApply owns the idempotent Start RPC. If that activity is
	// canceled or loses its response after agent accepted the request, the
	// executor already has guardState and can run PostApply on its disconnected
	// cleanup context.
	return guardState, nil
}

func (g *KubeVipDrainRestoreGuard) CheckPreApply(
	ctx context.Context,
	_ rollout.Cluster,
	node rollout.Node,
	guardState rollout.GuardState,
) (bool, error) {
	state, err := stateFromGuardState(guardState)
	if err != nil {
		return false, err
	}
	if state == nil {
		return true, nil
	}
	if state.DrainOperationID == "" {
		return false, errors.New("kube-vip guard state missing drain operation ID")
	}
	client, cleanup, err := g.dialNode(ctx, node)
	if err != nil {
		return false, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if _, err := client.StartK8SControlPlaneNodeGracefulShutdown(ctx, &nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest{
		OperationId:       state.DrainOperationID,
		StopKubeApiserver: false,
	}); err != nil {
		return false, fmt.Errorf("start kube-vip drain on node %s: %w", node.Name(), err)
	}
	resp, err := client.GetK8SControlPlaneNodeGracefulShutdownStatus(ctx, &nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest{
		OperationId: state.DrainOperationID,
	})
	if err != nil {
		return false, fmt.Errorf("get kube-vip drain status on node %s: %w", node.Name(), err)
	}
	done, err := nodeAgentOperationDone(resp.GetOperation())
	if err != nil {
		return false, fmt.Errorf("wait for kube-vip drain on node %s: %w", node.Name(), err)
	}
	return done, nil
}

func (g *KubeVipDrainRestoreGuard) PostApply(
	ctx context.Context,
	_ rollout.Cluster,
	node rollout.Node,
	_ rollout.Outcome,
	guardState rollout.GuardState,
) error {
	state, err := stateFromGuardState(guardState)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	client, cleanup, err := g.dialNode(ctx, node)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	_, err = client.StartKubeVipRestore(ctx, &nkeproto.StartKubeVipRestoreRequest{
		OperationId: state.RestoreOperationID,
	})
	if err != nil {
		return fmt.Errorf("start kube-vip restore on node %s: %w", node.Name(), err)
	}
	return nil
}

func (g *KubeVipDrainRestoreGuard) CheckPostApply(
	ctx context.Context,
	_ rollout.Cluster,
	node rollout.Node,
	_ rollout.Outcome,
	guardState rollout.GuardState,
) (bool, error) {
	state, err := stateFromGuardState(guardState)
	if err != nil {
		return false, err
	}
	if state == nil {
		return true, nil
	}
	client, cleanup, err := g.dialNode(ctx, node)
	if err != nil {
		return false, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	resp, err := client.StartKubeVipRestore(ctx, &nkeproto.StartKubeVipRestoreRequest{
		OperationId: state.RestoreOperationID,
	})
	if err != nil {
		return false, fmt.Errorf("get kube-vip restore status on node %s: %w", node.Name(), err)
	}
	done, err := nodeAgentOperationDone(resp.GetOperation())
	if err != nil {
		return false, fmt.Errorf("wait for kube-vip restore on node %s: %w", node.Name(), err)
	}
	return done, nil
}

func (g *KubeVipDrainRestoreGuard) dialNode(ctx context.Context, node rollout.Node) (nodeAgentKubeVipClient, func(), error) {
	if node == nil {
		return nil, nil, errors.New("node is nil")
	}
	if g.dialer == nil {
		return nil, nil, errors.New("kube-vip guard: dialer not configured")
	}
	client, cleanup, err := g.dialer(ctx, node)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent on node %s: %w", node.Name(), err)
	}
	if client == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("dial agent on node %s returned nil client", node.Name())
	}
	return client, cleanup, nil
}

type guardPayload struct {
	DrainOperationID   string `json:"drainOperationId"`
	RestoreOperationID string `json:"restoreOperationId"`
}

func (g *KubeVipDrainRestoreGuard) newState(ctx context.Context, cluster rollout.Cluster, node rollout.Node) guardPayload {
	seed := operationIDSeed(ctx, cluster, node)
	return guardPayload{
		DrainOperationID:   operationID("kube-vip-drain", seed),
		RestoreOperationID: operationID("kube-vip-restore", seed),
	}
}

func (s guardPayload) toGuardState(name string) (rollout.GuardState, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return rollout.GuardState{}, fmt.Errorf("marshal kube-vip guard state: %w", err)
	}
	return rollout.GuardState{GuardName: name, Payload: payload}, nil
}

func stateFromGuardState(state rollout.GuardState) (*guardPayload, error) {
	if len(state.Payload) == 0 {
		// No persisted state: a stateless guard, not an error. Callers
		// treat a nil payload as "nothing to check/restore" and proceed.
		//nolint:nilnil // (nil, nil) is the intentional "no state" signal
		return nil, nil
	}
	var out guardPayload
	if err := json.Unmarshal(state.Payload, &out); err != nil {
		return nil, fmt.Errorf("unmarshal kube-vip guard state: %w", err)
	}
	if out.RestoreOperationID == "" {
		return nil, errors.New("kube-vip guard state missing restore operation ID")
	}
	return &out, nil
}
