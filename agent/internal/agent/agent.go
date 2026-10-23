// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	sqids "github.com/sqids/sqids-go"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
	"github.com/dsx-ai-factory/nvke/agent/internal/redact"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
	"github.com/dsx-ai-factory/nvke/agent/pkg/deb"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

var (
	KubeConfig = "/etc/kubernetes/admin.conf"
	// kubeadmInitConfigPath is the InitConfiguration kubeadm init ran with; upload-certs must use the same one.
	kubeadmInitConfigPath = "/tmp/kubeadm-init-config.yaml"
	//go:embed rescan_mlx5.sh
	mlx5RescanScript string
)

// Enums for example purposes
const (
	JoinPhase             = "join"
	InitPhase             = "initialize"
	ResetPhase            = "reset"
	ReadyStatus           = "ready"
	PendingStatus         = "pending"
	InstallingStatus      = "installing"
	ErrorStatus           = "error"
	SuccessStatus         = "success"
	NotReadyStatus        = "notready"
	UninitializedStatus   = "uninitialized"
	joinFailedStatus      = "join failed"
	WorkerRole            = "worker"
	ControlPlaneRole      = "controlplane"
	UserScriptsDir        = "/usr/local/nvke/user-scripts"
	UploadDir             = "/tmp/nvke_uploads"
	NVKEDir               = "/var/lib/nvke"
	StateFile             = NVKEDir + "/state.json"
	NVKEAuditLog          = "/var/log/nvke-agent-audit.log"
	NVKEExecuteScriptsLog = "/var/log/nvke-execute-scripts.log"
	// TODO: May need to adjust the following settings, but for now let's start here and expand as needed
	ScriptStatusLimit       = 3
	maxScriptLogOutputBytes = 4096
	kubeletConfigPath       = "/var/lib/kubelet/config.yaml"

	// kubeletHardenedFileMode is the mode CIS 4.1.1 and 4.1.9 require on the
	// kubelet's systemd unit files and on kubeletConfigPath. Packages install the
	// units 0644 and kubeadm writes the config 0644, which leaves them world
	// readable.
	kubeletHardenedFileMode = 0o600

	kubeSystemNamespace  = "kube-system"
	kubeadmConfigMapName = "kubeadm-config"

	kubeadmCmdInit  = "init"
	kubeadmCmdJoin  = "join"
	kubeadmCmdPhase = "phase"

	scriptStatusSuccess = "Success"

	errMsgConfigRequired = "config is required"

	preflightSeverityWarning = "warning"
	preflightSeverityError   = "error"

	localDevEnvVar = "NKE_LOCAL_DEV"
)

// kubeletServiceFilePaths are the places a kubelet systemd unit or its kubeadm
// drop-in can live. CIS 4.1.1 checks "the kubelet service file", and kube-bench
// resolves that to the first of its candidates that exists, in this order:
// /etc/systemd/system/kubelet.service.d/10-kubeadm.conf,
// /etc/systemd/system/kubelet.service, /lib/systemd/system/kubelet.service.
//
// Neither the kubeadm-distro package nor the upstream pkgs.k8s.io packages
// install anything under /etc. The package installs both files under /lib, the
// upstream packages under /usr/lib, and on a merged-/usr system those are the
// same directory. So on a packaged node the file kube-bench checks is the vendor
// unit, not the drop-in. Hardening every location that exists covers whichever
// one a node's layout makes it land on.
var kubeletServiceFilePaths = []string{
	"/etc/systemd/system/kubelet.service.d/10-kubeadm.conf",
	"/etc/systemd/system/kubelet.service",
	"/lib/systemd/system/kubelet.service",
	"/lib/systemd/system/kubelet.service.d/10-kubeadm.conf",
	"/usr/lib/systemd/system/kubelet.service",
	"/usr/lib/systemd/system/kubelet.service.d/10-kubeadm.conf",
}

// isLocalDev reports whether the agent is running under the local development
// harness, where some kubeadm SystemVerification checks cannot succeed.
func isLocalDev() bool {
	return os.Getenv(localDevEnvVar) == "true"
}

var DefaultTigeraVersion *semver.Version = semver.MustParse("3.30.1")

type NodeStatusPersister interface {
	Persist(status *NodeStatus) error
	LoadFromStateFile() (*NodeStatus, error)
}

// kubernetesClientRegistry returns Kubernetes clients, rebuilding them when needed.
type kubernetesClientRegistry interface {
	Client() (kubernetes.Interface, error)
	LocalClient() (*kubernetes.Clientset, error)
}

type NodeAgentServer struct {
	pb.UnimplementedNodeAgentServer
	Runner     nvkeutils.CommandRunner
	FileWriter nvkeutils.FileWriter
	Persister  NodeStatusPersister

	NodeStatus *NodeStatus
	mu         sync.Mutex // protects NodeStatus

	encryptionMu                   sync.Mutex          // encryptionMu serializes all encryption-config mutations on this node.
	rewriteActive                  bool                // protected by mu; true when a rewrite goroutine is active in this process.
	activeOperations               map[string]struct{} // protected by mu; in-process liveness for generic async operations.
	operationStartMu               sync.Mutex          // serializes generic operation start persistence and publish.
	agentUpgradeMu                 sync.Mutex          // serializes durable self-upgrade admission on this node.
	nodeAgentCfgMu                 sync.Mutex          // serializes NodeSpec/config.yaml writes
	openVSwitchMu                  sync.Mutex          // serializes host network configuration changes
	openVSwitchSettleDelay         time.Duration       // overrides the post-netplan settle delay in tests
	openVSwitchConvergenceDelay    time.Duration       // overrides the asynchronous convergence poll delay in tests
	reloadPollInterval             time.Duration       // overrides the 2s poll interval in tests
	drainKubeVipPollInterval       time.Duration       // overrides kube-vip drain polling in tests
	drainKubeVipSocketQuietWindow  time.Duration       // overrides kube-vip socket quiet window in tests
	staticPodStartTimeout          time.Duration       // overrides static-pod restore timeout in tests
	certificateRenewalStopTimeout  time.Duration       // overrides certificate-renewal stop timeout in tests
	initConfigPath                 string              // overrides kubeadmInitConfigPath in tests
	certificateRenewalStartTimeout time.Duration       // overrides certificate-renewal start timeout in tests
	certificateRenewalPollInterval time.Duration       // overrides certificate-renewal polling in tests
	rebootLockPollInterval         time.Duration       // overrides the package-lock poll interval in tests
	rebootLockWaitTimeout          time.Duration       // overrides the package-lock wait timeout in tests
	etcdClient                     etcdMemberClient    // overrides the local etcd client in tests
	now                            func() time.Time    // overrides wall-clock reads in tests
	clientRegistry                 kubernetesClientRegistry

	BundleServerURL string
	BundleSource    *nvkeutils.BundleSource // Parsed bundle source configuration
	BundleIndexFile string
	// CniImageRegistry is the OCI registry prefix for CNI component images.
	// When set, it is passed to the CNI helm chart as --set global.registry=<value>,
	// allowing air-gap deployments to redirect pulls to a local mirror.
	CniImageRegistry string
	// RootPath is the root filesystem path prefix used for file operations.
	// Defaults to "" (real root). Set to a temp directory for testing (chroot-like behavior).
	RootPath string
	// kubeadmFileOps overrides ownership resolution in tests.
	kubeadmFileOps fileOps
	// apiServerReadyMaxAttempts overrides the production /readyz poll count
	// used by waitForAPIServerReady (and by waitForAPIServerReadyAfterMutation
	// for its mirror-pod swap wait). Zero means "use the production
	// constant" (apiServerReadyMaxAttempts package-level default).
	apiServerReadyMaxAttempts int
	// apiServerReadyPollInterval overrides the production /readyz poll
	// interval used by waitForAPIServerReady (and by
	// waitForAPIServerReadyAfterMutation for its mirror-pod swap wait).
	// Zero means "use the production constant"
	// (apiServerReadyPollInterval package-level default).
	apiServerReadyPollInterval time.Duration
	// ScriptExecutionLogger logs ExecuteScript/StreamExecuteScript call info and results to e.g. /var/log/nvke-execute-scripts.log.
	// If nil, script execution is not logged to a dedicated file.
	ScriptExecutionLogger *slog.Logger
	// AgentUpgradeStore is separate from NodeStatus because package replacement
	// restarts this process while the independent updater continues.
	AgentUpgradeStore    *agentupgrade.Store
	AgentUpgradeLauncher agentUpgradeLauncher

	kubeletConfigMu          sync.Mutex // serializes kubelet.conf reconfiguration transactions
	kubeletConfigAtomicWrite func(string, []byte, os.FileMode) error
	kubeVipFetchASN          func(context.Context, string) (uint32, error)
	staticPodMutationMu      sync.Mutex // serializes agent-owned static-pod manifest transactions

	// SPIFFESocketPath is the SPIFFE workload API address used to read this
	// node's X.509 SVID.
	SPIFFESocketPath       string
	spireAgentMu           sync.Mutex    // serializes SPIRE agent reconfiguration
	spireAgentPollInterval time.Duration // overrides the SVID poll interval in tests
	spireAgentWaitTimeout  time.Duration // overrides the SVID wait timeout in tests
	fetchX509SVID          func(context.Context, string) (*x509svid.SVID, error)
}

// Real implementations

func generateRequestID() string {
	s, err := sqids.New()
	if err != nil {
		slog.Error("failed to setup sqids instane", "err", err)
		slog.Error("will use simeple timestamp instead")
		currentTime := time.Now()
		return currentTime.Format("20060102T150405")
	}

	timestamp := uint64(time.Now().UnixNano() / int64(time.Millisecond))
	id, err := s.Encode([]uint64{timestamp})
	if err != nil {
		slog.Error("failed to encode timestamp", "err", err)
		slog.Error("will use simeple timestamp instead")
		currentTime := time.Now()
		return currentTime.Format("20060102T150405")
	}
	return id
}

func SetNodeStatus(status *NodeStatus) error {
	// NOTE:  We could do a lot here, but for now we just want to
	// dtermine two basic things:
	// 1. Is it a controlplane role?
	// 2. Does it appear to be "ready" (initialized, joined etc)
	// We could try and pull all the info together, but there are quite a few
	// *gotchas* with updates etc and most importantly our node-names don't always
	// match which means we'd have to do name parsing via kubelet pki cert inspection.
	// In other words, it's a significant lift, not what we need *today*
	//
	//
	kubeletConf := "/etc/kubernetes/kubelet.conf"
	if _, err := os.Stat(kubeletConf); os.IsNotExist(err) {
		// If there's no kubelet config, it's "nothing"
		slog.Info("kubelet config does not exist, node is not ready and no role assigned")
		return nil
	}
	status.Status = "ready"
	// Default to worker if it has a kubelet config
	status.Role = WorkerRole

	// Now check to see if it's a controlplane...
	manifests := []string{
		"/etc/kubernetes/manifests/kube-apiserver.yaml",
		"/etc/kubernetes/manifests/kube-controller-manager.yaml",
		"/etc/kubernetes/manifests/kube-scheduler.yaml",
	}
	allExist := true
	for _, f := range manifests {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			allExist = false
			break
		}
	}
	if allExist {
		status.Role = ControlPlaneRole
	}
	return nil
}

