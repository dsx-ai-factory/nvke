// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// GroupName is the Kubernetes group bound to cluster-admin.
	GroupName = "nvke:breakglass"

	// BindingName is the ClusterRoleBinding that maps GroupName to cluster-admin.
	BindingName = "nvke:breakglass"

	certWaitInterval = 200 * time.Millisecond
	certWaitTimeout  = 30 * time.Second
	certLifetime     = int32(365 * 24 * 3600)
)

// GenerateBreakglassKubeconfig mints an admin breakglass kubeconfig from the cluster's published kubeconfig.
func GenerateBreakglassKubeconfig(ctx context.Context, adminKubeconfig []byte, clusterName string) ([]byte, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(adminKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	cfg.Timeout = certWaitTimeout
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building a client from admin kubeconfig: %w", err)
	}
	return issue(ctx, client, adminKubeconfig, clusterName)
}

// issue mints an admin breakglass kubeconfig. adminKubeconfig is only used
// for the API server URL and cluster CA; its client key is not copied.
func issue(ctx context.Context, client kubernetes.Interface, adminKubeconfig []byte, clusterName string) ([]byte, error) {
	if err := applyBinding(ctx, client); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := mintClientCert(ctx, client, clusterName)
	if err != nil {
		return nil, err
	}
	return assembleKubeconfig(adminKubeconfig, certPEM, keyPEM)
}

func applyBinding(ctx context.Context, client kubernetes.Interface) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: BindingName},
		Subjects: []rbacv1.Subject{{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     GroupName,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
	}
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("applying %s ClusterRoleBinding: %w", BindingName, err)
	}
	return nil
}

func mintClientCert(ctx context.Context, client kubernetes.Interface, clusterName string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating client key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling client key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   GroupName + ":" + clusterName,
			Organization: []string{GroupName},
		},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating client CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, nil, fmt.Errorf("naming client CSR: %w", err)
	}
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nvke-breakglass-%x", suffix)},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           csrPEM,
			SignerName:        certificatesv1.KubeAPIServerClientSignerName,
			Usages:            []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
			ExpirationSeconds: certLifetimeSeconds(),
		},
	}
	created, err := client.CertificatesV1().CertificateSigningRequests().Create(ctx, csr, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("submitting client CSR: %w", err)
	}
	// ctx may already be cancelled when we return (wait timeout, caller abort).
	// The CSR is approved for a cluster-admin client cert and must not stay.
	defer func() {
		_ = client.CertificatesV1().CertificateSigningRequests().Delete(
			context.WithoutCancel(ctx), created.Name, metav1.DeleteOptions{},
		)
	}()

	created.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type:    certificatesv1.CertificateApproved,
		Status:  corev1.ConditionTrue,
		Reason:  "NvkectlBootstrap",
		Message: "approved by nvkectl bootstrap site",
	}}
	if _, err := client.CertificatesV1().CertificateSigningRequests().UpdateApproval(
		ctx, created.Name, created, metav1.UpdateOptions{},
	); err != nil {
		return nil, nil, fmt.Errorf("approving client CSR %s: %w", created.Name, err)
	}

	certPEM, err = waitForCertificate(ctx, client, created.Name)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

func certLifetimeSeconds() *int32 {
	seconds := certLifetime
	return &seconds
}

func waitForCertificate(ctx context.Context, client kubernetes.Interface, name string) ([]byte, error) {
	var certPEM []byte
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, certWaitInterval, certWaitTimeout, true, func(ctx context.Context) (bool, error) {
		got, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if isTransientAPIError(err) {
				lastErr = err
				return false, nil
			}
			return false, err
		}
		for _, cond := range got.Status.Conditions {
			if cond.Status != corev1.ConditionTrue {
				continue
			}
			switch cond.Type {
			case certificatesv1.CertificateDenied:
				return false, fmt.Errorf("client CSR %s denied: %s", name, cond.Message)
			case certificatesv1.CertificateFailed:
				return false, fmt.Errorf("client CSR %s failed: %s", name, cond.Message)
			}
		}
		if len(got.Status.Certificate) == 0 {
			return false, nil
		}
		certPEM = got.Status.Certificate
		return true, nil
	})
	if err != nil {
		if lastErr != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return nil, fmt.Errorf("waiting for client CSR %s to be signed: %w", name, lastErr)
		}
		return nil, fmt.Errorf("waiting for client CSR %s to be signed: %w", name, err)
	}
	return certPEM, nil
}

// isTransientAPIError reports poll errors that should be retried until the wait budget expires.
func isTransientAPIError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err)
}

func assembleKubeconfig(adminKubeconfig, certPEM, keyPEM []byte) ([]byte, error) {
	cfg, err := clientcmd.Load(adminKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	server, ca, err := clusterEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	out := clientcmdapi.NewConfig()
	out.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: ca,
	}
	out.AuthInfos["nvke-breakglass"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	out.Contexts["nvke-breakglass"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "nvke-breakglass"}
	out.CurrentContext = "nvke-breakglass"
	return clientcmd.Write(*out)
}

func clusterEndpoint(cfg *clientcmdapi.Config) (server string, ca []byte, err error) {
	if ctx := cfg.Contexts[cfg.CurrentContext]; ctx != nil {
		if cluster := cfg.Clusters[ctx.Cluster]; cluster != nil {
			return cluster.Server, cluster.CertificateAuthorityData, nil
		}
	}
	for _, cluster := range cfg.Clusters {
		if cluster.Server != "" {
			return cluster.Server, cluster.CertificateAuthorityData, nil
		}
	}
	return "", nil, fmt.Errorf("admin kubeconfig has no cluster endpoint")
}
