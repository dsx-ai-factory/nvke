// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"errors"

	"github.com/google/uuid"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type FakeClient struct {
	GetKubeconfigCalls []uuid.UUID
	Kubeconfigs        map[uuid.UUID]string
	KubeconfigErrors   map[uuid.UUID]error

	// GetKubeconfigCtxs records the context of each GetKubeconfig call so callers
	// can assert on per-call deadlines.
	GetKubeconfigCtxs []context.Context

	InstallCNIRequests []InstallCNICall
	InstallCNIErr      error

	AgentVersion               string
	AgentVersionErr            error
	KubeadmFilesUnsupported    bool
	KubeadmFilesSupportErr     error
	KubeadmFilesSupportCalls   []uuid.UUID
	ScheduleNodeRebootResponse *nvkeproto.ScheduleNodeRebootResponse
	ScheduleNodeRebootErr      error
	ScheduleNodeRebootCalls    []uuid.UUID
	GetNodeBootInfoResponse    *nvkeproto.GetNodeBootInfoResponse
	GetNodeBootInfoErr         error
	GetNodeBootInfoCalls       []uuid.UUID

	PreflightResp    *nvkeproto.ExecuteNodePreflightResponse
	PreflightErr     error
	PreflightCalls   int
	LastPreflightReq *nvkeproto.ExecuteNodePreflightRequest

	ReconfigureKubeletAPIServerCalls    []uuid.UUID
	ReconfigureKubeletAPIServerRequests []*nvkeproto.ReconfigureKubeletAPIServerRequest
	ReconfigureKubeletAPIServerResponse *nvkeproto.ReconfigureKubeletAPIServerResponse
	ReconfigureKubeletAPIServerErr      error
	StartOpenVSwitchCalls               []uuid.UUID
	StartOpenVSwitchHook                func(db.Node, *nvkeproto.StartOpenVSwitchReconciliationRequest) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error)
	GetOpenVSwitchStatusCalls           []uuid.UUID
	GetOpenVSwitchStatusHook            func(db.Node, *nvkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error)

	InitializeAPIEncryptionCalls             []uuid.UUID
	InitializeAPIEncryptionKeys              []string
	PromoteAPIEncryptionProvidersCalls       []uuid.UUID
	StartEncryptedObjectsRewriteCalls        []uuid.UUID
	StartEncryptedObjectsRewriteIDs          []string
	GetEncryptedObjectsRewriteStatusCalls    []uuid.UUID
	GetEncryptedObjectsRewriteStatusIDs      []string
	RemovePlainTextSecondaryEncryptionCalls  []uuid.UUID
	StartKubeVIPRestoreCalls                 []uuid.UUID
	StartKubeVIPRestoreIDs                   []string
	ExecuteScriptNames                       []string
	ExecuteScriptArgs                        [][]string
	KubeadmUpgradeApplyCalls                 []uuid.UUID
	KubeadmUpgradeApplyRequests              []*nvkeproto.KubeadmUpgradeApplyRequest
	KubeadmUpgradeApplyResponse              *nvkeproto.KubeadmUpgradeApplyResponse
	KubeadmUpgradeApplyError                 error
	ReconcileKubeVipCalls                    []uuid.UUID
	ReconcileKubeVipRequests                 []*nvkeproto.ReconcileKubeVipRequest
	ReconcileKubeVipResponse                 *nvkeproto.ReconcileKubeVipResponse
	ReconcileKubeVipError                    error
	StartKubernetesCertificateRenewalCalls   []uuid.UUID
	StartKubernetesCertificateRenewalHook    func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error)
	StartKubernetesCertificateRenewalV2Calls []uuid.UUID
	StartKubernetesCertificateRenewalV2Hook  func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalV2Request) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error)
	GetKubernetesCertificateRenewalCalls     []uuid.UUID
	GetKubernetesCertificateRenewalHook      func(db.Node, *nvkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
	RemoveEtcdMemberCalls                    []uuid.UUID
	RemoveEtcdMemberRequests                 []*nvkeproto.RemoveEtcdMemberRequest
	RemoveEtcdMemberResponse                 *nvkeproto.RemoveEtcdMemberResponse
	RemoveEtcdMemberError                    error

	// ExecuteScriptHook, when set, produces the response for ExecuteScript calls,
	// letting tests drive per-node script output (e.g. /readyz probes).
	ExecuteScriptHook func(node db.Node, scriptName, scriptContents string) (*nvkeproto.ExecuteScriptResponse, error)

	StartEncryptedObjectsRewriteHook func(node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	GetEncryptedObjectsRewriteHook   func(node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	GetEtcdEncryptionKeyIdentityHook func(node db.Node, digest string) (string, error)

	// ReconfigureSpireAgentHook and GetSpireAgentStatusHook produce the
	// responses for those RPCs. They may be called concurrently, so the fake
	// records no calls for them; a hook that needs to must synchronize itself.
	ReconfigureSpireAgentHook func(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureSpireAgentRequest) (*nvkeproto.ReconfigureSpireAgentResponse, error)
	GetSpireAgentStatusHook   func(ctx context.Context, node db.Node) (*nvkeproto.GetSpireAgentStatusResponse, error)

	// ReconfigureNodeAgentArgsHook produces the response for that RPC. Like the
	// two above it records no calls; a hook that needs to must synchronize itself.
	ReconfigureNodeAgentArgsHook func(ctx context.Context, node db.Node, args []string) (*nvkeproto.ReconfigureNodeAgentArgsResponse, error)
}

func (f *FakeClient) GetEtcdEncryptionKeyIdentity(_ context.Context, node db.Node, digest string) (string, error) {
	if f.GetEtcdEncryptionKeyIdentityHook != nil {
		return f.GetEtcdEncryptionKeyIdentityHook(node, digest)
	}
	return "", errors.New("encryption identity lookup is not configured")
}

func (f *FakeClient) GetKubeconfig(ctx context.Context, node db.Node) (string, error) {
	f.GetKubeconfigCalls = append(f.GetKubeconfigCalls, node.ID)
	f.GetKubeconfigCtxs = append(f.GetKubeconfigCtxs, ctx)
	if err := f.KubeconfigErrors[node.ID]; err != nil {
		return "", err
	}
	return f.Kubeconfigs[node.ID], nil
}

func (f *FakeClient) GetAgentVersion(context.Context, db.Node) (string, error) {
	return f.AgentVersion, f.AgentVersionErr
}

func (f *FakeClient) SupportsKubeadmFiles(_ context.Context, node db.Node) (bool, error) {
	f.KubeadmFilesSupportCalls = append(f.KubeadmFilesSupportCalls, node.ID)
	return !f.KubeadmFilesUnsupported, f.KubeadmFilesSupportErr
}

func (f *FakeClient) GetKubeadmStatus(context.Context, db.Node) (*nvkeproto.KubeadmStatusResponse, error) {
	return &nvkeproto.KubeadmStatusResponse{}, nil
}

func (f *FakeClient) ScheduleNodeReboot(_ context.Context, node db.Node) (*nvkeproto.ScheduleNodeRebootResponse, error) {
	f.ScheduleNodeRebootCalls = append(f.ScheduleNodeRebootCalls, node.ID)
	return f.ScheduleNodeRebootResponse, f.ScheduleNodeRebootErr
}

func (f *FakeClient) GetNodeBootInfo(_ context.Context, node db.Node) (*nvkeproto.GetNodeBootInfoResponse, error) {
	f.GetNodeBootInfoCalls = append(f.GetNodeBootInfoCalls, node.ID)
	return f.GetNodeBootInfoResponse, f.GetNodeBootInfoErr
}

func (f *FakeClient) PatchAPIServerManifest(context.Context, db.Node, *nvkeproto.PatchAPIServerManifestRequest) (*nvkeproto.PatchAPIServerManifestResponse, error) {
	return &nvkeproto.PatchAPIServerManifestResponse{}, nil
}

func (f *FakeClient) InitializeAPIEncryption(_ context.Context, node db.Node, encryptionKey string) error {
	f.InitializeAPIEncryptionCalls = append(f.InitializeAPIEncryptionCalls, node.ID)
	f.InitializeAPIEncryptionKeys = append(f.InitializeAPIEncryptionKeys, encryptionKey)
	return nil
}

func (f *FakeClient) PromoteAPIEncryptionProviders(_ context.Context, node db.Node) error {
	f.PromoteAPIEncryptionProvidersCalls = append(f.PromoteAPIEncryptionProvidersCalls, node.ID)
	return nil
}

func (f *FakeClient) StartEncryptedObjectsRewrite(_ context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	f.StartEncryptedObjectsRewriteCalls = append(f.StartEncryptedObjectsRewriteCalls, node.ID)
	f.StartEncryptedObjectsRewriteIDs = append(f.StartEncryptedObjectsRewriteIDs, operationID)
	if f.StartEncryptedObjectsRewriteHook != nil {
		return f.StartEncryptedObjectsRewriteHook(node, operationID)
	}
	return &nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{}, nil
}

func (f *FakeClient) GetEncryptedObjectsRewriteStatus(_ context.Context, node db.Node, operationID string) (*nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	f.GetEncryptedObjectsRewriteStatusCalls = append(f.GetEncryptedObjectsRewriteStatusCalls, node.ID)
	f.GetEncryptedObjectsRewriteStatusIDs = append(f.GetEncryptedObjectsRewriteStatusIDs, operationID)
	if f.GetEncryptedObjectsRewriteHook != nil {
		return f.GetEncryptedObjectsRewriteHook(node, operationID)
	}
	return &nvkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{}, nil
}

func (f *FakeClient) RemovePlainTextSecondaryEncryptionEntry(_ context.Context, node db.Node) error {
	f.RemovePlainTextSecondaryEncryptionCalls = append(f.RemovePlainTextSecondaryEncryptionCalls, node.ID)
	return nil
}

func (f *FakeClient) StartKubeVIPRestore(_ context.Context, node db.Node, operationID string) (*nvkeproto.NodeAgentOperationStatus, error) {
	f.StartKubeVIPRestoreCalls = append(f.StartKubeVIPRestoreCalls, node.ID)
	f.StartKubeVIPRestoreIDs = append(f.StartKubeVIPRestoreIDs, operationID)
	return &nvkeproto.NodeAgentOperationStatus{}, nil
}

func (f *FakeClient) ExecuteKubeadmInit(context.Context, db.Node, *nvkeproto.KubeadmInitRequest) (*nvkeproto.KubeadmInitStatusResponse, error) {
	return &nvkeproto.KubeadmInitStatusResponse{}, nil
}

func (f *FakeClient) InstallCNI(_ context.Context, node db.Node, req *nvkeproto.InstallCNIRequest) (*nvkeproto.InstallCNIResponse, error) {
	f.InstallCNIRequests = append(f.InstallCNIRequests, InstallCNICall{Node: node, Request: req})
	if f.InstallCNIErr != nil {
		return nil, f.InstallCNIErr
	}
	return &nvkeproto.InstallCNIResponse{Status: "ready"}, nil
}

func (f *FakeClient) ExecuteKubeadmJoin(context.Context, db.Node, *nvkeproto.KubeadmJoinRequest) (*nvkeproto.KubeadmJoinStatusResponse, error) {
	return &nvkeproto.KubeadmJoinStatusResponse{}, nil
}

func (f *FakeClient) ExecuteKubeadmReset(context.Context, db.Node, *nvkeproto.KubeadmResetRequest) (*nvkeproto.KubeadmResetStatusResponse, error) {
	return &nvkeproto.KubeadmResetStatusResponse{}, nil
}

func (f *FakeClient) ReconfigureKubeletAPIServer(_ context.Context, node db.Node, req *nvkeproto.ReconfigureKubeletAPIServerRequest) (*nvkeproto.ReconfigureKubeletAPIServerResponse, error) {
	f.ReconfigureKubeletAPIServerCalls = append(f.ReconfigureKubeletAPIServerCalls, node.ID)
	f.ReconfigureKubeletAPIServerRequests = append(f.ReconfigureKubeletAPIServerRequests, req)
	if f.ReconfigureKubeletAPIServerResponse == nil {
		return &nvkeproto.ReconfigureKubeletAPIServerResponse{}, f.ReconfigureKubeletAPIServerErr
	}
	return f.ReconfigureKubeletAPIServerResponse, f.ReconfigureKubeletAPIServerErr
}

func (f *FakeClient) StartOpenVSwitchReconciliation(
	_ context.Context,
	node db.Node,
	req *nvkeproto.StartOpenVSwitchReconciliationRequest,
) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
	f.StartOpenVSwitchCalls = append(f.StartOpenVSwitchCalls, node.ID)
	if f.StartOpenVSwitchHook == nil {
		return nil, errors.New("StartOpenVSwitchHook is nil")
	}
	return f.StartOpenVSwitchHook(node, req)
}

func (f *FakeClient) GetOpenVSwitchReconciliationStatus(
	_ context.Context,
	node db.Node,
	req *nvkeproto.GetOpenVSwitchReconciliationStatusRequest,
) (*nvkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
	f.GetOpenVSwitchStatusCalls = append(f.GetOpenVSwitchStatusCalls, node.ID)
	if f.GetOpenVSwitchStatusHook == nil {
		return nil, errors.New("GetOpenVSwitchStatusHook is nil")
	}
	return f.GetOpenVSwitchStatusHook(node, req)
}

func (f *FakeClient) KubeadmUpgradeApply(_ context.Context, node db.Node, req *nvkeproto.KubeadmUpgradeApplyRequest) (*nvkeproto.KubeadmUpgradeApplyResponse, error) {
	f.KubeadmUpgradeApplyCalls = append(f.KubeadmUpgradeApplyCalls, node.ID)
	f.KubeadmUpgradeApplyRequests = append(f.KubeadmUpgradeApplyRequests, req)
	return f.KubeadmUpgradeApplyResponse, f.KubeadmUpgradeApplyError
}

func (f *FakeClient) ReconcileKubeVip(_ context.Context, node db.Node, req *nvkeproto.ReconcileKubeVipRequest) (*nvkeproto.ReconcileKubeVipResponse, error) {
	f.ReconcileKubeVipCalls = append(f.ReconcileKubeVipCalls, node.ID)
	f.ReconcileKubeVipRequests = append(f.ReconcileKubeVipRequests, req)
	return f.ReconcileKubeVipResponse, f.ReconcileKubeVipError
}

func (f *FakeClient) StartKubernetesCertificateRenewal(
	_ context.Context,
	node db.Node,
	req *nvkeproto.StartKubernetesCertificateRenewalRequest,
) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
	f.StartKubernetesCertificateRenewalCalls = append(f.StartKubernetesCertificateRenewalCalls, node.ID)
	if f.StartKubernetesCertificateRenewalHook == nil {
		return nil, errors.New("StartKubernetesCertificateRenewalHook is nil")
	}
	return f.StartKubernetesCertificateRenewalHook(node, req)
}

