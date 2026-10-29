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

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

const (
	defaultStaticPodManifestDir = "/etc/kubernetes/manifests"
	defaultStaticPodBackupDir   = "/etc/kubernetes/manifests-backup"

	// defaultEtcdDefragParkDir is where the etcd defragmentation sequence parks
	// the manifests it owes a restore for, kept apart from the manifests parked
	// by a graceful shutdown.
	//
	// The directory is the whole discriminator. A manifest here means some
	// defragmentation moved it aside and has not put it back, which is a debt
	// nothing but this agent will settle; a manifest in the parent directory
	// means a caller deliberately took this node out of service and owns the
	// restore itself. Separating them by path is what lets the startup
	// reconciler act on sight, with no operation record to consult and no
	// ordering to get wrong - and it is why it cannot re-advertise the VIP on a
	// node somebody is recycling.
	//
	// A subdirectory rather than a suffix on the filename: the snapshot pruner
	// globs "<manifest>.*" in the parent, so a sibling named
	// "kube-apiserver.yaml.etcd-defrag" would match it and be deleted as an old
	// snapshot, destroying the only copy of a control-plane manifest.
	defaultEtcdDefragParkDir = defaultStaticPodBackupDir + "/etcd-defrag"

	defaultKubeVipManifestName = "kube-vip.yaml"
	// loopbackInterface is the device kube-vip binds the VIP to in ARP mode.
	loopbackInterface                = "lo"
	defaultKubeAPIServerManifestName = "kube-apiserver.yaml"
	defaultStaticPodExitTimeout      = 60
	defaultStaticPodStartTimeout     = 60
	defaultKubeVipApiserverPort      = 6443
	defaultKubeVipSocketQuietSeconds = 30
	defaultControlPlanePollInterval  = time.Second
	// Match kubeadm's static pod manifest directory convention; keep the agent-owned backup directory private.
	staticPodManifestDirMode = 0o755
	staticPodBackupDirMode   = 0o700

	operationTypeK8sControlPlaneNodeGracefulShutdown = "k8s_control_plane_node_graceful_shutdown"
	operationTypeKubeVipRestore                      = "kube_vip_restore"

	stepStoppingKubeAPIServer       = "stopping_kube_apiserver"
	stepWaitingKubeAPIServerExit    = "waiting_for_kube_apiserver_exit"
	stepRestoringKubeAPIServer      = "restoring_kube_apiserver"
	stepWaitingKubeAPIServerStart   = "waiting_for_kube_apiserver_start"
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
// When kube-vip is not configured, the VIP withdrawal steps are skipped. In both
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
		runner = &nvkeutils.ExecRunner{}
	}
	// The address is discarded because this RPC does not restore: the node is
	// meant to stay out of service until a caller asks for it back, and that
	// restore re-resolves from the manifest this parked.
	if _, err := s.withdrawKubeVipLocked(ctx, runner, progress, defaultStaticPodBackupDir); err != nil {
		return err
	}

	if shouldStopKubeAPIServer {
		return s.stopKubeAPIServerLocked(ctx, runner, progress, defaultStaticPodBackupDir)
	}
	return nil
}

// withdrawKubeVipLocked takes this node out of the control-plane VIP, or does
// nothing when kube-vip is not configured here. Callers must already hold
// staticPodMutationMu.
//
// "Withdraw" rather than "drain": no workload is drained, nothing is cordoned
// and nothing is evicted. What stops is this node's participation in the VIP.
//
// That takes three distinct actions, because advertising the address, accepting
// traffic for it, and connections already established to it are three different
// things: park the kube-vip manifest so it stops advertising, `ss -K` the
// established sockets so existing clients get an RST, and `ip addr del` the
// address from lo so nothing new is accepted. A fourth step then verifies no
// new sockets arrive, which catches a network that is still steering traffic
// here despite all three. See withdrawKubeVip.
// It reports what it withdrew, so a caller that restores within the same call
// can put back exactly that rather than working it out again afterwards. The
// withdrawal destroys the evidence: it parks the manifest and deletes the
// address from lo, which are two of the three things the address resolves from.
func (s *NodeAgentServer) withdrawKubeVipLocked(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	parkDir string,
) (kubeVipWithdrawal, error) {
	progress.SetStep(stepResolvingKubeVip)
	vip, fromManifest, err := s.resolveKubeVipAddressFromManifest(parkDir)
	if err != nil {
		return kubeVipWithdrawal{}, fmt.Errorf("resolve kube-vip address: %w", err)
	}
	if !fromManifest {
		vip, err = resolveKubeVipAddressFromLoopback(runner)
		switch {
		case err == nil:
		case errors.Is(err, errNoKubeVipAddress):
			slog.Info("kube-vip is not configured; skipping kube-vip withdrawal")
			return kubeVipWithdrawal{}, nil
		default:
			return kubeVipWithdrawal{}, fmt.Errorf("resolve kube-vip address: %w", err)
		}
	}
	withdrawal := kubeVipWithdrawal{vip: vip, manifestParked: fromManifest}
	return withdrawal, s.withdrawKubeVip(ctx, runner, progress, parkDir, vip)
}

