// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "gopkg.in/yaml.v2"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// encRotationWALEntry is the WAL checkpoint written atomically before the
// encryption config is updated. It is JSON-serialized to encRotationWALFile.
type encRotationWALEntry struct {
	// ReloadCountBefore is retained so existing checkpoints stay readable; new
	// checkpoints leave it zero. A process-local counter is not proof that the
	// intended config is loaded.
	ReloadCountBefore int `json:"reload_count_before"`
	// OpID identifies the specific rotation operation (see encRotationOpID) so
	// that a retry can detect a stale checkpoint left by a different, preceding
	// operation and reject the conflicting request.
	OpID string `json:"op_id"`
}

const (
	// encRotationWALFile is the WAL entry written atomically before the
	// encryption config is updated (JSON-encoded encRotationWALEntry).
	// It records the UpdateEncryptionConfig operation id and the apiserver reload
	// counter before the operation is applied.
	//
	// A matching pending operation verifies the intended on-disk configuration
	// against the local apiserver's loaded configuration identity, even after
	// process restart. A different operation cannot take over its checkpoint.
	// Already-desired config without a WAL still requires live loaded-config proof.
	encRotationWALFile = "/var/lib/nvke/enc-rotation.state"

	// encryptionReloadTimeout is the maximum time UpdateEncryptionConfig will
	// block waiting for the apiserver to confirm a reload. Callers that receive
	// DeadlineExceeded may safely retry.
	encryptionReloadTimeout = 120 * time.Second

	// defaultReloadPollInterval is how often the reload counter is polled.
	defaultReloadPollInterval = 2 * time.Second

	// encryptionMetricsFailLimit is the maximum duration of consecutive metric
	// poll failures tolerated during the wait. Exceeding it causes a hard error.
	encryptionMetricsFailLimit = 90 * time.Second

	// apiserverMetricsProbeTimeout bounds a single GET /metrics call.
	// Intentionally generous: /metrics returns much more data than /readyz
	// and a slow apiserver under load can take longer to serve it. The bound
	// only exists so a stalled TCP connection cannot hang the helper
	// indefinitely — healthy responses come back in well under a second.
	apiserverMetricsProbeTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// UpdateEncryptionConfig — main entry point
// ---------------------------------------------------------------------------

// UpdateEncryptionConfig applies a key-rotation mutation (add secondary key,
// promote primary, or remove old keys) to the EncryptionConfiguration, waits
// for the local apiserver to confirm it has reloaded the new config, and
// returns only after the new config is active in the apiserver's memory.
//
// The operation is idempotent and crash-safe:
//
//   - A WAL entry is written before any config change.  If the process
//     crashes or the RPC times out, the next call re-reads the WAL and
//     resumes from exactly the right point without re-applying the mutation.
//
//   - On DeadlineExceeded the WAL remains on disk so the caller can retry
//     safely. Other failures also preserve a pending WAL for recovery.
func (n *NodeAgentServer) UpdateEncryptionConfig(
	ctx context.Context,
	req *pb.UpdateEncryptionConfigRequest,
) (*pb.UpdateEncryptionConfigResponse, error) {
	defer logRPCSpan(ctx, "UpdateEncryptionConfig")()
	if n.NodeStatus.Role != ControlPlaneRole {
		return nil, errors.New("UpdateEncryptionConfig can only be run on controlplane nodes")
	}
	if err := validateUpdateEncryptionConfigRequest(req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, encryptionReloadTimeout)
	defer cancel()

	walPath := n.encRotationWALPath()

	opID, err := encRotationOpID(req)
	if err != nil {
		return nil, err
	}

	// Mutation admission and the WAL/config write share encryptionMu. Polling
	// runs outside it; the final bounded confirmation reacquires it so agent
	// writers cannot change ownership or configuration during cleanup.
	n.encryptionMu.Lock()
	result, err := n.tryCommitEncryptionConfigUpdate(ctx, req, walPath)
	n.encryptionMu.Unlock()

	if ctx.Err() != nil {
		return nil, encryptionConfirmationContextError(ctx)
	}
	if err != nil {
		return nil, err
	}
	// The loaded config identity, not a counter from an earlier apiserver
	// process, proves completion. Cleanup rechecks identity and ownership.
	if err := n.waitForEncryptionConfig(ctx, walPath, opID, result.configHash); err != nil {
		return nil, err
	}
	return &pb.UpdateEncryptionConfigResponse{}, nil
}

// ---------------------------------------------------------------------------
// Request validation (called first by UpdateEncryptionConfig)
// ---------------------------------------------------------------------------

// validateUpdateEncryptionConfigRequest returns an error if the request is
// structurally invalid (missing operation or malformed key). This runs before
// any lock is acquired so it is cheap to call on every retry.
func validateUpdateEncryptionConfigRequest(req *pb.UpdateEncryptionConfigRequest) error {
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_AddSecondaryKey:
		if err := validateEncryptionKey(op.AddSecondaryKey.GetNewKey()); err != nil {
			return fmt.Errorf("invalid new_key: %w", err)
		}
	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		if err := validateEncryptionKey(op.PromotePrimaryKey.GetNewKey()); err != nil {
			return fmt.Errorf("invalid new_key: %w", err)
		}
	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		if err := validateEncryptionKey(op.RemoveOldKeys.GetPrimaryKey()); err != nil {
			return fmt.Errorf("invalid primary_key: %w", err)
		}
	default:
		return errors.New("operation is required")
	}
	return nil
}

