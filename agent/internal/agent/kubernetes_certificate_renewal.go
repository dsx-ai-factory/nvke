// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
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

	certificateRenewalStagingDir = "/etc/kubernetes/nvke-cert-renewal-manifests"
	etcdContainerName            = "etcd"
	controllerManagerContainer   = "kube-controller-manager"
	schedulerContainerName       = "kube-scheduler"

	// Preserve the runbook's operational envelope: stopping static pods should
	// be quick, while startup gets longer for image/runtime and etcd recovery.
	defaultCertificateRenewalStopTimeout  = 2 * time.Minute
	defaultCertificateRenewalStartTimeout = 5 * time.Minute
	defaultCertificateRenewalPollInterval = 3 * time.Second
)

// certificateRenewalControlPlaneComponents is the restart order and matches the
// COMPONENTS array of the legacy k8s-cert-renewal.sh runbook. The pods restart
// one at a time; each must be replaced and the control plane healthy before the
// next manifest is moved.
var certificateRenewalControlPlaneComponents = []string{
	etcdContainerName,
	kubeAPIServerContainerName,
	controllerManagerContainer,
	schedulerContainerName,
}

var certificateRenewalRequiredManifests = []string{
	"kube-apiserver.yaml",
	"kube-controller-manager.yaml",
	"kube-scheduler.yaml",
	"etcd.yaml",
}

// StartKubernetesCertificateRenewal retains the V1 contract for older controllers:
// threshold-based leaf renewal with CRI running-state checks, not V2 membership
// and readiness checks. Callers must reuse the operation ID on retry and poll
// GetKubernetesCertificateRenewalStatus; RPC cancellation does not stop accepted work.
func (s *NodeAgentServer) StartKubernetesCertificateRenewal(
	ctx context.Context,
	req *pb.StartKubernetesCertificateRenewalRequest,
) (*pb.StartKubernetesCertificateRenewalResponse, error) {
	defer logRPCSpan(ctx, "StartKubernetesCertificateRenewal")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	operation, err := s.startKubernetesCertificateRenewal(
		ctx,
		req.GetOperationId(),
		int(req.GetThresholdDays()),
		false,
		0,
	)
	if err != nil {
		return nil, err
	}
	return &pb.StartKubernetesCertificateRenewalResponse{Operation: operation}, nil
}

// StartKubernetesCertificateRenewalV2 accepts asynchronous leaf renewal after
// which static pods must reload the certificates. Force bypasses expiration
// selection, never health checks. ExpectedEtcdMembers must be the caller's
// control-plane snapshot size; the caller is responsible for cluster ownership,
// quorum safety and serializing renewal across nodes.
//
// Keep the operation ID and inputs stable across retries, including lost start
// responses. Active or terminal records are reused; an interrupted operation may
// resume from its persisted phase. Accepted work outlives RPC cancellation.
func (s *NodeAgentServer) StartKubernetesCertificateRenewalV2(
	ctx context.Context,
	req *pb.StartKubernetesCertificateRenewalV2Request,
) (*pb.StartKubernetesCertificateRenewalV2Response, error) {
	defer logRPCSpan(ctx, "StartKubernetesCertificateRenewalV2")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if req.GetExpectedEtcdMembers() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "expected_etcd_members must be positive")
	}
	operation, err := s.startKubernetesCertificateRenewal(
		ctx,
		req.GetOperationId(),
		int(req.GetThresholdDays()),
		req.GetForce(),
		int(req.GetExpectedEtcdMembers()),
	)
	if err != nil {
		return nil, err
	}
	return &pb.StartKubernetesCertificateRenewalV2Response{Operation: operation}, nil
}

// startKubernetesCertificateRenewal attempts stranded-manifest recovery when no
// renewal worker is active, then uses shared operation tracking. Only this start
// path can resume interrupted renewal; status reads never trigger recovery.
func (s *NodeAgentServer) startKubernetesCertificateRenewal(
	ctx context.Context,
	operationID string,
	thresholdDays int,
	force bool,
	expectedEtcdMembers int,
) (*pb.KubernetesCertificateRenewalOperation, error) {
	if !s.isControlPlaneRole() {
		return nil, status.Error(codes.FailedPrecondition, "certificate renewal requires a control-plane node")
	}
	if thresholdDays <= 0 {
		return nil, status.Error(codes.InvalidArgument, "threshold_days must be positive")
	}

	operationID = strings.TrimSpace(operationID)
	if err := s.recoverInterruptedCertificateRenewal(operationID); err != nil {
		return nil, err
	}
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeKubernetesCertificateRenewal,
	}, func(ctx context.Context, progress nodeAgentOperationProgress) error {
		return s.runKubernetesCertificateRenewal(
			ctx,
			progress,
			thresholdDays,
			force,
			expectedEtcdMembers,
		)
	})
	if err != nil {
		return nil, err
	}
	return kubernetesCertificateRenewalOperationToProto(op), nil
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

