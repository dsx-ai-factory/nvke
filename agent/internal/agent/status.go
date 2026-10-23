// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
)

type RealPersister struct{}

type JSONFilePersister struct {
	Path string
}

type NodeStatus struct {
	NodeName        string   `json:"name"`
	AgentVersion    string   `json:"agentVersion"`
	Architecture    string   `json:"architecture"`
	OperatingSystem string   `json:"operatingSystem"`
	Status          string   `json:"status"`
	Phase           string   `json:"phase"`
	ErrorMessage    string   `json:"errorMessage"`
	CurrentMessage  string   `json:"currentMessage"`
	CompletedPhases []string `json:"completedPhases"`
	ListenStatus    string   `json:"listenStatus"`
	Role            string   `json:"role"`
	IsMaster        bool     `json:"isMaster"`
	KubeadmVersion  string   `json:"kubeadmVersion"`
	Bundles         []Bundle `json:"bundles"`
	Scripts         []Script `json:"scripts"`
	JoinInfo        JoinInfo `json:"joinInfo"`

	CNIPluginStatus  string `json:"cniPluginStatus"`
	CNIPlugin        string `json:"cniPlugin"`
	CNIPluginVersion string `json:"cniPluginVersion"`

	K8sAPIEncryption      K8sAPIEncryption              `json:"k8sAPIEncryption"`
	K8sAPIRewrite         K8sAPIRewriteOperation        `json:"k8sAPIRewriteOperation"`
	NodeAgentOperations   map[string]NodeAgentOperation `json:"nodeAgentOperations,omitempty"`
	K8sCertRenewalState   string                        `json:"k8sCertRenewalState,omitempty"`
	KubetrustIsConfigured bool                          `json:"kubetrustIsConfigured"`
	CalicoInstallLog      string                        `json:"calicoInstallLog"`
	KubeadmStatus         string                        `json:"kubeadmStatus"`
	KubeadmErrorMessage   string                        `json:"kubeadmErrorMessage"`

	MLX5ResetStatus   bool `json:"mlx5ResetStatus"`
	MLX5ResetDisabled bool `json:"mlx5ResetDisabled"`

	BundleIndexFile string `json:"bundleIndexFile"`
	BundleServerURL string `json:"bundleServerURL"`

	// InstalledPackages is the dpkg list of installed packages, refreshed after InstallPackages
	// and RemovePackages succeed (same source as ListPackages).
	InstalledPackages []InstalledDebPackage `json:"installedPackages"`
}

func cloneNodeStatus(status *NodeStatus) NodeStatus {
	if status == nil {
		return NodeStatus{}
	}
	statusCopy := *status
	statusCopy.CompletedPhases = cloneStringSlice(status.CompletedPhases)
	statusCopy.Bundles = cloneBundles(status.Bundles)
	statusCopy.Scripts = cloneScripts(status.Scripts)
	statusCopy.NodeAgentOperations = cloneNodeAgentOperations(status.NodeAgentOperations)
	statusCopy.InstalledPackages = cloneInstalledDebPackages(status.InstalledPackages)
	return statusCopy
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBundles(in []Bundle) []Bundle {
	if in == nil {
		return nil
	}
	out := make([]Bundle, len(in))
	for i, bundle := range in {
		out[i] = cloneBundle(bundle)
	}
	return out
}

func cloneBundle(in Bundle) Bundle {
	if in.Version != nil {
		version := *in.Version
		in.Version = &version
	}
	return in
}

func cloneScripts(in []Script) []Script {
	if in == nil {
		return nil
	}
	out := make([]Script, len(in))
	copy(out, in)
	return out
}

func cloneNodeAgentOperations(in map[string]NodeAgentOperation) map[string]NodeAgentOperation {
	if in == nil {
		return nil
	}
	out := make(map[string]NodeAgentOperation, len(in))
	for id, op := range in {
		out[id] = op
	}
	return out
}

func cloneInstalledDebPackages(in []InstalledDebPackage) []InstalledDebPackage {
	if in == nil {
		return nil
	}
	out := make([]InstalledDebPackage, len(in))
	copy(out, in)
	return out
}

type K8sAPIRewriteState string

const (
	K8sAPIRewriteStateRunning   K8sAPIRewriteState = "running"
	K8sAPIRewriteStateSucceeded K8sAPIRewriteState = "succeeded"
	K8sAPIRewriteStateFailed    K8sAPIRewriteState = "failed"
)

type K8sAPIRewriteOperation struct {
	ID           string             `json:"id"`
	State        K8sAPIRewriteState `json:"state"`
	ErrorMessage string             `json:"errorMessage"`
}

type NodeAgentOperationPhase string

const (
	NodeAgentOperationPhaseRunning   NodeAgentOperationPhase = "running"
	NodeAgentOperationPhaseSucceeded NodeAgentOperationPhase = "succeeded"
	NodeAgentOperationPhaseFailed    NodeAgentOperationPhase = "failed"
)

type NodeAgentOperation struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Phase        NodeAgentOperationPhase `json:"phase"`
	Step         string                  `json:"step"`
	Result       string                  `json:"result,omitempty"`
	ErrorMessage string                  `json:"errorMessage"`
}

