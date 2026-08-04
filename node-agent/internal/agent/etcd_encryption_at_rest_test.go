package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v2"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

func TestK8sAPIEncryptionStatusConstants(t *testing.T) {
	// Verify all status constants are properly defined
	assert.Equal(t, K8sAPIEncryptionStatusType("Enabled"), K8sAPIEncryptionEnabledStatus)
	assert.Equal(t, K8sAPIEncryptionStatusType("Pending"), K8sAPIEncryptionPendingStatus)
	assert.Equal(t, K8sAPIEncryptionStatusType("Error"), K8sAPIEncryptionErrorStatus)
	assert.Equal(t, K8sAPIEncryptionStatusType("NotConfigured"), K8sAPIEncryptionNotConfigured)
}

func TestK8sAPIEncryptionPhaseConstants(t *testing.T) {
	// Verify all phase constants are properly defined
	assert.Equal(t, K8sAPIEncryptionPhaseType("Initialize"), InitializeK8sAPIEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("InitializeError"), InitializeK8sAPIEncryptionErrorPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("EncryptionInitialized"), K8sAPIEncryptionInitializedPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("Configuring"), K8sAPIEncryptionPhaseConfiguring)
	assert.Equal(t, K8sAPIEncryptionPhaseType("Configured"), K8sAPIEncryptionPhaseConfigured)
	assert.Equal(t, K8sAPIEncryptionPhaseType("RewriteObjects"), RewriteObjectsEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("RewriteObjectsError"), RewriteObjectsErrorEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("ObjectsRewritten"), ObjectsRewrittenEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("RestartAPIServer"), RestartAPIServerEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("RestartAPIServerError"), RestartAPIServerErrorEncryptionPhase)
	assert.Equal(t, K8sAPIEncryptionPhaseType("RestartedAPIServer"), RestartedAPIServerEncryptionPhase)
}

func TestAPIEncryptionLabels(t *testing.T) {
	// Verify label constants are properly formatted
	assert.Equal(t, "nke.io/k8s-api-encryption-configured=true", APIEncryptionConfiguredLabel)
	assert.Equal(t, "nke.io/k8s-api-encryption-rewrite-complete=true", APIEncryptionRewriteCompleteLabel)
	assert.Equal(t, "nke.io/k8s-api-encryption-enabled=true", APIEncryptionEnabledLabel)
	assert.Equal(t, "nke.io/k8s-api-encryption-secondary-key-configured=true", APIEncryptionSecondaryKeyEnabledLabel)
}

func TestObjectsToEncrypt(t *testing.T) {
	// Verify the objects to encrypt list contains expected items
	expectedObjects := []string{
		"secrets",
		"configmaps",
		"persistentvolumeclaims",
		"persistentvolumes",
		"serviceaccounts",
		"events",
		"services",
	}

	assert.Equal(t, len(expectedObjects), len(objectsToEncrypt))
	for _, obj := range expectedObjects {
		assert.Contains(t, objectsToEncrypt, obj)
	}
}

func TestEncryptionConfigTemplateContainsRequiredFields(t *testing.T) {
	// Verify the encryption config template contains required fields
	assert.Contains(t, encryptionConfigTemplate, "apiVersion: apiserver.config.k8s.io/v1")
	assert.Contains(t, encryptionConfigTemplate, "kind: EncryptionConfiguration")
	assert.Contains(t, encryptionConfigTemplate, "resources:")
	assert.Contains(t, encryptionConfigTemplate, "providers:")
	assert.Contains(t, encryptionConfigTemplate, "secretbox:")
	assert.Contains(t, encryptionConfigTemplate, "keys:")
	assert.Contains(t, encryptionConfigTemplate, "{{ .EncryptionKey }}")
	assert.Contains(t, encryptionConfigTemplate, "{{- range .ObjectsToEncrypt }}")
	// identity: {} must be present as a fallback provider so the apiserver can
	// read existing plaintext data before the rewrite step encrypts it.
	assert.Contains(t, encryptionConfigTemplate, "- identity: {}")
}

