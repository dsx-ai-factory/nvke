// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// forwardKubeconfig fetches the admin kubeconfig from the node, forwards its
// API server port, and writes a copy that reaches the cluster through the
// forward. It also returns the API server's port on the node.
func (b bootstrap) forwardKubeconfig(ctx context.Context) (string, int, error) {
	raw, err := b.session.Fetch(ctx, kubeconfigPath)
	if err != nil {
		return "", 0, err
	}
	port, err := apiServerPort(raw)
	if err != nil {
		return "", 0, err
	}
	// The site's API server is on the node itself, so this one is loopback.
	local, err := b.session.Forward(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", 0, err
	}
	rewritten, err := rewriteServer(raw, local)
	if err != nil {
		return "", 0, err
	}
	outputDir, err := os.MkdirTemp("", "nke-e2e-site-")
	if err != nil {
		return "", 0, err
	}
	out := filepath.Join(outputDir, b.spec.Name+".kubeconfig")
	if err := os.WriteFile(out, rewritten, 0o600); err != nil {
		return "", 0, err
	}
	return out, port, nil
}

// KeepKubeconfig writes the cluster's admin kubeconfig into dir as
// name.kubeconfig, for a site that outlives the run. The run's forward ends
// with it, so the copy points at 127.0.0.1 on the API server's own port,
// which a forward to the node's loopback on that port reaches, and keeps the
// server name its certificate covers.
func KeepKubeconfig(cluster Cluster, dir, name string) (string, error) {
	if cluster.Kubeconfig == "" || cluster.APIServerPort == 0 {
		return "", fmt.Errorf("site %s was not bootstrapped, so it has no kubeconfig", name)
	}
	forwarded, err := os.ReadFile(cluster.Kubeconfig)
	if err != nil {
		return "", err
	}
	kept, err := rewriteServer(forwarded, net.JoinHostPort("127.0.0.1", strconv.Itoa(cluster.APIServerPort)))
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, name+".kubeconfig")
	if err := os.WriteFile(out, kept, 0o600); err != nil {
		return "", err
	}
	return out, nil
}

func apiServerPort(raw []byte) (int, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return 0, fmt.Errorf("parse kubeconfig: %w", err)
	}
	cluster, err := currentCluster(cfg)
	if err != nil {
		return 0, err
	}
	u, err := url.Parse(cluster.Server)
	if err != nil {
		return 0, fmt.Errorf("parse server %q: %w", cluster.Server, err)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return strconv.Atoi(port)
}

// rewriteServer points the current cluster at local. The API server
// certificate does not cover the loopback address the forward listens on, so
// TLS keeps verifying against the host the node advertised.
func rewriteServer(raw []byte, local string) ([]byte, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	cluster, err := currentCluster(cfg)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(cluster.Server)
	if err != nil {
		return nil, fmt.Errorf("parse server %q: %w", cluster.Server, err)
	}
	if _, _, err := net.SplitHostPort(local); err != nil {
		return nil, fmt.Errorf("forward address %q: %w", local, err)
	}
	if cluster.TLSServerName == "" {
		cluster.TLSServerName = u.Hostname()
	}
	u.Host = local
	cluster.Server = u.String()
	return clientcmd.Write(*cfg)
}

func currentCluster(cfg *clientcmdapi.Config) (*clientcmdapi.Cluster, error) {
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return nil, fmt.Errorf("kubeconfig current context %q not found", cfg.CurrentContext)
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("kubeconfig cluster %q not found", kctx.Cluster)
	}
	return cluster, nil
}
