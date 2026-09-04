// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
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

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
)

const (
	operationTypeKubernetesCertificateRenewal = "kubernetes_certificate_renewal"

	certificateRenewalStateRenewing       = "renewing"
	certificateRenewalStateRestartPending = "restart_pending"

	certificateRenewalResultUnchanged           = "unchanged"
	certificateRenewalResultRenewed             = "renewed"
	certificateRenewalResultFailedBeforeRestart = "failed_before_restart"
	certificateRenewalResultFailedRestored      = "failed_restored"
	certificateRenewalResultFailedNotRestored   = "failed_not_restored"

	certificateRenewalStagingDir = "/etc/kubernetes/nke-cert-renewal-manifests"

	// Preserve the runbook's operational envelope: stopping static pods should
	// be quick, while startup gets longer for image/runtime and etcd recovery.
	defaultCertificateRenewalStopTimeout  = 2 * time.Minute
	defaultCertificateRenewalStartTimeout = 5 * time.Minute
	defaultCertificateRenewalPollInterval = 3 * time.Second
)

var certificateRenewalControlPlaneComponents = []string{
	kubeAPIServerContainerName,
	"kube-controller-manager",
	"kube-scheduler",
	"etcd",
}

var certificateRenewalRequiredManifests = []string{
	"kube-apiserver.yaml",
	"kube-controller-manager.yaml",
	"kube-scheduler.yaml",
	"etcd.yaml",
}

// StartKubernetesCertificateRenewal starts a durable, asynchronous certificate
// renewal and static-pod restart transaction.
func (s *NodeAgentServer) StartKubernetesCertificateRenewal(
	ctx context.Context,
	req *pb.StartKubernetesCertificateRenewalRequest,
) (*pb.StartKubernetesCertificateRenewalResponse, error) {
	defer logRPCSpan(ctx, "StartKubernetesCertificateRenewal")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if !s.isControlPlaneRole() {
		return nil, status.Error(codes.FailedPrecondition, "certificate renewal requires a control-plane node")
	}
	if req.GetThresholdDays() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "threshold_days must be positive")
	}

	operationID := strings.TrimSpace(req.GetOperationId())
	if err := s.recoverInterruptedCertificateRenewal(operationID); err != nil {
		return nil, err
	}
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeKubernetesCertificateRenewal,
	}, func(ctx context.Context, progress nodeAgentOperationProgress) error {
		return s.runKubernetesCertificateRenewal(ctx, progress, int(req.GetThresholdDays()))
	})
	if err != nil {
		return nil, err
	}
	return &pb.StartKubernetesCertificateRenewalResponse{
		Operation: kubernetesCertificateRenewalOperationToProto(op),
	}, nil
}

// GetKubernetesCertificateRenewalStatus returns the persisted status for a
// certificate renewal operation.
func (s *NodeAgentServer) GetKubernetesCertificateRenewalStatus(
	ctx context.Context,
	req *pb.GetKubernetesCertificateRenewalStatusRequest,
) (*pb.GetKubernetesCertificateRenewalStatusResponse, error) {
	defer logRPCSpan(ctx, "GetKubernetesCertificateRenewalStatus")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	op, err := s.getNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeKubernetesCertificateRenewal,
	})
	if err != nil {
		return nil, err
	}
	return &pb.GetKubernetesCertificateRenewalStatusResponse{
		Operation: kubernetesCertificateRenewalOperationToProto(op),
	}, nil
}

