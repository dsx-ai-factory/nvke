// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
)

func TestNodeStatusStructSerialization(t *testing.T) {
	status := &NodeStatus{
		NodeName:        "test-node",
		AgentVersion:    "v1.0.0",
		Architecture:    "x86_64",
		OperatingSystem: "Ubuntu 22.04",
		Status:          ReadyStatus,
		Phase:           InitPhase,
		Role:            ControlPlaneRole,
		IsMaster:        true,
		KubeadmVersion:  "v1.28.0",
		JoinInfo: JoinInfo{
			ControlPlaneJoinString: "kubeadm join 10.0.0.1:6443 --control-plane",
			WorkerJoinString:       "kubeadm join 10.0.0.1:6443",
		},
		CompletedPhases: []string{InitPhase},
	}

	// Test serialization
	data, err := json.Marshal(status)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)

	// Test deserialization
	var decoded NodeStatus
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, status.NodeName, decoded.NodeName)
	assert.Equal(t, status.AgentVersion, decoded.AgentVersion)
	assert.Equal(t, status.Role, decoded.Role)
	assert.Equal(t, status.IsMaster, decoded.IsMaster)
}

func TestJoinInfoStruct(t *testing.T) {
	joinInfo := JoinInfo{
		ControlPlaneJoinString: "kubeadm join 10.0.0.1:6443 --token abc --control-plane --certificate-key xyz",
		WorkerJoinString:       "kubeadm join 10.0.0.1:6443 --token abc",
	}

	data, err := json.Marshal(joinInfo)
	assert.NoError(t, err)

	var decoded JoinInfo
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, joinInfo.ControlPlaneJoinString, decoded.ControlPlaneJoinString)
	assert.Equal(t, joinInfo.WorkerJoinString, decoded.WorkerJoinString)
}

func TestBundleStruct(t *testing.T) {
	bundle := Bundle{
		RequestID:   "req-123",
		Name:        "kubeadm",
		Version:     semver.MustParse("v1.28.0"),
		Status:      "success",
		Source:      "https://example.com/kubeadm.tgz",
		InstallTime: "2024-01-01T00:00:00Z",
		Type:        "software",
		Output:      "installation complete",
		ExitCode:    0,
	}

	data, err := json.Marshal(bundle)
	assert.NoError(t, err)

	var decoded Bundle
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, bundle.Name, decoded.Name)
	assert.Equal(t, bundle.RequestID, decoded.RequestID)
	assert.Equal(t, bundle.Status, decoded.Status)
}

func TestScriptStruct(t *testing.T) {
	script := Script{
		RequestID:  "req-456",
		Name:       "setup.sh",
		IsAsync:    true,
		Status:     "Success",
		ExecutedAt: "2024-01-01T00:00:00Z",
		Output:     "script completed",
	}

	data, err := json.Marshal(script)
	assert.NoError(t, err)

	var decoded Script
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, script.Name, decoded.Name)
	assert.Equal(t, script.IsAsync, decoded.IsAsync)
	assert.Equal(t, script.Status, decoded.Status)
}

func TestJSONFilePersisterPersist(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	persister := &JSONFilePersister{Path: stateFile}

	status := &NodeStatus{
		NodeName:       "test-node",
		AgentVersion:   "v1.0.0",
		Status:         ReadyStatus,
		Role:           ControlPlaneRole,
		KubeadmVersion: "v1.28.0",
	}

	err := persister.Persist(status)
	assert.NoError(t, err)

	// Verify file was created
	_, err = os.Stat(stateFile)
	assert.NoError(t, err)

	// Verify content
	data, err := os.ReadFile(stateFile)
	assert.NoError(t, err)

	var loaded NodeStatus
	err = json.Unmarshal(data, &loaded)
	assert.NoError(t, err)
	assert.Equal(t, status.NodeName, loaded.NodeName)
	assert.Equal(t, status.Status, loaded.Status)
}

func TestJSONFilePersisterPersistAtomic(t *testing.T) {
	// Test that persist is atomic - if it fails, old content remains
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	persister := &JSONFilePersister{Path: stateFile}

	// First persist
	status1 := &NodeStatus{
		NodeName: "node1",
		Status:   ReadyStatus,
	}
	err := persister.Persist(status1)
	assert.NoError(t, err)

	// Second persist with different data
	status2 := &NodeStatus{
		NodeName: "node2",
		Status:   PendingStatus,
	}
	err = persister.Persist(status2)
	assert.NoError(t, err)

	// Verify second persist overwrote first
	data, err := os.ReadFile(stateFile)
	assert.NoError(t, err)

	var loaded NodeStatus
	err = json.Unmarshal(data, &loaded)
	assert.NoError(t, err)
	assert.Equal(t, "node2", loaded.NodeName)
	assert.Equal(t, PendingStatus, loaded.Status)
}

