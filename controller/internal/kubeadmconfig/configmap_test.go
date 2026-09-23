// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/yaml"

	kubeadmv1beta3 "github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
)

// realisticV1beta3ClusterConfig is a production-shaped kubeadm ClusterConfiguration
// (v1beta3 map-style extraArgs) carrying the fields a real control plane sets, so
// the merge is exercised against more than a minimal fixture.
const realisticV1beta3ClusterConfig = `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
kubernetesVersion: v1.30.4
controlPlaneEndpoint: "10.0.0.100:6443"
certificatesDir: /etc/kubernetes/pki
imageRepository: registry.k8s.io
clusterName: prod-tenant
networking:
  serviceSubnet: 10.96.0.0/12
  podSubnet: 192.168.0.0/16
  dnsDomain: cluster.local
etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
      quota-backend-bytes: "8589934592"
    serverCertSANs:
    - 10.0.0.1
dns:
  imageTag: v1.11.1
apiServer:
  certSANs:
  - 10.0.0.100
  - api.tenant.example
  extraArgs:
    audit-log-path: /var/log/kubernetes/audit.log
    audit-log-maxage: "30"
  extraVolumes:
  - name: audit-logs
    hostPath: /var/log/kubernetes
    mountPath: /var/log/kubernetes
    pathType: DirectoryOrCreate
  timeoutForControlPlane: 4m0s
controllerManager:
  extraArgs:
    bind-address: 0.0.0.0
scheduler:
  extraArgs:
    bind-address: 0.0.0.0
`

func TestMergeOIDC_RealisticConfigRoundTrips(t *testing.T) {
	merged, changed, err := MergeOIDCIntoClusterConfiguration([]byte(realisticV1beta3ClusterConfig))
	require.NoError(t, err)
	require.True(t, changed)

	// The merged output must still parse as a typed kubeadm ClusterConfiguration
	// — a proxy for "kubeadm can still re-render from it after the merge".
	var parsed kubeadmv1beta3.ClusterConfiguration
	require.NoError(t, yaml.Unmarshal(merged, &parsed), "merged config must remain valid kubeadm ClusterConfiguration")

	// Unrelated fields survive the map round-trip intact.
	assert.Equal(t, "v1.30.4", parsed.KubernetesVersion)
	assert.Equal(t, "10.0.0.100:6443", parsed.ControlPlaneEndpoint)
	assert.Equal(t, "registry.k8s.io", parsed.ImageRepository)
	assert.Equal(t, "192.168.0.0/16", parsed.Networking.PodSubnet)
	require.NotNil(t, parsed.Etcd.Local)
	assert.Equal(t, "/var/lib/etcd", parsed.Etcd.Local.DataDir)
	assert.Equal(t, "8589934592", parsed.Etcd.Local.ExtraArgs["quota-backend-bytes"], "large numeric-string arg must not be mangled")
	assert.Contains(t, parsed.APIServer.CertSANs, "api.tenant.example")
	assert.Equal(t, "0.0.0.0", parsed.ControllerManager.ExtraArgs["bind-address"])
	assert.Equal(t, "0.0.0.0", parsed.Scheduler.ExtraArgs["bind-address"])
	require.NotNil(t, parsed.APIServer.TimeoutForControlPlane)

	// The apiServer's own pre-existing args/volumes survive alongside the OIDC additions.
	assert.Equal(t, "/var/log/kubernetes/audit.log", parsed.APIServer.ExtraArgs["audit-log-path"])
	assert.Equal(t, OIDCAuthConfigPath, parsed.APIServer.ExtraArgs[AuthConfigFlagName])
	volNames := make([]string, 0, len(parsed.APIServer.ExtraVolumes))
	for _, v := range parsed.APIServer.ExtraVolumes {
		volNames = append(volNames, v.Name)
	}
	assert.Contains(t, volNames, "audit-logs")
	assert.Contains(t, volNames, AuthConfigVolumeName)

	// Idempotent: a second merge changes nothing and is byte-identical.
	second, changed2, err := MergeOIDCIntoClusterConfiguration(merged)
	require.NoError(t, err)
	assert.False(t, changed2, "re-merging an already-merged realistic config must be a no-op")
	assert.Equal(t, string(merged), string(second))
}

