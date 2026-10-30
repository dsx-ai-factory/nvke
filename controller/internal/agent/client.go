// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

//nolint:interfacebloat // This mirrors the agent RPC surface used by controllers and activities.
type Client interface {
	GetKubeconfig(ctx context.Context, node db.Node) (string, error)
	GetAgentVersion(ctx context.Context, node db.Node) (string, error)
	GetEtcdEncryptionKeyIdentity(ctx context.Context, node db.Node, digest string) (string, error)
	SupportsKubeadmFiles(ctx context.Context, node db.Node) (bool, error)
	GetKubeadmStatus(ctx context.Context, node db.Node) (*nvkeproto.KubeadmStatusResponse, error)
	ScheduleNodeReboot(ctx context.Context, node db.Node) (*nvkeproto.ScheduleNodeRebootResponse, error)
	GetNodeBootInfo(ctx context.Context, node db.Node) (*nvkeproto.GetNodeBootInfoResponse, error)
	ExecuteKubeadmInit(ctx context.Context, node db.Node, req *nvkeproto.KubeadmInitRequest) (*nvkeproto.KubeadmInitStatusResponse, error)
	InstallCNI(ctx context.Context, node db.Node, req *nvkeproto.InstallCNIRequest) (*nvkeproto.InstallCNIResponse, error)
	ExecuteKubeadmJoin(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinRequest) (*nvkeproto.KubeadmJoinStatusResponse, error)
	ExecuteKubeadmReset(ctx context.Context, node db.Node, req *nvkeproto.KubeadmResetRequest) (*nvkeproto.KubeadmResetStatusResponse, error)
	ReconfigureKubeletAPIServer(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureKubeletAPIServerRequest) (*nvkeproto.ReconfigureKubeletAPIServerResponse, error)
	StartOpenVSwitchReconciliation(ctx context.Context, node db.Node, req *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error)
	GetOpenVSwitchReconciliationStatus(ctx context.Context, node db.Node, req *nvkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error)
	KubeadmUpgradeApply(ctx context.Context, node db.Node, req *nvkeproto.KubeadmUpgradeApplyRequest) (*nvkeproto.KubeadmUpgradeApplyResponse, error)
	ReconcileKubeVip(ctx context.Context, node db.Node, req *nvkeproto.ReconcileKubeVipRequest) (*nvkeproto.ReconcileKubeVipResponse, error)
	StartKubernetesCertificateRenewal(ctx context.Context, node db.Node, req *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error)
	StartKubernetesCertificateRenewalV2(ctx context.Context, node db.Node, req *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error)
	GetKubernetesCertificateRenewalStatus(ctx context.Context, node db.Node, req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
	RemoveEtcdMember(ctx context.Context, node db.Node, req *nvkeproto.RemoveEtcdMemberRequest) (*nvkeproto.RemoveEtcdMemberResponse, error)
	ExecuteNodePreflight(ctx context.Context, node db.Node, req *nvkeproto.ExecuteNodePreflightRequest) (*nvkeproto.ExecuteNodePreflightResponse, error)
	GetKubeadmJoinString(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error)
	UpdateAndFetchKubeAdmJoinStrings(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error)
	StartK8sControlPlaneNodeGracefulShutdown(ctx context.Context, node db.Node, req *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error)
	GetK8sControlPlaneNodeGracefulShutdownStatus(ctx context.Context, node db.Node, req *nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error)
	ExecuteScript(ctx context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nvkeproto.ExecuteScriptResponse, error)
	PatchAPIServerManifest(ctx context.Context, node db.Node, req *nvkeproto.PatchAPIServerManifestRequest) (*nvkeproto.PatchAPIServerManifestResponse, error)
	InitializeAPIEncryption(ctx context.Context, node db.Node, encryptionKey string) error
	PromoteAPIEncryptionProviders(ctx context.Context, node db.Node) error
	StartEncryptedObjectsRewrite(ctx context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	GetEncryptedObjectsRewriteStatus(ctx context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	RemovePlainTextSecondaryEncryptionEntry(ctx context.Context, node db.Node) error
	StartKubeVIPRestore(ctx context.Context, node db.Node, operationID string) (*nvkeproto.NodeAgentOperationStatus, error)
	ConvergeNodeAgent(ctx context.Context, node db.Node, target, method string) (string, bool, error)
	ReconfigureSpireAgent(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureSpireAgentRequest) (*nvkeproto.ReconfigureSpireAgentResponse, error)
	GetSpireAgentStatus(ctx context.Context, node db.Node) (*nvkeproto.GetSpireAgentStatusResponse, error)
	ReconfigureNodeAgentArgs(ctx context.Context, node db.Node, args []string) (*nvkeproto.ReconfigureNodeAgentArgsResponse, error)
}

type NodeAgentClient struct {
	transport *Transport
}

// NewClient builds a node-agent client with its own transport. Callers that
// also need the transport directly, such as the rollout subsystem, should build
// one with NewTransport and use NewClientWithTransport so a deployment has a
// single dial path.
func NewClient(
	ctx context.Context,
	cfg config.Config,
	trustDomain string,
	logger *slog.Logger,
) (*NodeAgentClient, error) {
	transport, err := NewTransport(ctx, cfg, trustDomain, logger)
	if err != nil {
		return nil, err
	}
	return NewClientWithTransport(transport), nil
}

// NewClientWithTransport wraps an existing transport, so the client and every
// other node-agent caller share one authenticated dial path.
func NewClientWithTransport(transport *Transport) *NodeAgentClient {
	return &NodeAgentClient{transport: transport}
}

func (c *NodeAgentClient) getGRPCConnection(ctx context.Context, node db.Node) (*grpc.ClientConn, func(), error) {
	return c.transport.Dial(ctx, node)
}

func (c *NodeAgentClient) GetKubeconfig(ctx context.Context, node db.Node) (string, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig from node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetKubeconfig(ctx, &nvkeproto.KubeconfigRequest{})
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig from node agent for node %s: %w", node.Name, err)
	}

	return resp.GetKubeconfig(), nil
}

func (c *NodeAgentClient) GetAgentVersion(ctx context.Context, node db.Node) (string, error) {
	// agent doesn't actually expose a GetAgentVersion RPC, so we use GetKubeadmStatus as a proxy.
	//
	// GetKubeadmStatus is used purely as a reachability probe (drives
	// NodeAgentReady) and to read the agent version. It is the lightest
	// read RPC the agent exposes that carries AgentVersion; the rest
	// of its kubeadm status payload is intentionally ignored, since node
	// health is sourced from the workload k8s Node object.
	status, err := c.GetKubeadmStatus(ctx, node)
	if err != nil {
		return "", fmt.Errorf("failed to get agent version from node agent for node %s: %w", node.Name, err)
	}
	return status.GetAgentVersion(), nil
}

func (c *NodeAgentClient) GetKubeadmStatus(ctx context.Context, node db.Node) (*nvkeproto.KubeadmStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm status from node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nvkeproto.KubeadmStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm status from node agent for node %s: %w", node.Name, err)
	}

	return resp, nil
}

func (c *NodeAgentClient) ScheduleNodeReboot(ctx context.Context, node db.Node) (*nvkeproto.ScheduleNodeRebootResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for reboot on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ScheduleNodeReboot(ctx, &nvkeproto.ScheduleNodeRebootRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support ScheduleNodeReboot; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to schedule reboot on node %s: %w", node.Name, err)
	}

	return resp, nil
}

func (c *NodeAgentClient) GetNodeBootInfo(ctx context.Context, node db.Node) (*nvkeproto.GetNodeBootInfoResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for boot info on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetNodeBootInfo(ctx, &nvkeproto.GetNodeBootInfoRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support GetNodeBootInfo; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get boot info from node %s: %w", node.Name, err)
	}

	return resp, nil
}

// PatchAPIServerManifest applies a strategic-merge patch plus typed flag ops to the
// kube-apiserver static-pod manifest on a control-plane node (used by the OIDC
// enable/disable rollout). The agent snapshots and self-restores on failure and
// returns a restored-vs-not-restored outcome so the rollout can halt vs cascade.
func (c *NodeAgentClient) PatchAPIServerManifest(ctx context.Context, node db.Node, req *nvkeproto.PatchAPIServerManifestRequest) (*nvkeproto.PatchAPIServerManifestResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to patch apiserver manifest on node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).PatchAPIServerManifest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to patch apiserver manifest on node agent for node %s: %w", node.Name, err)
	}

	return resp, nil
}