// StartKubernetesCertificateRenewalV2 requires an explicit hook so tests choose
// supported, unsupported or ambiguous acceptance rather than silently using V1.
func (f *FakeClient) StartKubernetesCertificateRenewalV2(
	_ context.Context,
	node db.Node,
	req *nvkeproto.StartKubernetesCertificateRenewalV2Request,
) (*nvkeproto.StartKubernetesCertificateRenewalV2Response, error) {
	f.StartKubernetesCertificateRenewalV2Calls = append(f.StartKubernetesCertificateRenewalV2Calls, node.ID)
	if f.StartKubernetesCertificateRenewalV2Hook == nil {
		return nil, errors.New("StartKubernetesCertificateRenewalV2Hook is nil")
	}
	return f.StartKubernetesCertificateRenewalV2Hook(node, req)
}

func (f *FakeClient) GetKubernetesCertificateRenewalStatus(
	_ context.Context,
	node db.Node,
	req *nvkeproto.GetKubernetesCertificateRenewalStatusRequest,
) (*nvkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
	f.GetKubernetesCertificateRenewalCalls = append(f.GetKubernetesCertificateRenewalCalls, node.ID)
	if f.GetKubernetesCertificateRenewalHook == nil {
		return nil, errors.New("GetKubernetesCertificateRenewalHook is nil")
	}
	return f.GetKubernetesCertificateRenewalHook(node, req)
}

