// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package agenttest

import (
	"context"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

type FakeClient struct {
	GetKubeconfigCalls []uuid.UUID
	Kubeconfigs        map[uuid.UUID]string
	KubeconfigErrors   map[uuid.UUID]error

	// GetKubeconfigCtxs records the context of each GetKubeconfig call so callers
	// can assert on per-call deadlines.
	GetKubeconfigCtxs []context.Context

	AgentVersion    string
	AgentVersionErr error

	PreflightResp    *nkeproto.ExecuteNodePreflightResponse
	PreflightErr     error
	PreflightCalls   int
	LastPreflightReq *nkeproto.ExecuteNodePreflightRequest

	InitializeAPIEncryptionCalls            []uuid.UUID
	InitializeAPIEncryptionKeys             []string
	PromoteAPIEncryptionProvidersCalls      []uuid.UUID
	StartEncryptedObjectsRewriteCalls       []uuid.UUID
	StartEncryptedObjectsRewriteIDs         []string
	GetEncryptedObjectsRewriteStatusCalls   []uuid.UUID
	GetEncryptedObjectsRewriteStatusIDs     []string
	RemovePlainTextSecondaryEncryptionCalls []uuid.UUID
	StartKubeVIPRestoreCalls                []uuid.UUID
	StartKubeVIPRestoreIDs                  []string
	ExecuteScriptNames                      []string

	// ExecuteScriptHook, when set, produces the response for ExecuteScript calls,
	// letting tests drive per-node script output (e.g. /readyz probes).
	ExecuteScriptHook func(node db.Node, scriptName, scriptContents string) (*nkeproto.ExecuteScriptResponse, error)

	// GetNodeInfoHook, when set, produces the response for GetNodeInfo calls, letting
	// tests drive node-agent operation status (e.g. kube-vip restore polling).
	GetNodeInfoHook func(node db.Node) (*nkeproto.NodeInfoResponse, error)
	GetNodeInfoResp *nkeproto.NodeInfoResponse
	GetNodeInfoErr  error

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

func (f *FakeClient) GetKubeadmStatus(context.Context, db.Node) (*nkeproto.KubeadmStatusResponse, error) {
	return &nkeproto.KubeadmStatusResponse{}, nil
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

func (f *FakeClient) GetNodeInfo(_ context.Context, node db.Node) (*nkeproto.NodeInfoResponse, error) {
	if f.GetNodeInfoHook != nil {
		return f.GetNodeInfoHook(node)
	}
	if f.GetNodeInfoResp != nil || f.GetNodeInfoErr != nil {
		return f.GetNodeInfoResp, f.GetNodeInfoErr
	}
	return &nkeproto.NodeInfoResponse{}, nil
}

func (f *FakeClient) ExecuteKubeadmInit(context.Context, db.Node, *nkeproto.KubeadmInitRequest) (*nkeproto.KubeadmInitStatusResponse, error) {
	return &nkeproto.KubeadmInitStatusResponse{}, nil
}

func (f *FakeClient) ExecuteKubeadmJoin(context.Context, db.Node, *nkeproto.KubeadmJoinRequest) (*nkeproto.KubeadmJoinStatusResponse, error) {
	return &nkeproto.KubeadmJoinStatusResponse{}, nil
}

func (f *FakeClient) ExecuteKubeadmReset(context.Context, db.Node, *nkeproto.KubeadmResetRequest) (*nkeproto.KubeadmResetStatusResponse, error) {
	return &nkeproto.KubeadmResetStatusResponse{}, nil
}

//nolint:staticcheck // Legacy RPC is required for pre-v0.1.15 node-agent compatibility.
func (f *FakeClient) InstallCNIPlugin(context.Context, db.Node, *nkeproto.InstallCNIPluginRequest) (*nkeproto.CNIPluginStatusResponse, error) {
	return &nkeproto.CNIPluginStatusResponse{}, nil
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

func (f *FakeClient) ExecuteScript(_ context.Context, node db.Node, scriptName, scriptContents string, _ []string) (*nkeproto.ExecuteScriptResponse, error) {
	f.ExecuteScriptNames = append(f.ExecuteScriptNames, scriptName)
	if f.ExecuteScriptHook != nil {
		return f.ExecuteScriptHook(node, scriptName, scriptContents)
	}
	return &nkeproto.ExecuteScriptResponse{}, nil
}