func mlx5Rescan() error {
	// If the DPU has not progressed far enough in booting the DPU ARM OS it may not serve DHCP
	// traffic by the time we try this the first time. Retry once to give it more time to finish.
	maxAttempts := 2
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			slog.Info("retrying mlx5 rescan script", "attempt", attempt, "max_attempts", maxAttempts)
		} else {
			slog.Info("executing mlx5 rescan script, detailed logs: /var/log/mlx5-rescan.log")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

		cmd := exec.CommandContext(ctx, "/bin/bash", "-c", mlx5RescanScript)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if ctx.Err() == context.DeadlineExceeded {
					slog.Error("mlx5Rescan script timeout after 5 minutes", "attempt", attempt)
					slog.Error("check /var/log/mlx5-rescan.log for detailed progress")
					lastErr = fmt.Errorf("mlx5RescanScript timed out %w", err)
					cancel()
					continue
				}
				exitCode := exitErr.ExitCode()
				// Exit code 60 means reset was performed successfully
				if exitCode == 60 {
					cancel()
					slog.Info("mlx5 rescan successful - network interfaces were reset", "output", string(output))
					slog.Info("Network interfaces were reset, restarting kubelet service")
					ctxRestart, cancelRestart := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancelRestart()
					restartCmd := exec.CommandContext(ctxRestart, "systemctl", "restart", kubeletServiceName)
					if restartOutput, restartErr := restartCmd.CombinedOutput(); restartErr != nil {
						slog.Error("failed to restart kubelet service", "err", restartErr, "output", string(restartOutput))
						// Don't return error here - the rescan was successful, kubelet restart is best-effort
					} else {
						slog.Info("kubelet service restarted successfully")
					}
					return nil
				}
				// Any other non-zero exit code is a failure
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					slog.Error("mlx5 rescan failed", "output", string(output), "exit code", status.ExitStatus(), "attempt", attempt)
					slog.Error("see /var/log/mlx5-rescan.log for detailed error information")
					lastErr = err
					cancel()
					continue
				}
			}
			slog.Error("mlx5 rescan failed", "output", string(output), "exit code", "unknown", "attempt", attempt)
			slog.Error("see /var/log/mlx5-rescan.log for detailed error information")
			lastErr = fmt.Errorf("failed to run script: %w", err)
			cancel()
			continue
		}

		// Exit code 0 means network interfaces were already available (no reset needed)
		cancel()
		slog.Info("mlx5 rescan successful, no action needed", "output", string(output))
		return nil
	}

	slog.Error("mlx5 rescan failed after all attempts", "attempts", maxAttempts, "last_error", lastErr)
	return lastErr
}

func (s *NodeAgentServer) setIndexURL(indexFileURL string) error {
	if indexFileURL != "" {
		s.BundleIndexFile = indexFileURL
		return nil
	}

	if s.BundleServerURL == "" {
		// Bundle source is optional (apt repos are preferred). Leave the index unset.
		return nil
	}

	rel, err := nvkeutils.GetBundleIndexPath()
	if err != nil {
		return fmt.Errorf("failed GetBundleIndexPath, can't set bundle index: %w", err)
	}

	s.BundleIndexFile = nvkeutils.JoinBundleURL(s.BundleServerURL, rel)
	return nil
}

func NewNodeAgentServer(doMlx5Rescan bool, bundleServerURL, indexFileURL, cniImageRegistry string) *NodeAgentServer {
	slog.Info("initializing node agent server")
	// Ensure the directory exists
	if err := os.MkdirAll(NVKEDir, 0o755); err != nil {
		slog.Error("failed to create the agent directory", "dir", NVKEDir, "err", err)
	}

	// Ensure the state file exists, but do NOT truncate if it's already there
	f, err := os.OpenFile(StateFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			slog.Error("failed to create state file", "err", err)
		}
	} else {
		// File was created just now — close immediately
		_ = f.Close()
	}

	// Bundle source is optional. Prefer apt repos; keep HTTP/OCI bundle install when configured.
	var bundleSource *nvkeutils.BundleSource
	parsedBundleURL := ""
	if strings.TrimSpace(bundleServerURL) == "" {
		slog.Warn("bundle-server-url not set; InstallBundle and InstallCollection(.json) are unavailable until configured — prefer apt repos via ConfigureAptRepo/InstallPackages")
	} else {
		var parseErr error
		bundleSource, parseErr = nvkeutils.ParseBundleSourceURI(bundleServerURL)
		if parseErr != nil {
			slog.Warn("failed to parse bundle source URL; continuing without bundle support", "url", bundleServerURL, "err", parseErr)
		} else {
			parsedBundleURL = bundleSource.URL
			slog.Info("parsed bundle source", "type", bundleSource.Type.String(), "url", bundleSource.URL, "originalURI", bundleSource.OriginalURI)
		}
	}

	clientRegistry := k8s.NewClientRegistry(KubeConfig)
	runner := &nvkeutils.ExecRunner{}
	server := &NodeAgentServer{
		Runner:            runner,
		FileWriter:        &nvkeutils.OSFileWriter{},
		Persister:         &JSONFilePersister{Path: StateFile},
		AgentUpgradeStore: agentupgrade.NewStore(agentupgrade.DefaultRoot),
		AgentUpgradeLauncher: systemdAgentUpgradeLauncher{
			runner: runner,
		},
		BundleSource:     bundleSource,
		BundleServerURL:  parsedBundleURL,
		CniImageRegistry: cniImageRegistry,
		kubeadmFileOps:   newDefaultFileOps(),
		clientRegistry:   clientRegistry,
		NodeStatus: &NodeStatus{
			AgentVersion:  "",
			NodeName:      nvkeutils.GetHostname(),
			Status:        UninitializedStatus,
			KubeadmStatus: "",
		},
	}

	slog.Info("calling setIndexURL with argument", "INDEX_FILE_URL", indexFileURL)
	if err := server.setIndexURL(indexFileURL); err != nil {
		slog.Warn("failed to set index url for bundle server; continuing without bundle index", "err", err)
	}

	state, err := server.Persister.LoadFromStateFile()
	if err != nil {
		slog.Error("failed to load state file", "err", err)
		if err := SetNodeStatus(server.NodeStatus); err != nil {
			slog.Error("failed to set k8s status", "error", err)
			slog.Error("status may be incorrect, but agent will start")
		}
	} else {
		server.NodeStatus = state
		// This normalizes any old syntax for things like ControlPlane back to controlplane etc
		server.UpdateState()

		// If this node has already completed join or init, ensure registerNode: false
		// is set on disk. This covers: (1) agent upgrades on existing nodes,
		// (2) kubeadm upgrade wiping the flag, (3) any other config reset.
		if slices.Contains(state.CompletedPhases, JoinPhase) || slices.Contains(state.CompletedPhases, InitPhase) {
			if err := server.disableKubeletSelfRegistration(); err != nil {
				slog.Error("startup: failed to disable kubelet self-registration", "err", err)
			}
		}
	}

	// Deliberately outside the branch above, and ungated by CompletedPhases.
	// disableKubeletSelfRegistration needs the state file to know whether the
	// node ever initialized, because rewriting a config that is not there would
	// be wrong. Tightening a mode needs no such knowledge: a missing file is a
	// no-op, so the safe thing on an unreadable state file is still to run. An
	// initialized node whose state file will not load would otherwise keep
	// kubeadm's 0644 with nothing left to correct it.
	if err := server.hardenKubeletFileModes(); err != nil {
		slog.Error("startup: failed to tighten kubelet file modes", "err", err)
	}

	server.NodeStatus.BundleIndexFile = server.BundleIndexFile
	server.NodeStatus.BundleServerURL = server.BundleServerURL
	server.NodeStatus.Architecture = nvkeutils.GetMachineArchitecture()

	if doMlx5Rescan {
		server.NodeStatus.MLX5ResetDisabled = false
		server.NodeStatus.MLX5ResetStatus = true
		err = mlx5Rescan()
		if err != nil {
			slog.Error("failed to run mlx5 rescan", "err", err)
			server.NodeStatus.MLX5ResetStatus = false
		}
	} else {
		slog.Info("mlx5 rescan is disabled")
		server.NodeStatus.MLX5ResetDisabled = true
		server.NodeStatus.MLX5ResetStatus = false
	}

	// Persist any updates/changes back to the state file
	if err := server.Persister.Persist(server.NodeStatus); err != nil {
		slog.Error("failed to persist initial status", "err", err)
	}

	return server
}

func getShortKubeadmVersion() (string, error) {
	// Check if kubeadm executable exists
	if _, err := exec.LookPath("kubeadm"); err != nil {
		return "", fmt.Errorf("kubeadm not found")
	}
	// Get kubeadm version
	cmd := exec.Command("kubeadm", "version", "-o", "short")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	// Trim any trailing newline characters
	out = bytes.TrimSpace(out)
	return string(out), nil
}

// canonicalKubernetesVersion normalizes Kubernetes versions for kubeadm --kubernetes-version / ClusterConfiguration.
func canonicalKubernetesVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "v" + s
}

func extraArgsContainKubernetesVersion(extra []string) bool {
	for _, a := range extra {
		a = strings.TrimSpace(a)
		if strings.HasPrefix(a, "--kubernetes-version=") {
			return true
		}
	}
	return false
}

// pinnedKubernetesVersionForCLIInit resolves --kubernetes-version when kubeadm init runs without init_config.
func pinnedKubernetesVersionForCLIInit(req *pb.KubeadmInitRequest) string {
	if strings.TrimSpace(req.GetInitConfig()) != "" {
		return ""
	}
	if v := canonicalKubernetesVersion(req.GetKubernetesVersion()); v != "" {
		return v
	}
	kVer, err := getShortKubeadmVersion()
	if err != nil {
		slog.Warn("could not resolve kubeadm binary version for --kubernetes-version pin", "error", err)
		return ""
	}
	return canonicalKubernetesVersion(kVer)
}

// Server logic
func (s *NodeAgentServer) ExecuteKubeadmJoin(
	ctx context.Context,
	req *pb.KubeadmJoinRequest,
) (*pb.KubeadmJoinStatusResponse, error) {
	defer logRPCSpan(ctx, "ExecuteKubeadmJoin")()
	response := &pb.KubeadmJoinStatusResponse{
		Status: PendingStatus,
		Phase:  JoinPhase,
	}

	slog.Info("issuing kubeadm join call", "request", loggableJoinRequest(req),
		"api_encryption_key_sha256", encryptionKeyDigest(req.GetApiEncryptionKey()))

	alreadyJoining := (s.NodeStatus.Phase == JoinPhase) &&
		(s.NodeStatus.Status == ReadyStatus || s.NodeStatus.Status == PendingStatus)

	if alreadyJoining {
		slog.Info("node already joined or joining, ignoring")
		response.Status = ErrorStatus
		response.ErrorMessage = "node is already joined or pending join"
		return response, nil
	}

	role := WorkerRole
	if strings.Contains(req.KubeadmJoinString, "--control-plane") {
		role = ControlPlaneRole
	}

	// Just override the above check if the request has a join config
	if req.JoinConfig != "" && strings.Contains(req.JoinConfig, "controlPlane:") {
		role = ControlPlaneRole
	}
	response.Role = role

	s.mu.Lock()
	s.NodeStatus.Phase = JoinPhase
	s.NodeStatus.Status = PendingStatus
	s.NodeStatus.Role = role
	s.NodeStatus.CurrentMessage = "Joining node"
	statusCopy := *s.NodeStatus
	s.mu.Unlock()

	// Persist what we have so far
	s.Persister.Persist(&statusCopy) //nolint:all

	args := strings.Fields(req.KubeadmJoinString) //[1:]
	slog.Info("issuing kubeadm join command", "args", redact.Args(args))
	go s.processKubeadmJoin(args, req)
	return response, nil
}

