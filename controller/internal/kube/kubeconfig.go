// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"errors"
	"fmt"
	"net/url"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// maxKubeconfigBytes bounds a tenant kubeconfig. A kubeadm admin.conf is a
	// few kilobytes.
	maxKubeconfigBytes = 64 << 10

	// SPIREExecCommand is the only credential plugin a tenant kubeconfig may name.
	SPIREExecCommand = "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"

	// The environment variables a SPIRE exec kubeconfig may set.
	SPIREEndpointSocketEnv      = "SPIFFE_ENDPOINT_SOCKET"
	SPIREJWTAudienceEnv         = "SPIFFE_JWT_AUDIENCE"
	SPIREJWTExchangeEndpointEnv = "SPIFFE_JWT_EXCHANGE_ENDPOINT"
)

// ErrInvalidKubeconfig wraps every error that rejects a kubeconfig's content,
// so callers can count rejections apart from other failures.
var ErrInvalidKubeconfig = errors.New("invalid tenant kubeconfig")

// adminKubeconfig is the part of a kubeadm admin.conf the controller uses.
type adminKubeconfig struct {
	clusterName string
	userName    string
	contextName string
	server      string
	caData      []byte
	certData    []byte
	keyData     []byte
}

// SanitizeAdminKubeconfig validates a control-plane node's admin.conf and
// returns a kubeconfig rebuilt from only its server, CA and client certificate.
// Its current context may set only an https server, inline CA data and an
// inline client certificate and key. Any other field, such as an exec plugin,
// a token file or a proxy, is rejected. Entries the current context does not
// name are dropped.
func SanitizeAdminKubeconfig(raw []byte) ([]byte, error) {
	admin, err := loadAdminKubeconfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKubeconfig, err)
	}

	out := clientcmdapi.NewConfig()
	out.Clusters[admin.clusterName] = &clientcmdapi.Cluster{
		Server:                   admin.server,
		CertificateAuthorityData: admin.caData,
	}
	out.AuthInfos[admin.userName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: admin.certData,
		ClientKeyData:         admin.keyData,
	}
	out.Contexts[admin.contextName] = &clientcmdapi.Context{Cluster: admin.clusterName, AuthInfo: admin.userName}
	out.CurrentContext = admin.contextName
	return clientcmd.Write(*out)
}

// NewRESTConfigFromAdminKubeconfig builds a REST config from a control-plane
// node's admin.conf. It accepts only what SanitizeAdminKubeconfig keeps.
func NewRESTConfigFromAdminKubeconfig(raw []byte) (*rest.Config, error) {
	admin, err := loadAdminKubeconfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKubeconfig, err)
	}
	return admin.restConfig(), nil
}

// NewRESTConfigFromKubeconfig builds a REST config from a published tenant
// kubeconfig Secret. It accepts an admin kubeconfig in the form
// SanitizeAdminKubeconfig writes, or a SPIRE exec kubeconfig whose audience is
// clusterName.
func NewRESTConfigFromKubeconfig(raw []byte, clusterName string) (*rest.Config, error) {
	restConfig, err := restConfigFromTenantKubeconfig(raw, clusterName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKubeconfig, err)
	}
	return restConfig, nil
}

func restConfigFromTenantKubeconfig(raw []byte, clusterName string) (*rest.Config, error) {
	cfg, err := loadKubeconfig(raw)
	if err != nil {
		return nil, err
	}

	kubeContext, cluster, auth, err := currentContext(cfg)
	if err != nil {
		return nil, err
	}
	if auth.Exec == nil {
		admin, err := parseAdminKubeconfig(cfg.CurrentContext, kubeContext, cluster, auth)
		if err != nil {
			return nil, err
		}
		return admin.restConfig(), nil
	}

	if err := validateCluster(cluster); err != nil {
		return nil, err
	}
	if err := validateSPIREExec(auth, clusterName); err != nil {
		return nil, err
	}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(*cfg, cfg.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build REST config from kubeconfig: %w", err)
	}
	restConfig.ExecProvider.PluginPolicy = clientcmdapi.PluginPolicy{
		PolicyType: clientcmdapi.PluginPolicyAllowlist,
		Allowlist:  []clientcmdapi.AllowlistEntry{{Command: SPIREExecCommand}},
	}
	return restConfig, nil
}

// loadKubeconfig deserializes raw. clientcmd.Load only decodes; it reads no
// files and runs no plugins.
func loadKubeconfig(raw []byte) (*clientcmdapi.Config, error) {
	if len(raw) > maxKubeconfigBytes {
		return nil, fmt.Errorf("kubeconfig is %d bytes, larger than the %d byte limit", len(raw), maxKubeconfigBytes)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}
	return cfg, nil
}

func loadAdminKubeconfig(raw []byte) (adminKubeconfig, error) {
	cfg, err := loadKubeconfig(raw)
	if err != nil {
		return adminKubeconfig{}, err
	}
	kubeContext, cluster, auth, err := currentContext(cfg)
	if err != nil {
		return adminKubeconfig{}, err
	}
	return parseAdminKubeconfig(cfg.CurrentContext, kubeContext, cluster, auth)
}