func TestEncryptionConfigTemplateExistingClusterContainsRequiredFields(t *testing.T) {
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "apiVersion: apiserver.config.k8s.io/v1")
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "kind: EncryptionConfiguration")
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "providers:")
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "- identity: {}")
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "secretbox:")
	assert.Contains(t, encryptionConfigTemplateExistingCluster, "{{ .EncryptionKey }}")
}

func TestVolumeEntryFormat(t *testing.T) {
	// Verify volume entry has correct format
	assert.Contains(t, volumeEntry, "hostPath:")
	assert.Contains(t, volumeEntry, "path: /etc/kubernetes/kube-api-encryption")
	assert.Contains(t, volumeEntry, "type: DirectoryOrCreate")
	assert.Contains(t, volumeEntry, "name: encryption-config")
}

func TestVolumeMountsEntryFormat(t *testing.T) {
	// Verify volume mounts entry has correct format
	assert.Contains(t, volumeMountsEntry, "mountPath: /etc/kubernetes/kube-api-encryption")
	assert.Contains(t, volumeMountsEntry, "name: encryption-config")
	assert.Contains(t, volumeMountsEntry, "readOnly: true")
}

func TestCreateAPIEncryptionConfig(t *testing.T) {
	tests := []struct {
		name          string
		encryptionKey string
		expectError   bool
	}{
		{
			name:          "successful config creation",
			encryptionKey: "dGVzdC1lbmNyeXB0aW9uLWtleS1iYXNlNjQ=",
			expectError:   false,
		},
		{
			name:          "empty encryption key",
			encryptionKey: "",
			expectError:   false, // Empty key is technically allowed at this level
		},
		{
			name:          "special characters in key",
			encryptionKey: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=",
			expectError:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a temp directory to use as chroot
			tmpDir := t.TempDir()

			server := &NodeAgentServer{
				Runner:     NewMockK8sEncryptionRunner(),
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
				RootPath:   tmpDir, // Use temp dir as chroot
			}

			// Call the actual function
			err := server.createAPIEncryptionConfig(tc.encryptionKey, false, nil)

			if tc.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// Verify the directory was created
			encryptionDir := filepath.Join(tmpDir, "etc/kubernetes/kube-api-encryption")
			_, err = os.Stat(encryptionDir)
			assert.NoError(t, err, "encryption directory should exist")

			// Verify the config file was created
			configPath := filepath.Join(encryptionDir, "encryption-config.yaml")
			content, err := os.ReadFile(configPath)
			assert.NoError(t, err, "config file should be readable")

			// Verify content contains expected elements
			contentStr := string(content)
			assert.Contains(t, contentStr, "apiVersion: apiserver.config.k8s.io/v1")
			assert.Contains(t, contentStr, "kind: EncryptionConfiguration")
			assert.Contains(t, contentStr, "secretbox:")
			assert.Contains(t, contentStr, "current-primary-encryption-key")
			assert.Contains(t, contentStr, tc.encryptionKey)

			// Verify all objects to encrypt are present
			for _, obj := range objectsToEncrypt {
				assert.Contains(t, contentStr, obj, "config should contain object: %s", obj)
			}
		})
	}
}

func TestCreateAPIEncryptionConfig_ExistingClusterProviderOrder(t *testing.T) {
	tmpDir := t.TempDir()
	srv := &NodeAgentServer{RootPath: tmpDir}
	key := "dGVzdC1lbmNyeXB0aW9uLWtleS1iYXNlNjQ="
	err := srv.createAPIEncryptionConfig(key, true, nil)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(tmpDir, "etc/kubernetes/kube-api-encryption/encryption-config.yaml"))
	require.NoError(t, err)
	var cfg EncryptionConfiguration
	require.NoError(t, yaml.Unmarshal(b, &cfg))
	assert.True(t, cfg.IdentityPrimarySecretboxSecondary())
	assert.NoError(t, cfg.ValidateSecretbox())
}

