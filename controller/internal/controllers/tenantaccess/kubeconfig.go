// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

const (
	// probeTimeout bounds a single control-plane node GetKubeconfig probe so one
	// stalled node cannot hold up this reconcile or the other nodes' probes.
	//
	// An unbounded probe against an unreachable agent would otherwise occupy a
	// controller-runtime worker indefinitely.
	probeTimeout = 10 * time.Second

	// tenantRequestTimeout bounds every request the controller makes against a
	// tenant cluster: the discovery round-trip when the client is built, and each
	// RBAC apply. Same reasoning as probeTimeout, one layer out.
	tenantRequestTimeout = 30 * time.Second

	// TrustBundleSecretKey is the data key inside the per-cluster trust-bundle
	// Secret (OIDC clusters only).
	TrustBundleSecretKey = "ca.crt"

	// SPIRE exec-auth constants. The JWT audience (per-cluster name), the SPIFFE
	// endpoint socket and the token-exchange endpoint (both from config) are
	// supplied at build time.
	spireExecAPIVersion = "client.authentication.k8s.io/v1"
	spireExecCommand    = "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"
)

// TrustBundleSecretName returns the Secret name that holds the per-cluster
// tenant CA trust bundle published into SecretsNamespace (OIDC clusters only).
func TrustBundleSecretName(clusterName string) string {
	return clusterName + "-cluster-trust-bundle"
}

// fetchAdminKubeconfig retrieves the verbatim admin.conf from a healthy
// control-plane node via the agent GetKubeconfig RPC. The second return
// value reports whether a ready control-plane node served a kubeconfig; when
// false with a nil error, the cluster's control plane is not yet reachable and
// the caller should skip this reconcile pass.
func (c *Controller) fetchAdminKubeconfig(ctx context.Context, cluster db.Cluster) ([]byte, bool, error) {
	controlPlaneNodes, err := c.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
		ControlPlane: ptr.To(true),
	})
	if err != nil {
		return nil, false, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(controlPlaneNodes)
	if len(candidates) == 0 {
		// Not-ready gate: no joined control-plane node exists yet.
		return nil, false, nil
	}

	// Probe the same node first on every pass. kubeadm issues each control-plane
	// node its own admin.conf, with its own client certificate, so probing them in
	// a different order publishes different bytes for the same cluster. ListNodes
	// has no ORDER BY, so without this the Secret is rewritten on most passes.
	slices.SortFunc(candidates, func(a, b db.Node) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	if !healthyCandidates {
		c.logger.Warn("No healthy joined control plane nodes found, falling back to joined nodes for kubeconfig extraction", "clusterID", cluster.ID, "clusterName", cluster.Name)
	}

	var candidateErrs []error
	for _, node := range candidates {
		kubeconfigBytes, err := c.probeKubeconfig(ctx, node)
		if err != nil {
			candidateErrs = append(candidateErrs, fmt.Errorf("node %s: %w", node.ID, err))
			continue
		}
		if kubeconfigBytes == "" {
			candidateErrs = append(candidateErrs, fmt.Errorf("node %s returned an empty kubeconfig", node.ID))
			continue
		}
		return []byte(kubeconfigBytes), true, nil
	}

	return nil, false, fmt.Errorf("failed to extract kubeconfig from %d control plane candidates: %w", len(candidates), errors.Join(candidateErrs...))
}

// probeKubeconfig fetches one node's admin.conf under probeTimeout. It is a
// method so the deferred cancel is scoped to the single probe rather than to the
// whole candidate loop.
func (c *Controller) probeKubeconfig(ctx context.Context, node db.Node) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	return c.nodeAgentClient.GetKubeconfig(ctx, node)
}

// buildSpireKubeconfig assembles a kubeconfig whose user is the SPIRE exec
// plugin, with the given server and CA. The JWT audience is the cluster name and
// the SPIFFE workload-API socket is spiffeSocket.
//
// exchangeEndpoint is passed through to the plugin and may be empty, in which
// case the plugin presents the JWT-SVID it fetched rather than exchanging it.
// The variable is omitted entirely when empty, so the kubeconfig published for a
// site that does not use an exchange is byte-identical to what it was before.
func buildSpireKubeconfig(clusterName, server string, caData []byte, spiffeSocket, exchangeEndpoint string) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caData,
	}
	env := []clientcmdapi.ExecEnvVar{
		{Name: "SPIFFE_ENDPOINT_SOCKET", Value: spiffeSocket},
		{Name: "SPIFFE_JWT_AUDIENCE", Value: clusterName},
	}
	if exchangeEndpoint != "" {
		env = append(env, clientcmdapi.ExecEnvVar{Name: "SPIFFE_JWT_EXCHANGE_ENDPOINT", Value: exchangeEndpoint})
	}
	cfg.AuthInfos["spiffe-user"] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      spireExecAPIVersion,
			Command:         spireExecCommand,
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			Env:             env,
		},
	}
	cfg.Contexts["spiffe"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "spiffe-user"}
	cfg.CurrentContext = "spiffe"
	return clientcmd.Write(*cfg)
}

// extractServerFromKubeconfig returns the cluster's API server URL
// (e.g. "https://host:6443") from a parsed kubeconfig.
func extractServerFromKubeconfig(cfg *clientcmdapi.Config) (string, error) {
	for _, c := range cfg.Clusters {
		if c.Server != "" {
			return c.Server, nil
		}
	}
	return "", errors.New("kubeconfig contains no server URL")
}

// extractCAFromKubeconfig returns the cluster CA as PEM bytes from a parsed
// kubeconfig.
func extractCAFromKubeconfig(cfg *clientcmdapi.Config) ([]byte, error) {
	for _, c := range cfg.Clusters {
		if len(c.CertificateAuthorityData) > 0 {
			return c.CertificateAuthorityData, nil
		}
	}
	return nil, errors.New("kubeconfig contains no certificate-authority-data")
}