// runKubernetesCertificateRenewal separates renewal intent from restart intent so
// recovery still reloads certificates that are no longer near expiry. A persisted
// renewing phase may repeat kubeadm renewal; restart_pending skips it. Once files
// are staged, failures attempt restoration, but restored files alone do not prove
// control-plane health or permit clearing the restart requirement.
func (s *NodeAgentServer) runKubernetesCertificateRenewal(
	ctx context.Context,
	progress nodeAgentOperationProgress,
	thresholdDays int,
	force bool,
	expectedEtcdMembers int,
) error {
	// Certificate renewal and graceful shutdown both move static-pod manifests.
	// Serialize them so one operation cannot restore another operation's files.
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}

	state := s.kubernetesCertificateRenewalState()
	progress.SetStep("checking_control_plane_health")
	switch state {
	case certificateRenewalStateRenewing, certificateRenewalStateRestartPending:
		if err := s.waitForCertificateRenewalControlPlaneHealth(ctx, runner, expectedEtcdMembers); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("wait for recovered control-plane health: %w", err)
		}
	case "":
		if err := s.checkCertificateRenewalControlPlaneHealth(runner, expectedEtcdMembers); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("pre-renewal control-plane health check: %w", err)
		}
	default:
		progress.SetResult(certificateRenewalResultFailedBeforeRestart)
		return fmt.Errorf("unknown persisted certificate renewal state %q", state)
	}

	if state != certificateRenewalStateRenewing && state != certificateRenewalStateRestartPending {
		var expiring []string
		if !force {
			progress.SetStep("checking_certificate_expiration")
			output, err := runner.Run("kubeadm", []string{"certs", "check-expiration"}, nil)
			if err != nil {
				progress.SetResult(certificateRenewalResultFailedBeforeRestart)
				return fmt.Errorf("kubeadm certs check-expiration: %w: %s", err, strings.TrimSpace(string(output)))
			}
			expiring, err = expiringKubernetesCertificates(string(output), thresholdDays)
			if err != nil {
				progress.SetResult(certificateRenewalResultFailedBeforeRestart)
				return fmt.Errorf("parse kubeadm certificate expiration: %w", err)
			}
			if len(expiring) == 0 {
				progress.SetResult(certificateRenewalResultUnchanged)
				return nil
			}
		}
		if err := s.setKubernetesCertificateRenewalState(certificateRenewalStateRenewing); err != nil {
			progress.SetResult(certificateRenewalResultFailedBeforeRestart)
			return fmt.Errorf("persist certificate renewal intent: %w", err)
		}
		state = certificateRenewalStateRenewing
		slog.Info(
			"Kubernetes certificates require renewal",
			"certificates", expiring,
			"thresholdDays", thresholdDays,
			"forced", force,
		)
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

	progress.SetStep("verifying_static_pod_manifests")
	if err := s.verifyCertificateRenewalManifests(); err != nil {
		progress.SetResult(certificateRenewalResultFailedBeforeRestart)
		return err
	}

	// Restarting an already restarted pod is harmless, so an interrupted
	// operation resumes by repeating the whole sequence from the first component.
	for index, component := range certificateRenewalControlPlaneComponents {
		if err := s.restartCertificateRenewalComponent(ctx, runner, progress, component, index > 0, expectedEtcdMembers); err != nil {
			return err
		}
	}
	if err := s.setKubernetesCertificateRenewalState(""); err != nil {
		progress.SetResult(certificateRenewalResultFailedRestored)
		return fmt.Errorf("clear certificate restart requirement: %w", err)
	}
	progress.SetResult(certificateRenewalResultRenewed)
	return nil
}

