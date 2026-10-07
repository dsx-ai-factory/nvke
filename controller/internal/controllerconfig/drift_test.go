// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

// These tests keep controllerconfig.Config in step with config.Config while
// both exist. Every environment variable config.Config parses is either
// migrated to a config.yaml key or stays on the environment path for a stated
// reason, and each claim is checked against the Go types and the chart.

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
)

// migratedEnvFile maps each environment variable the chart sets whose setting
// nke-controller-config carries to its config.yaml key. The chart's env guard
// reads the same file, so this is the only copy of the list.
const migratedEnvFile = "../../deploy/charts/controller/files/controller-config-env.yaml"

// loadMigrated reads migratedEnvFile.
func loadMigrated(t *testing.T) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(migratedEnvFile)
	require.NoError(t, err)
	var migrated map[string]string
	require.NoError(t, yaml.UnmarshalStrict(raw, &migrated), "%s must map names to keys, each name once", migratedEnvFile)
	require.NotEmpty(t, migrated, "%s lists no variables", migratedEnvFile)
	return migrated
}

// Reasons a variable keeps the environment-variable path.
const (
	credential   = "credential: a Secret keeps it out of a plain-text ConfigMap"
	downwardAPI  = "Kubernetes downward API fieldRef, which a ConfigMap cannot express"
	separateFile = "path to a separately mounted file"
	chartUnset   = "the chart never sets it, so every deployment runs on its Go default"
)

// stayEnv lists every environment variable that keeps the environment path,
// with the reason.
var stayEnv = map[string]string{
	"DB_CONNECTION_STRING":    credential,
	"CNPG_SUPERUSER_PASSWORD": credential,
	"CNPG_CLOUD_PASSWORD":     credential,
	"CNPG_TEMPORAL_PASSWORD":  credential,
	"DGXC_TENANT_USER":        credential,
	"DGXC_TENANT_PASSWORD":    credential,

	"POD_NAMESPACE": downwardAPI,
	// The chart's default is a fieldRef to status.hostIP; a literal
	// spiffe.spire.serverAddress is the override.
	"SPIRE_SERVER_ADDRESS":   downwardAPI,
	"AUTH_CONFIG_PATH":       separateFile,
	"CONTROLLER_CONFIG_PATH": separateFile,

	"LOG_LEVEL":                                             chartUnset,
	"DB_QUERY_LOG_LEVEL":                                    chartUnset,
	"DB_MAX_OPEN_CONNECTIONS":                               chartUnset,
	"SERVER_LISTEN_ADDRESS":                                 chartUnset,
	"OUTBOX_POLLING_INTERVAL":                               chartUnset,
	"OUTBOX_JITTER_FACTOR":                                  chartUnset,
	"OUTBOX_BATCH_SIZE":                                     chartUnset,
	"NODE_HEALTH_CHECK_DISABLED":                            chartUnset,
	"NODE_HEALTH_CHECK_TIMEOUT":                             chartUnset,
	"KUBE_VIP_FMDS_ENDPOINT":                                chartUnset,
	"NODE_REBOOT_INITIAL_TIMEOUT":                           chartUnset,
	"NODE_REBOOT_TIMEOUT_INCREMENT":                         chartUnset,
	"NODE_REBOOT_MAX_TIMEOUT":                               chartUnset,
	"NODE_REBOOT_POLLING_INTERVAL":                          chartUnset,
	"NODE_REBOOT_SOFT_REBOOT_ENABLED":                       chartUnset,
	"NODE_PROVISIONING_TIMEOUT":                             chartUnset,
	"NKE_SITE_CONTROLLER_NAME":                              chartUnset,
	"SPIRE_AGENT_STARTUP_TIMEOUT":                           chartUnset,
	"SPIRE_AGENT_STARTUP_RETRY_INTERVAL":                    chartUnset,
	"SPIRE_SERVER_API_ADDRESS":                              chartUnset,
	"NODE_AGENT_PORT":                                       chartUnset,
	"MANAGED_BY_K8S_PLATFORM":                               chartUnset,
	"LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP":              chartUnset,
	"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER": chartUnset,
	"TELEPORT_DEFAULT_CLUSTER_ENABLED":                      chartUnset,
	"TELEPORT_DEFAULT_CP_POOL_ENABLED":                      chartUnset,
	"TELEPORT_DEFAULT_WORKER_POOL_ENABLED":                  chartUnset,
	"NKE_OIDC_AUTH_AUTO_MIGRATION_ENABLED":                  chartUnset,
}

