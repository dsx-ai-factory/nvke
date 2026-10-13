// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig

import (
	"fmt"
	"slices"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// LoadStartupConfig returns the configuration the controller runs on: the
// environment, then the nke-controller-config file CONTROLLER_CONFIG_PATH names,
// validated together. It also returns the file, for the settings config.Config
// does not carry, such as oidc.providers.
func LoadStartupConfig() (config.Config, *Config, error) {
	cfg, err := config.ParseConfigFromEnv()
	if err != nil {
		return config.Config{}, nil, err
	}
	file, err := Load(cfg.ControllerConfigPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	file.ApplyTo(&cfg)
	if err := cfg.Validate(); err != nil {
		return config.Config{}, nil, fmt.Errorf("config validation failed: %w", err)
	}
	return cfg, file, nil
}

// ApplyTo copies the file into every field of cfg that has a controllerconfig
// tag, and leaves the fields that come from the environment alone.
func (c *Config) ApplyTo(cfg *config.Config) {
	cfg.AgentReadiness = c.AgentReadiness.runtimeConfig()
	cfg.FieldManagerName = c.FieldManager.Name
	cfg.FieldManagerRebootMachineName = c.FieldManager.RebootMachineName
	cfg.FieldManagerMachineHealthIssueName = c.FieldManager.MachineHealthIssueName
	cfg.ManagedByLabelValue = c.ManagedByLabelValue
	cfg.MetricsListenAddress = c.MetricsListenAddress
	cfg.SupportedInfraProviders = slices.Clone(c.SupportedInfraProviders)

	cfg.Tracing.Enabled = c.Tracing.Enabled
	cfg.Tracing.Endpoint = c.Tracing.Endpoint
	cfg.Tracing.Insecure = c.Tracing.Insecure
	cfg.Tracing.ServiceName = c.Tracing.ServiceName

	cfg.CertRenewalControllerEnabled = c.CertRenewal.ControllerEnabled
	cfg.CertRenewalInterval = c.CertRenewal.Interval.Duration
	cfg.CertRenewalThresholdDays = c.CertRenewal.ThresholdDays

	cfg.KubeadmImageRepository = c.KubeadmImageRepository
	cfg.CNIImageRegistry = c.CNIImageRegistry
	cfg.InstallCNIAfterInit = c.InstallCNIAfterInit
	cfg.NicoControllersEnabled = c.NicoControllersEnabled
	cfg.ProfilesDir = c.ProfilesDir
	cfg.RunbooksDir = c.RunbooksDir
	cfg.TenantAccessEnabled = c.TenantAccessEnabled

	cfg.NodeAgent.UpdateMethod = c.NodeAgent.UpdateMethod
	cfg.NodeAgent.SpireAttestationMethod = config.SpireAttestationMethod(c.NodeAgent.SpireAttestationMethod)
	cfg.NodeAgent.DSXImds.Endpoint = c.NodeAgent.DSXImds.Endpoint
	cfg.NodeAgent.DSXImds.PluginChecksum = c.NodeAgent.DSXImds.PluginChecksum
	cfg.NodeAgent.DSXImds.OperatorTenant = c.NodeAgent.DSXImds.OperatorTenant
	cfg.NodeAgent.TLSJWT.Enabled = c.NodeAgent.TLSJWT.Enabled
	cfg.NodeAgent.TLSJWT.Issuer = c.NodeAgent.TLSJWT.Issuer
	cfg.NodeAgent.TLSJWT.JWKSURI = c.NodeAgent.TLSJWT.JWKSURI
	cfg.NodeAgent.TLSJWT.Algorithms = slices.Clone(c.NodeAgent.TLSJWT.Algorithms)
	cfg.NodeAgent.TLSJWT.SubjectTrustDomain = c.NodeAgent.TLSJWT.SubjectTrustDomain
	cfg.NodeAgent.TLSJWT.DSXIdentityExchangeURL = c.NodeAgent.TLSJWT.DSXIdentityExchangeURL
	cfg.NodeAgent.TLSJWT.SubjectTenant = c.NodeAgent.TLSJWT.SubjectTenant
	cfg.NodeAgent.TLSJWT.SubjectService = c.NodeAgent.TLSJWT.SubjectService
	cfg.NodeAgentExternalMTLSEnabled = c.NodeAgent.ExternalMTLSEnabled

	shared := c.OIDC.ManagementOIDCAuth
	cfg.NvkeOIDCAuth.Enabled = shared.Enabled
	cfg.NvkeOIDCAuth.IssuerURL = shared.IssuerURL
	cfg.NvkeOIDCAuth.DiscoveryURL = shared.DiscoveryURL
	cfg.NvkeOIDCAuth.CABundleSecret.Namespace = shared.CABundleSecretRef.Namespace
	cfg.NvkeOIDCAuth.CABundleSecret.Name = shared.CABundleSecretRef.Name
	cfg.NvkeOIDCAuth.CABundleSecret.Key = shared.CABundleSecretRef.Key

	cfg.TemporalHostPort = c.Temporal.HostPort
	cfg.TemporalNamespace = c.Temporal.Namespace
	cfg.TemporalTaskQueue = c.Temporal.TaskQueue
	cfg.TemporalTLS.CaCert = c.Temporal.TLS.CACert
	cfg.TemporalTLS.ClientCert = c.Temporal.TLS.ClientCert
	cfg.TemporalTLS.ClientKey = c.Temporal.TLS.ClientKey
	cfg.TemporalTLS.ServerName = c.Temporal.TLS.ServerName

	cfg.SpiffeEnabled = c.Spire.SpiffeEnabled
	cfg.SpiffeEndpointSocket = c.Spire.SpiffeEndpointSocket
	cfg.SpiffeJWTExchangeEndpoint = c.Spire.SpiffeJWTExchangeEndpoint
	cfg.SpireServerNamespace = c.Spire.ServerNamespace
	cfg.SpireClassName = c.Spire.ClassName
	cfg.SpireTPMPubhashesConfigMapName = c.Spire.TPMPubhashesConfigMapName
	cfg.SpireServerPort = c.Spire.ServerPort
	cfg.SpireJoinTokenTTL = c.Spire.JoinTokenTTL.Duration

	cfg.Teleport.Enabled = c.Teleport.Enabled
	cfg.Teleport.IdentityFile = c.Teleport.IdentityFile
	cfg.Teleport.ProxyAddress = c.Teleport.ProxyAddress
	cfg.Teleport.Environment = c.Teleport.Environment
	cfg.Teleport.ManagedByLabelValue = c.Teleport.ManagedByLabelValue
	cfg.Teleport.TokenNamePrefix = c.Teleport.TokenNamePrefix
	cfg.Teleport.ResourceGroupLabelValue = c.Teleport.ResourceGroupLabelValue

	cfg.PermissiveMode = c.PermissiveMode
}
