// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidc implements the rollout.Operation that enables OIDC
// authentication on an existing tenant cluster's control plane: it delivers a
// kube-apiserver AuthenticationConfiguration to each control-plane node, one at
// a time, via the agent's PatchAPIServerManifest RPC.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/kubeadmconfig"
	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

const (
	// OperationName is the rollout registry key and Temporal activity name. It
	// names the logical operation (enable OIDC), independent of the agent RPC
	// it rides on, so it is unchanged by the move to PatchAPIServerManifest.
	OperationName = "oidc.enable-apiserver-authentication-config"
)

// EnableOIDCOperation enables OIDC on each control-plane node. rollout.Compile
// detects its optional Validator, ApplyToNodePayloadBuilder, and RollbackHandler
// capabilities. It is intentionally not a SatisfactionChecker: "already enabled"
// is tracked on the cluster row, which the framework Cluster does not expose, so
// the consumer workflow short-circuits before invoking the rollout.
const (
	nameLabel = "name"
)

type EnableOIDCOperation struct {
	nodeAgentClientDialer nodeAgentClientDialer
	k8sClient             client.Client
	oidcConfig            *config.NkeOIDCAuth
	// methodChecker verifies each control-plane agent implements the OIDC
	// RPCs (gRPC reflection) before any node is touched — the reusable capability
	// gate in place of an agent version floor.
	methodChecker nodeagent.MethodSupportChecker
}

var (
	_ rollout.Operation                 = (*EnableOIDCOperation)(nil)
	_ rollout.Validator                 = (*EnableOIDCOperation)(nil)
	_ rollout.ApplyToNodePayloadBuilder = (*EnableOIDCOperation)(nil)
	_ rollout.RollbackHandler           = (*EnableOIDCOperation)(nil)
)

// minK8sVersionForOIDC is the floor at which AuthenticationConfiguration is GA
// (structured authentication, kube-apiserver file auto-reload). Validate enforces
// it before any node is touched.
func minK8sVersionForOIDC() *version.Version { return version.MustParseGeneric("1.30.0") }

// NewEnableOIDCOperation constructs the operation. Runtime dependencies (the
// agent dialer, the client, and the OIDC config) are injected via options
// and validated by Validate before any node is touched.
func NewEnableOIDCOperation(opts ...Option) *EnableOIDCOperation {
	op := &EnableOIDCOperation{}
	for _, opt := range opts {
		opt(op)
	}
	return op
}

func (op *EnableOIDCOperation) Name() string { return OperationName }

// Validate implements rollout.Validator: it rejects the rollout up front if the
// runtime dependencies are missing or the cluster cannot host OIDC.
func (op *EnableOIDCOperation) Validate(ctx context.Context, cluster rollout.Cluster) error {
	if cluster == nil {
		return errors.New("cluster is nil")
	}
	cpNodes := cluster.ControlPlaneNodes()
	if len(cpNodes) == 0 {
		return fmt.Errorf("cluster %s has no control-plane nodes", cluster.Name())
	}
	if _, err := op.resolveNodeAgentClientDialer(); err != nil {
		return err
	}
	if op.k8sClient == nil {
		return errors.New("enable-oidc operation client is not configured")
	}
	if op.oidcConfig == nil {
		return errors.New("enable-oidc operation OIDC config is not configured")
	}
	if op.methodChecker == nil {
		return errors.New("enable-oidc operation agent method-support checker is not configured")
	}

	parsed, err := version.ParseGeneric(cluster.KubernetesVersion())
	if err != nil {
		return fmt.Errorf("parse cluster %s Kubernetes version %q: %w", cluster.Name(), cluster.KubernetesVersion(), err)
	}
	minVersion := minK8sVersionForOIDC()
	if parsed.LessThan(minVersion) {
		return fmt.Errorf("cluster %s Kubernetes version %s is below the %s required for AuthenticationConfiguration",
			cluster.Name(), parsed, minVersion)
	}

	// The cluster name is used verbatim as the OIDC audience and to name
	// per-cluster Kubernetes objects (e.g. the kubeconfig Secret), so it must be a
	// valid DNS name. Re-checked here because a brownfield cluster may predate the
	// constraint.
	if errs := validation.IsDNS1123Subdomain(cluster.Name()); len(errs) > 0 {
		return fmt.Errorf("cluster name %q is not a valid DNS name: %s", cluster.Name(), strings.Join(errs, "; "))
	}

	// Verify every control-plane agent implements the PatchAPIServerManifest
	// RPC before touching any node — a gRPC-reflection capability check (the
	// reusable nodeagent.MethodSupportChecker) rather than a brittle agent
	// version floor. An agent that predates the RPC fails here, cleanly, with
	// nothing mutated; ApplyToNode additionally maps a live Unimplemented as a
	// clean failure, covering agents that do not serve reflection.
	for _, node := range cpNodes {
		if err := op.methodChecker(ctx, node, nkeproto.NodeAgent_PatchAPIServerManifest_FullMethodName); err != nil {
			if nodeagent.IsMethodUnsupported(err) {
				return fmt.Errorf("node %s agent does not implement PatchAPIServerManifest (upgrade the agent): %w", node.Name(), err)
			}
			return fmt.Errorf("verify node %s agent supports OIDC enablement: %w", node.Name(), err)
		}
	}

	return nil
}