// kubeVipWithdrawal records what a withdrawal did, so the restore puts back that
// and nothing else.
type kubeVipWithdrawal struct {
	// vip is the address withdrawn from this node. Empty means kube-vip is not
	// configured here and nothing was withdrawn.
	vip string
	// manifestParked distinguishes a kube-vip that is a static pod from one that
	// is not. When the address came from lo rather than from a manifest there is
	// no manifest to park, and the restore must not try to put one back: it
	// would fail with the manifest neither active nor backed up, and report a
	// node unreturnable when all it needs is its address again.
	manifestParked bool
}

// withdrawKubeVip removes kube-vip from this node so it stops advertising and
// serving the control-plane VIP, while keeping the local apiserver available.
//
// Close stale sockets while the VIP is still assigned to lo. In testing,
// after removing the VIP from lo, ss -K could still destroy the local socket,
// but the expected RST was not sent to the remote peer for that established
// VIP:6443 connection. Closing first gives the kernel the best chance to emit
// an RST before the local VIP address is removed.
func (s *NodeAgentServer) withdrawKubeVip(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	parkDir string,
	vip string,
) error {
	if err := s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
		containerName: kubeVipContainerName,
		manifestName:  defaultKubeVipManifestName,
		parkDir:       parkDir,
		stoppingStep:  stepStoppingKubeVip,
		waitingStep:   stepWaitingKubeVipExit,
		timeout:       s.staticPodExitTimeoutOrDefault(),
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
			// Joined rather than formatted with %v, so both stay reachable
			// through errors.Is and errors.As.
			return errors.Join(
				fmt.Errorf("close stale remote VIP sockets: %w", closeErr),
				fmt.Errorf("remove kube-vip address from lo: %w", removeErr),
			)
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
	if err := s.waitForVipSocketQuietWindow(ctx, runner, vip, defaultKubeVipApiserverPort, s.kubeVipSocketQuietWindowOrDefault()); err != nil {
		return fmt.Errorf("vip socket quiet window failed: %w", err)
	}
	return nil
}

// runKubeVipRestore reverses the kube-vip manifest move performed by
// runK8sControlPlaneNodeGracefulShutdown. It intentionally does not touch
// kube-apiserver; callers must restore kube-vip only after the local apiserver
// is ready to serve requests. When kube-vip is not configured on this node
// (mirroring the withdrawal-side skip in runK8sControlPlaneNodeGracefulShutdown),
// there is nothing to restore and this is a no-op.
func (s *NodeAgentServer) runKubeVipRestore(
	ctx context.Context,
	progress nodeAgentOperationProgress,
) error {
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}
	// No withdrawal happened in this call, so the address is re-resolved from
	// the manifest the graceful shutdown parked.
	return s.restoreKubeVipLocked(ctx, runner, progress, defaultStaticPodBackupDir, kubeVipWithdrawal{})
}

