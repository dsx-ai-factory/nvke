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

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
)

// migrated maps each environment variable the chart sets to its key in
// config.yaml.
var migrated = map[string]string{
	"FIELD_MANAGER_NAME":                      "fieldManager.name",
	"FIELD_MANAGER_REBOOT_MACHINE_NAME":       "fieldManager.rebootMachineName",
	"FIELD_MANAGER_MACHINE_HEALTH_ISSUE_NAME": "fieldManager.machineHealthIssueName",
	"MANAGED_BY_LABEL_VALUE":                  "managedByLabelValue",
	"METRICS_LISTEN_ADDRESS":                  "metricsListenAddress",
	"SUPPORTED_INFRA_PROVIDERS":               "supportedInfraProviders",

	"OTEL_TRACING_ENABLED":        "tracing.enabled",
	"OTEL_EXPORTER_OTLP_ENDPOINT": "tracing.endpoint",
	"OTEL_EXPORTER_OTLP_INSECURE": "tracing.insecure",
	"OTEL_SERVICE_NAME":           "tracing.serviceName",

	"CERT_RENEWAL_CONTROLLER_ENABLED": "certRenewal.controllerEnabled",
	"CERT_RENEWAL_INTERVAL":           "certRenewal.interval",
	"CERT_RENEWAL_THRESHOLD_DAYS":     "certRenewal.thresholdDays",

	"KUBEADM_IMAGE_REPOSITORY": "kubeadmImageRepository",
	"CNI_IMAGE_REGISTRY":       "cniImageRegistry",
	"INSTALL_CNI_AFTER_INIT":   "installCNIAfterInit",
	"NICO_CONTROLLERS_ENABLED": "nicoControllersEnabled",
	"PROFILES_DIR":             "profilesDir",
	"RUNBOOKS_DIR":             "runbooksDir",
	"TENANT_ACCESS_ENABLED":    "tenantAccessEnabled",

	"NODE_AGENT_SPIRE_ATTESTATION_METHOD":          "nodeAgent.spireAttestationMethod",
	"NODE_AGENT_DSX_IMDS_ENDPOINT":                 "nodeAgent.dsxImds.endpoint",
	"NODE_AGENT_DSX_IMDS_PLUGIN_CHECKSUM":          "nodeAgent.dsxImds.pluginChecksum",
	"NODE_AGENT_DSX_IMDS_OPERATOR_TENANT":          "nodeAgent.dsxImds.operatorTenant",
	"NODE_AGENT_TLS_JWT_ENABLED":                   "nodeAgent.tlsJWT.enabled",
	"NODE_AGENT_TLS_JWT_ISSUER":                    "nodeAgent.tlsJWT.issuer",
	"NODE_AGENT_TLS_JWT_JWKS_URI":                  "nodeAgent.tlsJWT.jwksURI",
	"NODE_AGENT_TLS_JWT_ALGORITHMS":                "nodeAgent.tlsJWT.algorithms",
	"NODE_AGENT_TLS_JWT_SUBJECT_TRUST_DOMAIN":      "nodeAgent.tlsJWT.subjectTrustDomain",
	"NODE_AGENT_TLS_JWT_DSX_IDENTITY_EXCHANGE_URL": "nodeAgent.tlsJWT.dsxIdentityExchangeURL",
	"NODE_AGENT_TLS_JWT_SUBJECT_TENANT":            "nodeAgent.tlsJWT.subjectTenant",
	"NODE_AGENT_TLS_JWT_SUBJECT_SERVICE":           "nodeAgent.tlsJWT.subjectService",
	"NODE_AGENT_EXTERNAL_MTLS_ENABLED":             "nodeAgent.externalMTLSEnabled",

	"NKE_OIDC_AUTH_ENABLED":                    "oidc.managementOIDCAuth.enabled",
	"NKE_OIDC_AUTH_ISSUER_URL":                 "oidc.managementOIDCAuth.issuerURL",
	"NKE_OIDC_AUTH_DISCOVERY_URL":              "oidc.managementOIDCAuth.discoveryURL",
	"NKE_OIDC_AUTH_CA_BUNDLE_SECRET_NAMESPACE": "oidc.managementOIDCAuth.caBundleSecretRef.namespace",
	"NKE_OIDC_AUTH_CA_BUNDLE_SECRET_NAME":      "oidc.managementOIDCAuth.caBundleSecretRef.name",
	"NKE_OIDC_AUTH_CA_BUNDLE_SECRET_KEY":       "oidc.managementOIDCAuth.caBundleSecretRef.key",

	"TEMPORAL_HOST_PORT":        "temporal.hostPort",
	"TEMPORAL_NAMESPACE":        "temporal.namespace",
	"TEMPORAL_TASK_QUEUE":       "temporal.taskQueue",
	"TEMPORAL_MTLS_CA_CERT":     "temporal.tls.caCert",
	"TEMPORAL_MTLS_CLIENT_CERT": "temporal.tls.clientCert",
	"TEMPORAL_MTLS_CLIENT_KEY":  "temporal.tls.clientKey",
	"TEMPORAL_MTLS_SERVER_NAME": "temporal.tls.serverName",

	"SPIFFE_ENABLED":                      "spire.spiffeEnabled",
	"SPIFFE_ENDPOINT_SOCKET":              "spire.spiffeEndpointSocket",
	"SPIFFE_JWT_EXCHANGE_ENDPOINT":        "spire.spiffeJWTExchangeEndpoint",
	"SPIRE_SERVER_NAMESPACE":              "spire.serverNamespace",
	"SPIRE_CLASS_NAME":                    "spire.className",
	"SPIRE_TPM_PUBHASHES_CONFIG_MAP_NAME": "spire.tpmPubhashesConfigMapName",
	"SPIRE_SERVER_PORT":                   "spire.serverPort",
	"SPIRE_JOIN_TOKEN_TTL":                "spire.joinTokenTTL",

	"TELEPORT_ENABLED":                "teleport.enabled",
	"TELEPORT_IDENTITY_FILE":          "teleport.identityFile",
	"TELEPORT_PROXY_ADDRESS":          "teleport.proxyAddress",
	"TELEPORT_ENVIRONMENT":            "teleport.environment",
	"TELEPORT_MANAGED_BY_LABEL_VALUE": "teleport.managedByLabelValue",
	"TELEPORT_TOKEN_NAME_PREFIX":      "teleport.tokenNamePrefix",

	"PERMISSIVE_MODE": "permissiveMode",
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
	"SPIRE_SERVER_ADDRESS": downwardAPI,
	"AUTH_CONFIG_PATH":     separateFile,

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

	parsed := envVarNames(reflect.TypeFor[config.Config](), "")
	require.NotEmpty(t, parsed)

	for _, name := range parsed {
		_, isMigrated := migrated[name]
		_, staysEnv := stayEnv[name]
		switch {
		case isMigrated && staysEnv:
			t.Errorf("%s is in both migrated and stayEnv", name)
		case !isMigrated && !staysEnv:
			t.Errorf("config.Config parses %s, but it is in neither migrated nor stayEnv: "+
				"add it to controllerconfig.Config and migrated, or to stayEnv with a reason", name)
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

	keys := schemaKeys(reflect.TypeFor[controllerconfig.Config](), "")
	claimedBy := make(map[string]string, len(migrated))
	for name, key := range migrated {
		if !slices.Contains(keys, key) {
			t.Errorf("migrated maps %s to %s, which controllerconfig.Config does not have", name, key)
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

	raw, err := os.ReadFile(chartDeployment)
	require.NoError(t, err)
	chartSets := make(map[string]bool)
	for _, match := range envNamePattern.FindAllSubmatch(raw, -1) {
		chartSets[string(match[1])] = true
	}
	require.NotEmpty(t, chartSets, "found no environment variables in %s", chartDeployment)

	for name := range migrated {
		if !chartSets[name] {
			t.Errorf("migrated lists %s, but %s does not set it; move it to stayEnv as %q", name, chartDeployment, chartUnset)
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
