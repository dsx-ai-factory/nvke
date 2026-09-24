// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "gopkg.in/yaml.v2"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
	nkeutils "github.com/dsx-ai-factory/nke/agent/pkg/utils"
)

const (
	defaultStaticPodManifestDir = "/etc/kubernetes/manifests"
	defaultStaticPodBackupDir   = "/etc/kubernetes/manifests-backup"
	defaultKubeVipManifestName  = "kube-vip.yaml"
	// loopbackInterface is the device kube-vip binds the VIP to in ARP mode.
	loopbackInterface                = "lo"
	defaultKubeAPIServerManifestName = "kube-apiserver.yaml"
	defaultStaticPodExitTimeout      = 60
	defaultStaticPodStartTimeout     = 60
	defaultKubeVipApiserverPort      = 6443
	defaultKubeVipSocketQuietSeconds = 30
	defaultDrainKubeVipPollInterval  = time.Second
	// Match kubeadm's static pod manifest directory convention; keep the agent-owned backup directory private.
	staticPodManifestDirMode = 0o755
	staticPodBackupDirMode   = 0o700

	operationTypeK8sControlPlaneNodeGracefulShutdown = "k8s_control_plane_node_graceful_shutdown"
	operationTypeKubeVipRestore                      = "kube_vip_restore"

	stepStoppingKubeAPIServer       = "stopping_kube_apiserver"
	stepWaitingKubeAPIServerExit    = "waiting_for_kube_apiserver_exit"
	stepStoppingKubeVip             = "stopping_kube_vip"
	stepWaitingKubeVipExit          = "waiting_for_kube_vip_exit"
	stepRestoringKubeVip            = "restoring_kube_vip"
	stepWaitingKubeVipStart         = "waiting_for_kube_vip_start"
	stepWaitingVipSocketQuietWindow = "waiting_for_vip_socket_quiet_window"
	stepWaitingLoopbackVip          = "waiting_for_loopback_vip"
	stepResolvingKubeVip            = "resolving_kube_vip"
	stepClosingVipSockets           = "closing_vip_sockets"
	stepRemovingLoopbackVip         = "removing_loopback_vip"
)

type vipSocketPeer struct {
	IP   string
	Port string
}

func (s *NodeAgentServer) StartK8SControlPlaneNodeGracefulShutdown(
	ctx context.Context,
	req *pb.StartK8SControlPlaneNodeGracefulShutdownRequest,
) (*pb.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	defer logRPCSpan(ctx, "StartK8SControlPlaneNodeGracefulShutdown")()
	if req == nil {
		err := status.Error(codes.InvalidArgument, "request is nil")
		slog.Error("failed to start k8s control-plane node graceful shutdown", "err", err)
		return nil, err
	}

	operationID := strings.TrimSpace(req.GetOperationId())
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeK8sControlPlaneNodeGracefulShutdown,
	}, func(ctx context.Context, progress nodeAgentOperationProgress) error {
		return s.runK8sControlPlaneNodeGracefulShutdown(ctx, progress, req.GetStopKubeApiserver())
	})
	if err != nil {
		slog.Error("failed to start k8s control-plane node graceful shutdown",
			"operationID", operationID,
			"err", err)
		return nil, err
	}
	return &pb.StartK8SControlPlaneNodeGracefulShutdownResponse{
		Operation: nodeAgentOperationToProto(op),
	}, nil
}

func (s *NodeAgentServer) GetK8SControlPlaneNodeGracefulShutdownStatus(
	ctx context.Context,
	req *pb.GetK8SControlPlaneNodeGracefulShutdownStatusRequest,
) (*pb.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	defer logRPCSpan(ctx, "GetK8SControlPlaneNodeGracefulShutdownStatus")()
	if req == nil {
		err := status.Error(codes.InvalidArgument, "request is nil")
		slog.Error("failed to get k8s control-plane node graceful shutdown status", "err", err)
		return nil, err
	}
	operationID := strings.TrimSpace(req.GetOperationId())
	op, err := s.getNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeK8sControlPlaneNodeGracefulShutdown,
	})
	if err != nil {
		slog.Error("failed to get k8s control-plane node graceful shutdown status",
			"operationID", operationID,
			"err", err)
		return nil, err
	}
	return &pb.GetK8SControlPlaneNodeGracefulShutdownStatusResponse{
		Operation: nodeAgentOperationToProto(op),
	}, nil
}