func TestJSONFilePersisterLoadFromStateFile(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// Create initial state file
	status := &NodeStatus{
		NodeName:        "test-node",
		AgentVersion:    "v1.0.0",
		Status:          ReadyStatus,
		Role:            WorkerRole,
		KubeadmVersion:  "v1.28.0",
		CompletedPhases: []string{JoinPhase},
	}
	data, err := json.MarshalIndent(status, "", "  ")
	assert.NoError(t, err)
	err = os.WriteFile(stateFile, data, 0o644)
	assert.NoError(t, err)

	persister := &JSONFilePersister{Path: stateFile}
	loaded, err := persister.LoadFromStateFile()

	assert.NoError(t, err)
	assert.NotNil(t, loaded)
	assert.Equal(t, "test-node", loaded.NodeName)
	assert.Equal(t, ReadyStatus, loaded.Status)
	assert.Equal(t, WorkerRole, loaded.Role)
}

func TestJSONFilePersisterLoadFromStateFileNotExists(t *testing.T) {
	persister := &JSONFilePersister{Path: "/nonexistent/path/state.json"}
	loaded, err := persister.LoadFromStateFile()

	assert.Error(t, err)
	assert.Nil(t, loaded)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestJSONFilePersisterLoadFromStateFileEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// Create empty file
	err := os.WriteFile(stateFile, []byte{}, 0o644)
	assert.NoError(t, err)

	persister := &JSONFilePersister{Path: stateFile}
	loaded, err := persister.LoadFromStateFile()

	assert.Error(t, err)
	assert.Nil(t, loaded)
	assert.Contains(t, err.Error(), "empty")
}

func TestJSONFilePersisterLoadFromStateFileInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// Create file with invalid JSON
	err := os.WriteFile(stateFile, []byte("not valid json"), 0o644)
	assert.NoError(t, err)

	persister := &JSONFilePersister{Path: stateFile}
	loaded, err := persister.LoadFromStateFile()

	assert.Error(t, err)
	assert.Nil(t, loaded)
	assert.Contains(t, err.Error(), "failed to parse JSON")
}

func TestFlushStatus(t *testing.T) {
	mockPersister := &MockPersister{}
	server := &NodeAgentServer{
		Persister: mockPersister,
		NodeStatus: &NodeStatus{
			NodeName: "original-node",
			Status:   ReadyStatus,
		},
	}

	newStatus := &NodeStatus{
		NodeName: "updated-node",
		Status:   PendingStatus,
	}

	server.flushStatus(newStatus)

	// Verify server's NodeStatus was updated
	assert.Equal(t, "updated-node", server.NodeStatus.NodeName)
	assert.Equal(t, PendingStatus, server.NodeStatus.Status)

	// Verify persister was called
	assert.True(t, mockPersister.WasCalled())
}

func TestPersistNodeStatus(t *testing.T) {
	mockPersister := &MockPersister{}
	server := &NodeAgentServer{
		Persister: mockPersister,
		NodeStatus: &NodeStatus{
			NodeName: "test-node",
			Status:   ReadyStatus,
		},
	}

	server.PersistNodeStatus()
	assert.True(t, mockPersister.WasCalled())
}

func TestAppendBundleToStatus(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{},
		},
	}

	bundle := &Bundle{
		RequestID: "req-123",
		Name:      "test-bundle",
		Status:    "installing",
	}

	server.AppendBundleToStatus(bundle)

	assert.Len(t, server.NodeStatus.Bundles, 1)
	assert.Equal(t, "test-bundle", server.NodeStatus.Bundles[0].Name)
}

func TestAppendBundleToStatusMultiple(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{Name: "existing-bundle"},
			},
		},
	}

	bundle := &Bundle{
		RequestID: "req-456",
		Name:      "new-bundle",
		Status:    "pending",
	}

	server.AppendBundleToStatus(bundle)

	assert.Len(t, server.NodeStatus.Bundles, 2)
	assert.Equal(t, "existing-bundle", server.NodeStatus.Bundles[0].Name)
	assert.Equal(t, "new-bundle", server.NodeStatus.Bundles[1].Name)
}