// fileArtifact is one file staged on a control-plane node before the manifest
// mutation, carrying its own mode and owner.
type fileArtifact struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner"`
}

// authConfigArtifact is the rollout payload: the files to stage on each
// control-plane node, and which of them is the AuthenticationConfiguration
// (AuthConfigPath is the --authentication-config value and its directory is the
// mounted volume). Modeling the staged files as a list lets a future need — say
// a standalone issuer-CA bundle alongside the auth-config — add an entry without
// revving this schema, which matters because the payload rides Temporal
// workflow history. It carries no secret material (the issuer CA bundle is a
// public certificate), so it is safe in that history.
type authConfigArtifact struct {
	Files          []fileArtifact `json:"files"`
	AuthConfigPath string         `json:"authConfigPath"`
}

// BuildApplyToNodePayload implements rollout.ApplyToNodePayloadBuilder. It renders
// the auth-config once so every control-plane node receives byte-identical bytes;
// divergence would let one apiserver accept an audience another rejects.
func (op *EnableOIDCOperation) BuildApplyToNodePayload(ctx context.Context, cluster rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	if cluster == nil {
		return nil, errors.New("cluster is nil")
	}
	if op.oidcConfig == nil {
		return nil, errors.New("enable-oidc operation OIDC config is not configured")
	}

	var caBundle []byte
	if ref := op.oidcConfig.CABundleSecret; !ref.Empty() {
		var err error
		caBundle, err = kubeadmconfig.ReadOIDCCABundle(ctx, op.k8sClient, ref)
		if err != nil {
			return nil, fmt.Errorf("read OIDC CA bundle for cluster %s: %w", cluster.Name(), err)
		}
	}

	cfg, err := kubeadmconfig.BuildOIDCAuthenticationConfig(cluster.Name(), *op.oidcConfig, caBundle)
	if err != nil {
		return nil, fmt.Errorf("build OIDC authentication config for cluster %s: %w", cluster.Name(), err)
	}
	file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("render OIDC authentication config for cluster %s: %w", cluster.Name(), err)
	}
	out, err := json.Marshal(authConfigArtifact{
		Files: []fileArtifact{{
			Path:    file.Path,
			Content: file.Content,
			Mode:    file.Mode,
			Owner:   file.Owner,
		}},
		AuthConfigPath: file.Path,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal OIDC auth-config artifact: %w", err)
	}
	return rollout.ApplyToNodePayload(out), nil
}

// ApplyToNode implements rollout.Operation: it dials the target control-plane
// node and calls PatchAPIServerManifest, mapping the result to a rollout.Outcome.
func (op *EnableOIDCOperation) ApplyToNode(ctx context.Context, node rollout.Node, payload rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	if node == nil {
		return rollout.OutcomeFailedRestored, errors.New("node is nil")
	}
	if node.Role() != rollout.NodeRoleControlPlane {
		return rollout.OutcomeFailedRestored, fmt.Errorf("node %s has role %q; OIDC enablement only targets control-plane nodes",
			node.Name(), node.Role())
	}
	artifact, err := parsePayload(payload)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}
	req, err := buildEnablePatchRequest(artifact)
	if err != nil {
		return rollout.OutcomeFailedRestored, fmt.Errorf("build patch request for node %s: %w", node.Name(), err)
	}

	client, cleanup, err := op.dialNode(ctx, node)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}
	defer cleanup()

	resp, err := client.PatchAPIServerManifest(ctx, req)
	if err != nil {
		// No business Outcome was returned. These codes are raised before the
		// manifest is touched, so the node is left clean:
		//   - Unimplemented: the agent predates the RPC — nothing ran.
		//     Validate's reflection check should catch this up front; this is the
		//     belt-and-suspenders signal for agents that do not serve reflection.
		//   - InvalidArgument/FailedPrecondition: rejected before any mutation.
		// Any other code (transport drop, Internal) may have mutated the node.
		if c := status.Code(err); c == codes.Unimplemented || c == codes.InvalidArgument || c == codes.FailedPrecondition {
			return rollout.OutcomeFailedRestored, fmt.Errorf("enable OIDC on node %s: %w", node.Name(), err)
		}
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("enable OIDC on node %s: %w", node.Name(), err)
	}

	// The agent reports its own success/restore status via the Outcome enum
	// (it must return these with a nil error, per the two-lane contract, or the
	// FAILED_RESTORED/NOT_RESTORED distinction is lost over the wire).
	switch resp.GetOutcome() {
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED:
		return rollout.OutcomeApplied, nil
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED:
		return rollout.OutcomeUnchanged, nil
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED:
		return rollout.OutcomeFailedRestored, outcomeError("enable", node.Name(), resp)
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED,
		nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNSPECIFIED:
		// Both may have left the node mutated, so cascade-roll back. outcomeError
		// renders the exact reason (not-restored vs unspecified).
		return rollout.OutcomeFailedNotRestored, outcomeError("enable", node.Name(), resp)
	default:
		// Any unknown/future outcome: treat as not-restored and cascade-roll back.
		return rollout.OutcomeFailedNotRestored, outcomeError("enable", node.Name(), resp)
	}
}