// validateEncryptionKey checks that key is non-empty, valid standard base64
// (with padding, as produced by the NVKE controller when generating etcd
// encryption keys), and decodes to exactly 32 bytes (secretbox requirement).
func validateEncryptionKey(key string) error {
	if key == "" {
		return errors.New("encryption key must not be empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("encryption key must be standard base64-encoded (with padding): %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("encryption key must decode to 32 bytes, got %d", len(decoded))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Path / ID helpers (called early by UpdateEncryptionConfig)
// ---------------------------------------------------------------------------

// encRotationWALPath returns the absolute path to the rotation WAL file,
// respecting the server's RootPath for test isolation.
func (n *NodeAgentServer) encRotationWALPath() string {
	return filepath.Join(n.RootPath, strings.TrimPrefix(encRotationWALFile, "/"))
}

// encRotationOpID returns a short string that uniquely identifies the
// mutation type and target key for this request. It is stored in the WAL entry
// so that a retry can detect whether the on-disk checkpoint belongs to the
// same operation or to a different, preceding one (in which case it is stale).
func encRotationOpID(req *pb.UpdateEncryptionConfigRequest) (string, error) {
	var opType, key string
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_AddSecondaryKey:
		opType, key = "add", op.AddSecondaryKey.GetNewKey()
	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		opType, key = "promote", op.PromotePrimaryKey.GetNewKey()
	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		opType, key = "remove", op.RemoveOldKeys.GetPrimaryKey()
	default:
		return "", status.Errorf(codes.Internal, "unknown operation type %T", req.Operation)
	}
	// Store a short hash of the key rather than any key material.
	// 8 hex chars (32 bits) is sufficient for disambiguation.
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s:%x", opType, h[:4]), nil
}

// ---------------------------------------------------------------------------
// Phase 1: tryCommitEncryptionConfigUpdate — WAL recovery + config write (under lock)
// ---------------------------------------------------------------------------

// walRecoveryState classifies the WAL recovery state from the combination of
// on-disk config and WAL entry.
type walRecoveryState int

const (
	// walCompleted: config is in desired state without a WAL; confirm loaded
	// identity without writing another checkpoint or mutating the file.
	walCompleted walRecoveryState = iota
	// walReloadPending: config is in desired state, WAL entry belongs to this
	// op → wait for apiserver reload.
	walReloadPending
	// walStaleEntry: a WAL entry from a different operation exists →
	// caller must retry that operation first.
	walStale
	// walNeedsWrite: config is not in desired state and no conflicting WAL
	// entry → proceed with WAL write + config mutation.
	walNeedsWrite
)

// classifyWALState determines the recovery state from the on-disk config,
// WAL entry, and the requested operation's opID.
func classifyWALState(configInDesiredState, walExists bool, savedOpID, requestedOpID string) walRecoveryState {
	switch {
	case configInDesiredState && !walExists:
		return walCompleted
	case walExists && savedOpID != requestedOpID:
		// WAL from a different op — must be resolved before proceeding,
		// regardless of whether the config happens to be in the desired state.
		return walStale
	case configInDesiredState && walExists:
		// WAL belongs to this op; reload may still be pending.
		return walReloadPending
	default:
		// Config not in desired state, no conflicting WAL entry.
		return walNeedsWrite
	}
}

// prevEncResult is returned by tryCommitEncryptionConfigUpdate.
type prevEncResult struct {
	configHash string // agent-local only; never log or return this fingerprint
}

// tryCommitEncryptionConfigUpdate reads the current config and WAL entry, classifies
// the WAL recovery state, and writes the new config if needed. It MUST be
// called with n.encryptionMu held.
func (n *NodeAgentServer) tryCommitEncryptionConfigUpdate(
	ctx context.Context,
	req *pb.UpdateEncryptionConfigRequest,
	walPath string,
) (prevEncResult, error) {
	if ctx.Err() != nil {
		return prevEncResult{}, encryptionConfirmationContextError(ctx)
	}
	cfg, currentHash, err := n.readEncryptionConfigWithHash()
	if err != nil {
		return prevEncResult{}, err
	}
	if err := cfg.ValidateSecretbox(); err != nil {
		return prevEncResult{}, err
	}
	if err := cfg.validateMutationIdentity(req); err != nil {
		return prevEncResult{}, err
	}

	wal, walExists, err := readEncRotationWAL(walPath)
	if err != nil {
		return prevEncResult{}, err
	}

	opID, err := encRotationOpID(req)
	if err != nil {
		return prevEncResult{}, err
	}

	state := classifyWALState(cfg.IsInDesiredState(req), walExists, wal.OpID, opID)

	switch state {
	case walCompleted:
		slog.Info("UpdateEncryptionConfig: config already in desired state")
		return prevEncResult{configHash: currentHash}, nil

	case walReloadPending:
		// WAL state indicates this operation is a retry; caller will check outside the
		// lock whether reload is already confirmed, and block-wait if not.
		return prevEncResult{configHash: currentHash}, nil

	case walStale:
		slog.Error("stale WAL entry from a different operation; caller must retry the previous operation first",
			"saved_op_id", wal.OpID,
			"requested_op_id", opID,
		)
		return prevEncResult{}, status.Errorf(codes.FailedPrecondition,
			"a previous key-rotation operation (%s) did not complete cleanly; retry that operation before issuing a new one",
			wal.OpID,
		)
	case walNeedsWrite:
		return n.commitConfigWrite(ctx, cfg, currentHash, req, walPath)
	default:
		panic("impossible wal state")
	}
}

// readEncryptionConfig reads and parses the EncryptionConfiguration from disk.
func (n *NodeAgentServer) readEncryptionConfig() (*EncryptionConfiguration, error) {
	cfg, _, err := n.readEncryptionConfigWithHash()
	return cfg, err
}

// readEncryptionConfigWithHash binds parsed state and fingerprint to one read.
func (n *NodeAgentServer) readEncryptionConfigWithHash() (*EncryptionConfiguration, string, error) {
	configPath := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml")
	b, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("encryption config not found; caller must retry the previous operation first",
				"config_path", configPath,
			)
			return nil, "", status.Errorf(codes.FailedPrecondition,
				"encryption config not found at %s; run InitializeKubernetesAPIEncryption first",
				configPath,
			)
		}
		slog.Error("read encryption config", "err", err, "config_path", configPath)
		return nil, "", fmt.Errorf("read encryption config: %w", err)
	}
	cfg := &EncryptionConfiguration{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, "", fmt.Errorf("unmarshal encryption config: %w", err)
	}
	return cfg, encryptionConfigContentHash(b), nil
}

