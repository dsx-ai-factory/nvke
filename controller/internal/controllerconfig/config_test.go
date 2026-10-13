// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// baseConfig is every required key except nodeAgent, for tests that supply
// their own nodeAgent block.
const baseConfig = `
agentReadiness:
  workers: 8
  timeout: 30s
  interval: 2m
  jitter: 0.1
metricsListenAddress: 127.0.0.1:9090
supportedInfraProviders: [nico]
spire:
  joinTokenTTL: 1h
`

// minimalConfig satisfies every Validate constraint with the smallest possible
// document: everything optional left out, everything required present.
const minimalConfig = baseConfig + `nodeAgent:
  spireAttestationMethod: tpm
`

// A 64-character hex digest, the shape nodeAgent.dsxImds.pluginChecksum takes.
const exampleChecksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestLoadMinimalConfig(t *testing.T) {
	t.Parallel()

	cfg, err := controllerconfig.Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", cfg.MetricsListenAddress)
	assert.Equal(t, "tpm", cfg.NodeAgent.SpireAttestationMethod)
	assert.Equal(t, time.Hour, cfg.Spire.JoinTokenTTL.Duration)
}

func TestLoadFullConfig(t *testing.T) {
	t.Parallel()

	const full = `
agentReadiness:
  workers: 8
  timeout: 30s
  interval: 2m
  jitter: 0.1
fieldManager:
  name: cloud-workflow-engine
  rebootMachineName: cloud-workflow-engine-reboot-machine
  machineHealthIssueName: cloud-workflow-engine-machine-health-issue
managedByLabelValue: cloud-workflow-engine
metricsListenAddress: 127.0.0.1:9090
supportedInfraProviders: ["nico"]
tracing:
  enabled: true
  endpoint: localhost:4317
  insecure: true
  serviceName: cloud-workflow-engine
certRenewal:
  controllerEnabled: true
  interval: 12h
  thresholdDays: 30
kubeadmImageRepository: registry.k8s.io
cniImageRegistry: ""
installCNIAfterInit: false
allowLegacyCalicoClusterCreation: true
nicoControllersEnabled: true
profilesDir: ./profiles
runbooksDir: ""
tenantAccessEnabled: false
nodeAgent:
  spireAttestationMethod: dsxImds
  dsxImds:
    endpoint: http://169.254.169.254
    pluginChecksum: ` + exampleChecksum + `
    operatorTenant: acme
  tlsJWT:
    enabled: true
    issuer: https://issuer.example.com
    jwksURI: https://issuer.example.com/keys
    algorithms: [RS256]
    subjectTrustDomain: example.org
    dsxIdentityExchangeURL: https://exchange.example.com
    subjectTenant: acme
    subjectService: nke-node-agent
  externalMTLSEnabled: true
oidc:
  managementOIDCAuth:
    enabled: true
    issuerURL: https://spire.example.com
    discoveryURL: https://spire.example.com/.well-known/openid-configuration
    caBundleSecretRef:
      namespace: nke-system
      name: nke-oidc-ca-bundle
      key: ca.pem
  providers:
    - name: corporate
      issuerURL: https://login.example.com/tenant
      audiences: ["nvke-tenant-cluster"]
      usernameClaim: sub
      usernamePrefix: "corporate:"
      groupsClaim: groups
      groupsPrefix: "corporate:"
      requiredClaims:
        tenant_id: acme
      caBundle: |
        -----BEGIN CERTIFICATE-----
        ...
        -----END CERTIFICATE-----
temporal:
  hostPort: localhost:7233
  namespace: cloud-workflow-engine
  taskQueue: cloud-workflow-engine-tasks
  tls:
    caCert: /etc/temporal/ca.crt
    clientCert: /etc/temporal/client.crt
    clientKey: /etc/temporal/client.key
    serverName: temporal.example.com
spire:
  spiffeEnabled: true
  spiffeEndpointSocket: unix:///tmp/spire-agent/agent.sock
  spiffeJWTExchangeEndpoint: https://exchange.example.com
  serverNamespace: spire-server
  className: spire-mgmt-spire
  tpmPubhashesConfigMapName: tpm-pubhashes
  serverPort: "9091"
  joinTokenTTL: 1h
teleport:
  enabled: true
  identityFile: /etc/teleport/identity
  proxyAddress: teleport.example.com:443
  environment: prod
  managedByLabelValue: nke-cloud-workflow-engine
  tokenNamePrefix: nke-cwe-
  resourceGroupLabelValue: nke
permissiveMode: false
`

	cfg, err := controllerconfig.Load(writeConfig(t, full))
	require.NoError(t, err)

	assert.Equal(t, "cloud-workflow-engine", cfg.FieldManager.Name)
	assert.Equal(t, "cloud-workflow-engine-reboot-machine", cfg.FieldManager.RebootMachineName)
	assert.Equal(t, []string{"nico"}, cfg.SupportedInfraProviders)
	assert.Equal(t, 12*time.Hour, cfg.CertRenewal.Interval.Duration)
	assert.Equal(t, 30, cfg.CertRenewal.ThresholdDays)
	assert.True(t, cfg.AllowLegacyCalicoClusterCreation)
	assert.Equal(t, "dsxImds", cfg.NodeAgent.SpireAttestationMethod)
	assert.Equal(t, "acme", cfg.NodeAgent.DSXImds.OperatorTenant)
	assert.Equal(t, exampleChecksum, cfg.NodeAgent.DSXImds.PluginChecksum)
	assert.True(t, cfg.NodeAgent.TLSJWT.Enabled)
	assert.Equal(t, []string{"RS256"}, cfg.NodeAgent.TLSJWT.Algorithms)
	assert.True(t, cfg.NodeAgent.ExternalMTLSEnabled)

	assert.True(t, cfg.OIDC.ManagementOIDCAuth.Enabled)
	assert.Equal(t, "https://spire.example.com", cfg.OIDC.ManagementOIDCAuth.IssuerURL)
	require.Len(t, cfg.OIDC.Providers, 1)
	assert.Equal(t, "corporate", cfg.OIDC.Providers[0].Name)
	assert.Equal(t, map[string]string{"tenant_id": "acme"}, cfg.OIDC.Providers[0].RequiredClaims)

	assert.Equal(t, "nke", cfg.Teleport.ResourceGroupLabelValue)
	assert.Equal(t, "localhost:7233", cfg.Temporal.HostPort)
	assert.Equal(t, "/etc/temporal/ca.crt", cfg.Temporal.TLS.CACert)

	assert.True(t, cfg.Spire.SpiffeEnabled)
	assert.Equal(t, time.Hour, cfg.Spire.JoinTokenTTL.Duration)

	assert.True(t, cfg.Teleport.Enabled)
	assert.Equal(t, "prod", cfg.Teleport.Environment)
}

