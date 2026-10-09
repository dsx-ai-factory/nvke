// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidckubeconfig builds an oidc-login kubeconfig from an input
// kubeconfig.
package oidckubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	oidcUser    = "oidc"
	execCommand = "kubectl"
	execAPI     = "client.authentication.k8s.io/v1"
)

// Options are the OIDC exec-plugin settings stamped into the generated kubeconfig.
type Options struct {
	IssuerURL   string
	ClientID    string
	ExtraScopes []string
}

// Generate writes a kubeconfig whose cluster stanza is taken from the current
// context of kubeconfigPath and whose user is kubectl oidc-login get-token.
// The input user is not copied. A certificate-authority file is inlined as
// certificate-authority-data so the output does not depend on that path.
func Generate(kubeconfigPath string, opts Options) ([]byte, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig must not be empty")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}

	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", kubeconfigPath, err)
	}
	return render(cfg, filepath.Dir(kubeconfigPath), opts)
}

// FromKubeconfig writes an oidc-login kubeconfig from kubeconfig bytes.
// The cluster stanza is the current context's cluster. The input user is not
// copied. A certificate-authority path with no certificate-authority-data is
// rejected: the bytes have no directory to resolve that path against.
func FromKubeconfig(data []byte, opts Options) ([]byte, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return render(cfg, "", opts)
}

func (o Options) validate() error {
	if o.IssuerURL == "" {
		return fmt.Errorf("oidc-issuer-url must not be empty")
	}
	if o.ClientID == "" {
		return fmt.Errorf("oidc-client-id must not be empty")
	}
	return nil
}

func render(cfg *clientcmdapi.Config, baseDir string, opts Options) ([]byte, error) {
	clusterName, cluster, err := currentCluster(cfg)
	if err != nil {
		return nil, err
	}

	outCluster, err := copyCluster(baseDir, clusterName, cluster)
	if err != nil {
		return nil, err
	}

	out := clientcmdapi.NewConfig()
	out.Clusters[clusterName] = outCluster
	out.AuthInfos[oidcUser] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      execAPI,
			Command:         execCommand,
			Args:            execArgs(opts),
			InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
		},
	}
	contextName := oidcUser + "@" + clusterName
	out.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: oidcUser,
	}
	out.CurrentContext = contextName

	data, err := clientcmd.Write(*out)
	if err != nil {
		return nil, fmt.Errorf("serializing oidc kubeconfig: %w", err)
	}
	return data, nil
}

func currentCluster(cfg *clientcmdapi.Config) (string, *clientcmdapi.Cluster, error) {
	if cfg.CurrentContext == "" {
		return "", nil, fmt.Errorf("kubeconfig has no current-context")
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return "", nil, fmt.Errorf("kubeconfig current-context %q is not defined", cfg.CurrentContext)
	}
	if ctx.Cluster == "" {
		return "", nil, fmt.Errorf("kubeconfig context %q has no cluster", cfg.CurrentContext)
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return "", nil, fmt.Errorf("kubeconfig cluster %q is not defined", ctx.Cluster)
	}
	if cluster.Server == "" {
		return "", nil, fmt.Errorf("kubeconfig cluster %q has no server", ctx.Cluster)
	}
	return ctx.Cluster, cluster, nil
}

func copyCluster(baseDir, clusterName string, cluster *clientcmdapi.Cluster) (*clientcmdapi.Cluster, error) {
	out := &clientcmdapi.Cluster{
		Server:                   cluster.Server,
		TLSServerName:            cluster.TLSServerName,
		InsecureSkipTLSVerify:    cluster.InsecureSkipTLSVerify,
		CertificateAuthorityData: slices.Clone(cluster.CertificateAuthorityData),
		ProxyURL:                 cluster.ProxyURL,
		DisableCompression:       cluster.DisableCompression,
	}
	if len(out.CertificateAuthorityData) == 0 && cluster.CertificateAuthority != "" {
		if baseDir == "" {
			return nil, fmt.Errorf("kubeconfig cluster %q references certificate-authority %q and has no certificate-authority-data", clusterName, cluster.CertificateAuthority)
		}
		caPath := cluster.CertificateAuthority
		if !filepath.IsAbs(caPath) {
			caPath = filepath.Join(baseDir, caPath)
		}
		data, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("reading certificate-authority %q: %w", cluster.CertificateAuthority, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("certificate-authority %q is empty", cluster.CertificateAuthority)
		}
		out.CertificateAuthorityData = data
	}
	return out, nil
}

func execArgs(opts Options) []string {
	args := []string{
		"oidc-login",
		"get-token",
		"--oidc-issuer-url=" + opts.IssuerURL,
		"--oidc-client-id=" + opts.ClientID,
	}
	for _, scope := range opts.ExtraScopes {
		args = append(args, "--oidc-extra-scope="+scope)
	}
	return args
}