func (s *NodeAgentServer) runKubernetesCertificateRenewal(
	ctx context.Context,
	progress nodeAgentOperationProgress,
	thresholdDays int,
) (operationErr error) {
	// Certificate renewal and graceful shutdown both move static-pod manifests.
	// Serialize them so one operation cannot restore another operation's files.
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nkeutils.ExecRunner{}
	}

	state := s.kubernetesCertificateRenewalState()
	progress.SetStep("checking_control_plane_health")
	// Inspect the container runtime rather than the Kubernetes API because this
	// transaction deliberately stops the API server while manifests are staged.
	switch state {
	case certificateRenewalStateRenewing, certificateRenewalStateRestartPending:
		if err := s.waitForCertificateRenewalComponentState(ctx, runner, true, s.certificateRenewalStartTimeoutOrDefault()); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("wait for recovered control-plane components: %w", err)
		}
	case "":
		if err := allCertificateRenewalComponentsRunning(runner); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("pre-renewal control-plane health check: %w", err)
		}
	default:
		progress.SetResult(certificateRenewalResultFailedBeforeRestart)
		return fmt.Errorf("unknown persisted certificate renewal state %q", state)
	}

	if state != certificateRenewalStateRenewing && state != certificateRenewalStateRestartPending {
		progress.SetStep("checking_certificate_expiration")
		output, err := runner.Run("kubeadm", []string{"certs", "check-expiration"}, nil)
		if err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("kubeadm certs check-expiration: %w: %s", err, strings.TrimSpace(string(output)))
		}
		expiring, err := expiringKubernetesCertificates(string(output), thresholdDays)
		if err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("parse kubeadm certificate expiration: %w", err)
		}
		if len(expiring) == 0 {
			progress.SetResult(certificateRenewalResultUnchanged)
			return nil
		}
		if err := s.setKubernetesCertificateRenewalState(certificateRenewalStateRenewing); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("persist certificate renewal intent: %w", err)
		}
		state = certificateRenewalStateRenewing
		slog.Info("Kubernetes certificates require renewal", "certificates", expiring, "thresholdDays", thresholdDays)
	}

	if state == certificateRenewalStateRenewing {
		progress.SetStep("renewing_certificates")
		output, err := runner.Run("kubeadm", []string{"certs", "renew", "all"}, nil)
		if err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("kubeadm certs renew all: %w: %s", err, strings.TrimSpace(string(output)))
		}
		// Persist the restart boundary before moving manifests. If the agent exits
		// after staging begins, recovery must finish restarting the static pods so
		// they load the certificates kubeadm already replaced on disk.
		if err := s.setKubernetesCertificateRenewalState(certificateRenewalStateRestartPending); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("persist certificate restart requirement: %w", err)
		}
	}

	progress.SetStep("staging_static_pod_manifests")
	staged, restored, err := s.stageCertificateRenewalManifests()
	if err != nil {
		switch {
		case !staged:
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
		case restored:
			progress.SetResult(certificateRenewalResultFailedRestored)
		default:
			progress.SetResult(certificateRenewalResultFailedNotRestored)
		}
		return err
	}

	// Once any manifest is staged, every exit path must attempt restoration.
	needsRestore := true
	defer func() {
		if !needsRestore {
			return
		}
		restoreErr := s.restoreCertificateRenewalManifests()
		if restoreErr != nil {
			progress.SetResult(certificateRenewalResultFailedNotRestored)
			operationErr = errors.Join(operationErr, fmt.Errorf("restore static-pod manifests: %w", restoreErr))
			return
		}
		progress.SetResult(certificateRenewalResultFailedRestored)
	}()

	progress.SetStep("waiting_for_control_plane_stop")
	if err := s.waitForCertificateRenewalComponentState(ctx, runner, false, s.certificateRenewalStopTimeoutOrDefault()); err != nil {
		return fmt.Errorf("wait for control-plane components to stop: %w", err)
	}

	progress.SetStep("restoring_static_pod_manifests")
	if err := s.restoreCertificateRenewalManifests(); err != nil {
		return fmt.Errorf("restore static-pod manifests: %w", err)
	}
	needsRestore = false

	progress.SetStep("waiting_for_control_plane_start")
	if err := s.waitForCertificateRenewalComponentState(ctx, runner, true, s.certificateRenewalStartTimeoutOrDefault()); err != nil {
		progress.SetResult(certificateRenewalResultFailedRestored)
		return fmt.Errorf("wait for control-plane components to start: %w", err)
	}
	progress.SetStep("checking_final_control_plane_health")
	if err := allCertificateRenewalComponentsRunning(runner); err != nil {
		progress.SetResult(certificateRenewalResultFailedRestored)
		return fmt.Errorf("post-renewal control-plane health check: %w", err)
	}
	if err := s.setKubernetesCertificateRenewalState(""); err != nil {
		progress.SetResult(certificateRenewalResultFailedRestored)
		return fmt.Errorf("clear certificate restart requirement: %w", err)
	}
	progress.SetResult(certificateRenewalResultRenewed)
	return nil
}