func (s *NodeAgentServer) StartKubeVipRestore(
	ctx context.Context,
	req *pb.StartKubeVipRestoreRequest,
) (*pb.StartKubeVipRestoreResponse, error) {
	defer logRPCSpan(ctx, "StartKubeVipRestore")()
	if req == nil {
		err := status.Error(codes.InvalidArgument, "request is nil")
		slog.Error("failed to start kube-vip restore", "err", err)
		return nil, err
	}

	operationID := strings.TrimSpace(req.GetOperationId())
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeKubeVipRestore,
	}, func(ctx context.Context, progress nodeAgentOperationProgress) error {
		return s.runKubeVipRestore(ctx, progress)
	})
	if err != nil {
		slog.Error("failed to start kube-vip restore",
			"operationID", operationID,
			"err", err)
		return nil, err
	}
	return &pb.StartKubeVipRestoreResponse{
		Operation: nodeAgentOperationToProto(op),
	}, nil
}

// runK8sControlPlaneNodeGracefulShutdown starts the local control-plane
// shutdown sequence while preserving cluster availability. When kube-vip is
// configured it:
//  1. removes the kube-vip static pod manifest and waits for kube-vip to exit,
//  2. closes existing remote VIP:6443 sockets,
//  3. removes the VIP from loopback,
//  4. verifies a quiet window with no new remote VIP sockets.
//
// When kube-vip is not configured, the VIP drain steps are skipped. In both
// cases it optionally removes the kube-apiserver static pod manifest and waits
// for it to exit.
func (s *NodeAgentServer) runK8sControlPlaneNodeGracefulShutdown(
	ctx context.Context,
	progress nodeAgentOperationProgress,
	shouldStopKubeAPIServer bool,
) error {
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nkeutils.ExecRunner{}
	}
	progress.SetStep(stepResolvingKubeVip)
	vip, err := s.resolveKubeVipAddress(runner)
	switch {
	case err == nil:
		if drainErr := s.drainKubeVip(ctx, runner, progress, vip); drainErr != nil {
			return drainErr
		}
	case errors.Is(err, errNoKubeVipAddress):
		slog.Info("kube-vip is not configured; skipping kube-vip drain")
	default:
		return fmt.Errorf("resolve kube-vip address: %w", err)
	}

	if shouldStopKubeAPIServer {
		return s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
			containerName: kubeAPIServerContainerName,
			manifestName:  defaultKubeAPIServerManifestName,
			stoppingStep:  stepStoppingKubeAPIServer,
			waitingStep:   stepWaitingKubeAPIServerExit,
			timeout:       time.Duration(defaultStaticPodExitTimeout) * time.Second,
		})
	}
	return nil
}

// drainKubeVip removes kube-vip from this node so it stops advertising and
// serving the control-plane VIP, while keeping the local apiserver available.
//
// Close stale sockets while the VIP is still assigned to lo. In testing,
// after removing the VIP from lo, ss -K could still destroy the local socket,
// but the expected RST was not sent to the remote peer for that established
// VIP:6443 connection. Closing first gives the kernel the best chance to emit
// an RST before the local VIP address is removed.
func (s *NodeAgentServer) drainKubeVip(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	vip string,
) error {
	if err := s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
		containerName: kubeVipContainerName,
		manifestName:  defaultKubeVipManifestName,
		stoppingStep:  stepStoppingKubeVip,
		waitingStep:   stepWaitingKubeVipExit,
		timeout:       time.Duration(defaultStaticPodExitTimeout) * time.Second,
	}); err != nil {
		return err
	}
	// Close stale sockets while the VIP is still local; in testing this gave
	// the kernel the best chance to emit RSTs for those tuples. Then remove
	// the VIP to avoid accepting new traffic.
	progress.SetStep(stepClosingVipSockets)
	_, closeErr := s.closeRemoteVipPeers(runner, vip, defaultKubeVipApiserverPort)
	progress.SetStep(stepRemovingLoopbackVip)
	removeErr := s.removeKubeVipFromLoopback(runner, vip)
	if closeErr != nil {
		if removeErr != nil {
			return fmt.Errorf("close stale remote VIP sockets: %w; remove kube-vip address from lo: %v", closeErr, removeErr)
		}
		return fmt.Errorf("close stale remote VIP sockets: %w", closeErr)
	}
	if removeErr != nil {
		return fmt.Errorf("remove kube-vip address from lo: %w", removeErr)
	}

	// After kube-vip exits and the VIP is removed from lo, this node should stop
	// receiving new remote VIP:6443 connections. If new sockets still appear,
	// some part of the network is bypassing BGP ECMP withdrawal, for example a
	// stale sticky ARP entry on a DPU. Fail here before stopping the apiserver;
	// otherwise worker kubelets still pinned to this node can lose their API
	// path and become NotReady.
	progress.SetStep(stepWaitingVipSocketQuietWindow)
	if err := s.waitForVipSocketQuietWindow(ctx, runner, vip, defaultKubeVipApiserverPort, s.drainKubeVipSocketQuietWindowOrDefault()); err != nil {
		return fmt.Errorf("vip socket quiet window failed: %w", err)
	}
	return nil
}