func (s *NodeAgentServer) processKubeadmJoin(args []string, req *pb.KubeadmJoinRequest) {
	var output []byte
	var err error

	s.mu.Lock()
	ns := *s.NodeStatus
	s.mu.Unlock()
	defer s.flushStatus(&ns)

	// A failed join leaves bootstrap-kubelet.conf and a half-built control
	// plane behind, and kubeadm's preflight refuses to join over them.
	if ns.KubeadmStatus == joinFailedStatus {
		s.resetBeforeJoinRetry()
	}

	if err := s.writeFilesToDisk(req.GetFiles()); err != nil {
		slog.Error("failed to write kubeadm prerequisite files", "err", err)
		ns.Status = ErrorStatus
		ns.ErrorMessage = "Failed to write kubeadm prerequisite files: " + err.Error()
		ns.CurrentMessage = ns.ErrorMessage
		ns.Phase = JoinPhase
		return
	}

	// If the node role is controlPlane check for encryption settings
	if s.NodeStatus.Role == ControlPlaneRole && req.ApiEncryptionKey != "" {
		err = s.createAPIEncryptionConfig(req.ApiEncryptionKey, false, nil)
		if err != nil {
			slog.Error("failed to write config", "err", err)
			ns.Status = ErrorStatus
			ns.Phase = JoinPhase
			return
		}
	}

	if req.JoinConfig != "" {
		err = s.FileWriter.WriteFile("/tmp/kubeadm-join-config.yaml", []byte(req.JoinConfig), 0o644)
		if err != nil {
			slog.Error("failed to write config", "err", err)
			ns.Status = ErrorStatus
			ns.Phase = JoinPhase
			return
		}
		joinArgs := []string{"join", "--config", "/tmp/kubeadm-join-config.yaml"} //nolint:goconst // kubeadm argv fragment; naming each CLI flag reads worse than the literal
		if isLocalDev() {
			// SystemVerification checks kernel config via `modprobe configs` + /proc/config.gz.
			// On production bare-metal Ubuntu this succeeds (kernel ships with the configs module).
			// In Docker containers it fails because the container namespace cannot load kernel modules.
			// Safe to ignore: the actual kernel features kubeadm verifies (namespaces, cgroups, etc.)
			// are confirmed by the other preflight checks (CGROUPS_*, etc.) that do pass.
			joinArgs = append(joinArgs, "--ignore-preflight-errors=SystemVerification")
		}
		output, err = s.Runner.Run(
			"kubeadm",
			joinArgs,
			&nvkeutils.CommandOptions{},
		)
	} else {
		output, err = s.Runner.Run("kubeadm", args, &nvkeutils.CommandOptions{})
	}

	if err != nil {
		loggable := joinCredentials(req).String(string(output))
		slog.Error("join failed", "err", err, "output", loggable)
		ns.Status = ErrorStatus
		ns.CurrentMessage = err.Error()
		ns.KubeadmErrorMessage = loggable
		ns.KubeadmStatus = joinFailedStatus
	} else {
		ns.Status = ReadyStatus
		ns.CurrentMessage = "node successfully joined"
		ns.KubeadmErrorMessage = ""
		ns.KubeadmStatus = "join succeeded"

		if err := s.disableKubeletSelfRegistration(); err != nil {
			slog.Error("failed to disable kubelet self-registration", "err", err)
		}

		if err := s.hardenKubeletFileModes(); err != nil {
			slog.Error("failed to tighten kubelet file modes", "err", err)
		}
	}
	ns.CompletedPhases = append(ns.CompletedPhases, JoinPhase)
}

// kubeadmResetArgs is the argv every kubeadm reset in this package starts from.
func kubeadmResetArgs() []string {
	return []string{"reset", "-f"} //nolint:goconst // kubeadm argv fragment; naming each CLI flag reads worse than the literal
}

// resetBeforeJoinRetry runs kubeadm reset so a join retry starts from a clean
// node. Best effort: when the reset fails, the join runs and reports its own error.
func (s *NodeAgentServer) resetBeforeJoinRetry() {
	slog.Info("previous kubeadm join failed, resetting the node before retrying")
	output, err := s.Runner.Run("kubeadm", kubeadmResetArgs(), &nvkeutils.CommandOptions{})
	if err != nil {
		slog.Error("kubeadm reset before join retry failed", "err", err, "output", string(output))
	}
}

func (s *NodeAgentServer) ExecuteKubeadmInit(
	ctx context.Context,
	req *pb.KubeadmInitRequest,
) (*pb.KubeadmInitStatusResponse, error) {
	defer logRPCSpan(ctx, "ExecuteKubeadmInit")()
	slog.Info("issuing kubeadm init call", "request", loggableInitRequest(req),
		"api_encryption_key_sha256", encryptionKeyDigest(req.GetApiEncryptionKey()))
	response := &pb.KubeadmInitStatusResponse{}

	s.mu.Lock()
	ns := s.NodeStatus
	s.mu.Unlock()

	// Make sure it's not already initializing
	if strings.EqualFold(ns.Phase, InitPhase) && strings.EqualFold(ns.Status, PendingStatus) {
		slog.Info("node already initializing, ignoring")
		response.Status = ErrorStatus
		response.ErrorMessage = "node is already initializing"
		return response, nil
	}

	// We process args here before calling the goroutine to processKubeadmInit because
	// we want to fail fast; we don't want to launch the go routine and return only to
	// find out the required args weren't passed
	args := []string{}
	if req.GetInitConfig() == "" {
		slog.Info("no init config provided. Using arguments")
		if req.PodNetworkCidr == "" {
			response.ErrorMessage = "PodNetworkCidr is required"
			response.Status = ErrorStatus

			return response, nil
		}
		if req.ControlPlaneEndpoint == "" {
			response.ErrorMessage = "ControlPlaneEndpoint is required"
			response.Status = ErrorStatus

			return response, nil
		}

		args = []string{
			"init",
			fmt.Sprintf("--pod-network-cidr=%s", kubeadmPodNetworkCIDR(req.PodNetworkCidr)),
			fmt.Sprintf("--control-plane-endpoint=%s", req.ControlPlaneEndpoint),
			"--upload-certs", //nolint:goconst // kubeadm argv fragment; naming each CLI flag reads worse than the literal
		}

		if req.ServiceCidr != "" {
			args = append(args, fmt.Sprintf("--service-cidr=%s", req.ServiceCidr))
		}

		if req.ExtraApiCertSans != "" {
			args = append(args, fmt.Sprintf("--apiserver-cert-extra-sans=%s", req.ExtraApiCertSans))
		}

		if req.ExtraArgs != nil {
			args = append(args, req.ExtraArgs...)
		}

		if !extraArgsContainKubernetesVersion(req.ExtraArgs) {
			if v := pinnedKubernetesVersionForCLIInit(req); v != "" {
				args = append(args, "--kubernetes-version="+v)
			}
		}
	}

	// The legacy in-init CNI install fetches its bundle by index, so with no
	// bundle source it cannot succeed. Refuse before the goroutine starts:
	// kubeadm has not run yet, so recovery is a restart with the flag set rather
	// than a kubeadm reset. InstallBundle and InstallCollection already refuse
	// the same way; this is the third consumer of the same configuration.
	//
	// A gRPC error rather than an ErrorStatus response, because the caller is
	// `if _, err := ExecuteKubeadmInit(...)` and discards the response. A
	// response would be dropped and the caller would poll a node that never
	// changes state. The argument checks above have that shape and are
	// invisible for the same reason.
	if !req.GetNoCniInstall() && !req.GetSkipCniPluginInstall() && s.BundleIndexFile == "" {
		return nil, status.Error(codes.FailedPrecondition, bundleIndexUnconfiguredMessage(s.BundleServerURL))
	}

	s.mu.Lock()
	s.NodeStatus.Role = ControlPlaneRole
	s.NodeStatus.IsMaster = true
	s.NodeStatus.Phase = InitPhase
	s.NodeStatus.Status = PendingStatus
	s.NodeStatus.CurrentMessage = "Node is initializing"
	statusCopy := *s.NodeStatus
	s.mu.Unlock()

	resp := &pb.KubeadmInitStatusResponse{
		Status: s.NodeStatus.Status,
		Phase:  s.NodeStatus.Phase,
		Role:   s.NodeStatus.Role,
	}

	// Persist what we have so far
	s.Persister.Persist(&statusCopy) //nolint:all

	slog.Info("executing kubeadm init")
	go s.processKubeadmInit(args, req)

	return resp, nil
}

// kubeadmFailureMessage summarizes why kubeadm exited, for the one status field
// the orchestrator can see. kubeadm ends with the phase it failed in
// ("error execution phase wait-control-plane: ..."), which is the single most
// useful line, so prefer it and fall back to the process error.
func kubeadmFailureMessage(err error, output []byte, credentials *redact.Credentials) string {
	const limit = 300
	best := ""
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "error") || strings.Contains(line, "error execution phase") {
			best = line
		}
	}
	if best == "" {
		best = strings.TrimSpace(err.Error())
	}
	// Redact before truncating: cutting a credential short would leave part of it
	// in a form that is no longer recognizable.
	best = credentials.String(best)
	if len(best) > limit {
		best = best[:limit] + "..."
	}
	return "kubeadm init failed: " + best
}