// Rollback implements rollout.RollbackHandler: on cascade it disables OIDC on a
// previously-applied node. The framework runs rollbacks best-effort in reverse
// order, so it returns the error for logging rather than halting the cascade.
func (op *EnableOIDCOperation) Rollback(ctx context.Context, node rollout.Node, payload rollout.ApplyToNodePayload) error {
	if node == nil {
		return errors.New("node is nil")
	}
	artifact, err := parsePayload(payload)
	if err != nil {
		return err
	}
	disableReq, err := buildDisablePatchRequest(artifact)
	if err != nil {
		return fmt.Errorf("build disable patch request for node %s: %w", node.Name(), err)
	}
	client, cleanup, err := op.dialNode(ctx, node)
	if err != nil {
		return err
	}
	defer cleanup()
	resp, err := client.PatchAPIServerManifest(ctx, disableReq)
	if err != nil {
		return fmt.Errorf("disable OIDC on node %s: %w", node.Name(), err)
	}
	// Business outcomes ride on the response with a nil gRPC error (the RPC's
	// two-lane contract), so a FAILED_* outcome must surface as an error even
	// though the call itself succeeded. Mirrors the forward path's handling.
	return outcomeError("disable", node.Name(), resp)
}

// dialNode resolves the dialer and opens an agent connection for node,
// returning the client and a cleanup func the caller must defer. A missing
// dialer, a dial error, or a nil client is returned as an error so the caller
// maps it to the right outcome; the returned cleanup is always non-nil.
func (op *EnableOIDCOperation) dialNode(ctx context.Context, node rollout.Node) (nodeAgentOIDCClient, func(), error) {
	dialer, err := op.resolveNodeAgentClientDialer()
	if err != nil {
		return nil, nil, err
	}
	client, cleanup, err := dialer(ctx, node)
	if cleanup == nil {
		cleanup = func() {}
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("dial agent on node %s: %w", node.Name(), err)
	}
	if client == nil {
		cleanup()
		return nil, nil, fmt.Errorf("dial agent on node %s returned nil client", node.Name())
	}
	return client, cleanup, nil
}

// outcomeError maps a non-success PatchAPIServerManifest response to an error for
// the given verb ("enable"/"disable"), or nil for APPLIED/UNCHANGED. Shared by
// the forward apply and the rollback so both describe outcomes identically.
func outcomeError(verb, nodeName string, resp *nkeproto.PatchAPIServerManifestResponse) error {
	switch resp.GetOutcome() {
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED,
		nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED:
		return nil
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED:
		return fmt.Errorf("%s OIDC on node %s failed and was restored: %s", verb, nodeName, resp.GetErrorMessage())
	case nkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED,
		nkeproto.PatchAPIServerManifestResponse_OUTCOME_UNSPECIFIED:
		return fmt.Errorf("%s OIDC on node %s failed and was not restored: %s", verb, nodeName, resp.GetErrorMessage())
	default:
		return fmt.Errorf("%s OIDC on node %s returned unexpected outcome %v", verb, nodeName, resp.GetOutcome())
	}
}