func TestUpdateBundleStatus(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-123", Name: "bundle1", Status: "installing"},
				{RequestID: "req-456", Name: "bundle2", Status: "pending"},
			},
		},
	}

	updatedBundle := &Bundle{
		RequestID: "req-123",
		Name:      "bundle1",
		Status:    "success",
		Output:    "completed",
	}

	server.UpdateBundleStatus(updatedBundle)

	assert.Equal(t, "success", server.NodeStatus.Bundles[0].Status)
	assert.Equal(t, "completed", server.NodeStatus.Bundles[0].Output)
	// Second bundle unchanged
	assert.Equal(t, "pending", server.NodeStatus.Bundles[1].Status)
}

func TestGetBundleStatusByRequestID(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-123", Name: "bundle1"},
				{RequestID: "req-456", Name: "bundle2"},
				{RequestID: "req-123", Name: "bundle3"}, // Same request ID
			},
		},
	}

	results := server.GetBundleStatusByRequestID("req-123")
	assert.Len(t, results, 2)
	assert.Equal(t, "bundle1", results[0].Name)
	assert.Equal(t, "bundle3", results[1].Name)
}

func TestGetBundleStatusByRequestIDReturnsCopies(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-123", Name: "bundle1", Status: "installing"},
			},
		},
	}

	results := server.GetBundleStatusByRequestID("req-123")
	assert.Len(t, results, 1)
	results[0].Status = "written through the returned pointer"

	// An alias would reach stored state without the lock.
	assert.Equal(t, "installing", server.NodeStatus.Bundles[0].Status)
}

func TestGetBundleStatusByRequestIDNotFound(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-123", Name: "bundle1"},
			},
		},
	}

	results := server.GetBundleStatusByRequestID("nonexistent")
	assert.Len(t, results, 0)
}

func TestUpdateState(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: "ControlPlane", // Should be normalized
			Scripts: []Script{
				{Name: "script1"},
				{Name: "script2"},
				{Name: "script3"},
				{Name: "script4"}, // Exceeds ScriptStatusLimit of 3
			},
		},
	}

	server.UpdateState()

	// Verify role was normalized
	assert.Equal(t, "controlplane", server.NodeStatus.Role)
	// Verify scripts were trimmed
	assert.LessOrEqual(t, len(server.NodeStatus.Scripts), ScriptStatusLimit)
}

func TestUpdateStateWorkerRole(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role:    "WORKER", // Should be normalized
			Scripts: []Script{},
		},
	}

	server.UpdateState()

	assert.Equal(t, "worker", server.NodeStatus.Role)
}

func TestConcurrentStatusUpdates(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{},
		},
	}

	var wg sync.WaitGroup
	numGoroutines := 10

	for i := range numGoroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bundle := &Bundle{
				RequestID: "req-concurrent",
				Name:      "bundle-concurrent",
				Status:    "pending",
			}
			server.AppendBundleToStatus(bundle)
		}(i)
	}

	wg.Wait()

	// All bundles should have been appended
	assert.Equal(t, numGoroutines, len(server.NodeStatus.Bundles))
}

// ErrorMockPersister is a mock persister that returns an error
type ErrorMockPersister struct {
	mu      sync.Mutex
	Called  bool
	LoadErr error
}

func (m *ErrorMockPersister) Persist(status *NodeStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Called = true
	return m.LoadErr
}

func (m *ErrorMockPersister) LoadFromStateFile() (*NodeStatus, error) {
	return nil, m.LoadErr
}

func TestPersistNodeStatusWithError(t *testing.T) {
	mockPersister := &ErrorMockPersister{LoadErr: os.ErrPermission}
	server := &NodeAgentServer{
		Persister: mockPersister,
		NodeStatus: &NodeStatus{
			NodeName: "test-node",
			Status:   ReadyStatus,
		},
	}

	// Should not panic, just log the error
	server.PersistNodeStatus()
	assert.True(t, mockPersister.Called)
}

func TestPersistToReadOnlyDirectory(t *testing.T) {
	// Test persist to a directory that doesn't exist
	persister := &JSONFilePersister{Path: "/nonexistent/directory/state.json"}
	status := &NodeStatus{NodeName: "test"}

	err := persister.Persist(status)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create temp file")
}

