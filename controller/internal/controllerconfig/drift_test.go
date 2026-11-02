// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

// config.Config takes each field either from an environment variable (an env
// tag) or from nvke-controller-config (a controllerconfig tag naming its key).
// These tests check that split against the list of retired variables, the Go
// types, ApplyTo, and the chart.

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// migratedEnvFile maps each environment variable whose setting moved into
// nvke-controller-config to its config.yaml key. The chart's env guard reads
// the same file, so this is the only copy of the list.
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

	"LOG_LEVEL":                                chartUnset,
	"DB_QUERY_LOG_LEVEL":                       chartUnset,
	"DB_QUERY_LOG_VALUES":                      chartUnset,
	"DB_MAX_OPEN_CONNECTIONS":                  chartUnset,
	"SERVER_LISTEN_ADDRESS":                    chartUnset,
	"OUTBOX_POLLING_INTERVAL":                  chartUnset,
	"OUTBOX_JITTER_FACTOR":                     chartUnset,
	"OUTBOX_BATCH_SIZE":                        chartUnset,
	"NODE_HEALTH_CHECK_DISABLED":               chartUnset,
	"NODE_HEALTH_CHECK_TIMEOUT":                chartUnset,
	"KUBE_VIP_FMDS_ENDPOINT":                   chartUnset,
	"NODE_REBOOT_INITIAL_TIMEOUT":              chartUnset,
	"NODE_REBOOT_TIMEOUT_INCREMENT":            chartUnset,
	"NODE_REBOOT_MAX_TIMEOUT":                  chartUnset,
	"NODE_REBOOT_POLLING_INTERVAL":             chartUnset,
	"NODE_REBOOT_SOFT_REBOOT_ENABLED":          chartUnset,
	"NODE_PROVISIONING_TIMEOUT":                chartUnset,
	"NKE_SITE_CONTROLLER_NAME":                 chartUnset,
	"SPIRE_AGENT_STARTUP_TIMEOUT":              chartUnset,
	"SPIRE_AGENT_STARTUP_RETRY_INTERVAL":       chartUnset,
	"SPIRE_SERVER_API_ADDRESS":                 chartUnset,
	"NODE_AGENT_PORT":                          chartUnset,
	"MANAGED_BY_K8S_PLATFORM":                  chartUnset,
	"LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP": chartUnset,
	"TELEPORT_DEFAULT_CLUSTER_ENABLED":         chartUnset,
	"TELEPORT_DEFAULT_CP_POOL_ENABLED":         chartUnset,
	"TELEPORT_DEFAULT_WORKER_POOL_ENABLED":     chartUnset,
	"NKE_OIDC_AUTH_AUTO_MIGRATION_ENABLED":     chartUnset,
}

// configMapOnly lists config.yaml keys that never had an environment variable,
// because the ConfigMap is the first place they exist.
var configMapOnly = []string{
	"oidc.providers",
	"managementNamespace",
	// The RBAC ConfigMap's name, which became a value with the namespace it
	// lives in and so never had an environment variable either.
	"rbacConfigMapName",
	// New clusters default to OVN-Kubernetes. This is the switch internal
	// fleets set when they still create Calico clusters.
	"allowLegacyCalicoClusterCreation",
	"allowControlPlaneWithoutVIP",
	"nodeMetadataAllowlistedLabelKeys",
	"machineMetadataAllowlistedLabelKeys",
	"machineMetadataAllowlistedAnnotationKeys",
	// The dual-accept window for the Teleport "managed-by" value. It exists
	// because managedByLabelValue moved, which is newer than the environment.
	"teleport.acceptedManagedByLabelValues",
	// API authentication was a file of its own, never environment variables.
	// AUTH_CONFIG_PATH, which names that file, stays on the environment.
	"apiAuth.enabled",
	"apiAuth.audiences",
	"apiAuth.issuers",
	// Credential monitoring started in nvke-controller-config.
	"credentialMonitoring.enabled",
	"credentialMonitoring.inspectionInterval",
	"credentialMonitoring.observationMaxAge",
	"credentialMonitoring.warningDays",
}