// restartCertificateRenewalComponent moves one static-pod manifest away, waits
// for its container to disappear, restores it, and waits for a replacement
// container and full control-plane health before returning. Only this
// component's manifest is ever staged, so other pods and unrelated manifests such
// as kube-vip stay running. Once the manifest is staged every exit path attempts
// restoration. A failure before staging reports failed_before_restart only when
// no earlier component was restarted; afterwards all manifests are already restored.
func (s *NodeAgentServer) restartCertificateRenewalComponent(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	component string,
	earlierComponentsRestarted bool,
	expectedEtcdMembers int,
) (operationErr error) {
	failedUnstagedResult := certificateRenewalResultFailedBeforeRestart
	if earlierComponentsRestarted {
		failedUnstagedResult = certificateRenewalResultFailedRestored
	}

	oldContainerIDs, err := localStaticPodContainerIDs(runner, component)
	if err == nil && len(oldContainerIDs) == 0 {
		err = fmt.Errorf("no running local %s container", component)
	}
	if err != nil {
		progress.SetResult(failedUnstagedResult)
		return fmt.Errorf("identify %s container: %w", component, err)
	}

	progress.SetStep("staging_" + component + "_manifest")
	if err := s.stageCertificateRenewalManifest(component); err != nil {
		progress.SetResult(failedUnstagedResult)
		return err
	}

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

	progress.SetStep("waiting_for_" + component + "_stop")
	if err := s.waitForCertificateRenewalComponentStop(ctx, runner, component, oldContainerIDs); err != nil {
		return fmt.Errorf("wait for %s to stop: %w", component, err)
	}

	progress.SetStep("restoring_" + component + "_manifest")
	if err := s.restoreCertificateRenewalManifests(); err != nil {
		return fmt.Errorf("restore static-pod manifests: %w", err)
	}
	needsRestore = false

	progress.SetStep("waiting_for_" + component + "_start")
	if err := s.waitForCertificateRenewalComponentReplacement(ctx, runner, component, oldContainerIDs, expectedEtcdMembers); err != nil {
		progress.SetResult(certificateRenewalResultFailedRestored)
		return fmt.Errorf("post-restart check of %s: %w", component, err)
	}
	return nil
}

// parseKubeadmResidualTime parses the duration kubeadm prints in the residual
// time column. kubeadm uses apimachinery's HumanDuration, which emits one or two
// adjacent units such as 45s, 2m30s, 5h10m, 3d4h, 400d, or 2y30d.
func parseKubeadmResidualTime(value string) (time.Duration, error) {
	units := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'y': 365 * 24 * time.Hour}
	var total time.Duration
	for rest := value; rest != ""; {
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits == len(rest) {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		unit, ok := units[rest[digits]]
		if !ok {
			return 0, fmt.Errorf("unknown unit %q in duration %q", rest[digits], value)
		}
		amount, err := strconv.ParseInt(rest[:digits], 10, 64)
		if err != nil || amount > int64((math.MaxInt64-total)/unit) {
			return 0, fmt.Errorf("duration %q is out of range", value)
		}
		total += time.Duration(amount) * unit
		rest = rest[digits+1:]
	}
	return total, nil
}

// expiringKubernetesCertificates parses kubeadm's leaf-certificate table. The
// residual duration is field seven; the later CA table is intentionally out of
// scope. Missing certificates and unrecognized rows fail rather than suggest a
// healthy no-op; <invalid> means renew, and any other residual time must be
// shorter than the threshold.
func expiringKubernetesCertificates(output string, thresholdDays int) ([]string, error) {
	const kubeadmCertificateHeader = "CERTIFICATE"

	if thresholdDays <= 0 {
		return nil, fmt.Errorf("threshold must be positive")
	}
	threshold := time.Duration(thresholdDays) * 24 * time.Hour
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
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "!MISSING!" {
			return nil, fmt.Errorf("kubeadm reported a missing certificate: %q", line)
		}
		if len(fields) < 7 {
			return nil, fmt.Errorf("certificate row has %d fields: %q", len(fields), line)
		}
		residual := fields[6]
		if residual == "<invalid>" {
			expiring = append(expiring, fields[0])
			continue
		}
		remaining, err := parseKubeadmResidualTime(residual)
		if err != nil {
			return nil, fmt.Errorf("invalid residual time %q for %s: %w", residual, fields[0], err)
		}
		if remaining < threshold {
			expiring = append(expiring, fields[0])
		}
	}
	return expiring, nil
}

