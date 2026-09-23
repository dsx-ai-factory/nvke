// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

//nolint:interfacebloat // This mirrors the agent RPC surface used by controllers and activities.
type Client interface {
	GetKubeconfig(ctx context.Context, node db.Node) (string, error)
	GetAgentVersion(ctx context.Context, node db.Node) (string, error)
	SupportsKubeadmFiles(ctx context.Context, node db.Node) (bool, error)
	GetKubeadmStatus(ctx context.Context, node db.Node) (*nkeproto.KubeadmStatusResponse, error)
	ScheduleNodeReboot(ctx context.Context, node db.Node) (*nkeproto.ScheduleNodeRebootResponse, error)
	GetNodeBootInfo(ctx context.Context, node db.Node) (*nkeproto.GetNodeBootInfoResponse, error)
	ExecuteKubeadmInit(ctx context.Context, node db.Node, req *nkeproto.KubeadmInitRequest) (*nkeproto.KubeadmInitStatusResponse, error)
	InstallCNI(ctx context.Context, node db.Node, req *nkeproto.InstallCNIRequest) (*nkeproto.InstallCNIResponse, error)
	ExecuteKubeadmJoin(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinRequest) (*nkeproto.KubeadmJoinStatusResponse, error)
	ExecuteKubeadmReset(ctx context.Context, node db.Node, req *nkeproto.KubeadmResetRequest) (*nkeproto.KubeadmResetStatusResponse, error)
	ReconfigureKubeletAPIServer(ctx context.Context, node db.Node, req *nkeproto.ReconfigureKubeletAPIServerRequest) (*nkeproto.ReconfigureKubeletAPIServerResponse, error)
	StartOpenVSwitchReconciliation(ctx context.Context, node db.Node, req *nkeproto.StartOpenVSwitchReconciliationRequest) (*nkeproto.StartOpenVSwitchReconciliationResponse, error)
	GetOpenVSwitchReconciliationStatus(ctx context.Context, node db.Node, req *nkeproto.GetOpenVSwitchReconciliationStatusRequest) (*nkeproto.GetOpenVSwitchReconciliationStatusResponse, error)
	KubeadmUpgradeApply(ctx context.Context, node db.Node, req *nkeproto.KubeadmUpgradeApplyRequest) (*nkeproto.KubeadmUpgradeApplyResponse, error)
	ReconcileKubeVip(ctx context.Context, node db.Node, req *nkeproto.ReconcileKubeVipRequest) (*nkeproto.ReconcileKubeVipResponse, error)
	StartKubernetesCertificateRenewal(ctx context.Context, node db.Node, req *nkeproto.StartKubernetesCertificateRenewalRequest) (*nkeproto.StartKubernetesCertificateRenewalResponse, error)
	GetKubernetesCertificateRenewalStatus(ctx context.Context, node db.Node, req *nkeproto.GetKubernetesCertificateRenewalStatusRequest) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error)
	RemoveEtcdMember(ctx context.Context, node db.Node, req *nkeproto.RemoveEtcdMemberRequest) (*nkeproto.RemoveEtcdMemberResponse, error)
	ExecuteNodePreflight(ctx context.Context, node db.Node, req *nkeproto.ExecuteNodePreflightRequest) (*nkeproto.ExecuteNodePreflightResponse, error)
	GetKubeadmJoinString(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error)
	UpdateAndFetchKubeAdmJoinStrings(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error)
	StartK8sControlPlaneNodeGracefulShutdown(ctx context.Context, node db.Node, req *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error)
	GetK8sControlPlaneNodeGracefulShutdownStatus(ctx context.Context, node db.Node, req *nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error)
	ExecuteScript(ctx context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nkeproto.ExecuteScriptResponse, error)
	PatchAPIServerManifest(ctx context.Context, node db.Node, req *nkeproto.PatchAPIServerManifestRequest) (*nkeproto.PatchAPIServerManifestResponse, error)
	InitializeAPIEncryption(ctx context.Context, node db.Node, encryptionKey string) error
	PromoteAPIEncryptionProviders(ctx context.Context, node db.Node) error
	StartEncryptedObjectsRewrite(ctx context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	GetEncryptedObjectsRewriteStatus(ctx context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error)
	RemovePlainTextSecondaryEncryptionEntry(ctx context.Context, node db.Node) error
	StartKubeVIPRestore(ctx context.Context, node db.Node, operationID string) (*nkeproto.NodeAgentOperationStatus, error)
}