// processKubeadmInit runs kubeadm init with args and records the outcome in the node status.
func (s *NodeAgentServer) processKubeadmInit(args []string, req *pb.KubeadmInitRequest) {
	s.mu.Lock()
	ns := *s.NodeStatus
	s.mu.Unlock()
	defer s.flushStatus(&ns)

	if err := s.writeFilesToDisk(req.GetFiles()); err != nil {
		slog.Error("failed to write kubeadm prerequisite files", "err", err)
		ns.Status = ErrorStatus
		ns.ErrorMessage = "failed to write kubeadm prerequisite files: " + err.Error()
		return
	}

	if req.GetInitConfig() != "" {
		slog.Info("using custom kubeadm init config")
		configPath := s.initConfigFile()
		if err := s.FileWriter.WriteFile(configPath, []byte(req.GetInitConfig()), 0o644); err != nil {
			ns.Status = ErrorStatus
			ns.ErrorMessage = "failed to write init config file"
			return
		}
		// If it turns out we're using a config file, just overwrite the args that were built
		args = []string{"init", "--config", configPath, "--upload-certs"}
		if isLocalDev() {
			// SystemVerification checks kernel config via `modprobe configs` + /proc/config.gz.
			// On production bare-metal Ubuntu this succeeds (kernel ships with the configs module).
			// In Docker containers it fails because the container namespace cannot load kernel modules.
			// Safe to ignore: the actual kernel features kubeadm verifies (namespaces, cgroups, etc.)
			// are confirmed by the other preflight checks (CGROUPS_*, etc.) that do pass.
			args = append(args, "--ignore-preflight-errors=SystemVerification")
		}
	} else if err := os.Remove(s.initConfigFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A config left by an earlier custom init would otherwise be handed to upload-certs.
		ns.Status = ErrorStatus
		ns.ErrorMessage = "failed to remove stale init config file: " + err.Error()
		return
	}

	// Before we run init, check to see if encryption is requested and set up the encryption-config file
	if req.ApiEncryptionEnabled && req.ApiEncryptionKey != "" {
		if err := s.createAPIEncryptionConfig(req.ApiEncryptionKey, false, nil); err != nil {
			ns.Status = ErrorStatus
			ns.ErrorMessage = "Failed to initialize API encryption: " + err.Error()
			return
		}
	}

	output, err := s.Runner.Run("kubeadm", args, &nvkeutils.CommandOptions{})
	if err != nil {
		if strings.Contains(string(output), "FileAvailable") &&
			strings.Contains(string(output), "is in use") {
			slog.Info("already initialized, kubeadm returned 'in use'")
			return
		}
		loggable := initCredentials(req).String(string(output))
		slog.Error("init failed", "err", err, "stdout", loggable)
		ns.Status = ErrorStatus
		ns.ErrorMessage = "kubeadm init failed: " + err.Error()
		ns.KubeadmErrorMessage = loggable
		ns.KubeadmStatus = "init failed"
		// CurrentMessage is the only one of these the status RPC returns, so
		// leaving it at "Node is initializing" reports the failure as progress:
		// the orchestrator prints "kubeadm init failed: Node is initializing",
		// which names neither the phase nor the reason.
		ns.CurrentMessage = kubeadmFailureMessage(err, output, initCredentials(req))
		return
	}

	if output == nil {
		slog.Error("init failed, output for parseJoinCmds is nil", "err", err)
		ns.Status = ErrorStatus
		ns.ErrorMessage = "failed to parse join strings after kubeadm init"
		return
	}
	cpJoin, workerJoin := parseJoinCmds(string(output))
	if cpJoin == "" && workerJoin == "" {
		loggable := initCredentials(req).String(string(output))
		slog.Error("failed to parse join strings from output", "output", loggable)
		ns.Status = ErrorStatus
		ns.ErrorMessage = "failed to parse join strings from output of kubeadm init: " + loggable
		return
	}

	ns.Status = ReadyStatus
	ns.Phase = InitPhase
	ns.CurrentMessage = "Node is initialized"
	ns.JoinInfo = JoinInfo{
		ControlPlaneJoinString: cpJoin,
		WorkerJoinString:       workerJoin,
	}
	ns.CompletedPhases = append(ns.CompletedPhases, string(InitPhase))

	// Tighten here as well as after disableKubeletSelfRegistration below: kubeadm
	// has already written both files 0644 by this point, and the CNI install in
	// between can return early on failure, which would leave them world readable
	// until something else happened to run. Cheap to repeat -- a mode that is
	// already 0600 is skipped.
	if err := s.hardenKubeletFileModes(); err != nil {
		slog.Error("failed to tighten kubelet file modes", "err", err)
	}

	// Legacy in-init CNI: when no_cni_install is false (the default), install via InstallCNIPlugin
	// to match pre–InstallCNI callers. New callers set no_cni_install and use InstallCNI after init.
	if !req.GetNoCniInstall() && !req.GetSkipCniPluginInstall() {
		cniReq := installCNIPluginRequestFromKubeadmInit(req)
		if err := s.installCNIPlugin(cniReq); err != nil {
			slog.Error("failed to install CNI plugin", "err", err)
			ns.CNIPluginStatus = ErrorStatus
			ns.ErrorMessage = "failed to install CNI plugin: " + err.Error()
			// CurrentMessage too: ErrorMessage is not on the wire, and the line
			// above set CurrentMessage to "Node is initialized", so a caller
			// reading status sees that as the cause of the CNI failure.
			ns.CurrentMessage = "failed to install CNI plugin: " + err.Error()
			return
		}
		ns.CNIPlugin = cniReq.GetPluginName()
		ns.CNIPluginVersion = resolveCNIPluginVersion(cniReq)
		ns.CNIPluginStatus = ReadyStatus
	}

	if err := s.disableKubeletSelfRegistration(); err != nil {
		slog.Error("failed to disable kubelet self-registration", "err", err)
	}

	if err := s.hardenKubeletFileModes(); err != nil {
		slog.Error("failed to tighten kubelet file modes", "err", err)
	}

	ns.KubeadmErrorMessage = ""
	ns.KubeadmStatus = "init succeeded"
	slog.Info("kubeadm init process completed successfully")
}

// hardenKubeletFileModes tightens the kubelet's systemd unit files and its
// config file to 0600 (CIS 4.1.1 and 4.1.9). The units come from a package and
// the config from kubeadm, both at 0644, and kubeadm offers no configuration
// for the mode of either, so the only way to hold them at 0600 is to re-apply.
//
// The kubeadm-distro package ships its units 0600 already, so on an NVKE image
// this only has work to do on config.yaml. The units are still listed for nodes
// built from other packages, such as the upstream pkgs.k8s.io ones.
//
// Called from four places: agent startup, straight after a successful kubeadm
// init, and after each of the two disableKubeletSelfRegistration calls on the
// init and join paths. kubeadm rewrites config.yaml on init, on join and on
// upgrade, and a package upgrade reinstalls the units at the package's own
// mode, so a one-shot chmod during initial provisioning would silently drift
// back to 0644; the startup call is what corrects that drift on an existing
// node.
//
// The ordering against disableKubeletSelfRegistration matters and is not
// incidental: that function rewrites config.yaml with mode 0644, so hardening
// has to follow it rather than precede it or the mode is immediately undone.
//
// A missing file is not an error. A node carries only the unit locations its
// packages use, and on a fresh node this runs at startup before init or join,
// when config.yaml is not there yet either.
func (s *NodeAgentServer) hardenKubeletFileModes() error {
	var errs []error

	for _, rel := range append(slices.Clone(kubeletServiceFilePaths), kubeletConfigPath) {
		path := filepath.Join(s.RootPath, rel)

		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", path, err))
			continue
		}

		// Skip the chmod when it would be a no-op, so a healthy node does not
		// log or syscall on every agent restart.
		if info.Mode().Perm() == kubeletHardenedFileMode {
			continue
		}

		if err := os.Chmod(path, kubeletHardenedFileMode); err != nil {
			errs = append(errs, fmt.Errorf("chmod %s: %w", path, err))
			continue
		}

		slog.Info("tightened kubelet file mode",
			"path", path,
			"from", fmt.Sprintf("%#o", info.Mode().Perm()),
			"to", fmt.Sprintf("%#o", os.FileMode(kubeletHardenedFileMode)))
	}

	return errors.Join(errs...)
}

// disableKubeletSelfRegistration sets registerNode: false in the kubelet config
// on disk so that if the instance is later terminated and restarted outside of
// NVKE's control, the kubelet will not re-register the node (zombie prevention).
// The running kubelet is unaffected -- it already registered the node and keeps
// its in-memory config. The on-disk change only matters on next boot.
func (s *NodeAgentServer) disableKubeletSelfRegistration() error {
	path := filepath.Join(s.RootPath, kubeletConfigPath)

	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("failed to read kubelet config", "err", err, "path", path)
		return fmt.Errorf("read kubelet config: %w", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		slog.Error("failed to parse kubelet config", "err", err)
		return fmt.Errorf("parse kubelet config: %w", err)
	}

	config["registerNode"] = false

	out, err := yaml.Marshal(config)
	if err != nil {
		slog.Error("failed to marshal kubelet config", "err", err)
		return fmt.Errorf("marshal kubelet config: %w", err)
	}

	if err := s.FileWriter.WriteFile(path, out, 0o644); err != nil {
		slog.Error("failed to write kubelet config", "err", err, "path", path)
		return fmt.Errorf("write kubelet config: %w", err)
	}

	slog.Info("disabled kubelet self-registration", "path", path)
	return nil
}

func (s *NodeAgentServer) InstallCNI(
	ctx context.Context,
	req *pb.InstallCNIRequest,
) (*pb.InstallCNIResponse, error) {
	resp := &pb.InstallCNIResponse{}
	defer logRPCSpan(ctx, "InstallCNI")()

	if req.GetCniAptRole() == pb.CniAptRole_CNI_APT_ROLE_IMAGES {
		return resp, status.Errorf(codes.InvalidArgument, "%v", errCNIBadRequestImagesRole)
	}

	if req.GetSkipApt() {
		slog.DebugContext(ctx, "InstallCNI: skip_apt is deprecated and ignored (InstallCNI never runs apt)")
	}

	chartsPkgSpec, specErr := nvkeCniChartsDebLogicalSpec(req)
	if specErr != nil {
		return resp, status.Errorf(codes.InvalidArgument, "%v", specErr)
	}

	slog.InfoContext(ctx, "InstallCNI",
		"cni_variant", req.GetCniVariant().String(),
		"deprecated_cni_apt_role", req.GetCniAptRole().String(),
		"version", req.GetVersion(),
		"resolved_charts_package", chartsPkgSpec,
	)

	s.mu.Lock()
	ns := *s.NodeStatus
	s.mu.Unlock()
	defer s.flushStatus(&ns)

	if err := s.installCNI(ctx, req, chartsPkgSpec); err != nil {
		slog.Error("InstallCNI failed", "err", err)
		ns.CNIPluginStatus = ErrorStatus
		ns.ErrorMessage = "InstallCNI: " + err.Error()
		resp.ErrorMessage = err.Error()
		switch {
		case errors.Is(err, errCNIApplyRequiresControlPlane):
			return resp, status.Errorf(codes.InvalidArgument, "%v", err)
		case errors.Is(err, errCNIChartsDebNotInstalled):
			return resp, status.Errorf(codes.FailedPrecondition, "%v", err)
		case errors.Is(err, errOvnRequiresHostOpenVSwitch):
			return resp, status.Errorf(codes.FailedPrecondition, "%v", err)
		default:
			return resp, status.Errorf(codes.Internal, "%v", err)
		}
	}
	slog.InfoContext(ctx, "InstallCNI completed successfully", "charts_pkg_spec", chartsPkgSpec)

	binPkg := aptBinaryPackageName(chartsPkgSpec)
	ns.CNIPlugin = binPkg
	if v, err := dpkgInstalledVersion(ctx, binPkg); err == nil {
		ns.CNIPluginVersion = v
	}
	ns.CNIPluginStatus = ReadyStatus
	resp.Status = ReadyStatus
	resp.ResolvedAptPackage = chartsPkgSpec
	return resp, nil
}

// resolveCNIPluginVersion returns the version that was (or would be) used for installation.
// Used for status persistence so we record the actual version installed.
func resolveCNIPluginVersion(req *pb.InstallCNIPluginRequest) string {
	switch req.GetPluginName() {
	case cniPluginOVN:
		if v := req.GetPluginVersion(); v != "" {
			return v
		}
		return defaultOvnCniChartStream
	case cniPluginTigera, cniPluginCalico:
		if v := req.GetPluginVersion(); v != "" {
			return v
		}
		return DefaultTigeraVersion.String()
	default:
		return defaultOvnCniChartStream
	}
}

// installCNIPluginRequestFromKubeadmInit maps KubeadmInitRequest fields to InstallCNIPluginRequest for
// the legacy in-init CNI path.
func installCNIPluginRequestFromKubeadmInit(req *pb.KubeadmInitRequest) *pb.InstallCNIPluginRequest {
	cniReq := &pb.InstallCNIPluginRequest{
		PluginName:                   req.GetCniPluginName(),
		TigeraOperatorManifest:       req.GetTigeraOperatorManifest(),
		TigeraCustomResourceManifest: req.GetTigeraCustomResourceManifest(),
		PodNetworkCidr:               req.GetPodNetworkCidr(),
		ServiceCidr:                  req.GetServiceCidr(),
		CniConfig:                    req.GetCniConfig(),
	}
	switch req.GetCniPluginName() {
	case cniPluginOVN:
		cniReq.PluginVersion = req.GetOvnVersion()
		if cniReq.PluginVersion == "" {
			cniReq.PluginVersion = defaultOvnCniChartStream
		}
	case cniPluginTigera, cniPluginCalico:
		cniReq.PluginVersion = req.GetTigeraVersion()
	default:
		cniReq.PluginName = cniPluginOVN
		cniReq.PluginVersion = req.GetOvnVersion()
		if cniReq.PluginVersion == "" {
			cniReq.PluginVersion = defaultOvnCniChartStream
		}
	}
	return cniReq
}

func (s *NodeAgentServer) installCNIPlugin(req *pb.InstallCNIPluginRequest) error {
	switch req.GetPluginName() {
	case cniPluginOVN:
		slog.Info("installing ovn")
		return s.installOvnK8sCNIPlugin(req)
	case cniPluginTigera, cniPluginCalico:
		slog.Info("installing calico")
		versionStr := req.GetPluginVersion()
		if versionStr == "" {
			versionStr = DefaultTigeraVersion.String()
		}
		version, err := semver.NewVersion(versionStr)
		if err != nil {
			return fmt.Errorf("failed to parse tigera version: %w", err)
		}
		return s.installCalico(version, req.GetTigeraOperatorManifest(), req.GetTigeraCustomResourceManifest())
	default:
		return fmt.Errorf("unsupported CNI plugin %q", req.GetPluginName())
	}
}