// allCertificateRenewalComponentsRunning uses CRI state so it remains usable
// before, during, and after an API-server restart.
func allCertificateRenewalComponentsRunning(runner nvkeutils.CommandRunner) error {
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

// checkCertificateRenewalControlPlaneHealth preserves CRI-only checks for V1's
// zero member count. V2 also requires exactly the expected number of voters, no
// learners, cluster-wide etcd endpoint health and local API readiness. It does not
// calculate whether that membership can tolerate losing a voter; the controller
// must establish that before dispatch.
func (s *NodeAgentServer) checkCertificateRenewalControlPlaneHealth(
	runner nvkeutils.CommandRunner,
	expectedEtcdMembers int,
) error {
	if err := allCertificateRenewalComponentsRunning(runner); err != nil {
		return err
	}
	// A zero count comes from clients predating strict membership validation.
	// Their controller-side workflow did not snapshot the control-plane set.
	if expectedEtcdMembers == 0 {
		return nil
	}

	etcdContainerIDs, err := localStaticPodContainerIDs(runner, etcdContainerName)
	if err != nil {
		return err
	}
	if len(etcdContainerIDs) != 1 {
		return fmt.Errorf("expected exactly one running local etcd container, found %d", len(etcdContainerIDs))
	}
	etcdArgs := []string{
		"exec", etcdContainerIDs[0], "etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/server.crt",
		"--key=/etc/kubernetes/pki/etcd/server.key",
		"--dial-timeout=10s",
		"--command-timeout=30s",
	}
	memberOutput, err := runner.RunSplitOutput("crictl", append(etcdArgs, "member", "list", "--write-out=json"), nil)
	if stderr := strings.TrimSpace(string(memberOutput.Stderr)); stderr != "" {
		slog.Warn("crictl etcd member list wrote to stderr", "container", etcdContainerIDs[0], "stderr", stderr)
	}
	if err != nil {
		return fmt.Errorf("etcd member list: %w: stdout=%s stderr=%s",
			err, strings.TrimSpace(string(memberOutput.Stdout)), strings.TrimSpace(string(memberOutput.Stderr)))
	}
	var membership struct {
		Members []struct {
			IsLearner bool `json:"isLearner"`
		} `json:"members"`
	}
	if err := json.Unmarshal(memberOutput.Stdout, &membership); err != nil {
		return fmt.Errorf("parse etcd member list: %w", err)
	}
	voters := 0
	for _, member := range membership.Members {
		if !member.IsLearner {
			voters++
		}
	}
	if len(membership.Members) != expectedEtcdMembers || voters != expectedEtcdMembers {
		return fmt.Errorf(
			"etcd membership does not match control-plane snapshot: expected=%d members=%d voters=%d",
			expectedEtcdMembers,
			len(membership.Members),
			voters,
		)
	}
	endpointOutput, err := runner.Run("crictl", append(etcdArgs, "endpoint", "health", "--cluster"), nil)
	if err != nil {
		return fmt.Errorf("etcd endpoint health: %w: %s", err, strings.TrimSpace(string(endpointOutput)))
	}

	readyOutput, err := runner.Run("kubectl", []string{
		"--kubeconfig", s.rootedPath("/etc/kubernetes/admin.conf"),
		"--server", "https://localhost:6443",
		"--insecure-skip-tls-verify=true",
		"--request-timeout=30s",
		kubectlGetCommand, "--raw", "/readyz?verbose",
	}, nil)
	if err != nil {
		return fmt.Errorf("kube-apiserver readyz: %w: %s", err, strings.TrimSpace(string(readyOutput)))
	}
	return nil
}

// waitForCertificateRenewalControlPlaneHealth retries startup and readiness under
// one polling deadline: a running container may still be recovering etcd or the
// API server. The context stops polling, not an in-flight CommandRunner call.
func (s *NodeAgentServer) waitForCertificateRenewalControlPlaneHealth(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	expectedEtcdMembers int,
) error {
	ctx, cancel := context.WithTimeout(ctx, s.certificateRenewalStartTimeoutOrDefault())
	defer cancel()
	var healthErr error
	for ctx.Err() == nil {
		healthErr = s.checkCertificateRenewalControlPlaneHealth(runner, expectedEtcdMembers)
		if healthErr == nil {
			return nil
		}
		if err := sleepOrDone(ctx, s.certificateRenewalPollIntervalOrDefault()); err != nil {
			break
		}
	}
	return fmt.Errorf("control plane did not become healthy: %w", errors.Join(ctx.Err(), healthErr))
}

// localStaticPodContainerIDs returns running local containers of the exact
// kubeadm static pod: CRI lists only local containers, so matching the kube-system
// namespace and kubeadm's pod-name prefix excludes workloads with the same
// container name without assuming the OS hostname equals the Kubernetes node name.
func localStaticPodContainerIDs(runner nvkeutils.CommandRunner, component string) ([]string, error) {
	containers, err := listStaticPodContainers(runner, component)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, container := range containers {
		if container.Labels["io.kubernetes.container.name"] != component ||
			container.Labels["io.kubernetes.pod.namespace"] != kubeSystemNamespace ||
			!strings.HasPrefix(container.Labels["io.kubernetes.pod.name"], component+"-") {
			continue
		}
		ids = append(ids, container.ID)
	}
	return ids, nil
}

// waitForCertificateRenewalComponentStop waits until none of the pre-restart
// containers of the component are still running.
func (s *NodeAgentServer) waitForCertificateRenewalComponentStop(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	component string,
	oldContainerIDs []string,
) error {
	timeout := s.certificateRenewalStopTimeoutOrDefault()
	deadline := time.Now().Add(timeout)
	for {
		current, err := localStaticPodContainerIDs(runner, component)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(current, func(id string) bool { return slices.Contains(oldContainerIDs, id) }) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not stop after %s", component, timeout)
		}
		if err := sleepOrDone(ctx, s.certificateRenewalPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

// waitForCertificateRenewalComponentReplacement requires a container that is not
// one of the pre-restart containers and, under the same polling deadline, a
// fully healthy control plane: a new container may still be recovering, and the
// next component must not be stopped until it has.
func (s *NodeAgentServer) waitForCertificateRenewalComponentReplacement(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	component string,
	oldContainerIDs []string,
	expectedEtcdMembers int,
) error {
	ctx, cancel := context.WithTimeout(ctx, s.certificateRenewalStartTimeoutOrDefault())
	defer cancel()
	var waitErr error
	for ctx.Err() == nil {
		current, err := localStaticPodContainerIDs(runner, component)
		switch {
		case err != nil:
			waitErr = err
		case !slices.ContainsFunc(current, func(id string) bool { return !slices.Contains(oldContainerIDs, id) }):
			waitErr = fmt.Errorf("%s has no replacement container", component)
		default:
			waitErr = s.checkCertificateRenewalControlPlaneHealth(runner, expectedEtcdMembers)
		}
		if waitErr == nil {
			return nil
		}
		if err := sleepOrDone(ctx, s.certificateRenewalPollIntervalOrDefault()); err != nil {
			break
		}
	}
	return fmt.Errorf("control plane did not become healthy: %w", errors.Join(ctx.Err(), waitErr))
}

// verifyCertificateRenewalManifests requires the complete kubeadm control-plane
// manifest set and an empty staging directory before any pod is restarted. The
// persistent staging directory shares the /etc/kubernetes filesystem, so renames
// are atomic and survive agent restarts.
func (s *NodeAgentServer) verifyCertificateRenewalManifests() error {
	manifestDir := s.rootedPath(defaultStaticPodManifestDir)
	stagingDir := s.rootedPath(certificateRenewalStagingDir)
	if entries, err := os.ReadDir(stagingDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("certificate renewal staging directory is not empty: %s", stagingDir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect certificate renewal staging directory: %w", err)
	}
	for _, required := range certificateRenewalRequiredManifests {
		if _, err := os.Stat(filepath.Join(manifestDir, required)); err != nil {
			return fmt.Errorf("required static-pod manifest is unavailable: %w", err)
		}
	}
	return nil
}

// stageCertificateRenewalManifest moves a single component's manifest into the
// staging directory. The rename is atomic, so an error means nothing was staged.
func (s *NodeAgentServer) stageCertificateRenewalManifest(component string) error {
	manifestDir := s.rootedPath(defaultStaticPodManifestDir)
	stagingDir := s.rootedPath(certificateRenewalStagingDir)
	if err := os.MkdirAll(stagingDir, staticPodBackupDirMode); err != nil {
		return fmt.Errorf("create certificate renewal staging directory: %w", err)
	}
	if err := os.Chmod(stagingDir, staticPodBackupDirMode); err != nil {
		return fmt.Errorf("secure certificate renewal staging directory: %w", err)
	}
	name := component + ".yaml"
	if err := os.Rename(filepath.Join(manifestDir, name), filepath.Join(stagingDir, name)); err != nil {
		return fmt.Errorf("stage static-pod manifest %s: %w", name, err)
	}
	return nil
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