// InstallCNI is synchronous: no status to poll afterwards.
func (c *NodeAgentClient) InstallCNI(ctx context.Context, node db.Node, req *nvkeproto.InstallCNIRequest) (*nvkeproto.InstallCNIResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to install CNI on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).InstallCNI(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to install CNI on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmInit(ctx context.Context, node db.Node, req *nvkeproto.KubeadmInitRequest) (*nvkeproto.KubeadmInitStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}
	defer cleanup()

	if err := requireKubeadmEncryptionKeyName(ctx, conn, req.GetApiEncryptionKeyName()); err != nil {
		return nil, err
	}
	resp, err := nvkeproto.NewNodeAgentClient(conn).ExecuteKubeadmInit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmJoin(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinRequest) (*nvkeproto.KubeadmJoinStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm join on node %s: %w", node.Name, err)
	}
	defer cleanup()

	if err := requireKubeadmEncryptionKeyName(ctx, conn, req.GetApiEncryptionKeyName()); err != nil {
		return nil, err
	}
	// Returns only the RPC execution error; resp.Status is parsed by the caller
	// via the GetKubeadmStatus poll loop (see Activities.kubeadmJoin). Do not
	// promote resp.Status to a Go error here: a re-fired join on an already
	// joining/joined agent returns an error status, and callers rely on that
	// being a non-error to stay idempotent under retries.
	resp, err := nvkeproto.NewNodeAgentClient(conn).ExecuteKubeadmJoin(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm join on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmReset(ctx context.Context, node db.Node, req *nvkeproto.KubeadmResetRequest) (*nvkeproto.KubeadmResetStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm reset on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ExecuteKubeadmReset(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm reset on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ReconfigureKubeletAPIServer(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureKubeletAPIServerRequest) (*nvkeproto.ReconfigureKubeletAPIServerResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ReconfigureKubeletAPIServer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure kubelet API server on node %s: %w", node.Name, err)
	}
	return resp, nil
}

// ReconfigureSpireAgent points the node's SPIRE agent at a new SPIRE server and
// waits until the agent holds an SVID from it.
func (c *NodeAgentClient) ReconfigureSpireAgent(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureSpireAgentRequest) (*nvkeproto.ReconfigureSpireAgentResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ReconfigureSpireAgent(ctx, req)
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support ReconfigureSpireAgent; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure SPIRE agent on node %s: %w", node.Name, err)
	}
	return resp, nil
}

// ReconfigureNodeAgentArgs replaces the node agent's own startup arguments in
// its environment file, which is otherwise written once at bootstrap. The agent
// reads them only at startup, so the call changes nothing on the running
// process; the response reports whether a restart is still pending.
func (c *NodeAgentClient) ReconfigureNodeAgentArgs(ctx context.Context, node db.Node, args []string) (*nvkeproto.ReconfigureNodeAgentArgsResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ReconfigureNodeAgentArgs(ctx, &nvkeproto.ReconfigureNodeAgentArgsRequest{Args: args})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support ReconfigureNodeAgentArgs; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure node-agent arguments on node %s: %w", node.Name, err)
	}
	return resp, nil
}

// GetSpireAgentStatus reports the SPIRE server the node's agent points at and
// whether it holds a valid SVID from that server.
func (c *NodeAgentClient) GetSpireAgentStatus(ctx context.Context, node db.Node) (*nvkeproto.GetSpireAgentStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetSpireAgentStatus(ctx, &nvkeproto.GetSpireAgentStatusRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support GetSpireAgentStatus; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get SPIRE agent status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartOpenVSwitchReconciliation(
	ctx context.Context,
	node db.Node,
	req *nvkeproto.StartOpenVSwitchReconciliationRequest,
) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).StartOpenVSwitchReconciliation(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetOpenVSwitchReconciliationStatus(
	ctx context.Context,
	node db.Node,
	req *nvkeproto.GetOpenVSwitchReconciliationStatusRequest,
) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for Open vSwitch reconciliation status on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetOpenVSwitchReconciliationStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get Open vSwitch reconciliation status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) KubeadmUpgradeApply(ctx context.Context, node db.Node, req *nvkeproto.KubeadmUpgradeApplyRequest) (*nvkeproto.KubeadmUpgradeApplyResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for kubeadm upgrade on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).KubeadmUpgradeApply(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to apply kubeadm upgrade on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ReconcileKubeVip(ctx context.Context, node db.Node, req *nvkeproto.ReconcileKubeVipRequest) (*nvkeproto.ReconcileKubeVipResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for kube-vip reconciliation on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ReconcileKubeVip(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to reconcile kube-vip on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartKubernetesCertificateRenewal(
	ctx context.Context,
	node db.Node,
	req *nvkeproto.StartKubernetesCertificateRenewalRequest,
) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for certificate renewal on node %s: %w", node.Name, err)
	}
	defer cleanup()
	resp, err := nvkeproto.NewNodeAgentClient(conn).StartKubernetesCertificateRenewal(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start certificate renewal on node %s: %w", node.Name, err)
	}
	return resp, nil
}

// StartKubernetesCertificateRenewalV2 preserves the capability-distinct RPC and
// wrapped gRPC status so the activity can distinguish Unimplemented from an
// ambiguous transport failure. It neither selects fallback nor waits for renewal;
// canceling this RPC does not cancel work the agent has already accepted.
func (c *NodeAgentClient) StartKubernetesCertificateRenewalV2(
	ctx context.Context,
	node db.Node,
	req *nvkeproto.StartKubernetesCertificateRenewalV2Request,
) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for certificate renewal on node %s: %w", node.Name, err)
	}
	defer cleanup()
	resp, err := nvkeproto.NewNodeAgentClient(conn).StartKubernetesCertificateRenewalV2(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start V2 certificate renewal on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetKubernetesCertificateRenewalStatus(
	ctx context.Context,
	node db.Node,
	req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest,
) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for certificate renewal status on node %s: %w", node.Name, err)
	}
	defer cleanup()
	resp, err := nvkeproto.NewNodeAgentClient(conn).GetKubernetesCertificateRenewalStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get certificate renewal status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) RemoveEtcdMember(ctx context.Context, node db.Node, req *nvkeproto.RemoveEtcdMemberRequest) (*nvkeproto.RemoveEtcdMemberResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for etcd member removal on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).RemoveEtcdMember(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to remove etcd member from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteNodePreflight(ctx context.Context, node db.Node, req *nvkeproto.ExecuteNodePreflightRequest) (*nvkeproto.ExecuteNodePreflightResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute node preflight on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ExecuteNodePreflight(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute node preflight on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetKubeadmJoinString(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm join string from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetKubeadmJoinString(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm join string from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) UpdateAndFetchKubeAdmJoinStrings(ctx context.Context, node db.Node, req *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to update and fetch kubeadm join strings from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).UpdateAndFetchKubeAdmJoinStrings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to update and fetch kubeadm join strings from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartK8sControlPlaneNodeGracefulShutdown(ctx context.Context, node db.Node, req *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to start control-plane graceful shutdown on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).StartK8SControlPlaneNodeGracefulShutdown(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start control-plane graceful shutdown on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetK8sControlPlaneNodeGracefulShutdownStatus(ctx context.Context, node db.Node, req *nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get control-plane graceful shutdown status from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetK8SControlPlaneNodeGracefulShutdownStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get control-plane graceful shutdown status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteScript(ctx context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nvkeproto.ExecuteScriptResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute script on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).ExecuteScript(ctx, &nvkeproto.ExecuteScriptRequest{
		ScriptName:     scriptName,
		ScriptContents: scriptContents,
		Async:          false,
		Args:           args,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to execute script %q on node %s: %w", scriptName, node.Name, err)
	}

	return resp, nil
}

func (c *NodeAgentClient) InitializeAPIEncryption(ctx context.Context, node db.Node, encryptionKey string) error {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).InitializeKubernetesAPIEncryption(
		ctx,
		&nvkeproto.InitializeKubernetesAPIEncryptionRequest{EncryptionKey: encryptionKey},
	)
	if err != nil {
		return fmt.Errorf("failed to initialize API encryption on %s: %w", node.Name, err)
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		return fmt.Errorf("failed to initialize API encryption on %s: %s", node.Name, msg)
	}
	return nil
}

func (c *NodeAgentClient) PromoteAPIEncryptionProviders(ctx context.Context, node db.Node) error {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	if _, err := nvkeproto.NewNodeAgentClient(conn).PromoteKubernetesAPIEncryptionProviders(
		ctx,
		&nvkeproto.PromoteKubernetesAPIEncryptionProvidersRequest{},
	); err != nil {
		return fmt.Errorf("failed to promote API encryption providers on %s: %w", node.Name, err)
	}
	return nil
}

func (c *NodeAgentClient) StartEncryptedObjectsRewrite(ctx context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).StartEtcdEncryptionAtRestObjectsRewrite(
		ctx,
		&nvkeproto.StartEtcdEncryptionAtRestObjectsRewriteRequest{OperationId: operationID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start encrypted-object rewrite on %s: %w", node.Name, err)
	}
	return resp.GetOperation(), nil
}

func (c *NodeAgentClient) GetEncryptedObjectsRewriteStatus(ctx context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).GetEtcdEncryptionAtRestObjectsRewriteStatus(
		ctx,
		&nvkeproto.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest{OperationId: operationID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get encrypted-object rewrite status on %s: %w", node.Name, err)
	}
	return resp.GetOperation(), nil
}

func (c *NodeAgentClient) RemovePlainTextSecondaryEncryptionEntry(ctx context.Context, node db.Node) error {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).RemovePlainTextSecondaryEncryptionEntry(
		ctx,
		&nvkeproto.RemovePlainTextSecondaryEncryptionEntryRequest{},
	)
	if err != nil {
		return fmt.Errorf("failed to remove plaintext encryption entry on %s: %w", node.Name, err)
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		return fmt.Errorf("failed to remove plaintext encryption entry on %s: %s", node.Name, msg)
	}
	return nil
}

func (c *NodeAgentClient) StartKubeVIPRestore(ctx context.Context, node db.Node, operationID string) (*nvkeproto.NodeAgentOperationStatus, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nvkeproto.NewNodeAgentClient(conn).StartKubeVipRestore(
		ctx,
		&nvkeproto.StartKubeVipRestoreRequest{OperationId: operationID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start kube-vip restore on %s: %w", node.Name, err)
	}
	return resp.GetOperation(), nil
}
