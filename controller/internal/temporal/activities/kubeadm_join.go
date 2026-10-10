// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

type cachedJoinStrings struct {
	controlPlaneJoin string
	workerJoin       string
	fetchedAt        time.Time
	mu               sync.Mutex
}

// joinStringCache serializes and briefly reuses join-string refreshes per
// control-plane agent endpoint. The refresh RPC runs
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
		return KubeadmJoinResult{}, fmt.Errorf("agent returned empty kubeadm join string")
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to get node pool for node: %w", err)
	}

	joinConfig, err := kubeadmconfig.NewJoinConfigFromString(node.IP, node.Name, joinString)
	if err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to create kubeadm join config: %w", err)
	}

	joinConfig.KubeadmContainerMode, err = a.kubeadmContainerMode(ctx, cluster, node)
	if err != nil {
		return KubeadmJoinResult{}, err
	}
	joinConfig.KubernetesVersion = cluster.KubernetesVersion
	joinConfig.SystemReservedMemory = nodePool.SystemReservedMemory
	joinConfig.KubeReservedMemory = nodePool.KubeReservedMemory

	if authn := a.clusterAuthentication(cluster); node.ControlPlane && authn.Needed() {
		joinConfig.AuthenticationConfig, err = kubeadmconfig.BuildClusterAuthenticationConfig(ctx, a.k8sClient, authn)
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
		if err := a.validateEtcdQuota(ctx, cluster); err != nil {
			return KubeadmJoinResult{}, err
		}
	}
	if err := a.ensureKubeadmFiles(ctx, node, artifacts.Files); err != nil {
		return KubeadmJoinResult{}, fmt.Errorf("failed to prepare kubeadm files on node %s: %w", node.Name, err)
	}

	// ExecuteKubeadmJoin only starts the join; the agent runs kubeadm join in a
	// goroutine and reports progress via GetKubeadmStatus, which we poll below.
	// This is load-bearing for retries: if this activity times out or the
	// workflow retries while the agent is still joining, the re-fired
	// ExecuteKubeadmJoin returns an "already joining/joined" status that the
	// client intentionally does not surface as an error (see NodeAgentClient),
	// so we fall through to polling the in-flight join instead of starting a
	// second, overlapping kubeadm join.
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

func (a *Activities) validateEtcdQuota(ctx context.Context, cluster db.Cluster) error {
	controlPlane := true
	cpNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
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

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return err
	}

	for _, node := range joinedCPNodes {
		quota, err := extractEtcdQuotaFromNode(ctx, tenantClient, node.Name)
		if err != nil {
			return err
		}
		if err := validateEtcdQuotaValue(node.Name, quota); err != nil {
			return fmt.Errorf("%w. All etcd members must have the same quota value", err)
		}
	}

	quota, err := kubeadmconfig.EtcdQuotaInClusterConfiguration(ctx, tenantClient)
	if err != nil {
		return fmt.Errorf("failed to extract quota from kubeadm-config: %w", err)
	}
	if err := validateEtcdQuotaValue(joinedCPNodes[0].Name, quota); err != nil {
		return fmt.Errorf("%w. Kubeadm-config must have the same quota value", err)
	}

	return nil
}

func extractEtcdQuotaFromNode(ctx context.Context, tenantClient client.Client, nodeName string) (string, error) {
	var pod corev1.Pod
	if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "etcd-" + nodeName}, &pod); err != nil {
		return "", fmt.Errorf("failed to get etcd mirror Pod for node %s: %w", nodeName, err)
	}

	quota, err := etcdQuotaFromPod(pod)
	if err != nil {
		return "", fmt.Errorf("failed to extract etcd quota from node %s: %w", nodeName, err)
	}
	return quota, nil
}

func etcdQuotaFromPod(pod corev1.Pod) (string, error) {
	for _, container := range pod.Spec.Containers {
		if container.Name != "etcd" {
			continue
		}
		args := make([]string, 0, len(container.Command)+len(container.Args))
		args = append(args, container.Command...)
		args = append(args, container.Args...)
		for i, arg := range args {
			if quota, ok := strings.CutPrefix(arg, "--quota-backend-bytes="); ok && quota != "" {
				return quota, nil
			}
			if arg == "--quota-backend-bytes" && i+1 < len(args) && args[i+1] != "" {
				return args[i+1], nil
			}
		}
		return "", fmt.Errorf("quota-backend-bytes not found in etcd container command")
	}
	return "", fmt.Errorf("etcd container not found")
}

func validateEtcdQuotaValue(nodeName, quota string) error {
	if quota != kubeadmconfig.EtcdQuotaBackendBytes {
		return fmt.Errorf("quota-backend-bytes mismatch on node %s: expected %s, got %s",
			nodeName, kubeadmconfig.EtcdQuotaBackendBytes, quota)
	}
	return nil
}

func (a *Activities) getKubeadmJoinStrings(ctx context.Context, cpNode db.Node) (*nkeproto.KubeadmJoinStringResponse, error) {
	endpoint := net.JoinHostPort(cpNode.IP, a.cfg.NodeAgent.Port)
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
		return nil, fmt.Errorf("missing join strings from agent response")
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