// currentContext returns the current context and the cluster and user it names.
func currentContext(cfg *clientcmdapi.Config) (*clientcmdapi.Context, *clientcmdapi.Cluster, *clientcmdapi.AuthInfo, error) {
	kubeContext := cfg.Contexts[cfg.CurrentContext]
	if kubeContext == nil {
		return nil, nil, nil, fmt.Errorf("kubeconfig current context %q not found", cfg.CurrentContext)
	}
	if kubeContext.Namespace != "" || len(kubeContext.Extensions) > 0 {
		return nil, nil, nil, errors.New("kubeconfig context may set only a cluster and a user")
	}
	cluster := cfg.Clusters[kubeContext.Cluster]
	if cluster == nil {
		return nil, nil, nil, fmt.Errorf("kubeconfig cluster %q not found", kubeContext.Cluster)
	}
	auth := cfg.AuthInfos[kubeContext.AuthInfo]
	if auth == nil {
		return nil, nil, nil, fmt.Errorf("kubeconfig user %q not found", kubeContext.AuthInfo)
	}
	return kubeContext, cluster, auth, nil
}

func parseAdminKubeconfig(
	contextName string,
	kubeContext *clientcmdapi.Context,
	cluster *clientcmdapi.Cluster,
	auth *clientcmdapi.AuthInfo,
) (adminKubeconfig, error) {
	if err := validateCluster(cluster); err != nil {
		return adminKubeconfig{}, err
	}
	if err := validateAdminUser(auth); err != nil {
		return adminKubeconfig{}, err
	}
	return adminKubeconfig{
		clusterName: kubeContext.Cluster,
		userName:    kubeContext.AuthInfo,
		contextName: contextName,
		server:      cluster.Server,
		caData:      cluster.CertificateAuthorityData,
		certData:    auth.ClientCertificateData,
		keyData:     auth.ClientKeyData,
	}, nil
}

func (a adminKubeconfig) restConfig() *rest.Config {
	return &rest.Config{
		Host: a.server,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   a.caData,
			CertData: a.certData,
			KeyData:  a.keyData,
		},
	}
}

// validateCluster accepts an https server with inline CA data and nothing else.
func validateCluster(cluster *clientcmdapi.Cluster) error {
	if len(cluster.CertificateAuthorityData) == 0 {
		return errors.New("kubeconfig cluster has no certificate-authority-data")
	}
	if cluster.CertificateAuthority != "" || cluster.InsecureSkipTLSVerify || cluster.TLSServerName != "" ||
		cluster.ProxyURL != "" || len(cluster.Extensions) > 0 {
		return errors.New("kubeconfig cluster may set only server and certificate-authority-data")
	}
	server, err := url.Parse(cluster.Server)
	if err != nil {
		return fmt.Errorf("kubeconfig cluster server is not a URL: %w", err)
	}
	if server.Scheme != "https" || server.Host == "" || server.User != nil ||
		(server.Path != "" && server.Path != "/") || server.RawQuery != "" || server.Fragment != "" {
		return fmt.Errorf("kubeconfig cluster server %q must be https://host[:port]", cluster.Server)
	}
	return nil
}

// validateAdminUser accepts inline client certificate data, or no
// credentials, and nothing else.
func validateAdminUser(auth *clientcmdapi.AuthInfo) error {
	if auth.Exec != nil || auth.AuthProvider != nil || hasOtherCredentials(auth) {
		return errors.New("kubeconfig user may set only client-certificate-data and client-key-data")
	}
	if (len(auth.ClientCertificateData) == 0) != (len(auth.ClientKeyData) == 0) {
		return errors.New("kubeconfig user must set both client-certificate-data and client-key-data, or neither")
	}
	return nil
}

// validateSPIREExec accepts the SPIRE exec plugin for clusterName's audience and
// nothing else.
func validateSPIREExec(auth *clientcmdapi.AuthInfo, clusterName string) error {
	if auth.AuthProvider != nil || hasOtherCredentials(auth) ||
		len(auth.ClientCertificateData) > 0 || len(auth.ClientKeyData) > 0 {
		return errors.New("kubeconfig SPIRE user may set only exec")
	}
	exec := auth.Exec
	if exec.Command != SPIREExecCommand || len(exec.Args) > 0 || exec.ProvideClusterInfo {
		return fmt.Errorf("kubeconfig exec plugin must be %s with no arguments or cluster info", SPIREExecCommand)
	}
	audience := ""
	for _, env := range exec.Env {
		switch env.Name {
		case SPIREJWTAudienceEnv:
			audience = env.Value
		case SPIREEndpointSocketEnv, SPIREJWTExchangeEndpointEnv:
		default:
			return fmt.Errorf("kubeconfig exec plugin may not set environment variable %s", env.Name)
		}
	}
	if audience != clusterName {
		return fmt.Errorf("kubeconfig exec plugin audience %q does not match cluster %q", audience, clusterName)
	}
	return nil
}

// hasOtherCredentials reports whether auth sets any field other than
// client-certificate-data, client-key-data, exec and auth-provider. Callers
// check those themselves.
func hasOtherCredentials(auth *clientcmdapi.AuthInfo) bool {
	return auth.ClientCertificate != "" || auth.ClientKey != "" ||
		auth.Token != "" || auth.TokenFile != "" ||
		auth.Username != "" || auth.Password != "" ||
		auth.Impersonate != "" || auth.ImpersonateUID != "" ||
		len(auth.ImpersonateGroups) > 0 || len(auth.ImpersonateUserExtra) > 0 ||
		len(auth.Extensions) > 0
}
