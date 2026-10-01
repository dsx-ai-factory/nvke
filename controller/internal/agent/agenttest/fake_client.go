// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"errors"

	"github.com/google/uuid"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
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
	ScheduleNodeRebootResponse *nkeproto.ScheduleNodeRebootResponse
	ScheduleNodeRebootErr      error
	ScheduleNodeRebootCalls    []uuid.UUID
	GetNodeBootInfoResponse    *nkeproto.GetNodeBootInfoResponse
	GetNodeBootInfoErr         error
	GetNodeBootInfoCalls       []uuid.UUID

	PreflightResp    *nkeproto.ExecuteNodePreflightResponse
	PreflightErr     error
	PreflightCalls   int
	LastPreflightReq *nkeproto.ExecuteNodePreflightRequest

	ReconfigureKubeletAPIServerCalls    []uuid.UUID
	ReconfigureKubeletAPIServerRequests []*nkeproto.ReconfigureKubeletAPIServerRequest
	ReconfigureKubeletAPIServerResponse *nkeproto.ReconfigureKubeletAPIServerResponse
	ReconfigureKubeletAPIServerErr      error
	StartOpenVSwitchCalls               []uuid.UUID
	StartOpenVSwitchHook                func(db.Node, *nkeproto.StartOpenVSwitchReconciliationRequest) (*nkeproto.StartOpenVSwitchReconciliationResponse, error)
	GetOpenVSwitchStatusCalls           []uuid.UUID
	GetOpenVSwitchStatusHook            func(db.Node, *nkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nkeproto.GetOpenVSwitchReconciliationStatusResponse, error)

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
	KubeadmUpgradeApplyRequests              []*nkeproto.KubeadmUpgradeApplyRequest
	KubeadmUpgradeApplyResponse              *nkeproto.KubeadmUpgradeApplyResponse
	KubeadmUpgradeApplyError                 error
	ReconcileKubeVipCalls                    []uuid.UUID
	ReconcileKubeVipRequests                 []*nkeproto.ReconcileKubeVipRequest
	ReconcileKubeVipResponse                 *nkeproto.ReconcileKubeVipResponse
	ReconcileKubeVipError                    error
	StartKubernetesCertificateRenewalCalls   []uuid.UUID
	StartKubernetesCertificateRenewalHook    func(db.Node, *nkeproto.StartKubernetesCertificateRenewalRequest) (*nkeproto.StartKubernetesCertificateRenewalResponse, error)
	StartKubernetesCertificateRenewalV2Calls []uuid.UUID
	StartKubernetesCertificateRenewalV2Hook  func(db.Node, *nkeproto.StartKubernetesCertificateRenewalV2Request) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error)
	GetKubernetesCertificateRenewalCalls     []uuid.UUID
	GetKubernetesCertificateRenewalHook      func(db.Node, *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
	RemoveEtcdMemberCalls                    []uuid.UUID
	RemoveEtcdMemberRequests                 []*nkeproto.RemoveEtcdMemberRequest
	RemoveEtcdMemberResponse                 *nkeproto.RemoveEtcdMemberResponse
	RemoveEtcdMemberError                    error

	// ExecuteScriptHook, when set, produces the response for ExecuteScript calls,
	// letting tests drive per-node script output (e.g. /readyz probes).
	ExecuteScriptHook func(node db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error)

	StartEncryptedObjectsRewriteHook func(node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	GetEncryptedObjectsRewriteHook   func(node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
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

func (f *FakeClient) GetKubeadmStatus(context.Context, db.Node) (*nkeproto.KubeadmStatusResponse, error) {
	return &nkeproto.KubeadmStatusResponse{}, nil
}

func (f *FakeClient) ScheduleNodeReboot(_ context.Context, node db.Node) (*nkeproto.ScheduleNodeRebootResponse, error) {
	f.ScheduleNodeRebootCalls = append(f.ScheduleNodeRebootCalls, node.ID)
	return f.ScheduleNodeRebootResponse, f.ScheduleNodeRebootErr
}

func (f *FakeClient) GetNodeBootInfo(_ context.Context, node db.Node) (*nkeproto.GetNodeBootInfoResponse, error) {
	f.GetNodeBootInfoCalls = append(f.GetNodeBootInfoCalls, node.ID)
	return f.GetNodeBootInfoResponse, f.GetNodeBootInfoErr
}

func (f *FakeClient) PatchAPIServerManifest(context.Context, db.Node, *nkeproto.PatchAPIServerManifestRequest) (*nkeproto.PatchAPIServerManifestResponse, error) {
	return &nkeproto.PatchAPIServerManifestResponse{}, nil
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

func (f *FakeClient) StartEncryptedObjectsRewrite(_ context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	f.StartEncryptedObjectsRewriteCalls = append(f.StartEncryptedObjectsRewriteCalls, node.ID)
	f.StartEncryptedObjectsRewriteIDs = append(f.StartEncryptedObjectsRewriteIDs, operationID)
	if f.StartEncryptedObjectsRewriteHook != nil {
		return f.StartEncryptedObjectsRewriteHook(node, operationID)
	}
	return &nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{}, nil
}

func (f *FakeClient) GetEncryptedObjectsRewriteStatus(_ context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	f.GetEncryptedObjectsRewriteStatusCalls = append(f.GetEncryptedObjectsRewriteStatusCalls, node.ID)
	f.GetEncryptedObjectsRewriteStatusIDs = append(f.GetEncryptedObjectsRewriteStatusIDs, operationID)
	if f.GetEncryptedObjectsRewriteHook != nil {
		return f.GetEncryptedObjectsRewriteHook(node, operationID)
	}
	return &nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation{}, nil
}

func (f *FakeClient) RemovePlainTextSecondaryEncryptionEntry(_ context.Context, node db.Node) error {
	f.RemovePlainTextSecondaryEncryptionCalls = append(f.RemovePlainTextSecondaryEncryptionCalls, node.ID)
	return nil
}

func (f *FakeClient) StartKubeVIPRestore(_ context.Context, node db.Node, operationID string) (*nkeproto.NodeAgentOperationStatus, error) {
	f.StartKubeVIPRestoreCalls = append(f.StartKubeVIPRestoreCalls, node.ID)
	f.StartKubeVIPRestoreIDs = append(f.StartKubeVIPRestoreIDs, operationID)
	return &nkeproto.NodeAgentOperationStatus{}, nil
}

func (f *FakeClient) ExecuteKubeadmInit(context.Context, db.Node, *nkeproto.KubeadmInitRequest) (*nkeproto.KubeadmInitStatusResponse, error) {
	return &nkeproto.KubeadmInitStatusResponse{}, nil
}

func (f *FakeClient) InstallCNI(_ context.Context, node db.Node, req *nkeproto.InstallCNIRequest) (*nkeproto.InstallCNIResponse, error) {
	f.InstallCNIRequests = append(f.InstallCNIRequests, InstallCNICall{Node: node, Request: req})
	if f.InstallCNIErr != nil {
		return nil, f.InstallCNIErr
	}
	return &nkeproto.InstallCNIResponse{Status: "ready"}, nil
}

func (f *FakeClient) ExecuteKubeadmJoin(context.Context, db.Node, *nkeproto.KubeadmJoinRequest) (*nkeproto.KubeadmJoinStatusResponse, error) {
	return &nkeproto.KubeadmJoinStatusResponse{}, nil
}

func (f *FakeClient) ExecuteKubeadmReset(context.Context, db.Node, *nkeproto.KubeadmResetRequest) (*nkeproto.KubeadmResetStatusResponse, error) {
	return &nkeproto.KubeadmResetStatusResponse{}, nil
}

func (f *FakeClient) ReconfigureKubeletAPIServer(_ context.Context, node db.Node, req *nkeproto.ReconfigureKubeletAPIServerRequest) (*nkeproto.ReconfigureKubeletAPIServerResponse, error) {
	f.ReconfigureKubeletAPIServerCalls = append(f.ReconfigureKubeletAPIServerCalls, node.ID)
	f.ReconfigureKubeletAPIServerRequests = append(f.ReconfigureKubeletAPIServerRequests, req)
	if f.ReconfigureKubeletAPIServerResponse == nil {
		return &nkeproto.ReconfigureKubeletAPIServerResponse{}, f.ReconfigureKubeletAPIServerErr
	}
	return f.ReconfigureKubeletAPIServerResponse, f.ReconfigureKubeletAPIServerErr
}

func (f *FakeClient) StartOpenVSwitchReconciliation(
	_ context.Context,
	node db.Node,
	req *nkeproto.StartOpenVSwitchReconciliationRequest,
) (*nkeproto.StartOpenVSwitchReconciliationResponse, error) {
	f.StartOpenVSwitchCalls = append(f.StartOpenVSwitchCalls, node.ID)
	if f.StartOpenVSwitchHook == nil {
		return nil, errors.New("StartOpenVSwitchHook is nil")
	}
	return f.StartOpenVSwitchHook(node, req)
}

func (f *FakeClient) GetOpenVSwitchReconciliationStatus(
	_ context.Context,
	node db.Node,
	req *nkeproto.GetOpenVSwitchReconciliationStatusRequest,
) (*nkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
	f.GetOpenVSwitchStatusCalls = append(f.GetOpenVSwitchStatusCalls, node.ID)
	if f.GetOpenVSwitchStatusHook == nil {
		return nil, errors.New("GetOpenVSwitchStatusHook is nil")
	}
	return f.GetOpenVSwitchStatusHook(node, req)
}

func (f *FakeClient) KubeadmUpgradeApply(_ context.Context, node db.Node, req *nkeproto.KubeadmUpgradeApplyRequest) (*nkeproto.KubeadmUpgradeApplyResponse, error) {
	f.KubeadmUpgradeApplyCalls = append(f.KubeadmUpgradeApplyCalls, node.ID)
	f.KubeadmUpgradeApplyRequests = append(f.KubeadmUpgradeApplyRequests, req)
	return f.KubeadmUpgradeApplyResponse, f.KubeadmUpgradeApplyError
}

func (f *FakeClient) ReconcileKubeVip(_ context.Context, node db.Node, req *nkeproto.ReconcileKubeVipRequest) (*nkeproto.ReconcileKubeVipResponse, error) {
	f.ReconcileKubeVipCalls = append(f.ReconcileKubeVipCalls, node.ID)
	f.ReconcileKubeVipRequests = append(f.ReconcileKubeVipRequests, req)
	return f.ReconcileKubeVipResponse, f.ReconcileKubeVipError
}

func (f *FakeClient) StartKubernetesCertificateRenewal(
	_ context.Context,
	node db.Node,
	req *nkeproto.StartKubernetesCertificateRenewalRequest,
) (*nkeproto.StartKubernetesCertificateRenewalResponse, error) {
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
	req *nkeproto.StartKubernetesCertificateRenewalV2Request,
) (*nkeproto.StartKubernetesCertificateRenewalV2Response, error) {
	f.StartKubernetesCertificateRenewalV2Calls = append(f.StartKubernetesCertificateRenewalV2Calls, node.ID)
	if f.StartKubernetesCertificateRenewalV2Hook == nil {
		return nil, errors.New("StartKubernetesCertificateRenewalV2Hook is nil")
	}
	return f.StartKubernetesCertificateRenewalV2Hook(node, req)
}

func (f *FakeClient) GetKubernetesCertificateRenewalStatus(
	_ context.Context,
	node db.Node,
	req *nkeproto.GetKubernetesCertificateRenewalStatusRequest,
) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
	f.GetKubernetesCertificateRenewalCalls = append(f.GetKubernetesCertificateRenewalCalls, node.ID)
	if f.GetKubernetesCertificateRenewalHook == nil {
		return nil, errors.New("GetKubernetesCertificateRenewalHook is nil")
	}
	return f.GetKubernetesCertificateRenewalHook(node, req)
}

func (f *FakeClient) RemoveEtcdMember(_ context.Context, node db.Node, req *nkeproto.RemoveEtcdMemberRequest) (*nkeproto.RemoveEtcdMemberResponse, error) {
	f.RemoveEtcdMemberCalls = append(f.RemoveEtcdMemberCalls, node.ID)
	f.RemoveEtcdMemberRequests = append(f.RemoveEtcdMemberRequests, req)
	return f.RemoveEtcdMemberResponse, f.RemoveEtcdMemberError
}

func (f *FakeClient) ExecuteNodePreflight(_ context.Context, _ db.Node, req *nkeproto.ExecuteNodePreflightRequest) (*nkeproto.ExecuteNodePreflightResponse, error) {
	f.PreflightCalls++
	f.LastPreflightReq = req
	return f.PreflightResp, f.PreflightErr
}

func (f *FakeClient) GetKubeadmJoinString(context.Context, db.Node, *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error) {
	return &nkeproto.KubeadmJoinStringResponse{}, nil
}

func (f *FakeClient) UpdateAndFetchKubeAdmJoinStrings(context.Context, db.Node, *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error) {
	return &nkeproto.KubeadmJoinStringResponse{}, nil
}

func (f *FakeClient) StartK8sControlPlaneNodeGracefulShutdown(context.Context, db.Node, *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	return &nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse{}, nil
}

func (f *FakeClient) GetK8sControlPlaneNodeGracefulShutdownStatus(context.Context, db.Node, *nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	return &nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse{}, nil
}

func (f *FakeClient) ExecuteScript(_ context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nkeproto.ExecuteScriptResponse, error) {
	f.ExecuteScriptNames = append(f.ExecuteScriptNames, scriptName)
	f.ExecuteScriptArgs = append(f.ExecuteScriptArgs, args)
	if f.ExecuteScriptHook != nil {
		return f.ExecuteScriptHook(node, scriptName, scriptContents)
	}
	return &nkeproto.ExecuteScriptResponse{}, nil
}

type InstallCNICall struct {
	Node    db.Node
	Request *nkeproto.InstallCNIRequest
}