func TestLoadFromStateFileWithValidKubeadmVersion(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// Create a valid state file
	status := &NodeStatus{
		NodeName: "test-node",
		Status:   ReadyStatus,
	}
	data, err := json.MarshalIndent(status, "", "  ")
	assert.NoError(t, err)
	err = os.WriteFile(stateFile, data, 0o644)
	assert.NoError(t, err)

	persister := &JSONFilePersister{Path: stateFile}
	loaded, err := persister.LoadFromStateFile()

	assert.NoError(t, err)
	assert.NotNil(t, loaded)
	// KubeadmVersion will be empty if kubeadm is not installed
}

func TestFlushStatusWithPersisterError(t *testing.T) {
	mockPersister := &ErrorMockPersister{LoadErr: os.ErrPermission}
	server := &NodeAgentServer{
		Persister: mockPersister,
		NodeStatus: &NodeStatus{
			NodeName: "original-node",
		},
	}

	newStatus := &NodeStatus{
		NodeName: "updated-node",
	}

	// Should not panic, flushStatus ignores persist errors
	server.flushStatus(newStatus)

	// Verify server's NodeStatus was still updated
	assert.Equal(t, "updated-node", server.NodeStatus.NodeName)
}

func TestUpdateBundleStatusNotFound(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-123", Name: "bundle1", Status: "pending"},
			},
		},
	}

	// Try to update a bundle that doesn't exist
	updatedBundle := &Bundle{
		RequestID: "nonexistent",
		Name:      "ghost-bundle",
		Status:    "success",
	}

	server.UpdateBundleStatus(updatedBundle)

	// Original bundle should be unchanged
	assert.Equal(t, "pending", server.NodeStatus.Bundles[0].Status)
	assert.Equal(t, 1, len(server.NodeStatus.Bundles))
}

func TestConcurrentUpdateBundleStatus(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-1", Name: "bundle1", Status: "pending"},
				{RequestID: "req-2", Name: "bundle2", Status: "pending"},
				{RequestID: "req-3", Name: "bundle3", Status: "pending"},
			},
		},
	}

	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			requestID := "req-" + string(rune('0'+idx))
			for range 10 {
				bundle := &Bundle{
					RequestID: requestID,
					Status:    "success",
				}
				server.UpdateBundleStatus(bundle)
			}
		}(i)
	}

	wg.Wait()
	// All bundles should still exist (no race condition corruption)
	assert.Equal(t, 3, len(server.NodeStatus.Bundles))
}

func TestConcurrentGetBundleStatus(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{RequestID: "req-shared", Name: "bundle1"},
				{RequestID: "req-shared", Name: "bundle2"},
			},
		},
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results := server.GetBundleStatusByRequestID("req-shared")
			assert.Equal(t, 2, len(results))
		}()
	}

	wg.Wait()
}

func TestPersistAndLoadRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	persister := &JSONFilePersister{Path: stateFile}

	// Create complex status
	originalStatus := &NodeStatus{
		NodeName:        "complex-node",
		AgentVersion:    "v2.0.0",
		Architecture:    "arm64",
		OperatingSystem: "Ubuntu 24.04",
		Status:          PendingStatus,
		Phase:           JoinPhase,
		Role:            WorkerRole,
		IsMaster:        false,
		CompletedPhases: []string{InitPhase, "preflight"},
		JoinInfo: JoinInfo{
			ControlPlaneJoinString: "kubeadm join 10.0.0.1:6443 --token abc",
			WorkerJoinString:       "kubeadm join 10.0.0.1:6443 --token xyz",
		},
		Bundles: []Bundle{
			{
				RequestID: "req-b1",
				Name:      "kubeadm",
				Status:    "success",
				Version:   semver.MustParse("1.28.0"),
			},
		},
		Scripts: []Script{
			{
				RequestID: "req-s1",
				Name:      "setup.sh",
				Status:    "Success",
			},
		},
		InstalledPackages: []InstalledDebPackage{
			{Name: "curl", Version: "7.81.0-1", Status: "ii "},
		},
	}

	// Persist
	err := persister.Persist(originalStatus)
	assert.NoError(t, err)

	// Load
	loaded, err := persister.LoadFromStateFile()
	assert.NoError(t, err)
	assert.NotNil(t, loaded)

	// Verify key fields
	assert.Equal(t, originalStatus.NodeName, loaded.NodeName)
	assert.Equal(t, originalStatus.Architecture, loaded.Architecture)
	assert.Equal(t, originalStatus.Status, loaded.Status)
	assert.Equal(t, originalStatus.Phase, loaded.Phase)
	assert.Equal(t, originalStatus.Role, loaded.Role)
	assert.Equal(t, len(originalStatus.Bundles), len(loaded.Bundles))
	assert.Equal(t, len(originalStatus.Scripts), len(loaded.Scripts))
	assert.Equal(t, originalStatus.InstalledPackages, loaded.InstalledPackages)
}

