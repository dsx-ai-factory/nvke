// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capisecret "sigs.k8s.io/cluster-api/util/secret"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// defaultAPIServerPort is used when a kubeconfig server URL carries no explicit
	// port, which is how a URL is written when the port is https' default.
	defaultAPIServerPort = 443
)

// clusterCASecretName returns the Secret name the kubeadm bootstrap provider
// looks up for a cluster's CA. The name is fixed by the CAPI contract
// (util/secret.Name), not chosen by NVKE.
func clusterCASecretName(clusterName string) string {
	return capisecret.Name(clusterName, capisecret.ClusterCA)
}

// publishCAPIBootstrapContract satisfies the two cluster-scoped preconditions
// the kubeadm bootstrap provider checks before it will generate join data
// for a worker:
//   - a cluster CA Secret it can read
//   - a control-plane endpoint on the Cluster object
//
// Both are derived from a joined control-plane node's admin kubeconfig. That is
// the only place NVKE learns the endpoint kubeadm actually baked into the
// cluster: it is the kube-vip VIP when the cluster declares one, and the first
// control-plane node's IP when it does not, so reading it back beats
// recomputing it.
func (c *Controller) publishCAPIBootstrapContract(ctx context.Context, cluster db.Cluster, adminConf []byte) error {
	live, skip, err := c.refreshCluster(ctx, cluster)
	if err != nil || skip {
		return err
	}

	parsed, err := clientcmd.Load(adminConf)
	if err != nil {
		return fmt.Errorf("parsing kubeconfig for cluster %s: %w", live.Name, err)
	}

	ca, err := extractCAFromKubeconfig(parsed)
	if err != nil {
		return err
	}

	server, err := extractServerFromKubeconfig(parsed)
	if err != nil {
		return err
	}

	host, port, err := parseAPIEndpoint(server)
	if err != nil {
		return fmt.Errorf("parsing API server endpoint for cluster %s: %w", live.Name, err)
	}

	if err := c.upsertClusterCASecret(ctx, live, ca); err != nil {
		return err
	}

	return c.setControlPlaneEndpoint(ctx, live, host, port)
}

// upsertClusterCASecret publishes the tenant CA as the Secret the kubeadm
// bootstrap provider resolves by the CAPI naming contract.
func (c *Controller) upsertClusterCASecret(ctx context.Context, cluster db.Cluster, ca []byte) error {
	// CABPK's worker-join path calls Certificates.EnsureAllExist, which requires
	// tls.key even though a worker join only uses the CA certificate. The
	// placeholder is not a PEM private key, so a caller that actually needs the
	// CA key fails instead of using these bytes.
	//
	// Remove the placeholder after NVKE uses a CAPI release containing the fix:
	// https://github.com/kubernetes-sigs/cluster-api/issues/14388
	const clusterCAKeyPlaceholder = "NOT-A-KEY"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterCASecretName(cluster.Name),
			Namespace: c.secretsNamespace(),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			capisecret.TLSCrtDataName: ca,
			capisecret.TLSKeyDataName: []byte(clusterCAKeyPlaceholder),
		},
	}
	// The cluster CA Secret is not a kubeconfig, so it has no kubeconfig mode.
	capi.SetSecretOwnership(secret, cluster, "", c.cfg.ManagedByLabelValue)
	return c.upsertSecret(ctx, cluster, secret)
}

// setControlPlaneEndpoint writes the API server endpoint onto the CAPI Cluster.
//
// NVKE sets this directly rather than having it copied up from the
// InfrastructureCluster: NicoCluster has no controlPlaneEndpoint field, and NICo
// provisions machines without knowing the Kubernetes endpoint that will front
// them. The CAPI contract allows the creator to supply it, and the Cluster
// controller never overwrites an endpoint that is already valid.
func (c *Controller) setControlPlaneEndpoint(ctx context.Context, cluster db.Cluster, host string, port int32) error {
	capiCluster := &clusterv1.Cluster{}
	key := types.NamespacedName{Namespace: c.secretsNamespace(), Name: cluster.Name}
	if err := c.k8sClient.Get(ctx, key, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Info("Waiting for CAPI Cluster before publishing control-plane endpoint", "clusterName", cluster.Name)
			return nil
		}
		return fmt.Errorf("getting CAPI cluster %s: %w", cluster.Name, err)
	}

	if capiCluster.Spec.ControlPlaneEndpoint.Host == host && capiCluster.Spec.ControlPlaneEndpoint.Port == port {
		return nil
	}

	patch := ctrlclient.MergeFrom(capiCluster.DeepCopy())
	capiCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		Host: host,
		Port: port,
	}

	if err := c.k8sClient.Patch(ctx, capiCluster, patch); err != nil {
		return fmt.Errorf("setting control-plane endpoint on CAPI cluster %s: %w", cluster.Name, err)
	}

	c.logger.Info("Set CAPI control-plane endpoint", "clusterName", cluster.Name, "host", host, "port", port)
	return nil
}

// parseAPIEndpoint splits a kubeconfig server URL into the host and port halves
// of a CAPI APIEndpoint.
func parseAPIEndpoint(server string) (string, int32, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return "", 0, fmt.Errorf("parsing server URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("server URL %q has no host", server)
	}

	portText := parsed.Port()
	if portText == "" {
		return host, defaultAPIServerPort, nil
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("server URL %q has invalid port %q", server, portText)
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("server URL %q has out-of-range port %d", server, port)
	}
	return host, int32(port), nil
}
