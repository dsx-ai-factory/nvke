// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestUnit_ValidateProfileLoads(t *testing.T) {
	values, err := loadValidateProfile(writeProfile(t, `
NVKE_CONTROLLER_NAMESPACE: my-controller
NVKE_SITE_NAMESPACES: [kube-system, cert-manager, my-controller]
NVKE_SITE_ETCD_ENCRYPTED: true
NVKE_TENANT_GPU_NODES: 2
NVKE_SITE_KUBERNETES_VERSION: 1.30
NVKE_SITE_APISERVER_FLAGS: "audit-log-path=/var/log/audit.log;tls-cipher-suites=a,b"
`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"NVKE_CONTROLLER_NAMESPACE":    "my-controller",
		"NVKE_SITE_NAMESPACES":         "kube-system,cert-manager,my-controller",
		"NVKE_SITE_ETCD_ENCRYPTED":     "true",
		"NVKE_TENANT_GPU_NODES":        "2",
		"NVKE_SITE_KUBERNETES_VERSION": "1.30",
		"NVKE_SITE_APISERVER_FLAGS":    "audit-log-path=/var/log/audit.log;tls-cipher-suites=a,b",
	}, values, "a value is taken as written, so 1.30 is not read as the number 1.3")

	for name, body := range map[string]string{
		"comments only": "# nothing set\n",
		"empty":         "",
	} {
		t.Run(name, func(t *testing.T) {
			values, err := loadValidateProfile(writeProfile(t, body))
			require.NoError(t, err)
			require.Empty(t, values)
		})
	}
}

func TestUnit_ValidateProfileRejects(t *testing.T) {
	for name, body := range map[string]string{
		"a misspelled key":         "NVKE_SITE_NAMESPACE: my-controller\n",
		"a key that chooses a run": "NVKE_SITE_VALIDATE: true\n",
		"a credential":             "PROVIDER_API_KEY: secret\n",
		"auth turned off":          "NKE_E2E_AUTH_DISABLE: true\n",
		"flags as a list":          "NVKE_SITE_APISERVER_FLAGS: [a=b]\n",
		"a list of maps":           "NVKE_SITE_NAMESPACES: [{name: my-controller}]\n",
		"a map":                    "NVKE_SITE_COMPONENTS: {argocd: argocd}\n",
		"not YAML":                 "NVKE_SITE_NAMESPACES: [\n",
		"a key set twice":          "NVKE_SITE_NAMESPACES: a\nNVKE_SITE_NAMESPACES: b\n",
		"no value":                 "NVKE_SITE_NAMESPACES:\n",
		"a blank value":            "NVKE_SITE_NAMESPACES: \"\"\n",
		"an empty list":            "NVKE_SITE_NAMESPACES: []\n",
		"a blank list entry":       "NVKE_SITE_NAMESPACES: [a, \"\"]\n",
		"a list at the top":        "- NVKE_SITE_NAMESPACES\n",
		"a second document":        "NVKE_SITE_NAMESPACES: a\n---\nNVKE_SITE_VALIDATE: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadValidateProfile(writeProfile(t, body))
			require.Error(t, err)
		})
	}

	_, err := loadValidateProfile(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err, "a profile named but missing fails the run rather than validating with defaults")
}

func TestUnit_ValidateProfileEnvironmentWins(t *testing.T) {
	env := map[string]string{
		"NVKE_CONTROLLER_NAMESPACE": "from-env",
		"NVKE_SITE_NAMESPACES":      "",
	}
	set, overridden := applyValidateProfile(map[string]string{
		"NVKE_CONTROLLER_NAMESPACE":    "from-profile",
		"NVKE_SITE_NAMESPACES":         "my-controller",
		"NVKE_SITE_KUBERNETES_VERSION": "v1.36.3",
	}, func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}, func(key, value string) { env[key] = value })
	require.Equal(t, []string{"NVKE_SITE_KUBERNETES_VERSION"}, set)
	require.Equal(t, []string{"NVKE_CONTROLLER_NAMESPACE", "NVKE_SITE_NAMESPACES"}, overridden, "a variable set blank asks for the built-in default, so the profile leaves it")
	require.Equal(t, "from-env", env["NVKE_CONTROLLER_NAMESPACE"])
	require.Empty(t, env["NVKE_SITE_NAMESPACES"])
	require.Equal(t, "v1.36.3", env["NVKE_SITE_KUBERNETES_VERSION"])
}

