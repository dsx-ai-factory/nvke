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
	// Headless stamps the device-code grant so login does not open a browser
	// on the machine running kubectl. The identity provider client must allow
	// the device authorization grant.
	Headless bool
}

// Generate writes a kubeconfig whose cluster stanza is taken from the current
// context of kubeconfigPath and whose user is kubectl oidc-login get-token.
// The input user is not copied. A certificate-authority file is inlined as
// certificate-authority-data so the output does not depend on that path.
func Generate(kubeconfigPath string, opts Options) ([]byte, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig must not be empty")
	}
	if opts.IssuerURL == "" {
		return nil, fmt.Errorf("oidc-issuer-url must not be empty")
	}
	if opts.ClientID == "" {
		return nil, fmt.Errorf("oidc-client-id must not be empty")
	}

	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", kubeconfigPath, err)
	}

	clusterName, cluster, err := currentCluster(cfg)
	if err != nil {
		return nil, err
	}

	outCluster, err := copyCluster(kubeconfigPath, cluster)
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

func copyCluster(kubeconfigPath string, cluster *clientcmdapi.Cluster) (*clientcmdapi.Cluster, error) {
	out := &clientcmdapi.Cluster{
		Server:                   cluster.Server,
		TLSServerName:            cluster.TLSServerName,
		InsecureSkipTLSVerify:    cluster.InsecureSkipTLSVerify,
		CertificateAuthorityData: slices.Clone(cluster.CertificateAuthorityData),
		ProxyURL:                 cluster.ProxyURL,
		DisableCompression:       cluster.DisableCompression,
	}
	if len(out.CertificateAuthorityData) == 0 && cluster.CertificateAuthority != "" {
		caPath := cluster.CertificateAuthority
		if !filepath.IsAbs(caPath) {
			caPath = filepath.Join(filepath.Dir(kubeconfigPath), caPath)
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
	if opts.Headless {
		args = append(args, "--grant-type=device-code", "--skip-open-browser")
	}
	for _, scope := range opts.ExtraScopes {
		args = append(args, "--oidc-extra-scope="+scope)
	}
	return args
}