// restoreKubeVipLocked is runKubeVipRestore without the lock, for callers that
// already hold staticPodMutationMu because the restore is one step of a longer
// static-pod transaction. StartEtcdDefrag is one: it withdraws the VIP, takes
// etcd down and puts both back, and releasing the mutex between those steps
// would let another static-pod mutation interleave while the VIP is parked.
//
// sync.Mutex is not reentrant, so this split is what keeps the bundled sequence
// from deadlocking on itself.
// withdrawn describes a withdrawal performed earlier in this same call, and is
// the zero value when there was none - the standalone restore RPC and the
// startup reconciler both arrive that way and re-resolve, which works for them
// because reaching either means a manifest is parked.
//
// Given a withdrawal, this does not re-resolve. The withdrawal removed what
// re-resolution reads, so on a node where kube-vip is not a static pod there
// would be nothing left to find, and the restore would quietly decide kube-vip
// was not configured and skip - leaving the node out of the VIP while the
// operation reported success.
func (s *NodeAgentServer) restoreKubeVipLocked(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	parkDir string,
	withdrawn kubeVipWithdrawal,
) error {
	vip := withdrawn.vip
	restoreManifest := withdrawn.manifestParked
	if vip == "" {
		progress.SetStep(stepResolvingKubeVip)
		resolved, err := s.resolveKubeVipAddress(runner, parkDir)
		switch {
		case err == nil:
			vip = resolved
			restoreManifest = true
		case errors.Is(err, errNoKubeVipAddress):
			slog.Info("kube-vip is not configured; skipping kube-vip restore")
			return nil
		default:
			return fmt.Errorf("resolve kube-vip address: %w", err)
		}
	}

	// Skipped when the withdrawal had no manifest to park. Asking for one back
	// would fail with it neither active nor backed up, which is the truth but
	// not a problem: kube-vip runs some other way here and only the address was
	// taken.
	if restoreManifest {
		if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
			containerName: kubeVipContainerName,
			manifestName:  defaultKubeVipManifestName,
			parkDir:       parkDir,
			restoringStep: stepRestoringKubeVip,
			waitingStep:   stepWaitingKubeVipStart,
			timeout:       s.staticPodStartTimeoutOrDefault(),
		}); err != nil {
			return err
		}
	} else if err := s.addKubeVipToLoopback(runner, vip); err != nil {
		// Nothing else will put the address back, since there is no manifest for
		// kubelet to act on.
		return fmt.Errorf("restore kube-vip address on lo: %w", err)
	}

	progress.SetStep(stepWaitingLoopbackVip)
	if err := s.waitForKubeVipOnLoopback(ctx, runner, vip, s.staticPodStartTimeoutOrDefault()); err != nil {
		return fmt.Errorf("wait for kube-vip address on lo: %w", err)
	}
	return nil
}

// stopKubeAPIServerLocked parks the kube-apiserver manifest and waits for the
// container to exit. Callers must already hold staticPodMutationMu.
//
// This exists so a caller that is about to take the local etcd member down can
// stop drawing in-cluster traffic to this node first. Withdrawing the VIP is
// not enough on its own: the kubernetes.default Service resolves to the
// control-plane node IPs rather than the VIP, so kube-proxy keeps load
// balancing in-cluster clients onto this apiserver, and kubeadm points it at
// --etcd-servers=https://127.0.0.1:2379, its local member only. Left running it
// would serve those clients from an etcd that is gone.
//
// Stopping it also withdraws the node from that Service, because the apiserver
// publishes a lease under /registry/masterleases/<advertise-address> that the
// endpoint reconciler in every apiserver prunes once it stops being renewed.
// That is automatic in both directions - nothing has to add the address back.
func (s *NodeAgentServer) stopKubeAPIServerLocked(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	parkDir string,
) error {
	return s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
		containerName: kubeAPIServerContainerName,
		manifestName:  defaultKubeAPIServerManifestName,
		parkDir:       parkDir,
		stoppingStep:  stepStoppingKubeAPIServer,
		waitingStep:   stepWaitingKubeAPIServerExit,
		timeout:       s.staticPodExitTimeoutOrDefault(),
	})
}

// restoreKubeAPIServerLocked puts the kube-apiserver manifest back and waits for
// the container to run again. Callers must already hold staticPodMutationMu.
//
// Restore this before kube-vip, and only once the local etcd member is serving:
// the apiserver cannot republish its masterlease until etcd is back, and the VIP
// must not be advertised from a node whose apiserver is not yet ready.
func (s *NodeAgentServer) restoreKubeAPIServerLocked(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	parkDir string,
) error {
	return s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: kubeAPIServerContainerName,
		manifestName:  defaultKubeAPIServerManifestName,
		parkDir:       parkDir,
		restoringStep: stepRestoringKubeAPIServer,
		waitingStep:   stepWaitingKubeAPIServerStart,
		timeout:       s.staticPodStartTimeoutOrDefault(),
	})
}