// runKubeVipRestore reverses the kube-vip manifest move performed by
// runK8sControlPlaneNodeGracefulShutdown. It intentionally does not touch
// kube-apiserver; callers must restore kube-vip only after the local apiserver
// is ready to serve requests. When kube-vip is not configured on this node
// (mirroring the drain-side skip in runK8sControlPlaneNodeGracefulShutdown),
// there is nothing to restore and this is a no-op.
func (s *NodeAgentServer) runKubeVipRestore(
	ctx context.Context,
	progress nodeAgentOperationProgress,
) error {
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nkeutils.ExecRunner{}
	}

	progress.SetStep(stepResolvingKubeVip)
	vip, err := s.resolveKubeVipAddress(runner)
	switch {
	case err == nil:
		// fall through to restore below
	case errors.Is(err, errNoKubeVipAddress):
		slog.Info("kube-vip is not configured; skipping kube-vip restore")
		return nil
	default:
		return fmt.Errorf("resolve kube-vip address: %w", err)
	}

	if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: kubeVipContainerName,
		manifestName:  defaultKubeVipManifestName,
		restoringStep: stepRestoringKubeVip,
		waitingStep:   stepWaitingKubeVipStart,
		timeout:       s.staticPodStartTimeoutOrDefault(),
	}); err != nil {
		return err
	}

	progress.SetStep(stepWaitingLoopbackVip)
	if err := s.waitForKubeVipOnLoopback(ctx, runner, vip, s.staticPodStartTimeoutOrDefault()); err != nil {
		return fmt.Errorf("wait for kube-vip address on lo: %w", err)
	}
	return nil
}

var errNoKubeVipAddress = errors.New("no kube-vip address found")

func (s *NodeAgentServer) resolveKubeVipAddress(runner nkeutils.CommandRunner) (string, error) {
	vip, found, err := s.resolveKubeVipAddressFromManifest()
	if err != nil {
		return "", err
	}
	if found {
		return vip, nil
	}
	return resolveKubeVipAddressFromLoopback(runner)
}