func (f *FakeClient) RemoveEtcdMember(_ context.Context, node db.Node, req *nvkeproto.RemoveEtcdMemberRequest) (*nvkeproto.RemoveEtcdMemberResponse, error) {
	f.RemoveEtcdMemberCalls = append(f.RemoveEtcdMemberCalls, node.ID)
	f.RemoveEtcdMemberRequests = append(f.RemoveEtcdMemberRequests, req)
	return f.RemoveEtcdMemberResponse, f.RemoveEtcdMemberError
}

func (f *FakeClient) ExecuteNodePreflight(_ context.Context, _ db.Node, req *nvkeproto.ExecuteNodePreflightRequest) (*nvkeproto.ExecuteNodePreflightResponse, error) {
	f.PreflightCalls++
	f.LastPreflightReq = req
	return f.PreflightResp, f.PreflightErr
}

func (f *FakeClient) GetKubeadmJoinString(context.Context, db.Node, *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error) {
	return &nvkeproto.KubeadmJoinStringResponse{}, nil
}

func (f *FakeClient) UpdateAndFetchKubeAdmJoinStrings(context.Context, db.Node, *nvkeproto.KubeadmJoinStringRequest) (*nvkeproto.KubeadmJoinStringResponse, error) {
	return &nvkeproto.KubeadmJoinStringResponse{}, nil
}