// expiringKubernetesCertificates parses kubeadm's leaf-certificate table. The
// residual duration is field seven; the later CA table is intentionally out of
// scope, missing certificates are ignored, and hours or <invalid> mean renew.
func expiringKubernetesCertificates(output string, thresholdDays int) ([]string, error) {
	const kubeadmCertificateHeader = "CERTIFICATE"

	if thresholdDays <= 0 {
		return nil, fmt.Errorf("threshold must be positive")
	}
	var expiring []string
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "[check-expiration]") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == kubeadmCertificateHeader && fields[1] == "EXPIRES" {
			continue
		}
		if len(fields) >= 2 && fields[0] == kubeadmCertificateHeader && fields[1] == "AUTHORITY" {
			break
		}
		if len(fields) == 0 || fields[0] == "!MISSING!" {
			continue
		}
		if len(fields) < 7 {
			return nil, fmt.Errorf("certificate row has %d fields: %q", len(fields), line)
		}
		residual := fields[6]
		switch {
		case residual == "<invalid>":
			expiring = append(expiring, fields[0])
		case strings.HasSuffix(residual, "h"):
			expiring = append(expiring, fields[0])
		case strings.HasSuffix(residual, "d"):
			days, err := strconv.Atoi(strings.TrimSuffix(residual, "d"))
			if err != nil {
				return nil, fmt.Errorf("invalid residual time %q for %s", residual, fields[0])
			}
			if days < thresholdDays {
				expiring = append(expiring, fields[0])
			}
		}
	}
	return expiring, nil
}