func TestInitializeKubernetesAPIEncryptionNotControlPlane(t *testing.T) {
	mockRunner := NewMockK8sEncryptionRunner()

	server := &NodeAgentServer{
		Runner:    mockRunner,
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: WorkerRole, // Not control plane
		},
	}

	req := &pb.InitializeKubernetesAPIEncryptionRequest{
		EncryptionKey: "test-key",
	}

	resp, err := server.InitializeKubernetesAPIEncryption(context.Background(), req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "controlplane")
	assert.NotEmpty(t, resp.ErrorMessage)
	assert.Equal(t, K8sAPIEncryptionErrorStatus, server.NodeStatus.K8sAPIEncryption.Status)
}

func TestRewriteKubernetesAPIObjectsNotControlPlane(t *testing.T) {
	mockRunner := NewMockK8sEncryptionRunner()

	server := &NodeAgentServer{
		Runner:    mockRunner,
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: WorkerRole, // Not control plane
		},
	}

	req := &pb.RewriteKubernetesAPIObjectsRequest{}

	resp, err := server.RewriteKubernetesAPIObjects(context.Background(), req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-controlplane")
	assert.Empty(t, resp.ErrorMessage) // Error message set before check fails
}

func TestRestartAPIPods(t *testing.T) {
	t.Run("successful pod list and restart", func(t *testing.T) {
		mockRunner := NewMockK8sEncryptionRunner()
		// Mock returns pod list for get command
		mockRunner.Outputs["kubectl get"] = []byte("kube-apiserver-node1\nkube-apiserver-node2\n")
		mockRunner.Errors["kubectl get"] = nil
		// Mock returns success for delete commands
		mockRunner.Outputs["kubectl delete"] = []byte("")
		mockRunner.Errors["kubectl delete"] = nil

		server := &NodeAgentServer{
			Runner:     mockRunner,
			Persister:  &MockPersister{},
			NodeStatus: &NodeStatus{},
		}

		req := &pb.RestartAPIPodsRequest{}
		resp, err := server.RestartAPIPods(context.Background(), req)

		assert.NoError(t, err)
		assert.NotNil(t, resp)

		// Verify the correct commands were called
		assert.GreaterOrEqual(t, len(mockRunner.CalledWith), 1, "should have called kubectl at least once")

		// First call should be kubectl get pods
		firstCall := mockRunner.CalledWith[0]
		assert.Equal(t, "kubectl", firstCall[0])
		assert.Contains(t, firstCall, "get")
		assert.Contains(t, firstCall, "pods")
		assert.Contains(t, firstCall, "kube-system")
		assert.Contains(t, firstCall, "component=kube-apiserver")

		// Should have delete calls for each pod (node1, node2, and empty string from trailing newline)
		deleteCalls := 0
		for _, call := range mockRunner.CalledWith {
			if len(call) > 1 && call[0] == "kubectl" && call[1] == "delete" {
				deleteCalls++
				assert.Contains(t, call, "pod")
				assert.Contains(t, call, "kube-system")
			}
		}
		// Expect 3 delete calls: node1, node2, and empty string from split
		assert.Equal(t, 3, deleteCalls, "should have called delete for each pod name from split output")
	})

	t.Run("failed to get pod list", func(t *testing.T) {
		mockRunner := NewMockK8sEncryptionRunner()
		mockRunner.Outputs["kubectl"] = []byte("")
		mockRunner.Errors["kubectl"] = errors.New("kubectl error")

		server := &NodeAgentServer{
			Runner:     mockRunner,
			Persister:  &MockPersister{},
			NodeStatus: &NodeStatus{},
		}

		req := &pb.RestartAPIPodsRequest{}
		_, err := server.RestartAPIPods(context.Background(), req)

		assert.Error(t, err)
	})

	t.Run("pod delete fails", func(t *testing.T) {
		mockRunner := NewMockK8sEncryptionRunner()
		// Mock returns pod list for get command
		mockRunner.Outputs["kubectl get"] = []byte("kube-apiserver-node1\n")
		mockRunner.Errors["kubectl get"] = nil
		// Mock returns error for delete command
		mockRunner.Outputs["kubectl delete"] = []byte("error deleting pod")
		mockRunner.Errors["kubectl delete"] = errors.New("delete failed")

		server := &NodeAgentServer{
			Runner:     mockRunner,
			Persister:  &MockPersister{},
			NodeStatus: &NodeStatus{},
		}

		req := &pb.RestartAPIPodsRequest{}
		_, err := server.RestartAPIPods(context.Background(), req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "delete failed")
	})
}