// ---------------------------------------------------------------------------
// WAL read/write helpers
// ---------------------------------------------------------------------------

// readEncRotationWAL reads and deserializes the rotation WAL entry from disk.
// Returns (entry, true, nil) if the file exists and is valid JSON.
// Returns (zero, false, nil) if the file does not exist.
func readEncRotationWAL(path string) (encRotationWALEntry, bool, error) {
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return encRotationWALEntry{}, false, nil
	}
	if readErr != nil {
		return encRotationWALEntry{}, false, fmt.Errorf("read enc-rotation WAL: %w", readErr)
	}
	var s encRotationWALEntry
	if err := json.Unmarshal(data, &s); err != nil {
		return encRotationWALEntry{}, false, fmt.Errorf("parse enc-rotation state: %w", err)
	}
	return s, true, nil
}

// writeEncRotationWAL atomically serializes the WAL entry to disk,
// creating the directory if necessary.
func writeEncRotationWAL(path string, state encRotationWALEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o600)
}

// ---------------------------------------------------------------------------
// commitConfigWrite: WAL write + config mutation (called by tryCommitEncryptionConfigUpdate)
// ---------------------------------------------------------------------------

// commitConfigWrite is called (under lock) when the config must be written.
// It validates mutation preconditions, snapshots the reload counter, applies
// the mutation, and performs the two WAL writes (WAL ①, config ②).
func (n *NodeAgentServer) commitConfigWrite(
	ctx context.Context,
	cfg *EncryptionConfiguration,
	currentHash string,
	req *pb.UpdateEncryptionConfigRequest,
	walPath string,
) (prevEncResult, error) {
	if err := cfg.ValidateMutation(req); err != nil {
		return prevEncResult{}, err
	}

	// Establish that this apiserver exposes a loaded-config identity before
	// mutation. Missing/invalid metrics cannot be replaced by a counter guess.
	baseline, err := n.getApiserverEncryptionState(ctx)
	if err != nil {
		return prevEncResult{}, status.Errorf(codes.Unavailable, "cannot establish reload baseline (metrics unavailable): %v", err)
	}
	onDisk, err := n.encryptionConfigHash()
	if err != nil {
		return prevEncResult{}, err
	}
	if onDisk != currentHash {
		return prevEncResult{}, status.Error(codes.FailedPrecondition, "encryption config changed before mutation")
	}
	if baseline.configHash != currentHash {
		return prevEncResult{}, status.Error(codes.FailedPrecondition, "apiserver has not loaded the current encryption config; retry after it converges")
	}
	if ctx.Err() != nil {
		return prevEncResult{}, encryptionConfirmationContextError(ctx)
	}
	opID, err := encRotationOpID(req)
	if err != nil {
		return prevEncResult{}, err
	}

	// WAL step ①: commit intent to disk BEFORE mutating anything. If the process
	// crashes after this but before step ②, the next retry sees WAL +
	// config=old and re-runs from here. The op_id lets a retry detect a stale
	// checkpoint left by a different, preceding operation.
	if err := writeEncRotationWAL(walPath, encRotationWALEntry{OpID: opID}); err != nil {
		return prevEncResult{}, fmt.Errorf("write rotation WAL: %w", err)
	}

	cfg.ApplyMutation(req)

	// WAL step ②: atomic config write. On failure the WAL remains so the
	// next retry detects the partial state and re-writes the config.
	configPath := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml")
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return prevEncResult{}, fmt.Errorf("marshal encryption config: %w", err)
	}
	if err := atomicWriteFile(configPath, b, 0o600); err != nil {
		return prevEncResult{}, fmt.Errorf("write encryption config: %w", err)
	}

	return prevEncResult{configHash: encryptionConfigContentHash(b)}, nil
}