type NodeAgentClient struct {
	cfg            config.Config
	trustDomain    string
	tlsJWTVerifier *auth.Verifier
	logger         *slog.Logger
}

func NewClient(
	ctx context.Context,
	cfg config.Config,
	trustDomain string,
	logger *slog.Logger,
) (*NodeAgentClient, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var tlsJWTVerifier *auth.Verifier
	var tlsJWTLogger *slog.Logger
	if cfg.NodeAgentTLSJWTEnabled {
		tlsJWTLogger = logger.With("logger", "node-agent-auth")
		verifier, err := auth.SetupTokenVerifier(ctx, &auth.Config{
			Audiences: []string{auth.NodeAgentAudience},
			Issuers: []auth.Issuer{{
				Name:       auth.NodeAgentIssuerName,
				Issuer:     cfg.NodeAgentTLSJWTIssuer,
				JWKS:       &auth.JWKSKeys{URI: cfg.NodeAgentTLSJWTJWKSURI},
				Algorithms: cfg.NodeAgentTLSJWTAlgorithms,
			}},
		}, tlsJWTLogger)
		if err != nil {
			return nil, fmt.Errorf("setting up node-agent token verification: %w", err)
		}
		tlsJWTVerifier = verifier
	}

	return &NodeAgentClient{
		cfg:            cfg,
		trustDomain:    trustDomain,
		tlsJWTVerifier: tlsJWTVerifier,
		logger:         tlsJWTLogger,
	}, nil
}

func (c *NodeAgentClient) getGRPCConnection(ctx context.Context, node db.Node) (*grpc.ClientConn, func(), error) {
	endpoint := net.JoinHostPort(node.IP, c.cfg.NodeAgentPort)

	var closeFuncs []func() error
	cleanup := func() {
		for _, f := range closeFuncs {
			_ = f()
		}
	}
	// Select the connection's base TLS independently from the optional pre-gRPC
	// JWT exchange. A future external-client-certificate transport can provide
	// credentials here, skip verification of the agent's self-signed server
	// certificate, and rely on the JWT exchange for agent authentication.
	var baseTransportCredentials credentials.TransportCredentials
	baseSupportsTLSJWT := false
	if !node.SpiffeEnabled {
		baseTransportCredentials = insecure.NewCredentials()
	} else {
		if c.trustDomain == "" {
			return nil, nil, fmt.Errorf("trust domain is required for SPIFFE authentication")
		}

		workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(c.cfg.SpiffeEndpointSocket))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create SPIFFE workload API client: %w", err)
		}
		closeFuncs = append(closeFuncs, workloadClient.Close)

		x509Source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to create SPIFFE X509 source: %w", err)
		}
		closeFuncs = append(closeFuncs, x509Source.Close)

		expectedID, err := spiffeid.FromString(node.GetSpiffeID(c.trustDomain))
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to parse node SPIFFE ID: %w", err)
		}
		baseTransportCredentials = grpccredentials.MTLSClientCredentials(
			x509Source,
			x509Source,
			tlsconfig.AuthorizeID(expectedID),
		)
		baseSupportsTLSJWT = true
	}

	transportCredentials := baseTransportCredentials
	if c.tlsJWTVerifier != nil {
		if !baseSupportsTLSJWT {
			cleanup()
			return nil, nil, fmt.Errorf("node-agent base transport does not support TLS/JWT authentication")
		}
		var err error
		transportCredentials, err = newTLSJWTTransportCredentials(
			baseTransportCredentials,
			c.tlsJWTVerifier,
			c.cfg.NodeAgentTLSJWTSubjectTrustDomain,
			node,
			c.logger,
		)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("setting up node-agent TLS/JWT transport: %w", err)
		}
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to connect to node agent: %w", err)
	}
	closeFuncs = append(closeFuncs, conn.Close)

	return conn, cleanup, nil
}

func (c *NodeAgentClient) GetKubeconfig(ctx context.Context, node db.Node) (string, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig from node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetKubeconfig(ctx, &nkeproto.KubeconfigRequest{})
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

func (c *NodeAgentClient) GetKubeadmStatus(ctx context.Context, node db.Node) (*nkeproto.KubeadmStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm status from node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nkeproto.KubeadmStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm status from node agent for node %s: %w", node.Name, err)
	}

	return resp, nil
}