// InstalledDebPackage mirrors deb.Package / proto InstalledPackage for persisted node status.
type InstalledDebPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

type JoinInfo struct {
	ControlPlaneJoinString string `json:"controlPlaneJoinString"`
	WorkerJoinString       string `json:"workerJoinString"`
}
type Bundle struct {
	RequestID   string          `json:"requestId"`
	Name        string          `json:"name"`
	Source      string          `json:"source"`
	Version     *semver.Version `json:"bundleVersion"`
	Status      string          `json:"status"`
	Error       string          `json:"error"`
	InstallTime string          `json:"installedAt"`
	Type        string          `json:"bundleType"`
	Output      string          `json:"output"`
	ExitCode    int32           `json:"exitCode"`
}

type Script struct {
	RequestID  string `json:"requestId"`
	Name       string `json:"name"`
	IsAsync    bool   `json:"isAsync"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	ExecutedAt string `json:"executedAt"`
	Output     string `json:"output"`
}

// flushStatus persists/logs a *snapshot only* (NO WRITES here).
func (s *NodeAgentServer) flushStatus(ns *NodeStatus) {
	s.mu.Lock()
	*s.NodeStatus = cloneNodeStatus(ns)
	statusCopy := cloneNodeStatus(s.NodeStatus)
	s.mu.Unlock()
	s.Persister.Persist(&statusCopy) //nolint:errcheck
}

func (s *NodeAgentServer) UpdateState() {
	// trimScriptStatus takes a lock so don't add it somewhere that is also already
	// holding a lock
	s.trimScriptStatus()
	v, err := getShortKubeadmVersion()
	if err != nil {
		slog.Warn("failed to get kubeadm version, will set to KubeadmVersion to empty string", "error", err)
		v = ""
	}

	s.mu.Lock()
	s.NodeStatus.KubeadmVersion = v

	if strings.EqualFold(s.NodeStatus.Role, ControlPlaneRole) {
		s.NodeStatus.Role = ControlPlaneRole
	}

	if strings.EqualFold(s.NodeStatus.Role, WorkerRole) {
		s.NodeStatus.Role = WorkerRole
	}
	s.mu.Unlock()
}

func (p *JSONFilePersister) LoadFromStateFile() (*NodeStatus, error) {
	var status NodeStatus
	slog.Info("loading state file", "path", p.Path)

	// Check file existence
	_, err := os.Stat(p.Path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("state file does not exist: %s", p.Path)
	} else if err != nil {
		return nil, err
	}

	// Read full file
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("state file is empty: %s", p.Path)
	}

	// Try parsing
	if err := json.Unmarshal(data, &status); err != nil {
		// Debug: show first few bytes in error
		return nil, fmt.Errorf("failed to parse JSON (size=%d): %w\nFirst 100 bytes: %q", len(data), err, data[:min(100, len(data))])
	}

	// Update kubeadm version
	kubeadmVersion, err := getShortKubeadmVersion()
	if err != nil {
		if err.Error() == "kubeadm not found" {
			slog.Info("kubeadm not installed yet")
		} else {
			slog.Error("failed to capture kubeadm version", "err", err)
		}
	} else {
		status.KubeadmVersion = kubeadmVersion
	}

	return &status, nil
}

func (p *JSONFilePersister) Persist(status *NodeStatus) error {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Create a temporary file in the same directory
	dir := filepath.Dir(p.Path)
	tmpFile, err := os.CreateTemp(dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	// Ensure cleanup on error
	defer func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}()

	// Write data
	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Flush file to disk
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	// Close before renaming
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomically replace the old file
	if err := os.Rename(tmpName, p.Path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}

func (s *NodeAgentServer) PersistNodeStatus() {
	s.mu.Lock()
	statusCopy := cloneNodeStatus(s.NodeStatus)
	s.mu.Unlock()

	err := s.Persister.Persist(&statusCopy)
	if err != nil {
		slog.Error("failed to persist NodeStatusSnapshot", "ERR", err)
	}
}

func (s *NodeAgentServer) AppendBundleToStatus(bundle *Bundle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NodeStatus.Bundles = append(s.NodeStatus.Bundles, *bundle)
}

func (s *NodeAgentServer) UpdateBundleStatus(bundle *Bundle) {
	s.mu.Lock()
	for idx, b := range s.NodeStatus.Bundles {
		if b.RequestID == bundle.RequestID && b.Name == bundle.Name {
			s.NodeStatus.Bundles[idx] = *bundle
			break
		}
	}
	s.mu.Unlock()
}

// GetBundleStatusByRequestID returns copies of all bundles with the given
// request ID. If no bundles are found, an empty slice is returned.
//
// Copies, because a pointer into s.NodeStatus.Bundles outlives the lock and
// UpdateBundleStatus writes those fields from the async install goroutine.
func (s *NodeAgentServer) GetBundleStatusByRequestID(requestID string) []*Bundle {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Bundle
	for i := range s.NodeStatus.Bundles {
		if s.NodeStatus.Bundles[i].RequestID == requestID {
			bundle := s.NodeStatus.Bundles[i]
			out = append(out, &bundle)
		}
	}

	return out
}