// atomicWriteFile writes data to path atomically via a temp file + rename,
// preventing partial writes from corrupting the target file on crash.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".enc-cfg-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path) // atomic on same filesystem
}

// ---------------------------------------------------------------------------
// Phase 2 & 3: reload confirmation and cleanup
// ---------------------------------------------------------------------------

// getEncryptionReloadSuccessCount queries the local apiserver's Prometheus
// metrics endpoint directly (https://127.0.0.1:6443) and returns the
// cumulative count of successful encryption-config auto-reloads. Targeting
// the loopback address ensures each CP node polls its own apiserver.
func (n *NodeAgentServer) getEncryptionReloadSuccessCount() (int, error) {
	raw, err := fetchApiserverMetricsFromRegistry(n.clientRegistry)
	if err != nil {
		return 0, fmt.Errorf("get apiserver metrics: %w", err)
	}
	return parseEncryptionReloadCount(string(raw)), nil
}

// fetchApiserverMetricsFromRegistry uses client-go to GET /metrics from the
// local apiserver at 127.0.0.1:6443.
//
// The registry's LocalClient keeps this pointed at the node-local apiserver
// instead of the cluster VIP.
func fetchApiserverMetricsFromRegistry(registry kubernetesClientRegistry) ([]byte, error) {
	if registry == nil {
		return nil, errors.New("k8s client registry is not configured")
	}
	client, err := registry.LocalClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiserverMetricsProbeTimeout)
	defer cancel()
	return client.RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
}