// chartDeployment is the template whose env block decides what the chart sets.
const chartDeployment = "../../deploy/charts/controller/templates/deployment.yaml"

// envNamePattern matches an environment variable entry in chartDeployment.
// Uppercase-only, so volume, port, and container names do not match.
var envNamePattern = regexp.MustCompile(`(?m)^\s+- name: ([A-Z][A-Z0-9_]*)\s*$`)

func TestEveryConfigEnvVarStaysOnTheEnvironment(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	var parsed []string
	for _, f := range configLeaves(t) {
		if f.envName != "" {
			parsed = append(parsed, f.envName)
		}
	}
	require.NotEmpty(t, parsed)

	for _, name := range parsed {
		if key, retired := migrated[name]; retired {
			t.Errorf("config.Config parses %s, which nvke-controller-config replaced as %s; read it from the file", name, key)
			continue
		}
		if _, ok := stayEnv[name]; !ok {
			t.Errorf("config.Config parses %s, which is not in stayEnv: "+
				"add it to controllerconfig.Config and %s, or to stayEnv with a reason", name, migratedEnvFile)
		}
	}
	for name := range stayEnv {
		if !slices.Contains(parsed, name) {
			t.Errorf("stayEnv lists %s, but config.Config no longer parses it; remove the entry", name)
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
		if slices.Contains(configMapOnly, key) {
			t.Errorf("%s maps %s to %s, which configMapOnly says never had an environment variable", migratedEnvFile, name, key)
		}
		claimedBy[key] = name
	}

	for _, key := range keys {
		if _, claimed := claimedBy[key]; !claimed && !slices.Contains(configMapOnly, key) {
			t.Errorf("controllerconfig.Config has %s, but no environment variable maps to it and it is not in configMapOnly", key)
		}
	}
}

func TestEveryMigratedKeyHasOneConfigField(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	fieldFor := make(map[string]string, len(migrated))
	for _, f := range configLeaves(t) {
		if f.fileKey == "" {
			continue
		}
		if !slices.Contains(slices.Collect(mapValues(migrated)), f.fileKey) && !slices.Contains(configMapOnly, f.fileKey) {
			t.Errorf("config.Config.%s takes %s from the file, which %s does not list", f.name, f.fileKey, migratedEnvFile)
		}
		if other, taken := fieldFor[f.fileKey]; taken {
			t.Errorf("config.Config.%s and config.Config.%s both take %s", f.name, other, f.fileKey)
		}
		fieldFor[f.fileKey] = f.name
	}
	for name, key := range migrated {
		if _, ok := fieldFor[key]; !ok {
			t.Errorf("no config.Config field takes %s (formerly %s); give the field a controllerconfig tag", key, name)
		}
	}
}

// ApplyTo is written out field by field. For each key, this sets that key alone
// in the file and checks that ApplyTo changes exactly the field tagged with it,
// and never a field the environment supplies.
func TestApplyToFollowsTheTags(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)
	leaves := configLeaves(t)

	for _, key := range slices.Sorted(mapValues(migrated)) {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			var file controllerconfig.Config
			setNonZero(t, fileField(t, reflect.ValueOf(&file).Elem(), key), key)

			var cfg config.Config
			for _, f := range leaves {
				if f.envName != "" {
					setNonZero(t, reflect.ValueOf(&cfg).Elem().FieldByIndex(f.index), "env")
				}
			}
			before := cfg
			file.ApplyTo(&cfg)

			var changed []string
			for _, f := range leaves {
				got := reflect.ValueOf(cfg).FieldByIndex(f.index)
				was := reflect.ValueOf(before).FieldByIndex(f.index)
				if f.envName != "" {
					require.Equal(t, was.Interface(), got.Interface(), "ApplyTo changed config.Config.%s, which %s supplies", f.name, f.envName)
					continue
				}
				if !got.IsZero() {
					changed = append(changed, f.fileKey)
				}
			}
			require.Equal(t, []string{key}, changed, "setting %s changed the fields tagged with these keys", key)
		})
	}
}