var errNoKubeVipAddress = errors.New("no kube-vip address found")

func (s *NodeAgentServer) resolveKubeVipAddress(runner nvkeutils.CommandRunner, parkDir string) (string, error) {
	vip, found, err := s.resolveKubeVipAddressFromManifest(parkDir)
	if err != nil {
		return "", err
	}
	if found {
		return vip, nil
	}
	return resolveKubeVipAddressFromLoopback(runner)
}

func (s *NodeAgentServer) resolveKubeVipAddressFromManifest(parkDir string) (string, bool, error) {
	for _, path := range []string{s.kubeVipManifestPath(), s.kubeVipParkedManifestPath(parkDir)} {
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

func resolveKubeVipAddressFromLoopback(runner nvkeutils.CommandRunner) (string, error) {
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
	// parkDir is where the manifest is moved to, and so which family of park
	// this is. See defaultEtcdDefragParkDir.
	parkDir      string
	stoppingStep string
	waitingStep  string
	timeout      time.Duration
}

type staticPodRestoreSpec struct {
	containerName string
	manifestName  string
	// parkDir must match the one the stop used, or the restore looks for the
	// manifest where nothing parked it.
	parkDir       string
	restoringStep string
	waitingStep   string
	timeout       time.Duration
}

func (s *NodeAgentServer) stopStaticPod(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	spec staticPodStopSpec,
) error {
	progress.SetStep(spec.stoppingStep)
	containerIDs, err := listStaticPodContainerIDs(runner, spec.containerName)
	if err != nil {
		return fmt.Errorf("list %s containers: %w", spec.containerName, err)
	}
	if err := s.moveStaticPodManifest(spec.parkDir, spec.manifestName); err != nil {
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
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	spec staticPodRestoreSpec,
) error {
	progress.SetStep(spec.restoringStep)
	// Carry on when the listing fails rather than returning. The listing is only
	// the baseline for the new-container gate below, so losing it weakens that
	// gate; returning here would skip the rename entirely and leave a manifest
	// that was moved aside parked, with nothing scheduled to put it back. For
	// etcd that is the difference between a failed defragmentation and a member
	// that stays down.
	var baseline staticPodBaseline
	containerIDs, err := listStaticPodContainerIDs(runner, spec.containerName)
	if err != nil {
		slog.WarnContext(ctx, "failed to list containers before restoring static pod; restoring anyway",
			"container", spec.containerName, "err", err)
		baseline.unknown = true
	} else {
		baseline.containerIDs = containerIDs
	}
	manifestRestored, err := s.restoreStaticPodManifest(spec.parkDir, spec.manifestName)
	if err != nil {
		return fmt.Errorf("restore %s manifest: %w", spec.containerName, err)
	}
	progress.SetStep(spec.waitingStep)
	if !manifestRestored {
		// The manifest was already active, so nothing was renamed and there is
		// no restart to wait for.
		baseline = staticPodBaseline{anyContainerSatisfies: true}
	}
	if err := s.waitForStaticPodRunning(ctx, runner, spec.containerName, baseline, spec.timeout); err != nil {
		return fmt.Errorf("wait for %s start: %w", spec.containerName, err)
	}
	return nil
}

type staticPodContainerRestartSpec struct {
	containerName string
	stoppingStep  string
	waitingStep   string
	// stopTimeout bounds the SIGTERM-to-SIGKILL window handed to crictl.
	stopTimeout time.Duration
	// startTimeout bounds the wait for kubelet to run a replacement container.
	startTimeout time.Duration
}

// restartStaticPodContainer stops a static pod's container through the CRI and
// waits for kubelet to run a replacement from the manifest, which stays in
// place throughout.
//
// This is the restart path for a pod that has to come back. Moving the manifest
// aside also restarts the pod, but it makes the agent responsible for putting
// the manifest back: a crash between the move and the restore leaves the pod
// down with nothing scheduled to return it. Stopping the container instead
// leaves kubelet owning the restart, which it performs from the manifest it can
// still see, under the pod's own restartPolicy of Always.
//
// It also fails safe. The container listing is both the set to stop and the
// baseline that proves a restart happened, so a listing failure aborts before
// anything is touched - where the manifest-move path had to attempt the rename
// regardless, because a failed listing there could strand the manifest.
func (s *NodeAgentServer) restartStaticPodContainer(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	spec staticPodContainerRestartSpec,
) error {
	progress.SetStep(spec.stoppingStep)
	containerIDs, err := listStaticPodContainerIDs(runner, spec.containerName)
	if err != nil {
		return fmt.Errorf("list %s containers: %w", spec.containerName, err)
	}
	if len(containerIDs) == 0 {
		return fmt.Errorf("no running %s container to restart", spec.containerName)
	}

	// Floored at a second because crictl takes whole seconds, and rounding a
	// sub-second budget down to zero would turn a graceful stop into an
	// immediate kill - the opposite of what every caller here wants. Only a
	// test configures a budget that small.
	stopSeconds := strconv.Itoa(max(int(spec.stopTimeout.Seconds()), 1))
	for _, id := range containerIDs {
		slog.InfoContext(ctx, "stopping static pod container through the CRI",
			"container", spec.containerName, "containerID", id, "stopTimeout", spec.stopTimeout)
		output, stopErr := runner.Run("crictl", []string{"stop", "--timeout", stopSeconds, id}, nil)
		if stopErr != nil {
			return fmt.Errorf("crictl stop %s %s: %w: %s",
				spec.containerName, id, stopErr, strings.TrimSpace(string(output)))
		}
	}

	progress.SetStep(spec.waitingStep)
	// The pre-stop IDs are the baseline, so only a container outside that set
	// counts. Nothing was renamed, so a still-running old container would
	// otherwise satisfy the wait immediately.
	if err := s.waitForStaticPodRunning(ctx, runner, spec.containerName,
		staticPodBaseline{containerIDs: containerIDs}, spec.startTimeout); err != nil {
		return fmt.Errorf("wait for %s restart: %w", spec.containerName, err)
	}
	return nil
}

// handOffParkedManifestsToCaller moves everything the etcd defragmentation
// parked out of its own park directory and into the shared one, which is how
// the sequence stops owing a restore.
//
// It is for the branches that leave this node out of service on purpose: etcd
// did not come back healthy, and putting kube-apiserver or kube-vip back would
// route traffic to a member that cannot answer. The parent directory already
// means "parked, and a caller owns the restore" - it is where a graceful
// shutdown leaves a node held for recycling - so moving them there says exactly
// that.
//
// Without this the startup reconciler would find them still sitting in the
// defragmentation park directory and put them straight back, overruling the
// decision on the next restart.
//
// Only kube-vip has an RPC that undoes this: StartKubeVipRestore. Nothing
// restores kube-apiserver from here, which is the same position a graceful
// shutdown with stop_kube_apiserver leaves a node in, and is why the outcome
// this accompanies is documented as needing an operator. An etcd that will not
// start from its own unmodified manifest is not something the agent can fix, so
// the node stays down either way; what this guarantees is that it stays down
// deliberately rather than being handed back by the next restart.
func (s *NodeAgentServer) handOffParkedManifestsToCaller() error {
	var moved []string
	var errs []error
	for _, manifestName := range []string{defaultKubeAPIServerManifestName, defaultKubeVipManifestName} {
		src := s.rootedPath(filepath.Join(defaultEtcdDefragParkDir, manifestName))
		dst := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, manifestName))
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", src, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), staticPodBackupDirMode); err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", filepath.Dir(dst), err))
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("move %s to %s: %w", src, dst, err))
			continue
		}
		moved = append(moved, manifestName)
	}
	if len(moved) > 0 {
		slog.Warn("handed parked static pod manifests to the caller; this node stays out of service until they are restored",
			"manifests", moved, "dir", defaultStaticPodBackupDir)
	}
	return errors.Join(errs...)
}

