// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

// --- fakes ---

type fakeNode struct {
	role     rollout.NodeRole
	endpoint string
}

func (f fakeNode) ID() uuid.UUID          { return uuid.New() }
func (f fakeNode) Name() string           { return "cp-0" }
func (f fakeNode) Role() rollout.NodeRole { return f.role }
func (f fakeNode) AgentEndpoint() string  { return f.endpoint }

type fakeCluster struct {
	name string
	k8s  string
	cps  []rollout.Node
}

func (f fakeCluster) ID() uuid.UUID                     { return uuid.New() }
func (f fakeCluster) Name() string                      { return f.name }
func (f fakeCluster) KubernetesVersion() string         { return f.k8s }
func (f fakeCluster) ControlPlaneVIP() string           { return "10.0.0.1" }
func (f fakeCluster) ControlPlaneNodes() []rollout.Node { return f.cps }
func (f fakeCluster) Nodes() []rollout.Node             { return f.cps }

type fakeClient struct {
	patchResp *nkeproto.PatchAPIServerManifestResponse
	patchErr  error
	calls     []*nkeproto.PatchAPIServerManifestRequest
}

func (f *fakeClient) PatchAPIServerManifest(_ context.Context, req *nkeproto.PatchAPIServerManifestRequest, _ ...grpc.CallOption) (*nkeproto.PatchAPIServerManifestResponse, error) {
	f.calls = append(f.calls, req)
	if f.patchResp == nil && f.patchErr == nil {
		// Default to a benign success for callers (e.g. Rollback) that don't set one.
		return &nkeproto.PatchAPIServerManifestResponse{Outcome: nkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED}, nil
	}
	return f.patchResp, f.patchErr
}

func dialerFor(c nodeAgentOIDCClient) nodeAgentClientDialer {
	return func(_ context.Context, _ rollout.Node) (nodeAgentOIDCClient, func(), error) { return c, func() {}, nil }
}

func fakeK8sClient() client.Client {
	return fake.NewClientBuilder().Build()
}

