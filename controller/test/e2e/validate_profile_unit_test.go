// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
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
NVKE_CONTROLLER_NAMESPACE: nke
NVKE_SITE_NAMESPACES: [kube-system, cert-manager, nke]
NVKE_SITE_ETCD_ENCRYPTED: true
NVKE_TENANT_GPU_NODES: 2
NVKE_SITE_KUBERNETES_VERSION: 1.30
NVKE_SITE_APISERVER_FLAGS: "audit-log-path=/var/log/audit.log;tls-cipher-suites=a,b"
`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"NVKE_CONTROLLER_NAMESPACE":    "nke",
		"NVKE_SITE_NAMESPACES":         "kube-system,cert-manager,nke",
		"NVKE_SITE_ETCD_ENCRYPTED":     "true",
		"NVKE_TENANT_GPU_NODES":        "2",
		"NVKE_SITE_KUBERNETES_VERSION": "1.30",
		"NVKE_SITE_APISERVER_FLAGS":    "audit-log-path=/var/log/audit.log;tls-cipher-suites=a,b",
	}, values, "a value is taken as written, so 1.30 is not read as the number 1.3")

	values, err = loadValidateProfile(writeProfile(t, "# nothing set\n"))
	require.NoError(t, err)
	require.Empty(t, values)
}

func TestUnit_ValidateProfileRejects(t *testing.T) {
	for name, body := range map[string]string{
		"a misspelled key":         "NVKE_SITE_NAMESPACE: nke\n",
		"a key that chooses a run": "NVKE_SITE_VALIDATE: true\n",
		"a credential":             "PROVIDER_API_KEY: secret\n",
		"flags as a list":          "NVKE_SITE_APISERVER_FLAGS: [a=b]\n",
		"a list of maps":           "NVKE_SITE_NAMESPACES: [{name: nke}]\n",
		"a map":                    "NVKE_SITE_COMPONENTS: {argocd: argocd}\n",
		"not YAML":                 "NVKE_SITE_NAMESPACES: [\n",
		"a key set twice":          "NVKE_SITE_NAMESPACES: a\nNVKE_SITE_NAMESPACES: b\n",
		"no value":                 "NVKE_SITE_NAMESPACES:\n",
		"a list at the top":        "- NVKE_SITE_NAMESPACES\n",
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
		"NVKE_SITE_NAMESPACES":      "  ",
	}
	set, overridden, err := applyValidateProfile(map[string]string{
		"NVKE_CONTROLLER_NAMESPACE":    "from-profile",
		"NVKE_SITE_NAMESPACES":         "nke",
		"NVKE_SITE_KUBERNETES_VERSION": "v1.36.3",
	}, func(key string) string { return env[key] }, func(key, value string) error {
		env[key] = value
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"NVKE_SITE_KUBERNETES_VERSION", "NVKE_SITE_NAMESPACES"}, set, "a blank variable reads as unset, so the profile fills it")
	require.Equal(t, []string{"NVKE_CONTROLLER_NAMESPACE"}, overridden)
	require.Equal(t, "from-env", env["NVKE_CONTROLLER_NAMESPACE"])
	require.Equal(t, "nke", env["NVKE_SITE_NAMESPACES"])
	require.Equal(t, "v1.36.3", env["NVKE_SITE_KUBERNETES_VERSION"])
}

// Every site and tenant input is either one a profile can set or one left
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
	}

	require.Len(t, profileKeys, len(slices.Compact(slices.Sorted(slices.Values(profileKeys)))), "profileKeys holds a duplicate")
	for _, key := range notInProfile {
		require.NotContains(t, profileKeys, key)
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	input := regexp.MustCompile(`"((?:NVKE_SITE|NVKE_TENANT)_[A-Z0-9_]*[A-Z0-9])"`)
	found := map[string]bool{}
	for _, name := range tenantTwins(siteExpectationEnvs) {
		found[name] = true
	}
	for _, file := range files {
		// Unit tests name inputs that do not exist, to show they are ignored.
		if strings.HasSuffix(file, "_unit_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, match := range input.FindAllSubmatch(data, -1) {
			found[string(match[1])] = true
		}
	}
	for key := range found {
		if slices.Contains(notInProfile, key) {
			continue
		}
		require.Contains(t, profileKeys, key, "%s is an input; add it to profileKeys, or to notInProfile with the reason", key)
	}
	for _, key := range profileKeys {
		if strings.HasPrefix(key, sitePrefix) || strings.HasPrefix(key, tenantPrefix) {
			require.True(t, found[key], "profileKeys names %s, which nothing reads", key)
		}
	}
}
