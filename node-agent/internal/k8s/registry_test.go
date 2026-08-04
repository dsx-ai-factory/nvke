package k8s

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validTestKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
users:
- name: test
  user:
    token: test-token
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`

func TestClientRegistry(t *testing.T) {
	tests := []struct {
		name            string
		kubeconfigPath  string
		wantErrContains string
	}{
		{
			name:           "builds client from valid kubeconfig",
			kubeconfigPath: newKubeconfigFile(t, validTestKubeconfig),
		},
		{
			name:           "reuses client after first successful build",
			kubeconfigPath: newKubeconfigFile(t, validTestKubeconfig),
		},
		{
			name:            "returns access error for missing kubeconfig",
			kubeconfigPath:  "/missing/admin.conf",
			wantErrContains: "reading kubeconfig /missing/admin.conf",
		},
		{
			name:            "returns parse error for malformed kubeconfig",
			kubeconfigPath:  newKubeconfigFile(t, "not: [valid"),
			wantErrContains: "parsing kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewClientRegistry(tt.kubeconfigPath)
			t.Run("typed", func(t *testing.T) {
				client, err := registry.Client()
				if tt.wantErrContains == "" {
					require.NotNil(t, client)
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tt.wantErrContains)
				}
			})

			t.Run("dynamic", func(t *testing.T) {
				client, err := registry.DynamicClient()
				if tt.wantErrContains == "" {
					require.NotNil(t, client)
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tt.wantErrContains)
				}
			})

			t.Run("local", func(t *testing.T) {
				client, err := registry.LocalClient()
				if tt.wantErrContains == "" {
					require.NotNil(t, client)
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tt.wantErrContains)
				}
			})
		})
	}
}

func TestClientRegistryClientsAreCached(t *testing.T) {
	path := newKubeconfigFile(t, validTestKubeconfig)
	registry := NewClientRegistry(path)

	t.Run("typed", func(t *testing.T) {
		typedClient, err := registry.Client()
		require.NoError(t, err)
		require.NotNil(t, typedClient)

		cachedTypedClient, err := registry.Client()
		require.NoError(t, err)
		assert.True(t, typedClient == cachedTypedClient)
	})

	t.Run("dynamic", func(t *testing.T) {
		dynamicClient, err := registry.DynamicClient()
		require.NoError(t, err)
		require.NotNil(t, dynamicClient)

		cachedDynamicClient, err := registry.DynamicClient()
		require.NoError(t, err)
		assert.True(t, dynamicClient == cachedDynamicClient)
	})

	t.Run("local", func(t *testing.T) {
		localClient, err := registry.LocalClient()
		require.NoError(t, err)
		require.NotNil(t, localClient)

		cachedLocalClient, err := registry.LocalClient()
		require.NoError(t, err)
		assert.True(t, localClient == cachedLocalClient)
	})
}

func TestClientRegistryLocalClientUsesLoopbackApiserver(t *testing.T) {
	path := newKubeconfigFile(t, validTestKubeconfig)
	registry := NewClientRegistry(path)

	client, err := registry.LocalClient()
	require.NoError(t, err)
	require.NotNil(t, client)

	reqURL := client.RESTClient().Get().AbsPath("/metrics").URL().String()
	assert.Equal(t, "https://127.0.0.1:6443/metrics", reqURL)
}

func TestKubeconfigAccessError(t *testing.T) {
	path := "/missing/admin.conf"
	registry := NewClientRegistry(path)
	_, err := registry.Client()

	assert.ErrorContains(t, err, "reading kubeconfig /missing/admin.conf")
	assert.ErrorAs(t, err, &KubeconfigAccessError{})
}

func newKubeconfigFile(t *testing.T, kubeconfig string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.conf")
	require.NoError(t, os.WriteFile(path, []byte(kubeconfig), 0o600))

	return path
}