// configMapOnly lists config.yaml keys that have no environment variable,
// because the ConfigMap is the first place they exist.
var configMapOnly = []string{"oidc.providers"}

// chartDeployment is the template whose env block decides what the chart sets.
const chartDeployment = "../../deploy/charts/controller/templates/deployment.yaml"

// envNamePattern matches an environment variable entry in chartDeployment.
// Uppercase-only, so volume, port, and container names do not match.
var envNamePattern = regexp.MustCompile(`(?m)^\s+- name: ([A-Z][A-Z0-9_]*)\s*$`)

func TestEveryConfigEnvVarIsClassified(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	parsed := envVarNames(reflect.TypeFor[config.Config](), "")
	require.NotEmpty(t, parsed)

	for _, name := range parsed {
		_, isMigrated := migrated[name]
		_, staysEnv := stayEnv[name]
		switch {
		case isMigrated && staysEnv:
			t.Errorf("%s is in both %s and stayEnv", name, migratedEnvFile)
		case !isMigrated && !staysEnv:
			t.Errorf("config.Config parses %s, but it is in neither %s nor stayEnv: "+
				"add it to controllerconfig.Config and that file, or to stayEnv with a reason", name, migratedEnvFile)
		}
	}

	for _, table := range []map[string]string{migrated, stayEnv} {
		for name := range table {
			if !slices.Contains(parsed, name) {
				t.Errorf("%s is classified here, but config.Config no longer parses it; remove the entry", name)
			}
		}
	}
}

func TestEveryMigratedKeyExistsOnce(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	keys := schemaKeys(reflect.TypeFor[controllerconfig.Config](), "")
	claimedBy := make(map[string]string, len(migrated))
	for name, key := range migrated {
		if !slices.Contains(keys, key) {
			t.Errorf("%s maps %s to %s, which controllerconfig.Config does not have", migratedEnvFile, name, key)
		}
		if other, taken := claimedBy[key]; taken {
			t.Errorf("%s and %s both map to %s", name, other, key)
		}
		claimedBy[key] = name
	}

	for _, key := range keys {
		if _, claimed := claimedBy[key]; !claimed && !slices.Contains(configMapOnly, key) {
			t.Errorf("controllerconfig.Config has %s, but no environment variable maps to it and it is not in configMapOnly", key)
		}
	}
}

func TestClassificationMatchesTheChart(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	raw, err := os.ReadFile(chartDeployment)
	require.NoError(t, err)
	chartSets := make(map[string]bool)
	for _, match := range envNamePattern.FindAllSubmatch(raw, -1) {
		chartSets[string(match[1])] = true
	}
	require.NotEmpty(t, chartSets, "found no environment variables in %s", chartDeployment)

	for name := range migrated {
		if !chartSets[name] {
			t.Errorf("%s lists %s, but %s does not set it; move it to stayEnv as %q", migratedEnvFile, name, chartDeployment, chartUnset)
		}
	}
	for name, reason := range stayEnv {
		if reason == chartUnset && chartSets[name] {
			t.Errorf("stayEnv says the chart never sets %s, but %s does; migrate it", name, chartDeployment)
		}
	}
}

// envVarNames lists the variables caarlos0/env reads for typ, composing
// envPrefix into nested struct names the same way the library does.
func envVarNames(typ reflect.Type, prefix string) []string {
	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if tag, ok := field.Tag.Lookup("env"); ok {
			name, _, _ := strings.Cut(tag, ",")
			names = append(names, prefix+name)
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			names = append(names, envVarNames(field.Type, prefix+field.Tag.Get("envPrefix"))...)
		}
	}
	return names
}

// schemaKeys lists config.yaml's leaf keys by following json tags. A slice is
// a leaf, and so is metav1.Duration, which decodes from a single string.
func schemaKeys(typ reflect.Type, prefix string) []string {
	durationType := reflect.TypeFor[metav1.Duration]()
	keys := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if field.Type.Kind() == reflect.Struct && field.Type != durationType {
			keys = append(keys, schemaKeys(field.Type, prefix+name+".")...)
			continue
		}
		keys = append(keys, prefix+name)
	}
	return keys
}