func parseJoinCmds(s string) (string, string) {
	slog.Debug("executing parseJoinCmds", "input", redact.New().String(s))
	strNormailized := regexp.MustCompile("\r\n").ReplaceAllString(s, "\n")
	parsed := regexp.MustCompile(`\n\s*\n`).Split(strNormailized, -1)

	cpJoin := ""
	workerJoin := ""
	for _, v := range parsed {
		if strings.Contains(v, "kubeadm join") && strings.Contains(v, "--control-plane") {
			cpJoin = removeJoinStringDelimeters(v)
		}
		if strings.Contains(v, "kubeadm join") && !strings.Contains(v, "--control-plane") {
			workerJoin = removeJoinStringDelimeters(v)
		}
	}
	return cpJoin, workerJoin
}

func removeJoinStringDelimeters(s string) string {
	parsedStr := strings.ReplaceAll(s, "\n", "")
	parsedStr = strings.ReplaceAll(parsedStr, "\t", "")
	parsedStr = strings.ReplaceAll(parsedStr, "\\", "")
	return parsedStr
}

func (s *NodeAgentServer) GetKubeadmStatus(
	ctx context.Context,
	req *pb.KubeadmStatusRequest,
) (*pb.KubeadmStatusResponse, error) {
	defer logRPCSpan(ctx, "GetKubeadmStatus")()
	slog.Info("GetKubeadmStatus called")
	s.mu.Lock()
	statusCopy := *s.NodeStatus
	s.mu.Unlock()
	slog.Info("Returning KubeadmStatus", "status", statusCopy.Status, "phase", statusCopy.Phase, "cniInstallStatus", statusCopy.CNIPluginStatus, "currentMessage", statusCopy.CurrentMessage, "completedPhases", statusCopy.CompletedPhases, "agentVersion", statusCopy.AgentVersion)
	return &pb.KubeadmStatusResponse{
		KubeadmStatus:          statusCopy.Status,
		KubeadmPhase:           statusCopy.Phase,
		CniInstallStatus:       statusCopy.CNIPluginStatus,
		KubeadmCurrentMessage:  statusCopy.CurrentMessage,
		KubeadmCompletedPhases: statusCopy.CompletedPhases,
		AgentVersion:           statusCopy.AgentVersion,
	}, nil
}

func (s *NodeAgentServer) ExecuteKubeadmReset(
	ctx context.Context,
	req *pb.KubeadmResetRequest,
) (*pb.KubeadmResetStatusResponse, error) {
	defer logRPCSpan(ctx, "ExecuteKubeadmReset")()
	slog.Info("executing kubeadm reset")

	defer s.Persister.Persist(s.NodeStatus) //nolint:all
	s.NodeStatus.Phase = ResetPhase

	cmdArgs := kubeadmResetArgs()

	// Append --skip-phases=<name> for each entry in the request. Used by the
	// controller CP-teardown path to skip remove-etcd-member, which would otherwise hang in
	// kubeadm's etcd MemberRemove retry loop (~2 min budget) when the local etcd
	// has already been kicked out of the cluster's raft quorum.
	for _, phase := range req.SkipPhases {
		if phase == "" {
			continue
		}
		cmdArgs = append(cmdArgs, "--skip-phases="+phase)
	}

	// Upstream kubeadm treats flags and config as composable.
	// CLI flags like --force / --skip-phases take precedence over config fields where both exist.
	if req.ResetConfig != "" {
		slog.Info("using custom kubeadm reset config")
		err := os.WriteFile("/tmp/kubeadm-reset-config.yaml", []byte(req.ResetConfig), 0o644)
		if err != nil {
			slog.Error("failed to write kubeadm reset config", "err", err)
			s.NodeStatus.Status = ErrorStatus
			return nil, err
		}
		cmdArgs = append(cmdArgs, "--config", "/tmp/kubeadm-reset-config.yaml")
	}

	start := time.Now()
	output, err := s.Runner.Run("kubeadm", cmdArgs, &nvkeutils.CommandOptions{})
	slog.Info("kubeadm reset finished", "duration", time.Since(start).String(), "cmdArgs", cmdArgs, "err", err, "output", string(output))
	s.mu.Lock()
	if err != nil {
		slog.Error("failed to reset node", "err", err, "output", string(output))
		s.NodeStatus.Status = ErrorStatus
		s.NodeStatus.CurrentMessage = err.Error()
	} else {
		s.NodeStatus.Status = "reset"
		s.NodeStatus.CNIPluginStatus = ""
		s.NodeStatus.CurrentMessage = "node successfully reset/deleted"
		s.NodeStatus.CompletedPhases = append(s.NodeStatus.CompletedPhases, string(ResetPhase))
	}
	s.mu.Unlock()

	err = cleanupCNIFiles()
	if err != nil {
		slog.Error("failed to cleanup CNI files", "err", err)
		s.mu.Lock()
		s.NodeStatus.CNIPluginStatus = ErrorStatus
		s.NodeStatus.CurrentMessage = err.Error()
		s.mu.Unlock()
	}

	return &pb.KubeadmResetStatusResponse{}, nil
}

func (s *NodeAgentServer) ExecuteKubeadmPreflight(
	ctx context.Context,
	req *pb.KubeadmPreflightRequest,
) (*pb.KubeadmPreflightResponse, error) {
	defer logRPCSpan(ctx, "ExecuteKubeadmPreflight")()
	slog.Info("executing kubeadm preflight", "role", req.Role)

	if req.Config == "" {
		return &pb.KubeadmPreflightResponse{
			Status:       ErrorStatus,
			ErrorMessage: errMsgConfigRequired,
		}, nil
	}

	resp, err := s.runKubeadmPreflight(ctx, req.GetConfig(), req.GetRole())
	if err != nil {
		return nil, err
	}
	if resp.GetStatus() == ReadyStatus {
		slog.Info("kubeadm preflight completed successfully")
	} else {
		slog.Error("kubeadm preflight failed", "error_message", resp.GetErrorMessage(), "output", resp.GetRawOutput())
	}
	return resp, nil
}

func (s *NodeAgentServer) runKubeadmPreflight(
	ctx context.Context,
	configYAML string,
	role pb.KubeadmPreflightRole,
) (*pb.KubeadmPreflightResponse, error) {
	if strings.TrimSpace(configYAML) == "" {
		return &pb.KubeadmPreflightResponse{
			Status:       ErrorStatus,
			ErrorMessage: errMsgConfigRequired,
		}, nil
	}

	phase := kubeadmCmdInit
	if role == pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_JOIN {
		phase = kubeadmCmdJoin
	}

	configPath := fmt.Sprintf("/tmp/kubeadm-%s-preflight-config.yaml", phase)
	if err := s.FileWriter.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		// Reported to the caller in the response, like every other failure here.
		return &pb.KubeadmPreflightResponse{ //nolint:nilerr
			Status:       ErrorStatus,
			ErrorMessage: "failed to write preflight config file",
		}, nil
	}

	args := []string{phase, "phase", "preflight", "--config", configPath}
	output, err := s.Runner.Run("kubeadm", args, &nvkeutils.CommandOptions{})
	rawOutput := string(output)
	results := parsePreflightOutput(rawOutput)

	if err != nil {
		return &pb.KubeadmPreflightResponse{
			Status:       ErrorStatus,
			ErrorMessage: fmt.Sprintf("kubeadm %s phase preflight failed: %s", phase, err.Error()),
			Results:      results,
			RawOutput:    rawOutput,
		}, nil
	}

	return &pb.KubeadmPreflightResponse{
		Status:    ReadyStatus,
		Results:   results,
		RawOutput: rawOutput,
	}, nil
}

// parsePreflightOutput parses kubeadm preflight output into structured results.
//
// kubeadm emits the following formats
//
//  1. Structured warnings (printed immediately per check):
//     \t[WARNING Name]: message
//
//  2. Structured errors (buffered, printed together inside a fatal block):
//     [preflight] Some fatal errors occurred:
//     \t[ERROR Name]: message
//     [preflight] If you know what you are doing, ...
//
//  3. klog warnings emitted before checks run (e.g. CRI connection failure):
//     [preflight] WARNING: Couldn't create the interface ... : <error>
//
//  4. Workflow runner error wrapper (on the exit-status error line):
//     error execution phase preflight: preflight checks failed
//
// The parser extracts (1), (2), and (3) into PreflightCheckResult entries.
func parsePreflightOutput(output string) []*pb.PreflightCheckResult {
	var results []*pb.PreflightCheckResult
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)

		switch {
		// Structured [WARNING CheckName]: message
		case strings.HasPrefix(line, "[WARNING "):
			name, msg := parsePreflightLine(line, "[WARNING ")
			results = append(results, &pb.PreflightCheckResult{
				Name:     name,
				Severity: preflightSeverityWarning,
				Message:  msg,
			})

		// Structured [ERROR CheckName]: message
		case strings.HasPrefix(line, "[ERROR "):
			name, msg := parsePreflightLine(line, "[ERROR ")
			results = append(results, &pb.PreflightCheckResult{
				Name:     name,
				Severity: preflightSeverityError,
				Message:  msg,
			})

		// klog-style warning: "[preflight] WARNING: <message>"
		case strings.HasPrefix(line, "[preflight] WARNING:"):
			msg := strings.TrimSpace(strings.TrimPrefix(line, "[preflight] WARNING:"))
			results = append(results, &pb.PreflightCheckResult{
				Name:     "Preflight",
				Severity: preflightSeverityWarning,
				Message:  msg,
			})
		}
	}
	return results
}

// parsePreflightLine extracts the check name and message from a preflight output line.
// Example: "[WARNING Firewalld]: firewalld is active" -> ("Firewalld", "firewalld is active")
func parsePreflightLine(line, prefix string) (string, string) {
	// Remove the prefix (e.g. "[WARNING " or "[ERROR ")
	after := strings.TrimPrefix(line, prefix)
	// Find the closing bracket
	idx := strings.Index(after, "]")
	if idx < 0 {
		return "", line
	}
	name := after[:idx]
	msg := strings.TrimSpace(strings.TrimPrefix(after[idx+1:], ":"))
	return name, msg
}

// Clean up and CNI files
func cleanupCNIFiles() error {
	_, err := os.Stat("/var/lib/openvswitch")
	if os.IsNotExist(err) {
		return nil
	}
	ovsMatches, err := filepath.Glob(filepath.Join("/var/lib/openvswitch", "ovn*.db"))
	if err != nil {
		return fmt.Errorf("error finding files: %v", err)
	}

	cniMatches, err := filepath.Glob(filepath.Join("/etc/cni/net.d", "*"))
	if err != nil {
		return fmt.Errorf("error finding files: %v", err)
	}
	matches := append(ovsMatches, cniMatches...)
	for _, file := range matches {
		err := os.Remove(file)
		if err != nil {
			return fmt.Errorf("error deleting file %s: %v", file, err)
		}
		slog.Info("successfully deleted file", "file", file)
	}
	return nil
}

