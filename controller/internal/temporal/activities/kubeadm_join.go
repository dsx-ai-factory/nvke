// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kubeadmconfig"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

type cachedJoinStrings struct {
	controlPlaneJoin string
	workerJoin       string
	fetchedAt        time.Time
	mu               sync.Mutex
}

// joinStringCache serializes and briefly reuses join-string refreshes per
// control-plane node-agent endpoint. The refresh RPC runs
// `kubeadm init phase upload-certs --upload-certs`, which rewrites the
// certificate key used by control-plane joins. If parallel joins refresh that
// key independently, one node can try to decrypt kubeadm-certs with a key that
// was superseded by another refresh.
var joinStringCache sync.Map //nolint:gochecknoglobals // Process-wide cache shared by activity workers.

type KubeadmJoinInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

type KubeadmJoinResult struct {
	NodeName      string
	NodeIP        string
	IsInitialNode bool
}

func (a *Activities) KubeadmJoin(ctx context.Context, input KubeadmJoinInput) (KubeadmJoinResult, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to get node: %w", err)
	}
	if node.Conditions.Joined.Status == db.ConditionStatusTrue {
		// Already joined
		return KubeadmJoinResult{
			NodeName:      node.Name,
			NodeIP:        node.IP,
			IsInitialNode: true,
		}, nil
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[KubeadmJoinResult]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: a.nodeProvisioningConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (KubeadmJoinResult, error) {
			return a.kubeadmJoin(ctx, node)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) kubeadmJoin(ctx context.Context, node db.Node) (KubeadmJoinResult, error) {
	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}
	cpNode, err := a.findJoinedControlPlaneNode(ctx, node.ClusterID)
	if err != nil {
		return KubeadmJoinResult{}, err
	}

	joinStrings, err := a.getKubeadmJoinStrings(ctx, cpNode)
	if err != nil {
		return KubeadmJoinResult{}, err
	}
	joinString := joinStrings.GetWorkerJoinString()
	if node.ControlPlane {
		joinString = joinStrings.GetControlplaneJoinString()
	}
	if joinString == "" {
		return KubeadmJoinResult{}, fmt.Errorf("node-agent returned empty kubeadm join string")
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to get node pool for node: %w", err)
	}

	joinConfig, err := kubeadmconfig.NewJoinConfigFromString(node.IP, node.Name, joinString, nodeLabels(nodePool.NodeLabels))
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to create kubeadm join config: %w", err)
	}

	joinConfig.KubeadmContainerMode, err = a.kubeadmContainerMode(ctx, cluster, node)
	if err != nil {
		return KubeadmJoinResult{}, err
	}

	if node.ControlPlane && cluster.OIDCAuthEnabled {
		joinConfig.AuthenticationConfig, err = a.buildOIDCAuthenticationConfig(ctx, cluster.Name)
		if err != nil {
			return KubeadmJoinResult{}, fmt.Errorf("failed to build OIDC authentication config: %w", err)
		}
	}

	artifacts, err := joinConfig.Render()
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to render kubeadm join config: %w", err)
	}

	req := &nkeproto.KubeadmJoinRequest{
		JoinConfig:        string(artifacts.ConfigYAML),
		KubeadmJoinString: joinString,
		Files:             toNodeAgentFiles(artifacts.Files),
	}

	if node.ControlPlane && cluster.EtcdEncrypted {
		req.ApiEncryptionKey, err = a.requireKubeAPIEncryptionKey(ctx, cluster.ID)
		if err != nil {
			return KubeadmJoinResult{}, err
		}
	}

	if node.ControlPlane {
		if err := a.validateEtcdQuota(ctx, cluster.ID); err != nil {
			return KubeadmJoinResult{}, err
		}
	}

	if _, err := a.nodeAgentClient.ExecuteKubeadmJoin(ctx, node, req); err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to execute kubeadm join on node %s: %w", node.Name, err)
	}

	if err := a.waitForKubeadmJoinReady(ctx, node); err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed waiting for kubeadm join on node %s: %w", node.Name, err)
	}

	return KubeadmJoinResult{
		NodeName: node.Name,
		NodeIP:   node.IP,
	}, nil
}

func (a *Activities) waitForKubeadmJoinReady(ctx context.Context, node db.Node) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		status, err := a.nodeAgentClient.GetKubeadmStatus(ctx, node)
		if err != nil {
			return fmt.Errorf("failed to get kubeadm status: %w", err)
		}
		kubeadmStatus := strings.ToLower(status.GetKubeadmStatus())
		kubeadmPhase := strings.ToLower(status.GetKubeadmPhase())

		if kubeadmStatus == kubeadmStatusError {
			return fmt.Errorf("kubeadm join failed: %s", status.GetKubeadmCurrentMessage())
		}

		if kubeadmStatus == kubeadmStatusReady && kubeadmPhase == kubeadmPhaseJoin {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *Activities) findJoinedControlPlaneNode(ctx context.Context, clusterID uuid.UUID) (db.Node, error) {
	controlPlane := true
	nodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to list control-plane nodes: %w", err)
	}
	for _, node := range nodes {
		if node.Conditions.Joined.Status == db.ConditionStatusTrue && node.IP != "" {
			return node, nil
		}
	}
	return db.Node{}, fmt.Errorf("no joined control-plane node found for cluster %s", clusterID)
}