// Every input the module reads is either one a profile can set or one left
// out on purpose, so a new input cannot be forgotten in profileKeys.
func TestUnit_ValidateProfileCoversEveryInput(t *testing.T) {
	notInProfile := []string{
		validateProfileEnv,
		// Choose what a run does, and the image's ENV sets them.
		siteValidateEnv,
		tenantValidateEnv,
		expectedVersionRequireEnv,
		// Name one tenant, not the site.
		tenantClusterEnv,
		tenantTokenSecretEnv,
		tenantKubernetesVersionEnv,
		// Turns authentication off, which a site's file must not do for a run.
		"NKE_E2E_AUTH_DISABLE",
		// How long one run's token lives, not the site.
		"NKE_E2E_TOKEN_TTL",
		// Read only by specs the validation image does not run: the lanes that
		// build, upgrade or change a site.
		"NKE_E2E_BUNDLE_SERVER_URL",
		"NKE_E2E_DSX_IDENTITY_EXCHANGE",
		"NKE_E2E_NICO_VMS_CNI",
		"NKE_E2E_OIDC_ISSUER_HOST",
		"NKE_E2E_REQUIRE_AUTHN",
		"NKE_E2E_SPIRE_RELEASE",
		"NKE_E2E_UPGRADE_HOPS",
		"NVKE_CONTROLLER_RELEASES_DIR",
		"NVKE_E2E_KEEP_ON_FAILURE",
		"NVKE_E2E_MANAGEMENT_NAMESPACE",
		"NVKE_E2E_NODE_AGENT_BUNDLE_VOLUME",
		"NVKE_E2E_NODE_AGENT_UPGRADE",
		"NVKE_E2E_RBAC_CONFIGMAP",
	}

	require.Len(t, profileKeys, len(slices.Compact(slices.Sorted(slices.Values(profileKeys)))), "profileKeys holds a duplicate")
	for _, key := range notInProfile {
		require.NotContains(t, profileKeys, key)
	}

	input := regexp.MustCompile(`"((?:NVKE_SITE|NVKE_TENANT|NVKE_CONTROLLER|NVKE_VALIDATE|NVKE_E2E|NKE_E2E)_[A-Z0-9_]*[A-Z0-9])"`)
	found := map[string]bool{}
	require.NoError(t, filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// Unit tests name inputs that do not exist, to show they are ignored.
		if strings.HasSuffix(path, "_unit_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range input.FindAllSubmatch(data, -1) {
			found[string(match[1])] = true
		}
		return nil
	}))
	require.Contains(t, found, "NKE_E2E_AUDIENCE", "the scan reaches internal packages")

	for key := range found {
		if slices.Contains(notInProfile, key) {
			continue
		}
		require.Contains(t, profileKeys, key, "%s is an input; add it to profileKeys, or to notInProfile with the reason", key)
	}
	// A tenant twin is never written out: siteExpectations builds it from its
	// site input, so it is read exactly when that input is.
	twins := tenantTwins(siteExpectationEnvs)
	for i, twin := range twins {
		require.True(t, found[siteExpectationEnvs[i]], "%s is the twin of %s, which nothing reads", twin, siteExpectationEnvs[i])
	}
	for _, key := range profileKeys {
		if !slices.Contains(twins, key) {
			require.True(t, found[key], "profileKeys names %s, which nothing reads", key)
		}
	}
}