func (s *NodeAgentServer) GetKubeadmJoinString(
	ctx context.Context,
	req *pb.KubeadmJoinStringRequest,
) (*pb.KubeadmJoinStringResponse, error) {
	defer logRPCSpan(ctx, "GetKubeadmJoinString")()
	if !strings.EqualFold(s.NodeStatus.Role, ControlPlaneRole) {
		slog.Info("unable to fulfill GetKubeadmJoinString from non-controlplane node", "NodeRole", s.NodeStatus.Role)
		return nil, errors.New(
			"join strings not available, invalid node role, must be controlplane node",
		)
	}

	if s.NodeStatus.JoinInfo == (JoinInfo{}) {
		slog.Info("join strings not available")
		return nil, errors.New(
			"join strings not available, has node been initialized?",
		)
	}

	if s.NodeStatus.JoinInfo.ControlPlaneJoinString == "" || s.NodeStatus.JoinInfo.WorkerJoinString == "" {
		slog.Info("join strings not available")
		return nil, errors.New(
			"join strings not available, has node been initialized? Join strings are only stored on original master node, must use UpdateAndFetchKubeaAdmJoinStrings to generate new strings for nodes that aren't the original cluster master",
		)
	}

	response := &pb.KubeadmJoinStringResponse{
		ControlplaneJoinString: s.NodeStatus.JoinInfo.ControlPlaneJoinString,
		WorkerJoinString:       s.NodeStatus.JoinInfo.WorkerJoinString,
	}

	return response, nil
}

func (s *NodeAgentServer) GetKubeconfig(
	ctx context.Context,
	req *pb.KubeconfigRequest,
) (*pb.KubeconfigResponse, error) {
	defer logRPCSpan(ctx, "GetKubeconfig")()
	if !strings.EqualFold(s.NodeStatus.Role, ControlPlaneRole) {
		return nil, errors.New(
			"kubeconfig not available, invalid node role, must be controlplane node",
		)
	}

	if _, err := os.Stat(KubeConfig); os.IsNotExist(err) {
		return nil, errors.New("kubeconfig not available, file does not exist")
	}

	bytes, err := os.ReadFile(KubeConfig)
	if err != nil {
		return nil, errors.New("kubeconfig not available, failed to read file")
	}

	// NOTE: We're not substituting the hosts IP address any longer.  This wasn't a great solution
	// anyway, because of the forge external VIP anyway.  Now we have kube-vip so this should be done by the caller
	response := &pb.KubeconfigResponse{
		Kubeconfig: string(bytes),
	}
	return response, nil
}

// UpdateAndFetchKubeAdmJoinStrings updates certs and tokens and creates a new join string for both controlplane and worker
func (s *NodeAgentServer) UpdateAndFetchKubeAdmJoinStrings(
	ctx context.Context,
	req *pb.KubeadmJoinStringRequest,
) (*pb.KubeadmJoinStringResponse, error) {
	defer logRPCSpan(ctx, "UpdateAndFetchKubeAdmJoinStrings")()
	defer s.Persister.Persist(s.NodeStatus) //nolint:all

	if !strings.EqualFold(s.NodeStatus.Role, ControlPlaneRole) {
		slog.Error("unable to fulfill UpdateAndFetchKubeAdmJoinStrings from non-controlplane node", "NodeRole", s.NodeStatus.Role)
		return nil, errors.New(
			"join strings not available, invalid node role, must be controlplane node",
		)
	}

	newCert, err := s.uploadControlPlaneCerts()
	if err != nil {
		slog.Error("failed to upload new controlplane certs", "err", err)
		return nil, err
	}

	tokenArgs := []string{"token", "create", "--print-join-command"}
	output, err := s.Runner.Run("kubeadm", tokenArgs, &nvkeutils.CommandOptions{})
	if err != nil {
		loggable := redact.New().String(string(output))
		slog.Error("failed to create new join string", "err", err, "output", loggable)
		return nil, fmt.Errorf("kubeadm token create: %w: %s", err, strings.TrimSpace(loggable))
	}
	newJoinString := strings.TrimSpace(string(output))

	cpJoin := newJoinString + " --control-plane --certificate-key " + newCert
	workerJoin := newJoinString

	s.mu.Lock()
	s.NodeStatus.JoinInfo.ControlPlaneJoinString = cpJoin
	s.NodeStatus.JoinInfo.WorkerJoinString = workerJoin
	s.mu.Unlock()

	slog.Info("kubeadm update join commands completed successfully")
	response := &pb.KubeadmJoinStringResponse{
		ControlplaneJoinString: cpJoin,
		WorkerJoinString:       workerJoin,
	}
	return response, nil
}

// uploadControlPlaneCerts runs the kubeadm upload-certs phase and returns the certificate key it prints.
func (s *NodeAgentServer) uploadControlPlaneCerts() (string, error) {
	args := []string{kubeadmCmdInit, kubeadmCmdPhase, "upload-certs", "--upload-certs"}
	configPath := s.initConfigFile()
	if _, err := os.Stat(configPath); err == nil {
		args = append(args, "--config", configPath)
	}
	cmd := "kubeadm " + strings.Join(args, " ")
	output, err := s.Runner.Run("kubeadm", args, &nvkeutils.CommandOptions{})
	out := strings.TrimSpace(string(output))
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", cmd, err, redact.New().String(out))
	}
	_, after, found := strings.Cut(out, "Using certificate key:")
	fields := strings.Fields(after)
	if !found || len(fields) == 0 {
		return "", fmt.Errorf("%s: no certificate key in output: %s", cmd, redact.New().String(out))
	}
	// kubeadm join applies the same check: the key is the hex encoding of a 32-byte AES key.
	key, err := hex.DecodeString(fields[0])
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("%s: the certificate key in the output is not a 32-byte hex string", cmd)
	}
	return fields[0], nil
}

// initConfigFile returns the InitConfiguration file kubeadm init ran with.
func (s *NodeAgentServer) initConfigFile() string {
	if s.initConfigPath != "" {
		return s.initConfigPath
	}
	return kubeadmInitConfigPath
}

func (s *NodeAgentServer) ExecuteScript(
	ctx context.Context,
	req *pb.ExecuteScriptRequest,
) (*pb.ExecuteScriptResponse, error) {
	defer logRPCSpan(ctx, "ExecuteScript")()
	requestID := generateRequestID()
	resp := &pb.ExecuteScriptResponse{}
	resp.RequestId = requestID
	var err error

	slog.Info("Issuing script execution request", "requestID", requestID, "scriptName", req.ScriptName, "async", req.Async)
	if req.Async {
		go s.executeScript(req, requestID) //nolint:all
	} else {
		resp, err = s.executeScript(req, requestID)
	}
	return resp, err
}

func (s *NodeAgentServer) trimScriptStatus() {
	if len(s.NodeStatus.Scripts) > ScriptStatusLimit {
		startIndex := len(s.NodeStatus.Scripts) - ScriptStatusLimit
		s.mu.Lock()
		s.NodeStatus.Scripts = s.NodeStatus.Scripts[startIndex:]
		s.mu.Unlock()
	}
}

func (s *NodeAgentServer) addScript(script Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.NodeStatus.Scripts) >= ScriptStatusLimit {
		s.NodeStatus.Scripts = s.NodeStatus.Scripts[1:]
	}
	s.NodeStatus.Scripts = append(s.NodeStatus.Scripts, script)
}

// logScriptExecution writes call info and result to ScriptExecutionLogger when set. output is truncated for size.
func (s *NodeAgentServer) logScriptExecution(event, requestID string, req *pb.ExecuteScriptRequest, status string, exitCode int32, errMsg, output string) {
	if s.ScriptExecutionLogger == nil {
		return
	}
	if len(output) > maxScriptLogOutputBytes {
		output = output[:maxScriptLogOutputBytes] + "...[truncated]"
	}
	s.ScriptExecutionLogger.Info(event,
		"request_id", requestID,
		"script_name", req.GetScriptName(),
		"script_path", req.GetScriptPath(),
		"args", req.GetArgs(),
		"async", req.GetAsync(),
		"status", status,
		"exit_code", exitCode,
		"error_message", errMsg,
		"output", output,
	)
}

func (s *NodeAgentServer) executeScript(
	req *pb.ExecuteScriptRequest,
	requestID string,
) (*pb.ExecuteScriptResponse, error) {
	response := &pb.ExecuteScriptResponse{}
	response.RequestId = requestID

	defer s.Persister.Persist(s.NodeStatus) //nolint:all

	script := Script{
		RequestID:  requestID,
		Name:       req.ScriptName,
		IsAsync:    req.Async,
		ExecutedAt: time.Now().Format(time.RFC3339),
	}

	scriptPath, cleanup, err := resolveScript(req, requestID)
	if err != nil {
		errMsg := fmt.Sprintf("failed to write script: %s", err)

		script.Status = "Error"
		script.Error = errMsg
		script.Output = errMsg
		s.addScript(script)

		response.ExitCode = int32(1)
		response.Output = ""
		response.Status = ErrorStatus
		response.ErrorMessage = errMsg

		s.logScriptExecution("ExecuteScript", requestID, req, ErrorStatus, 1, errMsg, "")
		return response, nil
	}
	defer cleanup()

	output, err := s.Runner.Run(scriptPath, req.Args, &nvkeutils.CommandOptions{})
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		}

		script.Status = "Error"
		script.Error = err.Error()
		script.Output = string(output)

		s.addScript(script)

		response.Output = string(output)
		response.Status = ErrorStatus
		response.ExitCode = int32(exitCode)
		response.ErrorMessage = err.Error()

		s.logScriptExecution("ExecuteScript", requestID, req, ErrorStatus, int32(exitCode), err.Error(), string(output))
		return response, nil
	}

	response.ExitCode = int32(exitCode)
	response.Output = string(output)
	response.Status = SuccessStatus
	response.ErrorMessage = ""

	script.Status = scriptStatusSuccess
	s.addScript(script)
	s.logScriptExecution("ExecuteScript", requestID, req, SuccessStatus, int32(exitCode), "", string(output))
	return response, nil
}

func (s *NodeAgentServer) UploadFileBundle(stream pb.NodeAgent_UploadFileBundleServer) error {
	ctx := stream.Context()
	defer logRPCSpan(ctx, "UploadFileBundle")()
	slog.Info("received upload file bundle request")
	firstChunk, err := stream.Recv()
	if err == io.EOF {
		return fmt.Errorf("no file chunks received")
	}
	if err != nil {
		return fmt.Errorf("failed to receive first chunk: %v", err)
	}

	// Extract the destination path from the first chunk
	destination := firstChunk.Destination
	if destination == "" {
		return fmt.Errorf("destination path is required")
	}

	// Extract the filename from the first chunk
	filename := firstChunk.Filename
	if filename == "" {
		return fmt.Errorf("filename is required")
	}
	if !strings.Contains(filename, ".tgz") {
		return fmt.Errorf("file must be a .tgz archive")
	}

	// Ensure the upload dir exists...
	err = os.MkdirAll(UploadDir, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create upload directory: %v", err)
	}
	// Create the file in our upload directory...
	outFile, err := os.Create(UploadDir + "/" + filename)
	if err != nil {
		return fmt.Errorf(
			"failed to create file %s in upload directory %s: %v",
			filename,
			UploadDir,
			err,
		)
	}
	defer outFile.Close()

	// Write the first chunk to the file
	_, err = outFile.Write(firstChunk.Data)
	if err != nil {
		return fmt.Errorf("failed to write first chunk to file: %v", err)
	}

	// Continue receiving and writing the remaining chunks
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			// File transfer is complete
			slog.Info("file transfer complete, ensure destination directory exists")
			err = os.MkdirAll(destination, 0o755)
			if err != nil {
				return fmt.Errorf("failed to create destination directory: %v", err)
			}
			// Extract the tgz file from UploadDir into the destination directory
			_, err = s.Runner.Run(
				"tar",
				[]string{"-xvf", UploadDir + "/" + filename, "-C", destination},
				&nvkeutils.CommandOptions{},
			)
			if err != nil {
				return fmt.Errorf("failed to extract file: %v", err)
			}

			// Remove the uploaded file
			slog.Info("removing uploaded file from upload directory")
			err = os.Remove(UploadDir + "/" + filename)
			if err != nil {
				return fmt.Errorf("failed to remove uploaded file: %v", err)
			}

			return stream.SendAndClose(&pb.UploadStatus{
				Message: "Upload complete",
				Code:    200,
			})
		}
		if err != nil {
			return fmt.Errorf("failed to receive chunk: %v", err)
		}

		// Write each chunk to the file
		_, err = outFile.Write(chunk.Data)
		if err != nil {
			return fmt.Errorf("failed to write chunk to file: %v", err)
		}
	}
}