func TestLoadPathContract(t *testing.T) {
	t.Parallel()

	t.Run("empty path errors", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "path is empty")
	})

	t.Run("whitespace-only path errors", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load("   ")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "path is empty")
	})

	t.Run("missing file errors", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
		_, err := controllerconfig.Load(path)
		require.Error(t, err)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("unreadable file errors", func(t *testing.T) {
		t.Parallel()

		if os.Getuid() == 0 {
			t.Skip("running as root: file mode does not deny reads")
		}
		path := writeConfig(t, minimalConfig)
		require.NoError(t, os.Chmod(path, 0o000))
		_, err := controllerconfig.Load(path)
		require.Error(t, err)
		require.ErrorIs(t, err, fs.ErrPermission)
	})

	t.Run("empty file errors", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, ""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, "metricsListenAddress: \"unterminated\nspire: [\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse controller config")
	})

	t.Run("second YAML document errors", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+"\n---\nmetricsListenAddress: \"127.0.0.1:9999\"\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than one YAML document")
	})
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "unknown top-level field",
			yaml:    minimalConfig + "\nmetricsListenAdress: \"typo\"\n",
			wantErr: `unknown field "metricsListenAdress"`,
		},
		{
			name: "unknown field inside a grouped key",
			yaml: `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: 1h
  serverAdress: spire.example.com
nodeAgent:
  spireAttestationMethod: tpm
`,
			wantErr: `unknown field "spire.serverAdress"`,
		},
		{
			name: "unknown field two levels down",
			yaml: baseConfig + `nodeAgent:
  spireAttestationMethod: tpm
  tlsJWT:
    enabeld: true
`,
			wantErr: `unknown field "nodeAgent.tlsJWT.enabeld"`,
		},
		{
			// The attestation method lives under nodeAgent, as it does in
			// config.Config; under spire it is a typo, not an alias.
			name: "attestation method under spire",
			yaml: `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: 1h
  nodeAgentAttestationMethod: tpm
nodeAgent:
  spireAttestationMethod: tpm
`,
			wantErr: `unknown field "spire.nodeAgentAttestationMethod"`,
		},
		{
			// SPIRE_SERVER_ADDRESS stays an environment variable: the chart's
			// default sources it from the downward API.
			name: "SPIRE server address",
			yaml: `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: 1h
  serverAddress: spire.example.com
nodeAgent:
  spireAttestationMethod: tpm
`,
			wantErr: `unknown field "spire.serverAddress"`,
		},
		{
			name: "unknown field inside an oidc provider",
			yaml: minimalConfig + `oidc:
  providers:
    - name: corporate
      issuerUrl: https://login.example.com
      usernameClaim: sub
      caBundle: pem
`,
			wantErr: `unknown field "oidc.providers[0].issuerUrl"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateMetricsListenAddress(t *testing.T) {
	t.Parallel()

	_, err := controllerconfig.Load(writeConfig(t, `
spire:
  joinTokenTTL: 1h
nodeAgent:
  spireAttestationMethod: tpm
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metricsListenAddress must not be empty")
}