func (f *FakeClient) StartK8sControlPlaneNodeGracefulShutdown(context.Context, db.Node, *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	return &nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse{}, nil
}

func (f *FakeClient) GetK8sControlPlaneNodeGracefulShutdownStatus(context.Context, db.Node, *nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	return &nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse{}, nil
}

func (f *FakeClient) ExecuteScript(_ context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nvkeproto.ExecuteScriptResponse, error) {
	f.ExecuteScriptNames = append(f.ExecuteScriptNames, scriptName)
	f.ExecuteScriptArgs = append(f.ExecuteScriptArgs, args)
	if f.ExecuteScriptHook != nil {
		return f.ExecuteScriptHook(node, scriptName, scriptContents)
	}
	return &nvkeproto.ExecuteScriptResponse{}, nil
}

type InstallCNICall struct {
	Node    db.Node
	Request *nvkeproto.InstallCNIRequest
}

// ConvergeNodeAgent implements the agent convergence test surface.
func (f *FakeClient) ConvergeNodeAgent(ctx context.Context, node db.Node, target, _ string) (string, bool, error) {
	version, err := f.GetAgentVersion(ctx, node)
	return version, version == target || target == "", err
}

// ReconfigureSpireAgent implements agent.Client through ReconfigureSpireAgentHook.
func (f *FakeClient) ReconfigureSpireAgent(ctx context.Context, node db.Node, req *nvkeproto.ReconfigureSpireAgentRequest) (*nvkeproto.ReconfigureSpireAgentResponse, error) {
	if f.ReconfigureSpireAgentHook == nil {
		return nil, errors.New("ReconfigureSpireAgentHook is nil")
	}
	return f.ReconfigureSpireAgentHook(ctx, node, req)
}

// ReconfigureNodeAgentArgs implements agent.Client through ReconfigureNodeAgentArgsHook.
func (f *FakeClient) ReconfigureNodeAgentArgs(ctx context.Context, node db.Node, args []string) (*nvkeproto.ReconfigureNodeAgentArgsResponse, error) {
	if f.ReconfigureNodeAgentArgsHook == nil {
		return nil, errors.New("ReconfigureNodeAgentArgsHook is nil")
	}
	return f.ReconfigureNodeAgentArgsHook(ctx, node, args)
}

// GetSpireAgentStatus implements agent.Client through GetSpireAgentStatusHook.
func (f *FakeClient) GetSpireAgentStatus(ctx context.Context, node db.Node) (*nvkeproto.GetSpireAgentStatusResponse, error) {
	if f.GetSpireAgentStatusHook == nil {
		return nil, errors.New("GetSpireAgentStatusHook is nil")
	}
	return f.GetSpireAgentStatusHook(ctx, node)
}