func (c *NodeAgentClient) ScheduleNodeReboot(ctx context.Context, node db.Node) (*nkeproto.ScheduleNodeRebootResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for reboot on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ScheduleNodeReboot(ctx, &nkeproto.ScheduleNodeRebootRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("node agent on node %s does not support ScheduleNodeReboot; upgrade the node agent before retrying: %w", node.Name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to schedule reboot on node %s: %w", node.Name, err)
	}

	return resp, nil
}

func (c *NodeAgentClient) GetNodeBootInfo(ctx context.Context, node db.Node) (*nkeproto.GetNodeBootInfoResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for boot info on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetNodeBootInfo(ctx, &nkeproto.GetNodeBootInfoRequest{})
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
func (c *NodeAgentClient) PatchAPIServerManifest(ctx context.Context, node db.Node, req *nkeproto.PatchAPIServerManifestRequest) (*nkeproto.PatchAPIServerManifestResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to patch apiserver manifest on node agent for node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).PatchAPIServerManifest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to patch apiserver manifest on node agent for node %s: %w", node.Name, err)
	}

	return resp, nil
}

// InstallCNI is synchronous: no status to poll afterwards.
func (c *NodeAgentClient) InstallCNI(ctx context.Context, node db.Node, req *nkeproto.InstallCNIRequest) (*nkeproto.InstallCNIResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to install CNI on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).InstallCNI(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to install CNI on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmInit(ctx context.Context, node db.Node, req *nkeproto.KubeadmInitRequest) (*nkeproto.KubeadmInitStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ExecuteKubeadmInit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmJoin(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinRequest) (*nkeproto.KubeadmJoinStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm join on node %s: %w", node.Name, err)
	}
	defer cleanup()

	// Returns only the RPC execution error; resp.Status is parsed by the caller
	// via the GetKubeadmStatus poll loop (see Activities.kubeadmJoin). Do not
	// promote resp.Status to a Go error here: a re-fired join on an already
	// joining/joined agent returns an error status, and callers rely on that
	// being a non-error to stay idempotent under retries.
	resp, err := nkeproto.NewNodeAgentClient(conn).ExecuteKubeadmJoin(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm join on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteKubeadmReset(ctx context.Context, node db.Node, req *nkeproto.KubeadmResetRequest) (*nkeproto.KubeadmResetStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm reset on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ExecuteKubeadmReset(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeadm reset on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ReconfigureKubeletAPIServer(ctx context.Context, node db.Node, req *nkeproto.ReconfigureKubeletAPIServerRequest) (*nkeproto.ReconfigureKubeletAPIServerResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ReconfigureKubeletAPIServer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure kubelet API server on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartOpenVSwitchReconciliation(
	ctx context.Context,
	node db.Node,
	req *nkeproto.StartOpenVSwitchReconciliationRequest,
) (*nkeproto.StartOpenVSwitchReconciliationResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).StartOpenVSwitchReconciliation(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetOpenVSwitchReconciliationStatus(
	ctx context.Context,
	node db.Node,
	req *nkeproto.GetOpenVSwitchReconciliationStatusRequest,
) (*nkeproto.GetOpenVSwitchReconciliationStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for Open vSwitch reconciliation status on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetOpenVSwitchReconciliationStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get Open vSwitch reconciliation status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) KubeadmUpgradeApply(ctx context.Context, node db.Node, req *nkeproto.KubeadmUpgradeApplyRequest) (*nkeproto.KubeadmUpgradeApplyResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for kubeadm upgrade on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).KubeadmUpgradeApply(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to apply kubeadm upgrade on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ReconcileKubeVip(ctx context.Context, node db.Node, req *nkeproto.ReconcileKubeVipRequest) (*nkeproto.ReconcileKubeVipResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for kube-vip reconciliation on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ReconcileKubeVip(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to reconcile kube-vip on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartKubernetesCertificateRenewal(
	ctx context.Context,
	node db.Node,
	req *nkeproto.StartKubernetesCertificateRenewalRequest,
) (*nkeproto.StartKubernetesCertificateRenewalResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for certificate renewal on node %s: %w", node.Name, err)
	}
	defer cleanup()
	resp, err := nkeproto.NewNodeAgentClient(conn).StartKubernetesCertificateRenewal(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start certificate renewal on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetKubernetesCertificateRenewalStatus(
	ctx context.Context,
	node db.Node,
	req *nkeproto.GetKubernetesCertificateRenewalStatusRequest,
) (*nkeproto.GetKubernetesCertificateRenewalStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for certificate renewal status on node %s: %w", node.Name, err)
	}
	defer cleanup()
	resp, err := nkeproto.NewNodeAgentClient(conn).GetKubernetesCertificateRenewalStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get certificate renewal status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) RemoveEtcdMember(ctx context.Context, node db.Node, req *nkeproto.RemoveEtcdMemberRequest) (*nkeproto.RemoveEtcdMemberResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent for etcd member removal on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).RemoveEtcdMember(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to remove etcd member from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteNodePreflight(ctx context.Context, node db.Node, req *nkeproto.ExecuteNodePreflightRequest) (*nkeproto.ExecuteNodePreflightResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute node preflight on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ExecuteNodePreflight(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute node preflight on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetKubeadmJoinString(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm join string from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetKubeadmJoinString(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeadm join string from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) UpdateAndFetchKubeAdmJoinStrings(ctx context.Context, node db.Node, req *nkeproto.KubeadmJoinStringRequest) (*nkeproto.KubeadmJoinStringResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to update and fetch kubeadm join strings from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).UpdateAndFetchKubeAdmJoinStrings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to update and fetch kubeadm join strings from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) StartK8sControlPlaneNodeGracefulShutdown(ctx context.Context, node db.Node, req *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to start control-plane graceful shutdown on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).StartK8SControlPlaneNodeGracefulShutdown(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start control-plane graceful shutdown on node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) GetK8sControlPlaneNodeGracefulShutdownStatus(ctx context.Context, node db.Node, req *nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get control-plane graceful shutdown status from node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetK8SControlPlaneNodeGracefulShutdownStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get control-plane graceful shutdown status from node %s: %w", node.Name, err)
	}
	return resp, nil
}

func (c *NodeAgentClient) ExecuteScript(ctx context.Context, node db.Node, scriptName, scriptContents string, args []string) (*nkeproto.ExecuteScriptResponse, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to execute script on node %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).ExecuteScript(ctx, &nkeproto.ExecuteScriptRequest{
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

	resp, err := nkeproto.NewNodeAgentClient(conn).InitializeKubernetesAPIEncryption(
		ctx,
		&nkeproto.InitializeKubernetesAPIEncryptionRequest{EncryptionKey: encryptionKey},
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

	if _, err := nkeproto.NewNodeAgentClient(conn).PromoteKubernetesAPIEncryptionProviders(
		ctx,
		&nkeproto.PromoteKubernetesAPIEncryptionProvidersRequest{},
	); err != nil {
		return fmt.Errorf("failed to promote API encryption providers on %s: %w", node.Name, err)
	}
	return nil
}

func (c *NodeAgentClient) StartEncryptedObjectsRewrite(ctx context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).StartEtcdEncryptionAtRestObjectsRewrite(
		ctx,
		&nkeproto.StartEtcdEncryptionAtRestObjectsRewriteRequest{OperationId: operationID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start encrypted-object rewrite on %s: %w", node.Name, err)
	}
	return resp.GetOperation(), nil
}

func (c *NodeAgentClient) GetEncryptedObjectsRewriteStatus(ctx context.Context, node db.Node, operationID string) (*nkeproto.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).GetEtcdEncryptionAtRestObjectsRewriteStatus(
		ctx,
		&nkeproto.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest{OperationId: operationID},
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

	resp, err := nkeproto.NewNodeAgentClient(conn).RemovePlainTextSecondaryEncryptionEntry(
		ctx,
		&nkeproto.RemovePlainTextSecondaryEncryptionEntryRequest{},
	)
	if err != nil {
		return fmt.Errorf("failed to remove plaintext encryption entry on %s: %w", node.Name, err)
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		return fmt.Errorf("failed to remove plaintext encryption entry on %s: %s", node.Name, msg)
	}
	return nil
}

func (c *NodeAgentClient) StartKubeVIPRestore(ctx context.Context, node db.Node, operationID string) (*nkeproto.NodeAgentOperationStatus, error) {
	conn, cleanup, err := c.getGRPCConnection(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node agent on %s: %w", node.Name, err)
	}
	defer cleanup()

	resp, err := nkeproto.NewNodeAgentClient(conn).StartKubeVipRestore(
		ctx,
		&nkeproto.StartKubeVipRestoreRequest{OperationId: operationID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start kube-vip restore on %s: %w", node.Name, err)
	}
	return resp.GetOperation(), nil
}