func TestValidateSpireJoinTokenTTL(t *testing.T) {
	t.Parallel()

	// The chart's values.yaml owns the default, so an omitted key only reaches
	// Load from a document the chart did not render. That fails closed rather
	// than picking a value the operator never saw.
	t.Run("omitted is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, `
metricsListenAddress: 127.0.0.1:9090
nodeAgent:
  spireAttestationMethod: tpm
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spire.joinTokenTTL must be greater than 0")
	})

	t.Run("explicit zero is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: 0s
nodeAgent:
  spireAttestationMethod: tpm
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spire.joinTokenTTL must be greater than 0")
	})

	t.Run("explicit negative is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: -1h
nodeAgent:
  spireAttestationMethod: tpm
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spire.joinTokenTTL must be greater than 0")
	})
}

// attestationConfig is a valid document apart from the attestation settings
// under test. It ends inside the nodeAgent block, so callers can append more
// nodeAgent keys indented by two spaces.
func attestationConfig(spiffeEnabled bool, method string) string {
	return fmt.Sprintf(`
agentReadiness:
  workers: 8
  timeout: 30s
  interval: 2m
  jitter: 0.1
metricsListenAddress: 127.0.0.1:9090
supportedInfraProviders: [nico]
spire:
  spiffeEnabled: %t
  joinTokenTTL: 1h
nodeAgent:
  spireAttestationMethod: %s
`, spiffeEnabled, method)
}

func TestValidateNodeAgentAttestationMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		spiffeEnabled bool
		method        string
		wantErr       string
	}{
		{name: "tpm needs nothing else", method: "tpm"},
		{name: "joinToken with SPIFFE", spiffeEnabled: true, method: "joinToken"},
		{
			name:    "joinToken without SPIFFE",
			method:  "joinToken",
			wantErr: `spire.spiffeEnabled must be true when spireAttestationMethod is "joinToken"`,
		},
		{
			name:    "dsxImds without SPIFFE",
			method:  "dsxImds",
			wantErr: `spire.spiffeEnabled must be true when spireAttestationMethod is "dsxImds"`,
		},
		{
			name:          "unknown method",
			spiffeEnabled: true,
			method:        "bogus",
			wantErr:       `spireAttestationMethod must be one of "tpm", "joinToken", or "dsxImds", got "bogus"`,
		},
		{
			name:    "empty method",
			method:  `""`,
			wantErr: `spireAttestationMethod must be one of "tpm", "joinToken", or "dsxImds", got ""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, attestationConfig(tt.spiffeEnabled, tt.method)))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateDSXImds(t *testing.T) {
	t.Parallel()

	const endpoint = "http://169.254.169.254"
	const invalidChecksum = "pluginChecksum must be empty or a 64-character SHA-256 hex digest"
	const invalidEndpoint = "endpoint must be an absolute HTTP(S) URL with a host"

	tests := []struct {
		name           string
		operatorTenant string
		endpoint       string
		pluginChecksum string
		wantErr        string
	}{
		{name: "checksum left to the node image", operatorTenant: "acme", endpoint: endpoint},
		{name: "pinned checksum over HTTPS", operatorTenant: "acme", endpoint: "https://imds.example.com", pluginChecksum: exampleChecksum},
		{name: "missing operatorTenant", endpoint: endpoint, wantErr: "dsxImds: operatorTenant is required"},
		{
			// config.Config trims this; the file refuses it rather than
			// changing what the operator wrote.
			name:           "padded operatorTenant",
			operatorTenant: " acme",
			endpoint:       endpoint,
			wantErr:        `operatorTenant " acme" has leading or trailing whitespace`,
		},
		{
			name:           "operatorTenant that is not a SPIFFE path segment",
			operatorTenant: "acme/prod",
			endpoint:       endpoint,
			wantErr:        "operatorTenant must be a valid SPIFFE path segment",
		},
		{name: "missing endpoint", operatorTenant: "acme", wantErr: "dsxImds: endpoint is required"},
		{
			name:           "padded endpoint",
			operatorTenant: "acme",
			endpoint:       endpoint + " ",
			wantErr:        `endpoint "http://169.254.169.254 " has leading or trailing whitespace`,
		},
		{name: "endpoint without a scheme", operatorTenant: "acme", endpoint: "169.254.169.254", wantErr: invalidEndpoint},
		{name: "endpoint with a non-HTTP scheme", operatorTenant: "acme", endpoint: "ftp://169.254.169.254", wantErr: invalidEndpoint},
		{name: "short checksum", operatorTenant: "acme", endpoint: endpoint, pluginChecksum: "abc123", wantErr: invalidChecksum},
		{
			name:           "checksum of the right length that is not hex",
			operatorTenant: "acme",
			endpoint:       endpoint,
			pluginChecksum: strings.Repeat("z", 64),
			wantErr:        invalidChecksum,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			yaml := attestationConfig(true, "dsxImds") + fmt.Sprintf(`  dsxImds:
    operatorTenant: %q
    endpoint: %q
    pluginChecksum: %q
`, tt.operatorTenant, tt.endpoint, tt.pluginChecksum)

			_, err := controllerconfig.Load(writeConfig(t, yaml))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateNodeAgentTLSJWT(t *testing.T) {
	t.Parallel()

	const tlsJWT = `  tlsJWT:
    enabled: true
    issuer: https://issuer.example.com
    jwksURI: https://issuer.example.com/keys
    algorithms: [RS256]
    subjectTrustDomain: example.org
    dsxIdentityExchangeURL: https://exchange.example.com
    subjectTenant: acme
    subjectService: nke-node-agent
`
	const externalMTLS = "  externalMTLSEnabled: true\n"

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "enabled without SPIFFE or external mTLS",
			yaml:    attestationConfig(false, "tpm") + tlsJWT,
			wantErr: "spire.spiffeEnabled or externalMTLSEnabled must be true when tlsJWT.enabled is true",
		},
		{
			name:    "missing issuer",
			yaml:    attestationConfig(false, "tpm") + externalMTLS + strings.Replace(tlsJWT, "    issuer: https://issuer.example.com\n", "", 1),
			wantErr: "nodeAgent: tlsJWT: issuer is required when enabled is true",
		},
		{name: "fully configured over external mTLS", yaml: attestationConfig(false, "tpm") + externalMTLS + tlsJWT},
		{name: "fully configured over SPIFFE", yaml: attestationConfig(true, "tpm") + tlsJWT},
		{
			name:    "missing subjectTenant",
			yaml:    attestationConfig(true, "tpm") + strings.Replace(tlsJWT, "    subjectTenant: acme\n", "", 1),
			wantErr: "nodeAgent: tlsJWT: subjectTenant is required when enabled is true",
		},
		{
			// config.Config trims these; the file refuses padding rather than
			// changing what the operator wrote.
			name:    "padded subjectTenant",
			yaml:    attestationConfig(true, "tpm") + strings.Replace(tlsJWT, "subjectTenant: acme", `subjectTenant: " acme"`, 1),
			wantErr: `subjectTenant " acme" has leading or trailing whitespace`,
		},
		{
			name:    "subjectService that is not a DNS label",
			yaml:    attestationConfig(true, "tpm") + strings.Replace(tlsJWT, "subjectService: nke-node-agent", "subjectService: NVKE_Node_Agent", 1),
			wantErr: `subjectService "NVKE_Node_Agent" must be a lowercase DNS label`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, tt.yaml))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// The reserved namespaces are a security boundary: removing one should fail a
// test rather than quietly widen what an operator provider can claim.
func TestReservedIdentityPrefixes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"system:", "kubeadm:", "nke:", "nvke:", "nvidia:"}, controllerconfig.ReservedIdentityPrefixes())
}

func TestValidateOIDCReservedPrefixes(t *testing.T) {
	t.Parallel()

	provider := func(field, prefix string) string {
		return minimalConfig + fmt.Sprintf(`
oidc:
  providers:
    - name: corp
      issuerURL: https://login.example.com
      audiences: ["nke"]
      usernameClaim: sub
      groupsClaim: groups
      caBundle: pem
      %s: %q
`, field, prefix)
	}

	type testCase struct {
		name    string
		field   string
		prefix  string
		wantErr string
	}
	fields := []string{"usernamePrefix", "groupsPrefix"}
	reservedPrefixes := controllerconfig.ReservedIdentityPrefixes()
	allowedPrefixes := []string{"", "corp:", "sys", "System:", "nvke-team:", "nvidia"}
	tests := make([]testCase, 0, len(fields)*(2*len(reservedPrefixes)+len(allowedPrefixes)))
	for _, reserved := range reservedPrefixes {
		for _, field := range fields {
			tests = append(tests,
				testCase{
					name:    field + " is " + reserved,
					field:   field,
					prefix:  reserved,
					wantErr: fmt.Sprintf(`providers[0] (corp): %s %q is inside the reserved %q namespace`, field, reserved, reserved),
				},
				testCase{
					name:    field + " is inside " + reserved,
					field:   field,
					prefix:  reserved + "team:",
					wantErr: fmt.Sprintf(`%s %q is inside the reserved %q namespace`, field, reserved+"team:", reserved),
				},
			)
		}
	}
	for _, allowed := range allowedPrefixes {
		for _, field := range fields {
			tests = append(tests, testCase{name: fmt.Sprintf("%s %q is allowed", field, allowed), field: field, prefix: allowed})
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := controllerconfig.Load(writeConfig(t, provider(tt.field, tt.prefix)))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateOIDC(t *testing.T) {
	t.Parallel()

	t.Run("managementOIDCAuth enabled without issuerURL fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  managementOIDCAuth:
    enabled: true
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "managementOIDCAuth.issuerURL is required")
	})

	t.Run("more than the cap fails", func(t *testing.T) {
		t.Parallel()

		yaml := minimalConfig + "oidc:\n  providers:\n"
		for i := range controllerconfig.MaxOIDCProviders + 1 {
			yaml += `    - name: provider` + string(rune('a'+i)) + `
      issuerURL: https://login` + string(rune('a'+i)) + `.example.com
      usernameClaim: sub
      caBundle: pem
`
		}
		_, err := controllerconfig.Load(writeConfig(t, yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than the 5-entry cap")
	})

	t.Run("duplicate name fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
    - name: corporate
      issuerURL: https://b.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate name")
	})

	t.Run("duplicate issuerURL fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
    - name: other
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate issuerURL")
	})

	t.Run("issuerURL of the enabled managementOIDCAuth fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  managementOIDCAuth:
    enabled: true
    issuerURL: https://a.example.com
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already used by managementOIDCAuth")
	})

	t.Run("issuerURL of a disabled managementOIDCAuth passes", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  managementOIDCAuth:
    issuerURL: https://a.example.com
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
`))
		require.NoError(t, err)
	})

	t.Run("missing caBundle fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant"]
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "caBundle is required")
	})

	t.Run("missing audiences fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      caBundle: pem
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "audiences is required")
	})

	t.Run("empty audiences entry fails", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
oidc:
  providers:
    - name: corporate
      issuerURL: https://a.example.com
      usernameClaim: sub
      audiences: ["tenant", "  "]
      caBundle: pem
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "audiences[1] must not be empty")
	})

	t.Run("empty ManagementOIDCAuth CA bundle ref reports empty", func(t *testing.T) {
		t.Parallel()

		var ref controllerconfig.OIDCCABundleSecretRef
		assert.True(t, ref.Empty())
	})
}