// buildEnablePatchRequest renders the forward PatchAPIServerManifest request that
// wires OIDC into the apiserver: it stages the auth-config file, adds the
// auth-config hostPath volume + read-only mount (a strategic-merge patch, keyed
// by name/mountPath so it is idempotent), and sets --authentication-config to the
// file path (a convergent flag op, which strategic-merge cannot express on the
// keyless command list).
func buildEnablePatchRequest(a authConfigArtifact) (*nkeproto.PatchAPIServerManifestRequest, error) {
	smp, err := oidcVolumeMountSMP(filepath.Dir(a.AuthConfigPath), false)
	if err != nil {
		return nil, fmt.Errorf("render auth-config volume patch: %w", err)
	}
	return &nkeproto.PatchAPIServerManifestRequest{
		Files:                 filesToProto(a.Files),
		StrategicMergePatches: [][]byte{smp},
		FlagOps: []*nkeproto.FlagOp{{
			Op: &nkeproto.FlagOp_Set{Set: &nkeproto.SetFlag{Name: kubeadmconfig.AuthConfigFlagName, Value: a.AuthConfigPath}},
		}},
	}, nil
}

// filesToProto converts the payload's staged files to agent File messages.
func filesToProto(files []fileArtifact) []*nkeproto.File {
	out := make([]*nkeproto.File, len(files))
	for i, f := range files {
		out[i] = &nkeproto.File{Path: f.Path, Content: f.Content, Mode: f.Mode, Owner: f.Owner}
	}
	return out
}

// filePaths returns the on-host path of each staged file.
func filePaths(files []fileArtifact) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// buildDisablePatchRequest renders the inverse request: strip the
// --authentication-config flag, delete the auth-config volume + mount (a
// strategic-merge $patch: delete keyed by name/mountPath), and remove the staged
// files. Every field is idempotent, so a re-run against an already-clean node is
// a no-op (OUTCOME_UNCHANGED).
func buildDisablePatchRequest(a authConfigArtifact) (*nkeproto.PatchAPIServerManifestRequest, error) {
	smp, err := oidcVolumeMountSMP(filepath.Dir(a.AuthConfigPath), true)
	if err != nil {
		return nil, fmt.Errorf("render auth-config volume delete patch: %w", err)
	}
	return &nkeproto.PatchAPIServerManifestRequest{
		StrategicMergePatches: [][]byte{smp},
		FlagOps: []*nkeproto.FlagOp{{
			Op: &nkeproto.FlagOp_Remove{Remove: &nkeproto.RemoveFlag{Name: kubeadmconfig.AuthConfigFlagName}},
		}},
		RemoveFiles: filePaths(a.Files),
	}, nil
}

// oidcVolumeMountSMP builds the strategic-merge patch that adds (or, when del is
// true, deletes) the auth-config hostPath volume and its read-only mount on the
// kube-apiserver container. Emitted as JSON, which is valid strategic-merge input.
func oidcVolumeMountSMP(dir string, del bool) ([]byte, error) {
	mount := map[string]any{nameLabel: kubeadmconfig.AuthConfigVolumeName, "mountPath": dir}
	volume := map[string]any{nameLabel: kubeadmconfig.AuthConfigVolumeName}
	if del {
		mount["$patch"] = "delete"
		volume["$patch"] = "delete"
	} else {
		mount["readOnly"] = true
		volume["hostPath"] = map[string]any{"path": dir, "type": "DirectoryOrCreate"}
	}
	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"containers": []any{map[string]any{
				nameLabel:      "kube-apiserver",
				"volumeMounts": []any{mount},
			}},
			"volumes": []any{volume},
		},
	})
}

func (op *EnableOIDCOperation) resolveNodeAgentClientDialer() (nodeAgentClientDialer, error) {
	if op.nodeAgentClientDialer != nil {
		return op.nodeAgentClientDialer, nil
	}
	return nil, errors.New("enable-oidc operation agent dialer is not configured")
}

func parsePayload(payload rollout.ApplyToNodePayload) (authConfigArtifact, error) {
	if len(payload) == 0 {
		return authConfigArtifact{}, errors.New("OIDC auth-config artifact is empty")
	}
	var out authConfigArtifact
	if err := json.Unmarshal(payload, &out); err != nil {
		return authConfigArtifact{}, fmt.Errorf("unmarshal OIDC auth-config artifact: %w", err)
	}
	if len(out.Files) == 0 || out.AuthConfigPath == "" {
		return authConfigArtifact{}, errors.New("OIDC auth-config artifact is missing files or auth-config path")
	}
	authConfigStaged := false
	for _, f := range out.Files {
		if f.Path == "" || len(f.Content) == 0 {
			return authConfigArtifact{}, errors.New("OIDC auth-config artifact has a file with no path or content")
		}
		if f.Path == out.AuthConfigPath {
			authConfigStaged = true
		}
	}
	if !authConfigStaged {
		return authConfigArtifact{}, fmt.Errorf("OIDC auth-config artifact authConfigPath %q is not among the staged files", out.AuthConfigPath)
	}
	return out, nil
}