// parseEncryptionReloadCount parses raw Prometheus text and sums the cumulative
// successful auto-reload counts. Supports both metric names:
//   - k8s ≥1.30: apiserver_encryption_config_controller_automatic_reloads_total{...,status="success",...}
//   - k8s ≤1.29: apiserver_encryption_config_controller_automatic_reload_success_total{...}
func parseEncryptionReloadCount(metrics string) int {
	total := 0
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		isNewFormat := strings.HasPrefix(line, "apiserver_encryption_config_controller_automatic_reloads_total{") &&
			strings.Contains(line, `status="success"`)
		isOldFormat := strings.HasPrefix(line, "apiserver_encryption_config_controller_automatic_reload_success_total{")
		if !isNewFormat && !isOldFormat {
			continue
		}
		// Prometheus text format: metric_name{labels} value [timestamp]
		// Value is the second whitespace-separated token.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
			total += int(v)
		}
	}
	return total
}

// waitForEncryptionReload blocks until the local apiserver's encryption-config
// reload counter exceeds reloadCountBefore, indicating the new config is active.
//
// It returns codes.DeadlineExceeded if the overall timeout fires — the caller
// should treat this as a signal to retry (the WAL is still on disk).
//
// It returns a hard error if the metrics endpoint is consistently unavailable
// for encryptionMetricsFailLimit: in this case we cannot confirm reload, so
// we surface the failure rather than returning a false positive.
func (n *NodeAgentServer) waitForEncryptionReload(ctx context.Context, reloadCountBefore int) error {
	ctx, cancel := context.WithTimeout(ctx, encryptionReloadTimeout)
	defer cancel()

	ticker := time.NewTicker(n.reloadPoll())
	defer ticker.Stop()

	var metricDownSince time.Time // zero ↔ metrics are healthy

	for {
		select {
		case <-ctx.Done():
			return status.Errorf(codes.DeadlineExceeded,
				"timed out waiting for apiserver to reload encryption config; retry is safe")

		case <-ticker.C:
			R, err := n.getEncryptionReloadSuccessCount()
			if err != nil {
				if metricDownSince.IsZero() {
					metricDownSince = time.Now()
				} else if time.Since(metricDownSince) >= encryptionMetricsFailLimit {
					return status.Errorf(codes.Unavailable,
						"apiserver metrics endpoint consistently unavailable for %s: cannot confirm encryption config reload; retry after the apiserver recovers",
						encryptionMetricsFailLimit)
				}
				slog.Warn("encryption reload: metric poll failed",
					"downDuration", time.Since(metricDownSince).Round(time.Second),
					"err", err)
				continue
			}
			metricDownSince = time.Time{} // reset on success

			if R > reloadCountBefore {
				slog.Info("encryption config reload confirmed", "reloadCountBefore", reloadCountBefore, "reloadCount", R)
				return nil
			}
			slog.Info("waiting for encryption config reload", "reloadCount", R, "reloadCountBefore", reloadCountBefore)
		}
	}
}