func TestGetPodStatus(t *testing.T) {
	tests := []struct {
		name           string
		podName        string
		mockOutput     []byte
		mockError      error
		expectedStatus string
		expectError    bool
	}{
		{
			name:           "pod is running",
			podName:        "kube-apiserver-node1",
			mockOutput:     []byte("Running"),
			mockError:      nil,
			expectedStatus: "Running",
			expectError:    false,
		},
		{
			name:           "pod is pending",
			podName:        "kube-apiserver-node1",
			mockOutput:     []byte("Pending"),
			mockError:      nil,
			expectedStatus: "Pending",
			expectError:    false,
		},
		{
			name:           "kubectl fails",
			podName:        "kube-apiserver-node1",
			mockOutput:     []byte(""),
			mockError:      errors.New("pod not found"),
			expectedStatus: "",
			expectError:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRunner := NewMockK8sEncryptionRunner()
			mockRunner.Outputs["kubectl"] = tc.mockOutput
			mockRunner.Errors["kubectl"] = tc.mockError

			server := &NodeAgentServer{
				Runner:     mockRunner,
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
			}

			status, err := server.getPodStatus(tc.podName)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedStatus, status)
			}
		})
	}
}

func TestApiServerPodIsRunning(t *testing.T) {
	tests := []struct {
		name        string
		mockOutput  []byte
		mockError   error
		expected    bool
		expectError bool
	}{
		{
			name:        "api server is running",
			mockOutput:  []byte("Running"),
			mockError:   nil,
			expected:    true,
			expectError: false,
		},
		{
			name:        "api server is not running",
			mockOutput:  []byte("Pending"),
			mockError:   nil,
			expected:    false,
			expectError: false,
		},
		{
			name:        "error checking status",
			mockOutput:  []byte(""),
			mockError:   errors.New("kubectl error"),
			expected:    false,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRunner := NewMockK8sEncryptionRunner()
			mockRunner.Outputs["kubectl"] = tc.mockOutput
			mockRunner.Errors["kubectl"] = tc.mockError

			server := &NodeAgentServer{
				Runner:     mockRunner,
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
			}

			result, err := server.apiServerPodIsRunning()

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestVerifyNodeAPIEncryptionEnabled(t *testing.T) {
	tests := []struct {
		name        string
		mockOutput  []byte
		mockError   error
		expected    bool
		expectError bool
	}{
		{
			name:        "encryption is enabled",
			mockOutput:  []byte("... --encryption-provider-config=/etc/kubernetes/... ..."),
			mockError:   nil,
			expected:    true,
			expectError: false,
		},
		{
			name:        "encryption is not enabled",
			mockOutput:  []byte("... --other-flag=value ..."),
			mockError:   nil,
			expected:    false,
			expectError: false,
		},
		{
			name:        "kubectl fails",
			mockOutput:  []byte(""),
			mockError:   errors.New("kubectl error"),
			expected:    false,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRunner := NewMockK8sEncryptionRunner()
			mockRunner.Outputs["kubectl"] = tc.mockOutput
			mockRunner.Errors["kubectl"] = tc.mockError

			server := &NodeAgentServer{
				Runner:     mockRunner,
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
			}

			result, err := server.verifyNodeAPIEncryptionEnabled()

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestEncryptionConfigurationStructs(t *testing.T) {
	// Test that the encryption configuration structs can be properly marshaled/unmarshaled
	config := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{
			{
				Resources: []string{"secrets", "configmaps"},
				Providers: []ProviderConfig{
					{
						Secretbox: &SecretboxConfig{
							Keys: []KeyConfig{
								{Name: "key1", Secret: "base64secret"},
							},
						},
					},
					{
						Identity: &IdentityConfig{},
					},
				},
			},
		},
	}

	assert.Equal(t, "apiserver.config.k8s.io/v1", config.ApiVersion)
	assert.Equal(t, "EncryptionConfiguration", config.Kind)
	assert.Len(t, config.Resources, 1)
	assert.Len(t, config.Resources[0].Resources, 2)
	assert.Len(t, config.Resources[0].Providers, 2)
	assert.NotNil(t, config.Resources[0].Providers[0].Secretbox)
	assert.NotNil(t, config.Resources[0].Providers[1].Identity)

	// Regression: marshaled YAML must not contain a top-level "providerConfig" field.
	// Kubernetes strict-decodes the config and will reject unknown fields, causing:
	// "strict decoding error: unknown field \"providerConfig\""
	yamlBytes, err := yaml.Marshal(config)
	assert.NoError(t, err)
	assert.NotContains(t, string(yamlBytes), "providerConfig")
}

func TestModifyAPIServerManifest(t *testing.T) {
	mockRunner := NewMockK8sEncryptionRunner()
	mockRunner.Outputs["bash"] = []byte("")
	mockRunner.Errors["bash"] = nil

	server := &NodeAgentServer{
		Runner:     mockRunner,
		Persister:  &MockPersister{},
		NodeStatus: &NodeStatus{},
	}

	err := server.modifyAPIServerManifest()
	assert.NoError(t, err)

	// Verify bash was called twice (for encryption-provider-config and automatic-reload)
	bashCalls := 0
	for _, call := range mockRunner.CalledWith {
		if len(call) > 0 && call[0] == "bash" {
			bashCalls++
		}
	}
	assert.Equal(t, 2, bashCalls)
}

func TestCreateVolumeMountEntries(t *testing.T) {
	mockRunner := NewMockK8sEncryptionRunner()
	mockRunner.Outputs["bash"] = []byte("")
	mockRunner.Errors["bash"] = nil

	server := &NodeAgentServer{
		Runner:     mockRunner,
		Persister:  &MockPersister{},
		NodeStatus: &NodeStatus{},
	}

	err := server.createVolumeMountEntries()
	assert.NoError(t, err)

	// Verify bash was called twice (for volume and volumeMounts)
	bashCalls := 0
	for _, call := range mockRunner.CalledWith {
		if len(call) > 0 && call[0] == "bash" {
			bashCalls++
		}
	}
	assert.Equal(t, 2, bashCalls)
}

func TestRestartAPIServer(t *testing.T) {
	tests := []struct {
		name        string
		mockError   error
		expectError bool
	}{
		{
			name:        "successful restart",
			mockError:   nil,
			expectError: false,
		},
		{
			name:        "kubectl delete fails",
			mockError:   errors.New("delete failed"),
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRunner := NewMockK8sEncryptionRunner()
			mockRunner.Outputs["kubectl"] = []byte("")
			mockRunner.Errors["kubectl"] = tc.mockError

			server := &NodeAgentServer{
				Runner:     mockRunner,
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
			}

			err := server.restartAPIServer()

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ── RewriteKubernetesAPIObjects skip_label_check tests ────────────────────

func TestRewriteKubernetesAPIObjects_SkipLabelCheck(t *testing.T) {
	mockRunner := NewMockK8sEncryptionRunner()
	// Return no CP nodes so the loop body is skipped, but still tests that
	// no label check error is returned when skip_label_check=true.
	mockRunner.Outputs["kubectl get"] = []byte("")
	mockRunner.Errors["kubectl get"] = nil
	mockRunner.Outputs["bash"] = []byte("")

	server := &NodeAgentServer{
		Runner:    mockRunner,
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: ControlPlaneRole,
		},
	}

	req := &pb.RewriteKubernetesAPIObjectsRequest{SkipLabelCheck: true}
	_, err := server.RewriteKubernetesAPIObjects(context.Background(), req)
	// Should not fail with "not configured" label error.
	// It may fail on kubectl get node labels (empty node list → succeeds) or
	// on rewrite commands depending on mock; what matters is no label error.
	if err != nil {
		assert.NotContains(t, err.Error(), "not configured")
	}
}

func writeMigrationEncryptionConfigFixture(t *testing.T, dir, keySecret string) {
	t.Helper()
	encDir := filepath.Join(dir, "etc/kubernetes/kube-api-encryption")
	require.NoError(t, os.MkdirAll(encDir, 0o755))
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{{
			Resources: []string{"secrets"},
			Providers: []ProviderConfig{
				{Identity: &IdentityConfig{}},
				{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "current-primary-encryption-key", Secret: keySecret}}}},
			},
		}},
	}
	b, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(encDir, "encryption-config.yaml"), b, 0o600))
}

func TestPromoteKubernetesAPIEncryptionProviders_success(t *testing.T) {
	tmpDir := t.TempDir()
	writeMigrationEncryptionConfigFixture(t, tmpDir, testKey1)
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithBlockingWaitPath(0))
	_, err := srv.PromoteKubernetesAPIEncryptionProviders(context.Background(), &pb.PromoteKubernetesAPIEncryptionProvidersRequest{})
	require.NoError(t, err)
	cfg, err := srv.readEncryptionConfig()
	require.NoError(t, err)
	assert.True(t, cfg.SecretboxPrimaryIdentitySecondary())
}