// maxNodeAgentConfigBytes caps NodeSpec YAML size (protects disk and RPC).
const maxNodeAgentConfigBytes = 4 << 20 // 4 MiB

func (s *NodeAgentServer) WriteNodeAgentConfig(
	ctx context.Context,
	req *pb.WriteNodeAgentConfigRequest,
) (*pb.WriteNodeAgentConfigResponse, error) {
	defer logRPCSpan(ctx, "WriteNodeAgentConfig")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	data := req.GetConfigYaml()
	if len(data) == 0 {
		return nil, status.Error(codes.InvalidArgument, "config_yaml is required")
	}
	if len(data) > maxNodeAgentConfigBytes {
		return nil, status.Errorf(codes.InvalidArgument, "config_yaml exceeds max size (%d bytes)", maxNodeAgentConfigBytes)
	}

	s.nodeAgentCfgMu.Lock()
	defer s.nodeAgentCfgMu.Unlock()

	dir := filepath.Join(s.RootPath, "/etc/agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("failed to create agent config directory", "dir", dir, "err", err)
		return nil, status.Errorf(codes.Internal, "mkdir agent config dir: %v", err)
	}
	path := filepath.Join(s.RootPath, "/etc/agent/config.yaml")
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		slog.Error("failed to write agent config atomically", "path", path, "err", err)
		return nil, status.Errorf(codes.Internal, "atomic write config: %v", err)
	}

	slog.Info("wrote agent config.yaml", "path", path, "bytes", len(data))
	return &pb.WriteNodeAgentConfigResponse{
		Message: "config written",
		Code:    200,
	}, nil
}

func (s *NodeAgentServer) StreamExecuteScript(
	req *pb.ExecuteScriptRequest,
	stream pb.NodeAgent_StreamExecuteScriptServer,
) error {
	defer logRPCSpan(stream.Context(), "StreamExecuteScript")()
	if req.Async {
		return fmt.Errorf("async execution not supported for streaming")
	}

	requestID := generateRequestID()
	s.logScriptExecution("StreamExecuteScript_start", requestID, req, "start", 0, "", "")

	scriptPath, cleanup, err := resolveScript(req, requestID)
	if err != nil {
		slog.ErrorContext(stream.Context(), "failed to resolve script",
			"requestID", requestID,
			"scriptName", req.ScriptName,
			"err", err,
		)
		return status.Errorf(codes.Internal, "failed to resolve script: %v", err)
	}
	defer cleanup()

	// Run the command, streaming output to the gRPC stream as it arrives.
	cmdErr := streamCommand(stream, scriptPath, req.Args...)

	exitCode, errMsg := exitResult(cmdErr)
	s.logScriptExecution("StreamExecuteScript", requestID, req, errStatus(cmdErr), int32(exitCode), errMsg, "")
	return stream.Send(&pb.ScriptOutputChunk{
		ExitCode:   int32(exitCode),
		IsComplete: true,
	})
}

// resolveScript returns the path to the script to execute. Inline script
// contents are written to a unique temporary path so concurrent requests with
// the same logical script name cannot overwrite each other.
func resolveScript(req *pb.ExecuteScriptRequest, requestID string) (string, func(), error) {
	baseDir := UserScriptsDir
	if req.ScriptPath != "" {
		baseDir = req.ScriptPath
	}

	if req.ScriptContents == "" {
		return filepath.Join(baseDir, req.ScriptName), func() {}, nil
	}

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("failed to create script directory: %w", err)
	}

	pattern := fmt.Sprintf(".%s-%s-*", filepath.Base(req.ScriptName), requestID)
	scriptFile, err := os.CreateTemp(baseDir, pattern)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary script: %w", err)
	}
	scriptPath := scriptFile.Name()
	cleanup := func() {
		if err := os.Remove(scriptPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("failed to remove temporary script", "path", scriptPath, "err", err)
		}
	}

	if err := scriptFile.Chmod(0o755); err != nil {
		_ = scriptFile.Close()
		cleanup()
		return "", nil, fmt.Errorf("failed to make temporary script executable: %w", err)
	}
	if _, err := scriptFile.WriteString(req.ScriptContents); err != nil {
		_ = scriptFile.Close()
		cleanup()
		return "", nil, fmt.Errorf("failed to write temporary script: %w", err)
	}
	if err := scriptFile.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to close temporary script: %w", err)
	}

	return scriptPath, cleanup, nil
}

// streamCommand starts a command and streams its stdout/stderr to the gRPC
// stream. It returns the command's error (nil on exit 0). The function blocks
// until the command exits and all output is delivered.
func streamCommand(
	stream pb.NodeAgent_StreamExecuteScriptServer,
	path string,
	args ...string,
) error {
	cmd := exec.Command(path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Merge both pipes into a single channel so the streaming loop is simple.
	type chunk struct{ out, err string }
	chunks := make(chan chunk)
	var pipeWg sync.WaitGroup
	pipeWg.Add(2)
	scanPipe := func(r io.Reader, toChunk func(string) chunk) {
		defer pipeWg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			chunks <- toChunk(sc.Text() + "\n")
		}
	}
	go scanPipe(stdout, func(s string) chunk { return chunk{out: s} })
	go scanPipe(stderr, func(s string) chunk { return chunk{err: s} })

	// Once both scanners finish, close the merged channel and collect the
	// exit status. This ordering guarantees no pipe data is lost.
	doneChan := make(chan error, 1)
	go func() {
		pipeWg.Wait()
		close(chunks)
		doneChan <- cmd.Wait()
	}()

	// Deliver output to the gRPC stream as it arrives.
	for c := range chunks {
		select {
		case <-stream.Context().Done():
			_ = cmd.Process.Kill()
			slog.Info("client disconnected, killing process")
			return stream.Context().Err()
		default:
		}
		if err := stream.Send(&pb.ScriptOutputChunk{Output: c.out, Error: c.err}); err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("failed to send chunk: %w", err)
		}
	}

	return <-doneChan
}

// exitResult extracts an exit code and error message from a cmd.Wait() error.
func exitResult(cmdErr error) (int, string) {
	if cmdErr == nil {
		return 0, ""
	}
	if exitError, ok := cmdErr.(*exec.ExitError); ok {
		return exitError.ExitCode(), cmdErr.Error()
	}
	return 1, cmdErr.Error()
}

// errStatus returns "success" or "error" based on whether err is nil.
func errStatus(err error) string {
	if err != nil {
		return ErrorStatus
	}
	return SuccessStatus
}

// bundleIndexUnconfiguredMessage explains an empty BundleIndexFile. The two
// causes need different advice: with no bundle server the operator has set
// nothing, but with one set the index is derived from /etc/os-release and that
// derivation only knows Ubuntu 22 and 24, so telling them to set a flag they
// already set sends them in a circle.
func bundleIndexUnconfiguredMessage(bundleServerURL string) string {
	if bundleServerURL == "" {
		return "bundle index not configured: set --bundle-server-url or --index-file-url (prefer apt repos via ConfigureAptRepo/InstallPackages)"
	}
	return fmt.Sprintf(
		"bundle index not derived from --bundle-server-url %q: the index path is derived from /etc/os-release, which is recognized only for Ubuntu 22 and 24. Set --index-file-url explicitly.",
		bundleServerURL,
	)
}

func (s *NodeAgentServer) InstallBundle(
	ctx context.Context,
	req *pb.InstallBundleRequest,
) (*pb.InstallBundleResponse, error) {
	defer logRPCSpan(ctx, "InstallBundle")()
	if strings.EqualFold(strings.TrimSpace(req.GetName()), "agent") {
		return nil, status.Error(codes.FailedPrecondition, "use StartNodeAgentUpgrade to replace nvke-agent")
	}
	if s.BundleIndexFile == "" {
		return nil, status.Error(codes.FailedPrecondition, bundleIndexUnconfiguredMessage(s.BundleServerURL))
	}
	if len(req.InstallArgs) == 0 {
		return nil, fmt.Errorf("InstallArgs cannot be empty")
	}

	version, err := semver.NewVersion(req.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to parse version: %v", err)
	}

	slog.Info("[DEBUG] attempt to fetch bundle from index", "IDX", s.BundleIndexFile, "Name", req.Name, "Version", nvkeutils.VersionString(version), "ARCH", s.NodeStatus.Architecture)
	bundle, err := nvkeutils.FetchBundleFromIndex(ctx, s.BundleServerURL, s.BundleIndexFile, req.Name, version, s.NodeStatus.Architecture)
	if err != nil {
		return nil, err
	}
	slog.Info("[DEBUG] fetched bundle from index successfully", "BUNDLE", bundle)

	requestID := generateRequestID()
	response := &pb.InstallBundleResponse{RequestId: requestID}

	bundleStatus := &Bundle{
		RequestID:   requestID,
		Name:        req.Name,
		Version:     version,
		InstallTime: time.Now().Format(time.RFC3339),
		Status:      InstallingStatus,
		Source:      bundle.URL,
	}

	defer s.PersistNodeStatus()
	defer s.AppendBundleToStatus(bundleStatus)

	bundle.InstallCmd = req.InstallArgs

	if req.Async {
		go func() {
			if err := s.processBundleInstall(bundle, bundleStatus); err != nil {
				slog.Error("error installing bundle async", "err", err)
			}
		}()
	} else {
		if err := s.processBundleInstall(bundle, bundleStatus); err != nil {
			return nil, err
		}
	}

	return response, nil
}

func (s *NodeAgentServer) processBundleInstall(
	bundle *nvkeutils.Bundle,
	bundleStatus *Bundle,
) error {
	defer s.PersistNodeStatus()
	defer s.UpdateBundleStatus(bundleStatus)

	out, err := nvkeutils.InstallBundle(bundle, s.Runner, s.BundleSource)
	if err != nil {
		slog.Error("failed to install bundle", "name", bundle.Name, "version", nvkeutils.VersionString(bundle.Version), "err", err)
		bundleStatus.Status = ErrorStatus
		bundleStatus.Error = fmt.Sprintf("failed to install %s bundle version %s @ %s: %v", bundle.Name, bundle.Version, time.Now().Format(time.RFC3339), err)
		bundleStatus.Output = out

		if exitError, ok := err.(*exec.ExitError); ok {
			bundleStatus.ExitCode = int32(exitError.ExitCode())
		}
		return err
	}

	bundleStatus.Status = SuccessStatus
	slog.Info("bundle successfully installed", "name", bundle.Name, "version", nvkeutils.VersionString(bundle.Version))

	return nil
}

func (s *NodeAgentServer) GetBundleStatus(
	ctx context.Context,
	req *pb.GetBundleStatusRequest,
) (*pb.GetBundleStatusResponse, error) {
	defer logRPCSpan(ctx, "GetBundleStatus")()
	if req.RequestId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "requestId is required")
	}

	entries := s.GetBundleStatusByRequestID(req.RequestId)
	if len(entries) == 0 {
		slog.Info("bundle status not found", "requestId", req.RequestId)
		return nil, status.Errorf(codes.NotFound, "bundle status not found for requestId %s", req.RequestId)
	}

	resp := &pb.GetBundleStatusResponse{Statuses: []*pb.BundleStatus{}}

	for _, entry := range entries {
		version := ""
		if entry.Version != nil {
			version = entry.Version.String()
		}
		itemType := entry.Type
		if itemType == "" {
			itemType = "bundle"
		}
		resp.Statuses = append(resp.Statuses, &pb.BundleStatus{
			RequestId: entry.RequestID,
			Name:      entry.Name,
			Version:   version,
			Status:    entry.Status,
			Error:     entry.Error,
			Output:    entry.Output,
			ExitCode:  strconv.Itoa(int(entry.ExitCode)),
			ItemType:  itemType,
		})
	}

	return resp, nil
}