func (s *NodeAgentServer) resolveKubeVipAddressFromManifest() (string, bool, error) {
	for _, path := range []string{s.kubeVipManifestPath(), s.kubeVipDrainedManifestPath()} {
		vip, err := parseKubeVipManifestAddress(path)
		if err == nil {
			slog.Info("resolved kube-vip address from static manifest", "path", path, "vip", vip)
			return vip, true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", true, err
	}
	return "", false, nil
}

type kubeVipManifest struct {
	Spec struct {
		Containers []struct {
			Name string `yaml:"name"`
			Env  []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"env"`
		} `yaml:"containers"`
	} `yaml:"spec"`
}

func parseKubeVipManifestAddress(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var manifest kubeVipManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse kube-vip manifest %s: %w", path, err)
	}
	env := map[string]string{}
	for _, container := range manifest.Spec.Containers {
		if container.Name != "" && container.Name != kubeVipContainerName {
			continue
		}
		for _, entry := range container.Env {
			env[entry.Name] = strings.TrimSpace(entry.Value)
		}
		if env["address"] != "" {
			break
		}
	}
	if env["address"] == "" {
		return "", fmt.Errorf("parse kube-vip manifest %s: missing env address", path)
	}
	if iface := env["vip_interface"]; iface != "" && iface != loopbackInterface {
		return "", fmt.Errorf("parse kube-vip manifest %s: unsupported vip_interface %q, expected lo", path, iface)
	}
	if cidr := env["vip_cidr"]; cidr != "" && cidr != "32" {
		return "", fmt.Errorf("parse kube-vip manifest %s: unsupported vip_cidr %q, expected 32", path, cidr)
	}
	addr, err := netip.ParseAddr(env["address"])
	if err != nil {
		return "", fmt.Errorf("parse kube-vip manifest %s: invalid address %q: %w", path, env["address"], err)
	}
	if !addr.Is4() {
		return "", fmt.Errorf("parse kube-vip manifest %s: address %q is not IPv4", path, env["address"])
	}
	return addr.String(), nil
}

func resolveKubeVipAddressFromLoopback(runner nkeutils.CommandRunner) (string, error) {
	output, err := runner.Run("ip", []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}, nil) //nolint:goconst // ip(8) argv fragment; naming each CLI flag reads worse than the literal
	if err != nil {
		return "", fmt.Errorf("ip addr show dev lo: %w: %s", err, strings.TrimSpace(string(output)))
	}
	vips, err := parseLoopbackIPv4VIPs(string(output))
	if err != nil {
		return "", err
	}
	if len(vips) == 0 {
		return "", errNoKubeVipAddress
	}
	if len(vips) > 1 {
		return "", fmt.Errorf("multiple non-loopback /32 addresses on lo: %s", strings.Join(vips, ", "))
	}
	return vips[0], nil
}

func parseLoopbackIPv4VIPs(output string) ([]string, error) {
	var vips []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "inet" {
				continue
			}
			prefix, err := netip.ParsePrefix(fields[i+1])
			if err != nil {
				return nil, fmt.Errorf("parse lo address %q: %w", fields[i+1], err)
			}
			if prefix.Addr().Is4() && prefix.Bits() == 32 && !prefix.Addr().IsLoopback() {
				vips = append(vips, prefix.Addr().String())
			}
			break
		}
	}
	return vips, nil
}

type staticPodStopSpec struct {
	containerName string
	manifestName  string
	stoppingStep  string
	waitingStep   string
	timeout       time.Duration
}

type staticPodRestoreSpec struct {
	containerName string
	manifestName  string
	restoringStep string
	waitingStep   string
	timeout       time.Duration
}

func (s *NodeAgentServer) stopStaticPod(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	spec staticPodStopSpec,
) error {
	progress.SetStep(spec.stoppingStep)
	containerIDs, err := listStaticPodContainerIDs(runner, spec.containerName)
	if err != nil {
		return fmt.Errorf("list %s containers: %w", spec.containerName, err)
	}
	if err := s.moveStaticPodManifest(spec.manifestName); err != nil {
		return fmt.Errorf("move %s manifest: %w", spec.containerName, err)
	}
	progress.SetStep(spec.waitingStep)
	if err := s.waitForStaticPodExit(ctx, runner, spec.containerName, containerIDs, spec.timeout); err != nil {
		return fmt.Errorf("wait for %s exit: %w", spec.containerName, err)
	}
	return nil
}

func (s *NodeAgentServer) restoreStaticPod(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	spec staticPodRestoreSpec,
) error {
	progress.SetStep(spec.restoringStep)
	containerIDs, err := listStaticPodContainerIDs(runner, spec.containerName)
	if err != nil {
		return fmt.Errorf("list existing %s containers: %w", spec.containerName, err)
	}
	manifestRestored, err := s.restoreStaticPodManifest(spec.manifestName)
	if err != nil {
		return fmt.Errorf("restore %s manifest: %w", spec.containerName, err)
	}
	progress.SetStep(spec.waitingStep)
	if !manifestRestored {
		containerIDs = nil
	}
	if err := s.waitForStaticPodRunning(ctx, runner, spec.containerName, containerIDs, spec.timeout); err != nil {
		return fmt.Errorf("wait for %s start: %w", spec.containerName, err)
	}
	return nil
}

func (s *NodeAgentServer) moveStaticPodManifest(manifestName string) error {
	src := s.rootedPath(filepath.Join(defaultStaticPodManifestDir, manifestName))
	dst := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, manifestName))
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("static pod manifest not present; assuming already moved", "path", src, "manifest", manifestName)
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), staticPodBackupDirMode); err != nil {
		return err
	}
	slog.Info("moving static pod manifest to trigger kubelet SIGTERM", "src", src, "dst", dst, "manifest", manifestName)
	return os.Rename(src, dst)
}

func (s *NodeAgentServer) restoreStaticPodManifest(manifestName string) (bool, error) {
	src := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, manifestName))
	dst := s.rootedPath(filepath.Join(defaultStaticPodManifestDir, manifestName))
	if _, err := os.Stat(dst); err == nil {
		if err := verifyRestoredStaticPodManifest(src, dst, manifestName); err != nil {
			return false, err
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("static pod manifest is neither active at %s nor backed up at %s", dst, src)
		}
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), staticPodManifestDirMode); err != nil {
		return false, err
	}
	slog.Info("restoring static pod manifest", "src", src, "dst", dst, "manifest", manifestName)
	return true, os.Rename(src, dst)
}

func verifyRestoredStaticPodManifest(src, dst, manifestName string) error {
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("static pod manifest already active and no backup remains; assuming already restored", "path", dst, "manifest", manifestName)
			return nil
		}
		return err
	}
	srcHash, err := fileSHA256(src)
	if err != nil {
		return err
	}
	dstHash, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if srcHash != dstHash {
		return fmt.Errorf("static pod manifest already active at %s but differs from backup at %s", dst, src)
	}
	slog.Info("static pod manifest already active and matches backup; assuming already restored", "path", dst, "backup", src, "manifest", manifestName)
	return nil
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (s *NodeAgentServer) kubeVipManifestPath() string {
	return s.rootedPath(filepath.Join(defaultStaticPodManifestDir, defaultKubeVipManifestName))
}

func (s *NodeAgentServer) kubeVipDrainedManifestPath() string {
	return s.rootedPath(filepath.Join(defaultStaticPodBackupDir, defaultKubeVipManifestName))
}

func (s *NodeAgentServer) rootedPath(path string) string {
	if s.RootPath == "" {
		return path
	}
	return filepath.Join(s.RootPath, strings.TrimPrefix(path, "/"))
}

func listStaticPodContainerIDs(runner nkeutils.CommandRunner, containerName string) ([]string, error) {
	output, err := runner.RunSplitOutput("crictl", []string{
		"ps",
		"--label", "io.kubernetes.container.name=" + containerName,
		"-o", "json",
	}, nil)
	if stderr := strings.TrimSpace(string(output.Stderr)); stderr != "" {
		slog.Warn("crictl ps static pod wrote to stderr", "container", containerName, "stderr", stderr)
	}
	if err != nil {
		return nil, fmt.Errorf("crictl ps %s: %w: %s", containerName, err, strings.TrimSpace(string(output.Stderr)))
	}
	var ps struct {
		Containers []struct {
			ID string `json:"id"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(output.Stdout, &ps); err != nil {
		return nil, fmt.Errorf("parse crictl ps %s JSON: %w: %s", containerName, err, strings.TrimSpace(string(output.Stdout)))
	}
	ids := make([]string, 0, len(ps.Containers))
	for _, container := range ps.Containers {
		id := strings.TrimSpace(container.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *NodeAgentServer) waitForStaticPodExit(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	containerName string,
	initialContainerIDs []string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	if len(initialContainerIDs) == 0 {
		slog.Info("no static pod containers were running before manifest removal", "container", containerName)
	}
	for {
		containerIDs, err := listStaticPodContainerIDs(runner, containerName)
		if err != nil {
			return err
		}
		if len(containerIDs) == 0 {
			slog.Info("static pod containers are no longer running", "container", containerName, "initialContainerIDs", initialContainerIDs)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container still running after %s: %s", timeout, strings.Join(containerIDs, ", "))
		}
		if err := sleepOrDone(ctx, s.drainKubeVipPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

func (s *NodeAgentServer) waitForStaticPodRunning(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	containerName string,
	previousContainerIDs []string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	previousContainerIDSet := make(map[string]struct{}, len(previousContainerIDs))
	for _, id := range previousContainerIDs {
		previousContainerIDSet[id] = struct{}{}
	}
	for {
		containerIDs, err := listStaticPodContainerIDs(runner, containerName)
		if err != nil {
			return err
		}
		newContainerIDs := newStaticPodContainerIDs(containerIDs, previousContainerIDSet)
		if len(newContainerIDs) > 0 {
			slog.Info("static pod containers are running", "container", containerName, "containerIDs", containerIDs)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("container not running after %s", timeout)
		}
		if err := sleepOrDone(ctx, s.drainKubeVipPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

func newStaticPodContainerIDs(containerIDs []string, previousContainerIDSet map[string]struct{}) []string {
	if len(previousContainerIDSet) == 0 {
		return containerIDs
	}
	newContainerIDs := make([]string, 0, len(containerIDs))
	for _, id := range containerIDs {
		if _, ok := previousContainerIDSet[id]; ok {
			continue
		}
		newContainerIDs = append(newContainerIDs, id)
	}
	return newContainerIDs
}

func sleepOrDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *NodeAgentServer) removeKubeVipFromLoopback(runner nkeutils.CommandRunner, vip string) error {
	output, err := runner.Run("ip", []string{"addr", "del", vip + "/32", "dev", "lo"}, nil)
	if err == nil {
		slog.Info("removed kube-vip address from lo", "vip", vip)
		return nil
	}
	current, currentErr := runner.Run("ip", []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}, nil)
	if currentErr == nil {
		vips, parseErr := parseLoopbackIPv4VIPs(string(current))
		if parseErr == nil && !slices.Contains(vips, vip) {
			slog.Info("kube-vip address is already absent from lo", "vip", vip)
			return nil
		}
	}
	return fmt.Errorf("ip addr del %s/32 dev lo: %w: %s", vip, err, strings.TrimSpace(string(output)))
}

func (s *NodeAgentServer) waitForKubeVipOnLoopback(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	vip string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for {
		output, err := runner.Run("ip", []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}, nil)
		if err != nil {
			return fmt.Errorf("ip addr show dev lo: %w: %s", err, strings.TrimSpace(string(output)))
		}
		vips, err := parseLoopbackIPv4VIPs(string(output))
		if err != nil {
			return err
		}
		if slices.Contains(vips, vip) {
			slog.Info("kube-vip address restored on lo", "vip", vip)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("kube-vip address %s not present on lo after %s", vip, timeout)
		}
		if err := sleepOrDone(ctx, s.drainKubeVipPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

func (s *NodeAgentServer) closeRemoteVipPeers(runner nkeutils.CommandRunner, vip string, port int) (int, error) {
	peers, err := s.listRemoteVipPeers(runner, vip, port)
	if err != nil {
		return 0, err
	}
	if len(peers) == 0 {
		slog.Info("no stale remote VIP sockets to close", "vip", vip, "port", port)
		return 0, nil
	}

	for _, peer := range peers {
		slog.Info("closing stale remote VIP socket", "vip", vip, "port", port, "peerIP", peer.IP, "peerPort", peer.Port)
		output, err := runner.Run("ss", []string{
			"-Ktan", "state", "established",
			"src", vip, "sport", "=", ":" + strconv.Itoa(port),
			"dst", peer.IP, "dport", "=", ":" + peer.Port,
		}, nil)
		if err != nil {
			return 0, fmt.Errorf("ss -K %s:%d -> %s:%s: %w: %s", vip, port, peer.IP, peer.Port, err, strings.TrimSpace(string(output)))
		}
	}
	return len(peers), nil
}

func (s *NodeAgentServer) waitForVipSocketQuietWindow(
	ctx context.Context,
	runner nkeutils.CommandRunner,
	vip string,
	port int,
	quietWindow time.Duration,
) error {
	deadline := time.Now().Add(quietWindow)
	for {
		peers, err := s.listRemoteVipPeers(runner, vip, port)
		if err != nil {
			return err
		}
		if len(peers) > 0 {
			return fmt.Errorf("new remote VIP:%d sockets appeared after kube-vip exit: %s", port, formatVipSocketPeers(vip, port, peers))
		}
		if !time.Now().Before(deadline) {
			slog.Info("no new remote VIP sockets observed", "vip", vip, "port", port, "quietWindow", quietWindow)
			return nil
		}
		if err := sleepOrDone(ctx, s.drainKubeVipPollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

func (s *NodeAgentServer) drainKubeVipPollIntervalOrDefault() time.Duration {
	if s != nil && s.drainKubeVipPollInterval > 0 {
		return s.drainKubeVipPollInterval
	}
	return defaultDrainKubeVipPollInterval
}

func (s *NodeAgentServer) drainKubeVipSocketQuietWindowOrDefault() time.Duration {
	if s != nil && s.drainKubeVipSocketQuietWindow > 0 {
		return s.drainKubeVipSocketQuietWindow
	}
	return time.Duration(defaultKubeVipSocketQuietSeconds) * time.Second
}

func (s *NodeAgentServer) staticPodStartTimeoutOrDefault() time.Duration {
	if s != nil && s.staticPodStartTimeout > 0 {
		return s.staticPodStartTimeout
	}
	return time.Duration(defaultStaticPodStartTimeout) * time.Second
}

func formatVipSocketPeers(vip string, port int, peers []vipSocketPeer) string {
	parts := make([]string, 0, len(peers))
	for _, peer := range peers {
		parts = append(parts, fmt.Sprintf("%s:%d -> %s:%s", vip, port, peer.IP, peer.Port))
	}
	return strings.Join(parts, ", ")
}

func (s *NodeAgentServer) listRemoteVipPeers(runner nkeutils.CommandRunner, vip string, port int) ([]vipSocketPeer, error) {
	output, err := runner.Run("ss", []string{
		"-Htan", "state", "established",
		"src", vip, "sport", "=", ":" + strconv.Itoa(port),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("ss list VIP sockets: %w: %s", err, strings.TrimSpace(string(output)))
	}
	localIPs, err := currentLocalIPs()
	if err != nil {
		return nil, fmt.Errorf("list local interface addresses: %w", err)
	}
	return parseRemoteVipPeers(string(output), vip, localIPs)
}

func currentLocalIPs() (map[string]struct{}, error) {
	localIPs := map[string]struct{}{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err != nil {
			continue
		}
		localIPs[prefix.Addr().String()] = struct{}{}
	}
	return localIPs, nil
}

func parseRemoteVipPeers(output, vip string, localIPs map[string]struct{}) ([]vipSocketPeer, error) {
	var peers []vipSocketPeer
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, fmt.Errorf("unexpected ss output line %q", line)
		}
		peerIP, peerPort, err := parseSocketEndpoint(fields[3])
		if err != nil {
			return nil, fmt.Errorf("parse peer endpoint %q: %w", fields[3], err)
		}
		if peerIP == "" || peerPort == "" || peerIP == vip || isLoopbackIP(peerIP) {
			continue
		}
		if _, ok := localIPs[peerIP]; ok {
			continue
		}
		peers = append(peers, vipSocketPeer{IP: peerIP, Port: peerPort})
	}
	return peers, nil
}

func parseSocketEndpoint(endpoint string) (string, string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", "", fmt.Errorf("empty endpoint")
	}
	if strings.HasPrefix(endpoint, "[") {
		end := strings.LastIndex(endpoint, "]:")
		if end < 0 {
			return "", "", fmt.Errorf("missing bracketed port")
		}
		return normalizeSocketIP(endpoint[1:end]), endpoint[end+2:], nil
	}
	idx := strings.LastIndex(endpoint, ":")
	if idx < 0 || idx == len(endpoint)-1 {
		return "", "", fmt.Errorf("missing port")
	}
	return normalizeSocketIP(endpoint[:idx]), endpoint[idx+1:], nil
}

func normalizeSocketIP(ip string) string {
	ip = strings.Trim(ip, "[]")
	ip = strings.TrimPrefix(ip, "::ffff:")
	return ip
}

func isLoopbackIP(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}