// reloadPoll returns the effective poll interval (test override or default).
func (n *NodeAgentServer) reloadPoll() time.Duration {
	if n.reloadPollInterval != 0 {
		return n.reloadPollInterval
	}
	return defaultReloadPollInterval
}

func (n *NodeAgentServer) markK8sAPIEncryptionErrorLocked() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
}

func (n *NodeAgentServer) setRewriteEncryptedNodeNameLocked(hostname string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.NodeStatus.NodeName = hostname
}

func (n *NodeAgentServer) StartEtcdEncryptionAtRestObjectsRewrite(
	ctx context.Context,
	req *pb.StartEtcdEncryptionAtRestObjectsRewriteRequest,
) (*pb.StartEtcdEncryptionAtRestObjectsRewriteResponse, error) {
	defer logRPCSpan(ctx, "StartEtcdEncryptionAtRestObjectsRewrite")()

	operationID := strings.TrimSpace(req.GetOperationId())
	if operationID == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if n.NodeStatus.Role != ControlPlaneRole {
		return nil, status.Error(codes.FailedPrecondition, "StartEtcdEncryptionAtRestObjectsRewrite can only be run on controlplane nodes")
	}

	resources := append([]string(nil), objectsToEncrypt...)

	result := n.tryBeginObjectRewrite(operationID)
	switch result.decision {
	case noop:
		return &pb.StartEtcdEncryptionAtRestObjectsRewriteResponse{Operation: rewriteOperationToProto(result.rewriteOp)}, nil
	case reject:
		return nil, rewriteStartConflictError(result.rewriteOp, result.rewriteActive)
	case start:
		// Start the requested logical operation. This also recovers a persisted
		// nonterminal same-operation status after agent restart, because
		// Kubernetes API object rewrites are idempotent.
	default:
		return nil, status.Errorf(codes.Internal, "unknown rewrite start decision %d", result.decision)
	}

	if err := n.persistRewriteOperationSnapshot(result.statusCopy, operationID, result.rewriteOp); err != nil {
		n.rollbackObjectRewriteStart(operationID, result.previousRewriteOp)
		return nil, status.Errorf(codes.Internal, "persist rewrite operation: %v", err)
	}
	go n.runAsyncRewriteEncryptedObjects(operationID, resources)

	return &pb.StartEtcdEncryptionAtRestObjectsRewriteResponse{Operation: rewriteOperationToProto(result.rewriteOp)}, nil
}

func (n *NodeAgentServer) GetEtcdEncryptionAtRestObjectsRewriteStatus(
	ctx context.Context,
	req *pb.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest,
) (*pb.GetEtcdEncryptionAtRestObjectsRewriteStatusResponse, error) {
	defer logRPCSpan(ctx, "GetEtcdEncryptionAtRestObjectsRewriteStatus")()

	operationID := strings.TrimSpace(req.GetOperationId())
	if operationID == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	rewriteOp := n.getRewriteOperationSnapshot()
	if rewriteOp.ID != operationID {
		return nil, status.Errorf(codes.NotFound, "rewrite operation %s not found", operationID)
	}
	return &pb.GetEtcdEncryptionAtRestObjectsRewriteStatusResponse{Operation: rewriteOperationToProto(rewriteOp)}, nil
}