func cpNode() rollout.Node {
	return fakeNode{role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.2:9000"}
}

func validArtifact(t *testing.T) rollout.ApplyToNodePayload {
	t.Helper()
	op := NewEnableOIDCOperation(
		WithClient(fakeK8sClient()),
		WithOIDCConfig(&config.NkeOIDCAuth{Enabled: true, IssuerURL: "https://issuer.example"}),
	)
	payload, err := op.BuildApplyToNodePayload(context.Background(), fakeCluster{name: "tenant-1", k8s: "1.30.0", cps: []rollout.Node{cpNode()}})
	require.NoError(t, err)
	return payload
}

// --- ApplyToNode outcome mapping ---

func TestApplyToNode_OutcomeMapping(t *testing.T) {
	resp := func(o nkeproto.PatchAPIServerManifestResponse_Outcome) *nkeproto.PatchAPIServerManifestResponse {
		return &nkeproto.PatchAPIServerManifestResponse{Outcome: o}
	}
	cases := []struct {
		name    string
		client  *fakeClient
		want    rollout.Outcome
		wantErr bool
	}{
		{"applied->applied", &fakeClient{patchResp: resp(nkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED)}, rollout.OutcomeApplied, false},
		{"unchanged->unchanged", &fakeClient{patchResp: resp(nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED)}, rollout.OutcomeUnchanged, false},
		{"failed_restored->failed_restored", &fakeClient{patchResp: resp(nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED)}, rollout.OutcomeFailedRestored, true},
		{"failed_not_restored->failed_not_restored", &fakeClient{patchResp: resp(nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED)}, rollout.OutcomeFailedNotRestored, true},
		{"unspecified->failed_not_restored", &fakeClient{patchResp: resp(nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNSPECIFIED)}, rollout.OutcomeFailedNotRestored, true},
		{"grpc invalid_argument->failed_restored", &fakeClient{patchErr: status.Error(codes.InvalidArgument, "bad")}, rollout.OutcomeFailedRestored, true},
		{"grpc failed_precondition->failed_restored", &fakeClient{patchErr: status.Error(codes.FailedPrecondition, "not cp")}, rollout.OutcomeFailedRestored, true},
		{"grpc unimplemented->failed_restored", &fakeClient{patchErr: status.Error(codes.Unimplemented, "no such method")}, rollout.OutcomeFailedRestored, true},
		{"grpc internal->failed_not_restored", &fakeClient{patchErr: status.Error(codes.Internal, "mid-write")}, rollout.OutcomeFailedNotRestored, true},
	}
	payload := validArtifact(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := NewEnableOIDCOperation(WithDialer(nil))
			op.nodeAgentClientDialer = dialerFor(tc.client)
			got, err := op.ApplyToNode(context.Background(), cpNode(), payload)
			assert.Equal(t, tc.want, got)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestApplyToNode_GuardsBeforeDial(t *testing.T) {
	payload := validArtifact(t)
	op := NewEnableOIDCOperation()
	op.nodeAgentClientDialer = dialerFor(&fakeClient{}) // would succeed if reached

	t.Run("nil node", func(t *testing.T) {
		got, err := op.ApplyToNode(context.Background(), nil, payload)
		assert.Equal(t, rollout.OutcomeFailedRestored, got)
		assert.Error(t, err)
	})
	t.Run("worker role rejected", func(t *testing.T) {
		got, err := op.ApplyToNode(context.Background(), fakeNode{role: rollout.NodeRoleWorker}, payload)
		assert.Equal(t, rollout.OutcomeFailedRestored, got)
		assert.Error(t, err)
	})
	t.Run("empty payload", func(t *testing.T) {
		got, err := op.ApplyToNode(context.Background(), cpNode(), rollout.ApplyToNodePayload(nil))
		assert.Equal(t, rollout.OutcomeFailedRestored, got)
		assert.Error(t, err)
	})
	t.Run("dial failure -> failed_restored (node untouched)", func(t *testing.T) {
		dialErrOp := NewEnableOIDCOperation()
		dialErrOp.nodeAgentClientDialer = func(_ context.Context, _ rollout.Node) (nodeAgentOIDCClient, func(), error) {
			return nil, nil, errors.New("connection refused")
		}
		got, err := dialErrOp.ApplyToNode(context.Background(), cpNode(), payload)
		assert.Equal(t, rollout.OutcomeFailedRestored, got)
		assert.Error(t, err)
	})
}

// --- Validate gates ---

func TestValidate(t *testing.T) {
	ready := func() *EnableOIDCOperation {
		op := NewEnableOIDCOperation(
			WithClient(fakeK8sClient()),
			WithOIDCConfig(&config.NkeOIDCAuth{Enabled: true, IssuerURL: "https://issuer.example"}),
		)
		// Report the RPC as supported by default; subtests override it.
		op.methodChecker = func(context.Context, rollout.Node, string) error { return nil }
		return op
	}
	withDialer := func(op *EnableOIDCOperation) *EnableOIDCOperation {
		op.nodeAgentClientDialer = dialerFor(&fakeClient{})
		return op
	}
	goodCluster := fakeCluster{name: "tenant-1", k8s: "1.30.0", cps: []rollout.Node{cpNode()}}

	t.Run("happy", func(t *testing.T) {
		require.NoError(t, withDialer(ready()).Validate(context.Background(), goodCluster))
	})
	t.Run("nil cluster", func(t *testing.T) {
		assert.Error(t, withDialer(ready()).Validate(context.Background(), nil))
	})
	t.Run("no control-plane nodes", func(t *testing.T) {
		assert.Error(t, withDialer(ready()).Validate(context.Background(), fakeCluster{name: "tenant-1", k8s: "1.30.0"}))
	})
	t.Run("missing dialer", func(t *testing.T) {
		assert.Error(t, ready().Validate(context.Background(), goodCluster))
	})
	t.Run("missing method checker", func(t *testing.T) {
		op := withDialer(ready())
		op.methodChecker = nil
		assert.Error(t, op.Validate(context.Background(), goodCluster))
	})
	t.Run("unsupported enable RPC rejected", func(t *testing.T) {
		op := withDialer(ready())
		op.methodChecker = func(context.Context, rollout.Node, string) error {
			return nodeagent.ErrMethodUnsupported
		}
		err := op.Validate(context.Background(), goodCluster)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not implement")
	})
	t.Run("k8s below 1.30 rejected", func(t *testing.T) {
		assert.Error(t, withDialer(ready()).Validate(context.Background(), fakeCluster{name: "tenant-1", k8s: "1.29.5", cps: []rollout.Node{cpNode()}}))
	})
	t.Run("k8s with v-prefix and suffix accepted", func(t *testing.T) {
		require.NoError(t, withDialer(ready()).Validate(context.Background(), fakeCluster{name: "tenant-1", k8s: "v1.31.2+nke1", cps: []rollout.Node{cpNode()}}))
	})
	t.Run("non-DNS-1123 cluster name rejected", func(t *testing.T) {
		assert.Error(t, withDialer(ready()).Validate(context.Background(), fakeCluster{name: "Tenant_1", k8s: "1.30.0", cps: []rollout.Node{cpNode()}}))
	})
}

// --- Rollback + payload determinism ---

func TestRollback_DisablesNode(t *testing.T) {
	c := &fakeClient{}
	op := NewEnableOIDCOperation()
	op.nodeAgentClientDialer = dialerFor(c)
	require.NoError(t, op.Rollback(context.Background(), cpNode(), validArtifact(t)))
	require.Len(t, c.calls, 1, "Rollback must issue one PatchAPIServerManifest call")
	req := c.calls[0]
	require.Len(t, req.GetFlagOps(), 1)
	require.NotNil(t, req.GetFlagOps()[0].GetRemove(), "Rollback must issue the inverse (RemoveFlag)")
	assert.Equal(t, kubeadmconfig.AuthConfigFlagName, req.GetFlagOps()[0].GetRemove().GetName())
	assert.Contains(t, req.GetRemoveFiles(), "/etc/kubernetes/oidc/auth-config.yaml")
}

func TestRollback_DialFailureReturnsError(t *testing.T) {
	op := NewEnableOIDCOperation()
	op.nodeAgentClientDialer = func(_ context.Context, _ rollout.Node) (nodeAgentOIDCClient, func(), error) {
		return nil, nil, errors.New("connection refused")
	}
	err := op.Rollback(context.Background(), cpNode(), validArtifact(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial agent")
}

func TestRollback_FailedOutcomeReturnsError(t *testing.T) {
	// A FAILED_* outcome rides on the response with a nil gRPC error (two-lane
	// contract), so Rollback must surface it as an error rather than silently
	// reporting success.
	cases := []struct {
		name    string
		outcome nkeproto.PatchAPIServerManifestResponse_Outcome
		wantMsg string
	}{
		{"failed_restored", nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED, "failed and was restored"},
		{"failed_not_restored", nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED, "failed and was not restored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeClient{patchResp: &nkeproto.PatchAPIServerManifestResponse{
				Outcome:      tc.outcome,
				ErrorMessage: "boom",
			}}
			op := NewEnableOIDCOperation()
			op.nodeAgentClientDialer = dialerFor(c)
			err := op.Rollback(context.Background(), cpNode(), validArtifact(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.Contains(t, err.Error(), "boom")
		})
	}
}

func TestParsePayload_Errors(t *testing.T) {
	t.Run("empty payload", func(t *testing.T) {
		_, err := parsePayload(nil)
		require.Error(t, err)
	})
	t.Run("malformed json", func(t *testing.T) {
		_, err := parsePayload(rollout.ApplyToNodePayload("{not json"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshal")
	})
	t.Run("missing content or path", func(t *testing.T) {
		_, err := parsePayload(rollout.ApplyToNodePayload(`{"authConfigPath":""}`))
		require.Error(t, err)
	})
}

func TestBuildApplyToNodePayload_DeterministicAndNonEmpty(t *testing.T) {
	op := NewEnableOIDCOperation(
		WithClient(fakeK8sClient()),
		WithOIDCConfig(&config.NkeOIDCAuth{Enabled: true, IssuerURL: "https://issuer.example"}),
	)
	cluster := fakeCluster{name: "tenant-1", k8s: "1.30.0", cps: []rollout.Node{cpNode()}}
	first, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	second, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)
	assert.Equal(t, first, second, "payload must be byte-identical across builds (cross-CP consistency)")

	parsed, err := parsePayload(first)
	require.NoError(t, err)
	require.Len(t, parsed.Files, 1)
	assert.Equal(t, "/etc/kubernetes/oidc/auth-config.yaml", parsed.AuthConfigPath)
	assert.Equal(t, parsed.AuthConfigPath, parsed.Files[0].Path)
	assert.NotEmpty(t, parsed.Files[0].Content)
}

func TestBuildPatchRequests_MultipleFiles(t *testing.T) {
	// The payload models staged files as a list, so a second file (e.g. a CA
	// bundle) is staged on enable and removed on disable without a schema change,
	// while the flag/volume still key off the designated auth-config path.
	art := authConfigArtifact{
		Files: []fileArtifact{
			{Path: "/etc/kubernetes/oidc/auth-config.yaml", Content: []byte("auth"), Mode: 0o600, Owner: "root:root"},
			{Path: "/etc/kubernetes/oidc/ca.crt", Content: []byte("ca"), Mode: 0o644, Owner: "root:root"},
		},
		AuthConfigPath: "/etc/kubernetes/oidc/auth-config.yaml",
	}

	enable, err := buildEnablePatchRequest(art)
	require.NoError(t, err)
	require.Len(t, enable.GetFiles(), 2, "both files staged")
	assert.Equal(t, uint32(0o644), enable.GetFiles()[1].GetMode(), "each file carries its own mode")
	require.Len(t, enable.GetFlagOps(), 1)
	assert.Equal(t, "/etc/kubernetes/oidc/auth-config.yaml",
		enable.GetFlagOps()[0].GetSet().GetValue(), "flag keys off the auth-config path")

	disable, err := buildDisablePatchRequest(art)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{"/etc/kubernetes/oidc/auth-config.yaml", "/etc/kubernetes/oidc/ca.crt"},
		disable.GetRemoveFiles(), "both files removed on disable")
}