// moveStaticPodManifest parks a manifest in parkDir, which is what makes
// kubelet tear the pod down. parkDir decides who owes the restore: see
// defaultEtcdDefragParkDir.
func (s *NodeAgentServer) moveStaticPodManifest(parkDir, manifestName string) error {
	src := s.rootedPath(filepath.Join(defaultStaticPodManifestDir, manifestName))
	dst := s.rootedPath(filepath.Join(parkDir, manifestName))
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

func (s *NodeAgentServer) restoreStaticPodManifest(parkDir, manifestName string) (bool, error) {
	src := s.rootedPath(filepath.Join(parkDir, manifestName))
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

func (s *NodeAgentServer) kubeVipParkedManifestPath(parkDir string) string {
	return s.rootedPath(filepath.Join(parkDir, defaultKubeVipManifestName))
}

func (s *NodeAgentServer) rootedPath(path string) string {
	if s.RootPath == "" {
		return path
	}
	return filepath.Join(s.RootPath, strings.TrimPrefix(path, "/"))
}

// staticPodContainer retains CRI identity labels so callers can distinguish a
// kubeadm component from an unrelated workload with the same container name.
type staticPodContainer struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
}

func listStaticPodContainerIDs(runner nvkeutils.CommandRunner, containerName string) ([]string, error) {
	containers, err := listStaticPodContainers(runner, containerName)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		ids = append(ids, container.ID)
	}
	return ids, nil
}