func (n *NodeAgentServer) runAsyncRewriteEncryptedObjects(operationID string, resources []string) {
	err := n.rewriteEncryptedObjects(context.Background(), resources)

	statusCopy, rewriteOp := n.finishObjectRewrite(operationID, err)
	_ = n.persistRewriteOperationSnapshot(statusCopy, operationID, rewriteOp)
}

func (n *NodeAgentServer) rewriteEncryptedObjects(
	ctx context.Context,
	resources []string,
) error {
	hostname, err := os.Hostname()
	if err != nil {
		slog.Error("failed to get hostname", "err", err)
		n.markK8sAPIEncryptionErrorLocked()
		return err
	}
	n.setRewriteEncryptedNodeNameLocked(hostname)

	cpNodes, err := k8s.GetNodeNamesByLabel(ctx, "node-role.kubernetes.io/control-plane=")
	if err != nil {
		slog.Error("failed to get control-plane nodes", "err", err)
		n.markK8sAPIEncryptionErrorLocked()
		return err
	}

	slog.Info("rewriting api objects via client-go", "kinds", resources)
	if err := k8s.RewriteEncryptableAPIObjects(ctx, resources); err != nil {
		slog.Error("failed to rewrite api objects", "err", err)
		n.markK8sAPIEncryptionErrorLocked()
		return err
	}

	for _, node := range cpNodes {
		if node == "" {
			continue
		}
		slog.Info("labeling node", "node", node)
		if err := labelAPIEncryptionNode(ctx, node, APIEncryptionRewriteCompleteLabel); err != nil {
			slog.Error("failed to label node", "node", node, "err", err)
			n.markK8sAPIEncryptionErrorLocked()
			return err
		}
	}
	return nil
}

func (n *NodeAgentServer) finishObjectRewrite(operationID string, rewriteErr error) (NodeStatus, K8sAPIRewriteOperation) {
	n.mu.Lock()
	defer n.mu.Unlock()

	rewriteOp := n.NodeStatus.K8sAPIRewrite
	if rewriteErr != nil {
		if rewriteOp.ID == operationID {
			rewriteOp.State = K8sAPIRewriteStateFailed
			rewriteOp.ErrorMessage = rewriteErr.Error()
			n.NodeStatus.K8sAPIRewrite = rewriteOp
		}
	} else if rewriteOp.ID == operationID {
		rewriteOp.State = K8sAPIRewriteStateSucceeded
		rewriteOp.ErrorMessage = ""
		n.NodeStatus.K8sAPIRewrite = rewriteOp
	}
	n.rewriteActive = false
	return *n.NodeStatus, rewriteOp
}

type rewriteDecision int

const (
	start rewriteDecision = iota
	noop
	reject
)

type objectRewriteResult struct {
	decision          rewriteDecision
	rewriteOp         K8sAPIRewriteOperation
	previousRewriteOp K8sAPIRewriteOperation
	rewriteActive     bool
	statusCopy        NodeStatus
}

func (n *NodeAgentServer) tryBeginObjectRewrite(operationID string) objectRewriteResult {
	n.mu.Lock()
	defer n.mu.Unlock()

	rewriteOp := n.NodeStatus.K8sAPIRewrite
	rewriteActive := n.rewriteActive
	decision := decideStartObjectRewrite(rewriteOp, operationID, rewriteActive)
	result := objectRewriteResult{
		decision:          decision,
		rewriteOp:         rewriteOp,
		previousRewriteOp: rewriteOp,
		rewriteActive:     rewriteActive,
	}
	if decision != start {
		return result
	}

	rewriteOp = newRewriteOperation(operationID)
	n.NodeStatus.K8sAPIRewrite = rewriteOp
	n.rewriteActive = true
	result.rewriteOp = rewriteOp
	result.statusCopy = *n.NodeStatus
	return result
}