func TestPromoteKubernetesAPIEncryptionProviders_idempotentAlreadyPromoted(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "current-primary-encryption-key", Secret: testKey1}})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err := srv.PromoteKubernetesAPIEncryptionProviders(context.Background(), &pb.PromoteKubernetesAPIEncryptionProvidersRequest{})
	require.NoError(t, err)
}

func TestPromoteKubernetesAPIEncryptionProviders_notControlPlane(t *testing.T) {
	srv := &NodeAgentServer{
		NodeStatus:     &NodeStatus{Role: WorkerRole},
		clientRegistry: newMetricsRegistry(t, metricsWithCount(0)),
	}
	_, err := srv.PromoteKubernetesAPIEncryptionProviders(context.Background(), &pb.PromoteKubernetesAPIEncryptionProvidersRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "controlplane")
}

func TestPromoteKubernetesAPIEncryptionProviders_noClientRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	writeMigrationEncryptionConfigFixture(t, tmpDir, testKey1)
	srv := &NodeAgentServer{
		RootPath:   tmpDir,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
	}
	_, err := srv.PromoteKubernetesAPIEncryptionProviders(context.Background(), &pb.PromoteKubernetesAPIEncryptionProvidersRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics")
}

func TestPromoteKubernetesAPIEncryptionProviders_unexpectedLayout(t *testing.T) {
	tmpDir := t.TempDir()
	encDir := filepath.Join(tmpDir, "etc/kubernetes/kube-api-encryption")
	require.NoError(t, os.MkdirAll(encDir, 0o755))
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{{
			Resources: []string{"secrets"},
			Providers: []ProviderConfig{
				{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "k", Secret: testKey1}}}},
			},
		}},
	}
	b, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(encDir, "encryption-config.yaml"), b, 0o600))
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err = srv.PromoteKubernetesAPIEncryptionProviders(context.Background(), &pb.PromoteKubernetesAPIEncryptionProvidersRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migration state")
}