// findArg returns the value of the named extraArg from a merged
// ClusterConfiguration, tolerating either kubeadm shape (v1beta3 map or
// v1beta4 list) since the merge preserves whichever the input used.
func findArg(t *testing.T, merged []byte, name string) (string, bool) {
	t.Helper()
	switch args := apiServerField(t, merged, "extraArgs").(type) {
	case map[string]any:
		v, ok := args[name].(string)
		return v, ok
	case []any:
		for _, item := range args {
			entry, ok := item.(map[string]any)
			require.True(t, ok)
			if entry["name"] == name {
				v, _ := entry["value"].(string)
				return v, true
			}
		}
		return "", false
	default:
		t.Fatalf("extraArgs has unexpected shape %T", args)
		return "", false
	}
}

// findVolume returns the named extraVolume from a merged ClusterConfiguration.
func findVolume(t *testing.T, merged []byte, name string) (map[string]any, bool) {
	t.Helper()
	vols := apiServerField(t, merged, "extraVolumes")
	list, ok := vols.([]any)
	require.True(t, ok, "extraVolumes must marshal back as a list, got %T", vols)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		require.True(t, ok)
		if entry["name"] == name {
			return entry, true
		}
	}
	return nil, false
}

func apiServerField(t *testing.T, merged []byte, field string) any {
	t.Helper()
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &cfg))
	api, ok := cfg["apiServer"].(map[string]any)
	require.True(t, ok, "apiServer must be a mapping")
	return api[field]
}

func TestMergeOIDCIntoClusterConfiguration_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "malformed YAML",
			input: "apiServer: {extraArgs: [unterminated",
		},
		{
			name:  "missing apiVersion",
			input: "kind: ClusterConfiguration\n",
		},
		{
			name:  "unsupported apiVersion",
			input: "apiVersion: kubeadm.k8s.io/v1beta2\nkind: ClusterConfiguration\n",
		},
		{
			// A kubeadm document of a supported apiVersion but a non-cluster kind
			// (the apiVersion group is shared with Init/JoinConfiguration) must be
			// rejected, not silently truncated by the ClusterConfiguration round-trip.
			name:  "wrong kind",
			input: "apiVersion: kubeadm.k8s.io/v1beta3\nkind: InitConfiguration\n",
		},
		{
			name:  "apiServer wrong type",
			input: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer: not-a-mapping\n",
		},
		{
			name:  "extraArgs wrong scalar type",
			input: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs: 42\n",
		},
		{
			name:  "extraVolumes wrong type",
			input: "apiVersion: kubeadm.k8s.io/v1beta3\nkind: ClusterConfiguration\napiServer:\n  extraVolumes: not-a-list\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := MergeOIDCIntoClusterConfiguration([]byte(tc.input))
			require.Error(t, err)
		})
	}
}

func TestMergeOIDCIntoClusterConfiguration_AddsAndNormalizes(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name: "minimal v1beta4 list-shaped config",
			input: `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - name: profiling
    value: "false"
`,
		},
		{
			name: "v1beta3 map-shaped extraArgs preserved as a map",
			input: `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
apiServer:
  extraArgs:
    profiling: "false"
    bind-address: 0.0.0.0
`,
		},
		{
			name: "no apiServer key at all",
			input: `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.31.4
`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(tc.input))
			require.NoError(t, err)

			// authentication-config extraArg present with the canonical path.
			val, ok := findArg(t, merged, AuthConfigFlagName)
			assert.True(t, ok, "authentication-config extraArg must be present")
			assert.Equal(t, OIDCAuthConfigPath, val)

			// oidc-auth-config volume present and mirrors the greenfield render.
			vol, ok := findVolume(t, merged, AuthConfigVolumeName)
			require.True(t, ok, "oidc-auth-config volume must be present")
			assert.Equal(t, map[string]any{
				"name":      AuthConfigVolumeName,
				"hostPath":  defaultOIDCConfigDirectory,
				"mountPath": defaultOIDCConfigDirectory,
				"pathType":  "DirectoryOrCreate",
				"readOnly":  true,
			}, vol)
		})
	}
}