// decideStartObjectRewrite models StartEtcdEncryptionAtRestObjectsRewrite
// as a small state machine. The persisted rewrite status is the client-visible
// operation state; rewriteActive is only in-process liveness for the goroutine.
//
//	                           Start(req.operation_id)
//	                                     |
//	                                     v
//	+----------------+   start   +----------------+   success/fail   +----------+
//	| none/terminal  | --------> | active running | ---------------> | terminal |
//	+----------------+           +----------------+                  +----------+
//	        |                            |
//	        | same op                    | agent restart
//	        v                            v
//	return existing            +--------------------+
//	                           | persisted running  |
//	                           | rewriteActive=false|
//	                           +--------------------+
//	                                     |
//	                                     | Start(same op)
//	                                     v
//	                                  restart
//
//	Start(different op) while active or persisted-running is rejected.
func decideStartObjectRewrite(
	rewriteOp K8sAPIRewriteOperation,
	requestedID string,
	rewriteActive bool,
) rewriteDecision {
	if rewriteActive {
		if rewriteOp.ID == requestedID {
			return noop
		}
		return reject
	}

	if rewriteOp.ID == requestedID {
		if k8sAPIRewriteTerminal(rewriteOp.State) {
			return noop
		}
		return start
	}

	if rewriteOp.ID != "" && !k8sAPIRewriteTerminal(rewriteOp.State) {
		return reject
	}

	return start
}

func rewriteStartConflictError(rewriteOp K8sAPIRewriteOperation, rewriteActive bool) error {
	if rewriteActive {
		return status.Error(codes.FailedPrecondition, "a rewrite operation is already running")
	}
	return status.Errorf(codes.FailedPrecondition,
		"rewrite operation %s is already %s", rewriteOp.ID, rewriteOp.State)
}

func newRewriteOperation(operationID string) K8sAPIRewriteOperation {
	return K8sAPIRewriteOperation{
		ID:    operationID,
		State: K8sAPIRewriteStateRunning,
	}
}

func (n *NodeAgentServer) rollbackObjectRewriteStart(operationID string, previous K8sAPIRewriteOperation) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rewriteActive && n.NodeStatus.K8sAPIRewrite.ID == operationID {
		n.NodeStatus.K8sAPIRewrite = previous
		n.rewriteActive = false
	}
}

func (n *NodeAgentServer) getRewriteOperationSnapshot() K8sAPIRewriteOperation {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.NodeStatus.K8sAPIRewrite
}

func (n *NodeAgentServer) persistRewriteOperationSnapshot(
	statusCopy NodeStatus,
	operationID string,
	rewriteOp K8sAPIRewriteOperation,
) error {
	if n.Persister != nil {
		if err := n.Persister.Persist(&statusCopy); err != nil {
			slog.Error(
				"failed to persist K8sAPIRewrite operation",
				"err", err,
				"operationID", operationID,
				"nodeName", statusCopy.NodeName,
				"role", statusCopy.Role,
				"rewriteOperation", rewriteOp,
			)
			return fmt.Errorf("persist K8sAPIRewrite operation: %w", err)
		}
	}
	return nil
}

func rewriteOperationToProto(rewriteOp K8sAPIRewriteOperation) *pb.EtcdEncryptionAtRestObjectsRewriteOperation {
	return &pb.EtcdEncryptionAtRestObjectsRewriteOperation{
		OperationId:  rewriteOp.ID,
		State:        k8sAPIRewriteStateToProto(rewriteOp.State),
		ErrorMessage: rewriteOp.ErrorMessage,
	}
}

func k8sAPIRewriteStateToProto(state K8sAPIRewriteState) pb.EtcdEncryptionAtRestObjectsRewriteState {
	switch state {
	case K8sAPIRewriteStateRunning:
		return pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING
	case K8sAPIRewriteStateSucceeded:
		return pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED
	case K8sAPIRewriteStateFailed:
		return pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_FAILED
	default:
		return pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_UNSPECIFIED
	}
}

func k8sAPIRewriteTerminal(state K8sAPIRewriteState) bool {
	return state == K8sAPIRewriteStateSucceeded || state == K8sAPIRewriteStateFailed
}