// A provider's discoveryURL gets the checks kube-apiserver applies to it, so a
// value it would reject stops the controller at startup instead of failing
// every new cluster's control plane.
func TestValidateOIDCProviderDiscoveryURL(t *testing.T) {
	t.Parallel()

	provider := func(name, issuerURL, discoveryURL string) string {
		return fmt.Sprintf(`    - name: %s
      issuerURL: %s
      discoveryURL: %q
      usernameClaim: sub
      audiences: ["tenant"]
      caBundle: pem
`, name, issuerURL, discoveryURL)
	}
	const (
		issuer   = "https://login.example.com"
		internal = "https://login.internal.example/.well-known/openid-configuration"
	)
	shared := func(enabled bool) string {
		return fmt.Sprintf(`  managementOIDCAuth:
    enabled: %t
    issuerURL: https://management.example.com
    discoveryURL: %s
`, enabled, internal)
	}

	tests := []struct {
		name    string
		oidc    string
		wantErr string
	}{
		{name: "an https URL other than the issuer loads", oidc: "  providers:\n" + provider("corporate", issuer, internal)},
		{name: "http fails", oidc: "  providers:\n" + provider("corporate", issuer, "http://login.internal.example/discovery"), wantErr: "must be an absolute https URL"},
		{name: "a relative URL fails", oidc: "  providers:\n" + provider("corporate", issuer, "/.well-known/openid-configuration"), wantErr: "must be an absolute https URL"},
		{name: "a port with no hostname fails", oidc: "  providers:\n" + provider("corporate", issuer, "https://:443/.well-known/openid-configuration"), wantErr: "must be an absolute https URL"},
		{name: "credentials fail", oidc: "  providers:\n" + provider("corporate", issuer, "https://user:secret@login.internal.example/discovery"), wantErr: "must not contain a username or password"},
		{name: "a query fails", oidc: "  providers:\n" + provider("corporate", issuer, "https://login.internal.example/discovery?tenant=a"), wantErr: "must not contain a query or fragment"},
		{name: "the issuerURL itself fails", oidc: "  providers:\n" + provider("corporate", issuer, issuer+"/"), wantErr: "must differ from issuerURL"},
		{name: "padding fails", oidc: "  providers:\n" + provider("corporate", issuer, " "+internal), wantErr: "leading or trailing whitespace"},
		{
			name:    "the same URL on two providers fails",
			oidc:    "  providers:\n" + provider("corporate", issuer, internal) + provider("other", "https://other.example.com", internal),
			wantErr: "duplicate discoveryURL",
		},
		{
			name:    "the discoveryURL of the enabled managementOIDCAuth fails",
			oidc:    shared(true) + "  providers:\n" + provider("corporate", issuer, internal),
			wantErr: "already used by managementOIDCAuth",
		},
		{name: "the discoveryURL of a disabled managementOIDCAuth loads", oidc: shared(false) + "  providers:\n" + provider("corporate", issuer, internal)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := controllerconfig.Load(writeConfig(t, minimalConfig+"oidc:\n"+tt.oidc))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.OIDC.Providers, 1)
			assert.Equal(t, internal, cfg.OIDC.Providers[0].DiscoveryURL)
		})
	}
}

func TestValidateSupportedInfraProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string // replaces minimalConfig's supportedInfraProviders line
		wantErr string
	}{
		{name: "both providers", line: "supportedInfraProviders: [nico, infra]\n"},
		{
			// config.Config defaults an unset list to ["nico"]; the chart
			// renders that default, so an omitted key here fails closed.
			name:    "omitted",
			wantErr: "supportedInfraProviders: must contain at least one provider",
		},
		{name: "empty list", line: "supportedInfraProviders: []\n", wantErr: "must contain at least one provider"},
		{name: "empty name", line: `supportedInfraProviders: [nico, ""]` + "\n", wantErr: "[1] must not be empty"},
		{name: "padded name", line: `supportedInfraProviders: [" nico"]` + "\n", wantErr: `[0] " nico" has leading or trailing whitespace`},
		{name: "unknown provider", line: "supportedInfraProviders: [aws]\n", wantErr: `[0] "aws" is not a supported provider`},
		{name: "duplicate provider", line: "supportedInfraProviders: [nico, nico]\n", wantErr: `[1] "nico" duplicates [0]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			yaml := strings.Replace(minimalConfig, "supportedInfraProviders: [nico]\n", tt.line, 1)
			_, err := controllerconfig.Load(writeConfig(t, yaml))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateTeleport(t *testing.T) {
	t.Parallel()

	t.Run("disabled skips validation", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig))
		require.NoError(t, err)
	})

	t.Run("enabled requires identityFile and proxyAddress", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
teleport:
  enabled: true
  environment: prod
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identityFile is required")
	})

	t.Run("invalid environment rejected", func(t *testing.T) {
		t.Parallel()

		_, err := controllerconfig.Load(writeConfig(t, minimalConfig+`
teleport:
  enabled: true
  identityFile: /etc/teleport/identity
  proxyAddress: teleport.example.com:443
  environment: staging
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `environment must be "prod" or "non-prod"`)
	})
}

// Both cases exercise metav1.Duration's own UnmarshalJSON, not custom code in
// this package: a bare number is not the JSON string it requires, and an
// unparsable string fails time.ParseDuration. Asserting on them here is a
// contract check, so a metav1.Duration upgrade that loosens either one shows up
// here rather than surfacing as a silently-accepted config value.
func TestDurationRejectsBareNumber(t *testing.T) {
	t.Parallel()

	_, err := controllerconfig.Load(writeConfig(t, `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: 3600
nodeAgent:
  spireAttestationMethod: tpm
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot unmarshal number into Go struct field SpireConfig.spire.joinTokenTTL of type string")
}

func TestDurationRejectsInvalidString(t *testing.T) {
	t.Parallel()

	_, err := controllerconfig.Load(writeConfig(t, `
metricsListenAddress: 127.0.0.1:9090
spire:
  joinTokenTTL: "not-a-duration"
nodeAgent:
  spireAttestationMethod: tpm
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid duration")
}