func TestChartSetsNoRetiredVariable(t *testing.T) {
	t.Parallel()
	migrated := loadMigrated(t)

	raw, err := os.ReadFile(chartDeployment)
	require.NoError(t, err)
	chartSets := make(map[string]bool)
	for _, match := range envNamePattern.FindAllSubmatch(raw, -1) {
		chartSets[string(match[1])] = true
	}
	require.NotEmpty(t, chartSets, "found no environment variables in %s", chartDeployment)

	for name, key := range migrated {
		if chartSets[name] {
			t.Errorf("%s sets %s, which the controller no longer reads; render %s in nvke-controller-config instead", chartDeployment, name, key)
		}
	}
	for name, reason := range stayEnv {
		if reason == chartUnset && chartSets[name] {
			t.Errorf("stayEnv says the chart never sets %s, but %s does; move it into nvke-controller-config", name, chartDeployment)
		}
	}
}

// configLeaf is one field of config.Config that a value comes from: either an
// environment variable or a key in nvke-controller-config.
type configLeaf struct {
	name    string
	index   []int
	envName string
	fileKey string
}

// configLeaves walks config.Config, composing envPrefix into variable names the
// way caarlos0/env does. A field with neither tag must be a struct to descend into.
func configLeaves(t *testing.T) []configLeaf {
	t.Helper()
	var walk func(typ reflect.Type, index []int, name, envPrefix string) []configLeaf
	walk = func(typ reflect.Type, index []int, name, envPrefix string) []configLeaf {
		var leaves []configLeaf
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			fieldIndex := append(slices.Clone(index), i)
			fieldName := name + field.Name
			envTag, hasEnv := field.Tag.Lookup("env")
			fileKey, hasFile := field.Tag.Lookup("controllerconfig")
			switch {
			case hasEnv && hasFile:
				t.Errorf("config.Config.%s has both an env and a controllerconfig tag", fieldName)
			case hasEnv:
				envName, _, _ := strings.Cut(envTag, ",")
				leaves = append(leaves, configLeaf{name: fieldName, index: fieldIndex, envName: envPrefix + envName})
			case hasFile:
				leaves = append(leaves, configLeaf{name: fieldName, index: fieldIndex, fileKey: fileKey})
			case field.Type.Kind() == reflect.Struct:
				leaves = append(leaves, walk(field.Type, fieldIndex, fieldName+".", envPrefix+field.Tag.Get("envPrefix"))...)
			default:
				t.Errorf("config.Config.%s has neither an env nor a controllerconfig tag", fieldName)
			}
		}
		return leaves
	}
	return walk(reflect.TypeFor[config.Config](), nil, "", "")
}

// fileField returns the field of a controllerconfig.Config value at a dotted
// config.yaml key, following json tags.
func fileField(t *testing.T, v reflect.Value, key string) reflect.Value {
	t.Helper()
	for _, part := range strings.Split(key, ".") {
		found := false
		for i := range v.NumField() {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
			if name == part {
				v = v.Field(i)
				found = true
				break
			}
		}
		require.True(t, found, "controllerconfig.Config has no %s", key)
	}
	return v
}

// setNonZero gives v a value that is not its zero value.
func setNonZero(t *testing.T, v reflect.Value, label string) {
	t.Helper()
	switch {
	case v.Type() == reflect.TypeFor[metav1.Duration]():
		v.Set(reflect.ValueOf(metav1.Duration{Duration: 3 * time.Second}))
	case v.Kind() == reflect.String:
		v.SetString("set-" + label)
	case v.Kind() == reflect.Bool:
		v.SetBool(true)
	case v.Kind() == reflect.Int || v.Kind() == reflect.Int64:
		v.SetInt(7)
	case v.Kind() == reflect.Float64:
		v.SetFloat(0.5)
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String:
		v.Set(reflect.ValueOf([]string{"set-" + label}).Convert(v.Type()))
	default:
		t.Fatalf("setNonZero does not handle %s for %s", v.Type(), label)
	}
}

// mapValues iterates a map's values.
func mapValues(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
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