// allCertificateRenewalComponentsRunning uses CRI state so it remains usable
// before, during, and after an API-server restart.
func allCertificateRenewalComponentsRunning(runner nkeutils.CommandRunner) error {
	var missing []string
	for _, component := range certificateRenewalControlPlaneComponents {
		ids, err := listStaticPodContainerIDs(runner, component)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			missing = append(missing, component)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("components not running: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *NodeAgentServer) waitForCertificateRenewalComponentState(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	wantRunning bool,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for {
		var mismatched []string
		for _, component := range certificateRenewalControlPlaneComponents {
			ids, err := listStaticPodContainerIDs(runner, component)
			if err != nil {
				return err
			}
			if (len(ids) > 0) != wantRunning {
				mismatched = append(mismatched, component)
			}
		}
		if len(mismatched) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("components did not become %s after %s: %s",
				map[bool]string{true: "running", false: "stopped"}[wantRunning],
				timeout,
				strings.Join(mismatched, ", "))
		}
		if err := sleepOrDone(ctx, s.certificateRenewalPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

// stageCertificateRenewalManifests verifies the complete kubeadm control-plane
// set before moving anything. The persistent staging directory shares the
// /etc/kubernetes filesystem, so renames are atomic and survive agent restarts.
func (s *NodeAgentServer) stageCertificateRenewalManifests() (bool, bool, error) {
	staged := false
	manifestDir := s.rootedPath(defaultStaticPodManifestDir)
	stagingDir := s.rootedPath(certificateRenewalStagingDir)
	if entries, err := os.ReadDir(stagingDir); err == nil && len(entries) > 0 {
		return false, false, fmt.Errorf("certificate renewal staging directory is not empty: %s", stagingDir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, fmt.Errorf("inspect certificate renewal staging directory: %w", err)
	}

	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return false, false, fmt.Errorf("read static-pod manifest directory: %w", err)
	}
	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		present[entry.Name()] = struct{}{}
		if entry.IsDir() {
			return false, false, fmt.Errorf("static-pod manifest directory contains subdirectory %s", entry.Name())
		}
	}
	for _, required := range certificateRenewalRequiredManifests {
		if _, ok := present[required]; !ok {
			return false, false, fmt.Errorf("required static-pod manifest is missing: %s", required)
		}
	}
	if err := os.MkdirAll(stagingDir, staticPodBackupDirMode); err != nil {
		return false, false, fmt.Errorf("create certificate renewal staging directory: %w", err)
	}
	if err := os.Chmod(stagingDir, staticPodBackupDirMode); err != nil {
		return false, false, fmt.Errorf("secure certificate renewal staging directory: %w", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := os.Rename(filepath.Join(manifestDir, entry.Name()), filepath.Join(stagingDir, entry.Name())); err != nil {
			restoreErr := s.restoreCertificateRenewalManifests()
			return staged, restoreErr == nil, errors.Join(
				fmt.Errorf("stage static-pod manifest %s: %w", entry.Name(), err),
				restoreErr,
			)
		}
		staged = true
	}
	if !staged {
		return false, false, errors.New("no static-pod manifests were staged")
	}
	return true, false, nil
}

func (s *NodeAgentServer) restoreCertificateRenewalManifests() error {
	manifestDir := s.rootedPath(defaultStaticPodManifestDir)
	stagingDir := s.rootedPath(certificateRenewalStagingDir)
	entries, err := os.ReadDir(stagingDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read certificate renewal staging directory: %w", err)
	}
	if err := os.MkdirAll(manifestDir, staticPodManifestDirMode); err != nil {
		return fmt.Errorf("create static-pod manifest directory: %w", err)
	}

	var restoreErr error
	for _, entry := range entries {
		if entry.IsDir() {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("staged manifest %s is a directory", entry.Name()))
			continue
		}
		src := filepath.Join(stagingDir, entry.Name())
		dst := filepath.Join(manifestDir, entry.Name())
		if _, err := os.Lstat(dst); err == nil {
			// A concurrent recovery may already have restored this manifest. Remove
			// only an identical staged copy; never overwrite independently changed
			// active state.
			same, compareErr := filesHaveSameContents(src, dst)
			if compareErr != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("compare staged manifest %s: %w", entry.Name(), compareErr))
				continue
			}
			if !same {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("active manifest %s differs from staged copy", entry.Name()))
				continue
			}
			if err := os.Remove(src); err != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("remove duplicate staged manifest %s: %w", entry.Name(), err))
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("inspect active manifest %s: %w", entry.Name(), err))
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore static-pod manifest %s: %w", entry.Name(), err))
		}
	}
	if restoreErr != nil {
		return restoreErr
	}
	if err := os.Remove(stagingDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty certificate renewal staging directory: %w", err)
	}
	return nil
}

func filesHaveSameContents(first, second string) (bool, error) {
	firstHash, err := fileSHA256(first)
	if err != nil {
		return false, err
	}
	secondHash, err := fileSHA256(second)
	if err != nil {
		return false, err
	}
	return firstHash == secondHash, nil
}