// listStaticPodContainers queries running local containers without depending on
// the API server. The name label alone does not prove static-pod identity: callers
// needing an exact kubeadm pod must also check namespace and pod-name labels.
// Stderr is kept separate so runtime warnings do not corrupt the JSON response.
func listStaticPodContainers(runner nvkeutils.CommandRunner, containerName string) ([]staticPodContainer, error) {
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
		Containers []staticPodContainer `json:"containers"`
	}
	if err := json.Unmarshal(output.Stdout, &ps); err != nil {
		return nil, fmt.Errorf("parse crictl ps %s JSON: %w: %s", containerName, err, strings.TrimSpace(string(output.Stdout)))
	}
	containers := make([]staticPodContainer, 0, len(ps.Containers))
	for _, container := range ps.Containers {
		container.ID = strings.TrimSpace(container.ID)
		if container.ID == "" {
			continue
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (s *NodeAgentServer) waitForStaticPodExit(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
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
		if err := sleepOrDone(ctx, s.controlPlanePollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

// waitForStaticPodRunning waits for the container to be running again after a
// manifest was put back.
//
// What counts as "again" depends on what the caller knows, which is why the
// baseline is a type rather than a slice. A known set of pre-restore container
// IDs proves a restart when something outside it appears. An absent baseline is
// two different situations that must not be conflated: when no swap is expected
// any running container is the answer, but when the listing simply failed, the
// container still running from before the move would otherwise satisfy the
// wait. In that case the container has to be observed gone first, which is
// cheap here because the manifest is renamed back before this is called and the
// kubelet takes tens of seconds to notice it.
func (s *NodeAgentServer) waitForStaticPodRunning(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	containerName string,
	baseline staticPodBaseline,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	previousContainerIDSet := make(map[string]struct{}, len(baseline.containerIDs))
	for _, id := range baseline.containerIDs {
		previousContainerIDSet[id] = struct{}{}
	}
	observedAbsent := false
	for {
		containerIDs, err := listStaticPodContainerIDs(runner, containerName)
		if err != nil {
			return err
		}
		if len(containerIDs) == 0 {
			observedAbsent = true
		}
		if staticPodRestarted(containerIDs, previousContainerIDSet, baseline, observedAbsent) {
			slog.Info("static pod containers are running", "container", containerName, "containerIDs", containerIDs)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("container not running after %s", timeout)
		}
		if err := sleepOrDone(ctx, s.controlPlanePollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

// staticPodBaseline is what the caller knows about the containers that were
// running before a manifest was restored.
type staticPodBaseline struct {
	// containerIDs are the IDs seen before the restore. Empty means none were
	// seen, which is only meaningful alongside the two flags below.
	containerIDs []string
	// anyContainerSatisfies records that no swap is expected, because the
	// manifest was already in place and nothing was renamed. Whatever is
	// running is the right answer.
	anyContainerSatisfies bool
	// unknown records that the listing failed, so the empty set is absence of
	// evidence rather than evidence of absence.
	unknown bool
}

func staticPodRestarted(
	containerIDs []string,
	previousContainerIDSet map[string]struct{},
	baseline staticPodBaseline,
	observedAbsent bool,
) bool {
	switch {
	case baseline.anyContainerSatisfies:
		return len(containerIDs) > 0
	case baseline.unknown:
		return observedAbsent && len(containerIDs) > 0
	default:
		return len(newStaticPodContainerIDs(containerIDs, previousContainerIDSet)) > 0
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

// addKubeVipToLoopback puts the VIP back on lo directly. It is only for the node
// whose kube-vip is not a static pod: everywhere else kube-vip reclaims the
// address itself once its manifest is back, and waitForKubeVipOnLoopback is what
// confirms it. Treats an address that is already present as success, so a
// partially completed withdrawal can be undone.
func (s *NodeAgentServer) addKubeVipToLoopback(runner nvkeutils.CommandRunner, vip string) error {
	output, err := runner.Run("ip", []string{"addr", "add", vip + "/32", "dev", "lo"}, nil)
	if err == nil {
		slog.Info("restored kube-vip address on lo", "vip", vip)
		return nil
	}
	current, currentErr := runner.Run("ip", []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}, nil)
	if currentErr == nil {
		vips, parseErr := parseLoopbackIPv4VIPs(string(current))
		if parseErr == nil && slices.Contains(vips, vip) {
			slog.Info("kube-vip address is already present on lo", "vip", vip)
			return nil
		}
	}
	return fmt.Errorf("ip addr add %s dev lo: %w: %s", vip, err, strings.TrimSpace(string(output)))
}

func (s *NodeAgentServer) removeKubeVipFromLoopback(runner nvkeutils.CommandRunner, vip string) error {
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
	runner nvkeutils.CommandRunner,
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
		if err := sleepOrDone(ctx, s.controlPlanePollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

func (s *NodeAgentServer) closeRemoteVipPeers(runner nvkeutils.CommandRunner, vip string, port int) (int, error) {
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
	runner nvkeutils.CommandRunner,
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
		if err := sleepOrDone(ctx, s.controlPlanePollIntervalOrDefault()); err != nil {
			return err
		}
	}
}

// controlPlanePollIntervalOrDefault paces every loop that watches local
// control-plane state settle: a static pod's container exiting or coming back,
// and the VIP leaving or returning to loopback. It is not specific to kube-vip
// despite where it lives - the static-pod waits serve etcd and kube-apiserver
// too - and it is one knob rather than four because they all poll a local
// command whose cost is the same.
//
// The timeouts those loops carry bound how long they wait between commands,
// not the commands themselves: CommandRunner takes no context, so a crictl,
// ip or ss that never returns hangs its loop regardless of the deadline, and
// does so holding staticPodMutationMu. That is a property of the runner
// interface rather than of this flow - every static-pod operation in the agent
// shares it - and the only case given its own guard is the one command here
// expected to take a while, `crictl stop`, through defaultEtcdExitTimeout.
// Giving CommandRunner a context would fix it for all of them.
func (s *NodeAgentServer) controlPlanePollIntervalOrDefault() time.Duration {
	if s != nil && s.controlPlanePollInterval > 0 {
		return s.controlPlanePollInterval
	}
	return defaultControlPlanePollInterval
}

func (s *NodeAgentServer) kubeVipSocketQuietWindowOrDefault() time.Duration {
	if s != nil && s.kubeVipSocketQuietWindow > 0 {
		return s.kubeVipSocketQuietWindow
	}
	return time.Duration(defaultKubeVipSocketQuietSeconds) * time.Second
}

func (s *NodeAgentServer) staticPodStartTimeoutOrDefault() time.Duration {
	if s != nil && s.staticPodStartTimeout > 0 {
		return s.staticPodStartTimeout
	}
	return time.Duration(defaultStaticPodStartTimeout) * time.Second
}

// staticPodExitTimeoutOrDefault bounds the wait for a parked static pod's
// container to exit. It is overridable for the same reason the start timeout is:
// without it a test covering a container that will not exit has to sit through
// the whole production budget to reach the assertion.
func (s *NodeAgentServer) staticPodExitTimeoutOrDefault() time.Duration {
	if s != nil && s.staticPodExitTimeout > 0 {
		return s.staticPodExitTimeout
	}
	return time.Duration(defaultStaticPodExitTimeout) * time.Second
}

func formatVipSocketPeers(vip string, port int, peers []vipSocketPeer) string {
	parts := make([]string, 0, len(peers))
	for _, peer := range peers {
		parts = append(parts, fmt.Sprintf("%s:%d -> %s:%s", vip, port, peer.IP, peer.Port))
	}
	return strings.Join(parts, ", ")
}

func (s *NodeAgentServer) listRemoteVipPeers(runner nvkeutils.CommandRunner, vip string, port int) ([]vipSocketPeer, error) {
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