// The merge must keep the document's kubeadm shape: a v1beta3 map stays a map
// and a v1beta4 list stays a list. Emitting the other shape under the original
// apiVersion would be rejected by `kubeadm config validate` on the node.
func TestMergeOIDCIntoClusterConfiguration_PreservesShape(t *testing.T) {
	t.Run("v1beta3 stays a map", func(t *testing.T) {
		merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(
			"apiVersion: kubeadm.k8s.io/v1beta3\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    profiling: \"false\"\n",
		))
		require.NoError(t, err)
		_, ok := apiServerField(t, merged, "extraArgs").(map[string]any)
		assert.True(t, ok, "v1beta3 extraArgs must remain a map")
	})
	t.Run("v1beta3 absent extraArgs added as a map", func(t *testing.T) {
		merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(
			"apiVersion: kubeadm.k8s.io/v1beta3\nkind: ClusterConfiguration\n",
		))
		require.NoError(t, err)
		_, ok := apiServerField(t, merged, "extraArgs").(map[string]any)
		assert.True(t, ok, "an absent v1beta3 extraArgs must be added as a map")
	})
	t.Run("v1beta4 stays a list", func(t *testing.T) {
		merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(
			"apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n",
		))
		require.NoError(t, err)
		_, ok := apiServerField(t, merged, "extraArgs").([]any)
		assert.True(t, ok, "v1beta4 extraArgs must be a list")
	})
}

func TestMergeOIDCIntoClusterConfiguration_PreservesExistingArgs(t *testing.T) {
	input := `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
apiServer:
  extraArgs:
    profiling: "false"
    audit-log-maxage: "30"
`
	merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(input))
	require.NoError(t, err)

	profiling, ok := findArg(t, merged, "profiling")
	assert.True(t, ok)
	assert.Equal(t, "false", profiling)

	maxage, ok := findArg(t, merged, "audit-log-maxage")
	assert.True(t, ok)
	assert.Equal(t, "30", maxage)

	authCfg, ok := findArg(t, merged, AuthConfigFlagName)
	assert.True(t, ok)
	assert.Equal(t, OIDCAuthConfigPath, authCfg)
}

func TestMergeOIDCIntoClusterConfiguration_Idempotent(t *testing.T) {
	input := `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - name: profiling
    value: "false"
  extraVolumes:
  - name: audit-policies
    hostPath: /etc/kubernetes/policies
    mountPath: /etc/kubernetes/policies
    pathType: DirectoryOrCreate
    readOnly: true
`
	first, _, err := MergeOIDCIntoClusterConfiguration([]byte(input))
	require.NoError(t, err)

	second, secondChanged, err := MergeOIDCIntoClusterConfiguration(first)
	require.NoError(t, err)

	// Byte-stable AND reported as unchanged: a second merge of already-merged
	// output is an exact no-op, so the caller can skip the ConfigMap write.
	assert.Equal(t, string(first), string(second), "merge must be byte-stable on already-merged input")
	assert.False(t, secondChanged, "re-merging already-merged config must report changed=false")

	// Exactly one authentication-config arg and one oidc-auth-config volume (no append-duplication).
	args := apiServerField(t, second, "extraArgs").([]any)
	var argCount int
	for _, a := range args {
		if a.(map[string]any)["name"] == AuthConfigFlagName {
			argCount++
		}
	}
	assert.Equal(t, 1, argCount, "authentication-config must appear exactly once")

	vols := apiServerField(t, second, "extraVolumes").([]any)
	var volCount int
	for _, v := range vols {
		if v.(map[string]any)["name"] == AuthConfigVolumeName {
			volCount++
		}
	}
	assert.Equal(t, 1, volCount, "oidc-auth-config volume must appear exactly once")
}

// A pre-existing authentication-config arg with a stale value is upserted to the
// canonical path rather than duplicated.
func TestMergeOIDCIntoClusterConfiguration_UpsertsStaleArg(t *testing.T) {
	input := `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - name: authentication-config
    value: /old/stale/path.yaml
`
	merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(input))
	require.NoError(t, err)

	val, ok := findArg(t, merged, AuthConfigFlagName)
	assert.True(t, ok)
	assert.Equal(t, OIDCAuthConfigPath, val)

	args := apiServerField(t, merged, "extraArgs").([]any)
	assert.Len(t, args, 1, "stale arg must be replaced in place, not appended")
}