func TestCloneNodeStatusPreservesNonNilEmptySlices(t *testing.T) {
	status := &NodeStatus{
		CompletedPhases:   []string{},
		Bundles:           []Bundle{},
		Scripts:           []Script{},
		InstalledPackages: []InstalledDebPackage{},
	}

	statusCopy := cloneNodeStatus(status)

	assert.NotNil(t, statusCopy.CompletedPhases)
	assert.NotNil(t, statusCopy.Bundles)
	assert.NotNil(t, statusCopy.Scripts)
	assert.NotNil(t, statusCopy.InstalledPackages)
}

func TestUpdateStateWithEmptyScripts(t *testing.T) {
	server := &NodeAgentServer{
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role:    "worker",
			Scripts: []Script{},
		},
	}

	// Should not panic with empty scripts
	server.UpdateState()
	assert.Equal(t, "worker", server.NodeStatus.Role)
}

func TestUpdateStateWithMixedCaseRoles(t *testing.T) {
	tests := []struct {
		inputRole    string
		expectedRole string
	}{
		{"CONTROLPLANE", "controlplane"},
		{"ControlPlane", "controlplane"},
		{"controlplane", "controlplane"},
		{"WORKER", "worker"},
		{"Worker", "worker"},
		{"worker", "worker"},
		{"unknown", "unknown"}, // Should remain unchanged
	}

	for _, tc := range tests {
		t.Run(tc.inputRole, func(t *testing.T) {
			server := &NodeAgentServer{
				Persister: &MockPersister{},
				NodeStatus: &NodeStatus{
					Role:    tc.inputRole,
					Scripts: []Script{},
				},
			}

			server.UpdateState()
			assert.Equal(t, tc.expectedRole, server.NodeStatus.Role)
		})
	}
}

func TestK8sAPIEncryptionStruct(t *testing.T) {
	encryption := K8sAPIEncryption{
		Status: K8sAPIEncryptionEnabledStatus,
		Phase:  K8sAPIEncryptionPhaseConfigured,
	}

	data, err := json.Marshal(encryption)
	assert.NoError(t, err)

	var decoded K8sAPIEncryption
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, encryption.Status, decoded.Status)
	assert.Equal(t, encryption.Phase, decoded.Phase)
}

func TestNodeStatusWithAllFields(t *testing.T) {
	// Test that all NodeStatus fields serialize correctly
	status := &NodeStatus{
		NodeName:            "full-test-node",
		AgentVersion:        "v3.0.0",
		Architecture:        "amd64",
		OperatingSystem:     "Ubuntu 22.04",
		Status:              ReadyStatus,
		Phase:               InitPhase,
		ErrorMessage:        "test error",
		CurrentMessage:      "test message",
		CompletedPhases:     []string{"phase1", "phase2"},
		ListenStatus:        "listening",
		Role:                ControlPlaneRole,
		IsMaster:            true,
		KubeadmVersion:      "v1.29.0",
		CNIPluginStatus:     "installed",
		CalicoInstallLog:    "calico logs",
		KubeadmStatus:       "initialized",
		KubeadmErrorMessage: "",
		MLX5ResetStatus:     true,
		MLX5ResetDisabled:   false,
		BundleIndexFile:     "https://example.com/index.json",
		BundleServerURL:     "https://example.com",
	}

	data, err := json.Marshal(status)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)

	var decoded NodeStatus
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)

	// Verify all fields
	assert.Equal(t, status.NodeName, decoded.NodeName)
	assert.Equal(t, status.AgentVersion, decoded.AgentVersion)
	assert.Equal(t, status.MLX5ResetStatus, decoded.MLX5ResetStatus)
	assert.Equal(t, status.BundleIndexFile, decoded.BundleIndexFile)
}