func (s *NodeAgentServer) InstallCollection(ctx context.Context, req *pb.InstallCollectionRequest) (*pb.InstallCollectionResponse, error) {
	defer logRPCSpan(ctx, "InstallCollection")()
	if req.CollectionURL == "" {
		slog.ErrorContext(ctx, "collectionURL is required")
		return nil, status.Errorf(codes.InvalidArgument, "collectionURL is required")
	}

	isJSONCollection := strings.HasSuffix(strings.ToLower(req.CollectionURL), ".json")
	if isJSONCollection && s.BundleIndexFile == "" {
		return nil, status.Error(codes.FailedPrecondition, bundleIndexUnconfiguredMessage(s.BundleServerURL))
	}

	requestID := generateRequestID()
	slog.InfoContext(ctx, "Installing collection", "collection_url", req.GetCollectionURL(), "request_id", requestID)
	defer s.PersistNodeStatus()

	// Route by file suffix: .json -> bundles, .txt -> apt packages
	switch {
	case isJSONCollection:
		profile := nvkeutils.Profile{}
		if err := nvkeutils.FetchJSON(ctx, req.CollectionURL, &profile); err != nil {
			slog.Error("failed to fetch collection", "url", req.CollectionURL, "err", err)
			return nil, status.Errorf(codes.Internal, "failed to fetch collection from %s: %v", req.CollectionURL, err)
		}
		for _, bundleReq := range profile.Bundles {
			if strings.EqualFold(strings.TrimSpace(bundleReq.Name), "agent") {
				return nil, status.Error(codes.FailedPrecondition, "use StartNodeAgentUpgrade to replace nvke-agent")
			}
		}

		index, err := nvkeutils.FetchIndex(ctx, s.BundleIndexFile)
		if err != nil {
			slog.Error("failed to fetch index", "index_file", s.BundleIndexFile, "err", err)
			return nil, status.Errorf(codes.Internal, "failed to fetch index: %v", err)
		}

		for _, bundleReq := range profile.Bundles {
			slog.InfoContext(ctx, "Adding bundle", "name", bundleReq.Name, "version", nvkeutils.VersionString(bundleReq.Version), "request_id", requestID)

			bundle, ok := index.Find(bundleReq.Name, bundleReq.Version, s.NodeStatus.Architecture)
			if !ok {
				slog.Error("bundle not found in index", "name", bundleReq.Name, "version", nvkeutils.VersionString(bundleReq.Version), "architecture", s.NodeStatus.Architecture)
				return nil, status.Errorf(codes.Internal, "bundle %s version %s not found in index", bundleReq.Name, bundleReq.Version)
			}

			// Build full URL for HTTP/S3 sources (OCI uses registry, not URL)
			if s.BundleSource != nil && s.BundleSource.IsHTTP() {
				bundle.URL = s.BundleServerURL + bundle.Path
			}

			if len(bundleReq.InstallCmd) > 0 {
				bundle.InstallCmd = bundleReq.InstallCmd
			}

			status := &Bundle{
				RequestID:   requestID,
				Name:        bundleReq.Name,
				Version:     bundleReq.Version,
				InstallTime: time.Now().Format(time.RFC3339),
				Status:      InstallingStatus,
				Source:      bundle.URL,
			}

			s.AppendBundleToStatus(status)
			s.PersistNodeStatus()

			go func(b *nvkeutils.Bundle, st *Bundle) {
				if err := s.processBundleInstall(b, st); err != nil {
					slog.Error("error installing bundle async", "err", err)
				}
			}(bundle, status)
		}

	case strings.HasSuffix(strings.ToLower(req.CollectionURL), ".txt"):
		text, err := nvkeutils.FetchText(ctx, req.CollectionURL)
		if err != nil {
			slog.ErrorContext(ctx, "failed to fetch collection", "url", req.CollectionURL, "err", err)
			return nil, status.Errorf(codes.Internal, "failed to fetch collection from %s: %v", req.CollectionURL, err)
		}

		packages := parseDebCollectionText(text)
		if len(packages) == 0 {
			slog.ErrorContext(ctx, "collection file contains no packages", "url", req.CollectionURL)
			return nil, status.Errorf(codes.InvalidArgument, "collection file contains no packages")
		}
		for _, pkg := range packages {
			if isNodeAgentPackageArgument(pkg) {
				return nil, status.Error(codes.FailedPrecondition, "use StartNodeAgentUpgrade to replace nvke-agent")
			}
		}

		// Add status entry for tracking; caller uses GetBundleStatus(request_id) for both .json and .txt
		debStatus := &Bundle{
			RequestID:   requestID,
			Name:        "deb-collection",
			Source:      req.CollectionURL,
			Status:      InstallingStatus,
			InstallTime: time.Now().Format(time.RFC3339),
			Type:        "deb-collection",
			Output:      strings.Join(packages, ", "),
		}
		s.AppendBundleToStatus(debStatus)
		s.PersistNodeStatus()

		installReq := &pb.InstallPackagesRequest{
			Packages: packages,
			Update:   true,
		}
		go func() {
			if err := s.processInstallPackages(context.Background(), installReq, debStatus); err != nil {
				slog.ErrorContext(ctx, "failed to install packages from collection", "url", req.CollectionURL, "packages", packages, "err", err)
			}
		}()
		slog.InfoContext(ctx, "started async deb install from collection", "packages", packages, "request_id", requestID)

	default:
		return nil, status.Errorf(codes.InvalidArgument, "collectionURL must end with .json or .txt, got: %s", req.CollectionURL)
	}

	return &pb.InstallCollectionResponse{RequestId: requestID}, nil
}

// parseDebCollectionText parses a .txt collection file: one package per line, skip empty lines and # comments.
func parseDebCollectionText(txt string) []string {
	var packages []string
	for _, line := range strings.Split(txt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		packages = append(packages, line)
	}
	return packages
}

// GetSupportBundle gathers node support artifacts (state, logs, audit) into a tgz and returns it.
// Callers may specify additional_files (absolute paths) to include in the bundle.
func (s *NodeAgentServer) GetSupportBundle(ctx context.Context, req *pb.GetSupportBundleRequest) (*pb.GetSupportBundleResponse, error) {
	defer logRPCSpan(ctx, "GetSupportBundle")()
	root := s.RootPath
	tmpDir, err := os.MkdirTemp("", "nvke-support-bundle-")
	if err != nil {
		slog.ErrorContext(ctx, "failed to create support bundle temp dir", "err", err)
		return nil, status.Errorf(codes.Internal, "failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy /var/lib/nvke/state.json
	srcState := filepath.Join(root, StateFile)
	if err := copyFileIntoBundle(srcState, filepath.Join(tmpDir, "state.json")); err != nil {
		slog.DebugContext(ctx, "state file not included in support bundle", "path", srcState, "err", err)
	}

	// nvke-agent logs via journalctl
	out, err := s.Runner.Run("journalctl", []string{"-u", "nvke-agent", "--no-pager"}, nil)
	if err != nil {
		slog.DebugContext(ctx, "journalctl nvke-agent failed", "err", err)
	} else {
		if wErr := os.WriteFile(filepath.Join(tmpDir, "nvke-agent.journal.log"), out, 0o644); wErr != nil {
			slog.ErrorContext(ctx, "failed to write nvke-agent journal to bundle", "err", wErr)
		}
	}

	// /var/lib/nvke-agent-audit.log
	srcAudit := filepath.Join(root, NVKEAuditLog)
	if err := copyFileIntoBundle(srcAudit, filepath.Join(tmpDir, "nke-audit.log")); err != nil {
		slog.DebugContext(ctx, "audit log not included in support bundle", "path", srcAudit, "err", err)
	}

	// /var/log/nvke-execute-scripts.log
	srcExecuteScripts := filepath.Join(root, NVKEExecuteScriptsLog)
	if err := copyFileIntoBundle(srcExecuteScripts, filepath.Join(tmpDir, "nvke-execute-scripts.log")); err != nil {
		slog.DebugContext(ctx, "execute scripts log not included in support bundle", "path", srcExecuteScripts, "err", err)
	}

	// Additional files requested by the caller (absolute paths only, resolved under root)
	// AKA the YOLO section
	additionalDir := filepath.Join(tmpDir, "additional")
	_ = os.MkdirAll(additionalDir, 0o755)
	for i, p := range req.GetAdditionalFiles() {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		cleaned := filepath.Clean(trimmed)
		if !filepath.IsAbs(cleaned) {
			slog.DebugContext(ctx, "skipping non-absolute path in support bundle additional_files", "path", p)
			continue
		}
		var fullPath string
		if root != "" {
			fullPath = filepath.Join(root, strings.TrimPrefix(cleaned, string(filepath.Separator)))
			rel, err := filepath.Rel(root, fullPath)
			if err != nil || strings.HasPrefix(rel, "..") {
				slog.DebugContext(ctx, "skipping path outside root in support bundle", "path", p, "root", root)
				continue
			}
		} else {
			fullPath = cleaned
		}
		// Safe name for tarball: additional/0_var_log_myapp.log to avoid collisions
		safeName := strings.ReplaceAll(cleaned, string(filepath.Separator), "_")
		if safeName == "" || safeName == "_" {
			safeName = fmt.Sprintf("file_%d", i)
		}
		dst := filepath.Join(additionalDir, fmt.Sprintf("%d_%s", i, safeName))
		if err := copyFileIntoBundle(fullPath, dst); err != nil {
			slog.DebugContext(ctx, "additional file not included in support bundle", "path", fullPath, "err", err)
		}
	}

	// Installed Debian packages inventory (name version status per line)
	iter, err := deb.List(ctx)
	if err != nil {
		slog.DebugContext(ctx, "dpkg-query failed, skipping installed-packages in support bundle", "err", err)
	} else {
		var sb strings.Builder
		for p := range iter {
			sb.WriteString(fmt.Sprintf("%s %s %s\n", p.Name, p.Version, p.Status))
		}
		pkgPath := filepath.Join(tmpDir, "installed-packages.txt")
		if wErr := os.WriteFile(pkgPath, []byte(sb.String()), 0o644); wErr != nil {
			slog.ErrorContext(ctx, "failed to write installed-packages to support bundle", "err", wErr)
		}
	}

	tgz, err := createTarball(tmpDir)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create support bundle tarball", "err", err)
		return nil, status.Errorf(codes.Internal, "failed to create support bundle: %v", err)
	}
	return &pb.GetSupportBundleResponse{BundleTgz: tgz}, nil
}

func (s *NodeAgentServer) isControlPlaneRole() bool {
	if s == nil || s.NodeStatus == nil {
		return false
	}
	return strings.EqualFold(s.NodeStatus.Role, ControlPlaneRole)
}

// copyFileIntoBundle copies a single file from src to dst if it exists. dst parent dir must exist.
func copyFileIntoBundle(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	if err != nil {
		return fmt.Errorf("failed to copy %s to %s: %w", src, dst, err)
	}
	return nil
}

// createTarball creates a gzipped tar of the given directory and returns the bytes.
func createTarball(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	baseDir := filepath.Base(dir)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.Join("nvke-support-bundle", baseDir, rel)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