func (s *NodeAgentServer) recoverInterruptedCertificateRenewal(operationID string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	op, found := s.NodeStatus.NodeAgentOperations[operationID]
	_, active := s.activeOperations[operationID]
	anyCertificateRenewalActive := false
	for id, candidate := range s.NodeStatus.NodeAgentOperations {
		if candidate.Type != operationTypeKubernetesCertificateRenewal {
			continue
		}
		if _, candidateActive := s.activeOperations[id]; candidateActive {
			anyCertificateRenewalActive = true
			break
		}
	}
	stale := found && op.Type == operationTypeKubernetesCertificateRenewal &&
		!nodeAgentOperationTerminal(op.Phase) && !active
	s.mu.Unlock()
	if anyCertificateRenewalActive {
		return nil
	}

	s.staticPodMutationMu.Lock()
	restoreErr := s.restoreCertificateRenewalManifests()
	s.staticPodMutationMu.Unlock()
	if restoreErr != nil {
		if stale {
			s.recordFailedCertificateRenewalRecovery(operationID, restoreErr)
		}
		return status.Errorf(codes.FailedPrecondition, "recover interrupted certificate renewal: %v", restoreErr)
	}
	if !stale {
		return nil
	}

	s.mu.Lock()
	delete(s.NodeStatus.NodeAgentOperations, operationID)
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()
	if s.Persister != nil {
		if err := s.Persister.Persist(&statusCopy); err != nil {
			return status.Errorf(codes.Internal, "persist recovered certificate renewal operation: %v", err)
		}
	}
	return nil
}

func (s *NodeAgentServer) recordFailedCertificateRenewalRecovery(operationID string, recoveryErr error) {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	op, ok := s.NodeStatus.NodeAgentOperations[operationID]
	if ok {
		op.Phase = NodeAgentOperationPhaseFailed
		op.Step = "recovering_static_pod_manifests"
		op.Result = certificateRenewalResultFailedNotRestored
		op.ErrorMessage = recoveryErr.Error()
		s.NodeStatus.NodeAgentOperations[operationID] = op
	}
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()
	if ok && s.Persister != nil {
		if err := s.Persister.Persist(&statusCopy); err != nil {
			slog.Error("failed to persist certificate renewal recovery failure", "operationID", operationID, "err", err)
		}
	}
}

func (s *NodeAgentServer) kubernetesCertificateRenewalState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NodeStatus == nil {
		return ""
	}
	return s.NodeStatus.K8sCertRenewalState
}

func (s *NodeAgentServer) setKubernetesCertificateRenewalState(state string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	s.NodeStatus.K8sCertRenewalState = state
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()
	if s.Persister == nil {
		return nil
	}
	if err := s.Persister.Persist(&statusCopy); err != nil {
		return fmt.Errorf("persist certificate renewal state %q: %w", state, err)
	}
	return nil
}

func kubernetesCertificateRenewalOperationToProto(op NodeAgentOperation) *pb.KubernetesCertificateRenewalOperation {
	return &pb.KubernetesCertificateRenewalOperation{
		Operation: nodeAgentOperationToProto(op),
		Outcome:   kubernetesCertificateRenewalOutcomeToProto(op.Result),
	}
}

func kubernetesCertificateRenewalOutcomeToProto(result string) pb.KubernetesCertificateRenewalOperation_Outcome {
	switch result {
	case certificateRenewalResultUnchanged:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED
	case certificateRenewalResultRenewed:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED
	case certificateRenewalResultFailedBeforeRestart:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART
	case certificateRenewalResultFailedRestored:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED
	case certificateRenewalResultFailedNotRestored:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED
	default:
		return pb.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED
	}
}

func (s *NodeAgentServer) certificateRenewalStopTimeoutOrDefault() time.Duration {
	if s.certificateRenewalStopTimeout > 0 {
		return s.certificateRenewalStopTimeout
	}
	return defaultCertificateRenewalStopTimeout
}

func (s *NodeAgentServer) certificateRenewalStartTimeoutOrDefault() time.Duration {
	if s.certificateRenewalStartTimeout > 0 {
		return s.certificateRenewalStartTimeout
	}
	return defaultCertificateRenewalStartTimeout
}

func (s *NodeAgentServer) certificateRenewalPollIntervalOrDefault() time.Duration {
	if s.certificateRenewalPollInterval > 0 {
		return s.certificateRenewalPollInterval
	}
	return defaultCertificateRenewalPollInterval
}