func (a *Activities) validateEtcdQuota(ctx context.Context, clusterID uuid.UUID) error {
	controlPlane := true
	cpNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return fmt.Errorf("failed to list control-plane nodes: %w", err)
	}

	var joinedCPNodes []db.Node
	for _, node := range cpNodes {
		if node.Conditions.Joined.Status == db.ConditionStatusTrue && node.IP != "" {
			joinedCPNodes = append(joinedCPNodes, node)
		}
	}
	if len(joinedCPNodes) == 0 {
		return fmt.Errorf("no joined control-plane nodes found for etcd quota validation")
	}

	for _, node := range joinedCPNodes {
		quota, err := a.extractEtcdQuotaFromNode(ctx, node.ID)
		if err != nil {
			return err
		}
		if err := validateEtcdQuotaValue(node.Name, quota); err != nil {
			return fmt.Errorf("%w. All etcd members must have the same quota value", err)
		}
	}

	quota, err := a.extractKubeadmConfigEtcdQuota(ctx, joinedCPNodes[0].ID)
	if err != nil {
		return fmt.Errorf("failed to extract quota from kubeadm-config: %w", err)
	}
	if err := validateEtcdQuotaValue(joinedCPNodes[0].Name, quota); err != nil {
		return fmt.Errorf("%w. Kubeadm-config must have the same quota value", err)
	}

	return nil
}

func (a *Activities) extractEtcdQuotaFromNode(ctx context.Context, nodeID uuid.UUID) (string, error) {
	result, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      nodeID,
		RunbookName: "manifest-etcd-quota",
	})
	if err != nil {
		return "", fmt.Errorf("failed to extract etcd quota from node %s: %w", nodeID, err)
	}
	quota := strings.TrimSpace(result.Output)
	if quota == "" {
		return "", fmt.Errorf("empty quota-backend-bytes returned on node %s", nodeID)
	}
	return quota, nil
}

func (a *Activities) extractKubeadmConfigEtcdQuota(ctx context.Context, nodeID uuid.UUID) (string, error) {
	result, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      nodeID,
		RunbookName: "kubeadm-etcd-quota",
	})
	if err != nil {
		return "", fmt.Errorf("failed to extract kubeadm-config etcd quota from node %s: %w", nodeID, err)
	}
	quota := strings.TrimSpace(result.Output)
	if quota == "" {
		return "", fmt.Errorf("empty quota-backend-bytes returned from kubeadm-config")
	}
	return quota, nil
}

func validateEtcdQuotaValue(nodeName, quota string) error {
	if quota != kubeadmconfig.EtcdQuotaBackendBytes {
		return fmt.Errorf("quota-backend-bytes mismatch on node %s: expected %s, got %s",
			nodeName, kubeadmconfig.EtcdQuotaBackendBytes, quota)
	}
	return nil
}

func (a *Activities) getKubeadmJoinStrings(ctx context.Context, cpNode db.Node) (*nkeproto.KubeadmJoinStringResponse, error) {
	endpoint := net.JoinHostPort(cpNode.IP, a.cfg.NodeAgentPort)
	entry, _ := joinStringCache.LoadOrStore(endpoint, &cachedJoinStrings{})
	cached, ok := entry.(*cachedJoinStrings)
	if !ok {
		return nil, fmt.Errorf("join string cache contained unexpected type %T", entry)
	}

	cached.mu.Lock()
	defer cached.mu.Unlock()

	if cached.controlPlaneJoin != "" && cached.workerJoin != "" && time.Since(cached.fetchedAt) < joinStringCacheTTL {
		return &nkeproto.KubeadmJoinStringResponse{
			ControlplaneJoinString: cached.controlPlaneJoin,
			WorkerJoinString:       cached.workerJoin,
		}, nil
	}

	resp, err := a.nodeAgentClient.UpdateAndFetchKubeAdmJoinStrings(ctx, cpNode, &nkeproto.KubeadmJoinStringRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetControlplaneJoinString() == "" || resp.GetWorkerJoinString() == "" {
		return nil, fmt.Errorf("missing join strings from node-agent response")
	}

	cached.controlPlaneJoin = stripJoinString(resp.GetControlplaneJoinString())
	cached.workerJoin = stripJoinString(resp.GetWorkerJoinString())
	cached.fetchedAt = time.Now()

	return &nkeproto.KubeadmJoinStringResponse{
		ControlplaneJoinString: cached.controlPlaneJoin,
		WorkerJoinString:       cached.workerJoin,
	}, nil
}

func stripJoinString(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "\\", "")
	return s
}