// The v1beta3 map branch has its own upsert path: a pre-existing
// authentication-config key with a stale value must be replaced in place,
// staying a single-entry map (mirrors UpsertsStaleArg, which covers the
// v1beta4 list branch).
func TestMergeOIDCIntoClusterConfiguration_UpsertsStaleArgV1beta3Map(t *testing.T) {
	input := `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
apiServer:
  extraArgs:
    authentication-config: /old/stale/path.yaml
`
	merged, changed, err := MergeOIDCIntoClusterConfiguration([]byte(input))
	require.NoError(t, err)
	assert.True(t, changed, "a stale value must be reported as changed")

	val, ok := findArg(t, merged, AuthConfigFlagName)
	assert.True(t, ok)
	assert.Equal(t, OIDCAuthConfigPath, val)

	args, ok := apiServerField(t, merged, "extraArgs").(map[string]any)
	require.True(t, ok, "v1beta3 extraArgs must remain a map")
	assert.Len(t, args, 1, "stale arg must be replaced in place, not duplicated")
}

func TestOIDCVolume_V1beta3AndV1beta4Converge(t *testing.T) {
	// The greenfield/v1beta3 render (authenticationConfigVolume) and the
	// v1beta4 write-back volume must serialize identically, so a later kubeadm
	// re-render (upgrade) and this migration converge on the same apiserver
	// volume. Compare the marshaled bytes so drift in either trips this test
	// rather than diverging silently in prod.
	v3, err := yaml.Marshal(authenticationConfigVolume())
	require.NoError(t, err)
	v4, err := yaml.Marshal(authenticationConfigVolumeV1beta4())
	require.NoError(t, err)
	assert.Equal(t, string(v3), string(v4))
}

func TestMergeEncryptionIntoClusterConfiguration_PreservesShapeAndIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name: "v1beta3 map",
			input: `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
kubernetesVersion: v1.31.4
etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
      quota-backend-bytes: "17179869184"
apiServer:
  extraArgs:
    audit-log-path: /var/log/kubernetes/audit.log
`,
		},
		{
			name: "v1beta4 list",
			input: `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.31.4
etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
    - name: quota-backend-bytes
      value: "17179869184"
apiServer:
  extraArgs:
  - name: audit-log-path
    value: /var/log/kubernetes/audit.log
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged, changed, err := MergeEncryptionIntoClusterConfiguration([]byte(tc.input))
			require.NoError(t, err)
			assert.True(t, changed)

			configPath, ok := findArg(t, merged, encryptionProviderConfigFlagName)
			require.True(t, ok)
			assert.Equal(t, defaultEncryptionConfigPath, configPath)
			reload, ok := findArg(t, merged, encryptionProviderConfigReloadFlagName)
			require.True(t, ok)
			assert.Equal(t, "true", reload)
			volume, ok := findVolume(t, merged, encryptionConfigVolumeName)
			require.True(t, ok)
			assert.Equal(t, defaultEncryptionConfigDirectory, volume["hostPath"])
			assert.Equal(t, defaultEncryptionConfigDirectory, volume["mountPath"])
			assert.Equal(t, true, volume["readOnly"])

			quota, err := EtcdQuotaFromClusterConfiguration(merged)
			require.NoError(t, err)
			assert.Equal(t, EtcdQuotaBackendBytes, quota)

			second, secondChanged, err := MergeEncryptionIntoClusterConfiguration(merged)
			require.NoError(t, err)
			assert.False(t, secondChanged)
			assert.Equal(t, string(merged), string(second))
		})
	}
}

func TestEncryptionVolume_V1beta3AndV1beta4Converge(t *testing.T) {
	v3, err := yaml.Marshal(encryptionVolume())
	require.NoError(t, err)
	v4, err := yaml.Marshal(encryptionVolumeV1beta4())
	require.NoError(t, err)
	assert.Equal(t, string(v3), string(v4))
}

func TestEtcdQuotaFromClusterConfiguration_ErrorsWhenMissing(t *testing.T) {
	_, err := EtcdQuotaFromClusterConfiguration([]byte(`apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
etcd:
  local:
    dataDir: /var/lib/etcd
`))
	require.ErrorContains(t, err, "quota-backend-bytes not found")
}

func TestSetKubernetesVersionInClusterConfiguration(t *testing.T) {
	const input = `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.30.9
`

	updated, changed, err := SetKubernetesVersionInClusterConfiguration([]byte(input), "1.31.4")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Contains(t, string(updated), "kubernetesVersion: v1.31.4")

	second, secondChanged, err := SetKubernetesVersionInClusterConfiguration(updated, "v1.31.4")
	require.NoError(t, err)
	assert.False(t, secondChanged)
	assert.Equal(t, string(updated), string(second))

	_, _, err = SetKubernetesVersionInClusterConfiguration([]byte(input), "v1.31")
	require.ErrorContains(t, err, "expected X.Y.Z")
}
