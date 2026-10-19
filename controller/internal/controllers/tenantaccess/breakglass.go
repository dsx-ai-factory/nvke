// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"slices"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// breakglassGroup is the tenant group bound to cluster-admin. It matches
	// the site-bootstrap breakglass identity.
	breakglassGroup = "nvke:breakglass"

	// breakglassBinding is the ClusterRoleBinding that maps breakglassGroup to cluster-admin.
	breakglassBinding = "nvke:breakglass"

	breakglassUser = "nvke-breakglass"

	// breakglassRoleKind and breakglassRoleName are the ClusterRole the binding grants.
	breakglassRoleKind = "ClusterRole"
	breakglassRoleName = "cluster-admin"

	// defaultRenewBeforeDays is the chart default for scheduledMaintenance.certRenewal.thresholdDays.
	defaultRenewBeforeDays = 30

	// breakglassCSRName is the one in-flight client CSR on a tenant cluster.
	// A later reconcile continues this object instead of minting another key.
	breakglassCSRName = "nvke-breakglass"

	// breakglassKeySecretKey is the private key on the admin Secret while the CSR
	// is in flight. The CSR returns a certificate, not this key, and the
	// kubeconfig needs both. The key is cleared before value is written, so a
	// failed write mints a new key.
	breakglassKeySecretKey = "breakglass-key"

	certLifetime = int32(365 * 24 * 3600)
)

// issueAdminBreakglass returns the kubeconfig to store in the admin Secret.
// A still-valid client cert is reused when the CA in admin.conf is unchanged.
// A server change keeps that cert. A CA change is reissued. A certificate
// already on the CSR is kept only when it chains to that CA.
func (c *Controller) issueAdminBreakglass(ctx context.Context, adminConf []byte, cluster db.Cluster, existing []byte) ([]byte, error) {
	days := c.cfg.CertRenewalThresholdDays
	if days <= 0 {
		days = defaultRenewBeforeDays
	}
	client, err := c.tenantClientFor(adminConf)
	if err != nil {
		return nil, err
	}
	if err := applyBreakglassBinding(ctx, client); err != nil {
		return nil, err
	}
	if isValidBreakglassKubeconfig(existing, time.Now(), time.Duration(days)*24*time.Hour) {
		reused, ok, err := reuseBreakglassKubeconfig(existing, adminConf)
		if err != nil {
			return nil, err
		}
		if ok {
			return reused, nil
		}
	}

	certPEM, keyPEM, err := c.reconcileBreakglassCSR(ctx, client, cluster)
	if err != nil {
		return nil, err
	}
	return assembleBreakglassKubeconfig(adminConf, certPEM, keyPEM)
}

// reconcileBreakglassCSR moves the one nvke-breakglass CSR forward.
// The certificate is issued by kube-controller-manager after approval, so a reconcile
// that creates or approves the CSR returns before the certificate is there.
// The next reconcile observes it. Once the certificate is present, the CSR is
// deleted and the stored key is cleared before the caller writes the kubeconfig,
// so a failed write mints a new CSR. A signed CSR whose private key is already
// gone is deleted so the name can be used again.
func (c *Controller) reconcileBreakglassCSR(ctx context.Context, client ctrlclient.Client, cluster db.Cluster) ([]byte, []byte, error) {
	clusterName := cluster.Name
	csr := &certificatesv1.CertificateSigningRequest{}
	err := client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, csr)
	if apierrors.IsNotFound(err) {
		csr, err = c.createBreakglassCSR(ctx, client, cluster)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := c.trustedBreakglassCSR(ctx, clusterName, csr); err != nil {
		if delErr := client.Delete(ctx, csr); delErr != nil && !apierrors.IsNotFound(delErr) {
			return nil, nil, fmt.Errorf("%w; deleting untrusted client CSR %s: %w", err, csr.Name, delErr)
		}
		return nil, nil, err
	}
	if cond := cmp.Or(csrCondition(csr, certificatesv1.CertificateDenied), csrCondition(csr, certificatesv1.CertificateFailed)); cond != nil {
		if err := client.Delete(ctx, csr); err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("deleting failed client CSR %s (%s): %w", csr.Name, cond.Message, err)
		}
		return nil, nil, fmt.Errorf("client CSR %s failed: %s", csr.Name, cond.Message)
	}
	if csrCondition(csr, certificatesv1.CertificateApproved) == nil {
		if err := approveBreakglassCSR(ctx, client, csr); err != nil {
			return nil, nil, err
		}
	}
	if len(csr.Status.Certificate) == 0 {
		return nil, nil, fmt.Errorf("client CSR %s is not signed yet", csr.Name)
	}
	keyPEM, err := c.breakglassClientKey(ctx, clusterName)
	if err != nil {
		if delErr := client.Delete(ctx, csr); delErr != nil && !apierrors.IsNotFound(delErr) {
			return nil, nil, fmt.Errorf("%w; deleting orphaned client CSR %s: %w", err, csr.Name, delErr)
		}
		return nil, nil, err
	}
	if err := client.Delete(ctx, csr); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("deleting client CSR %s: %w", csr.Name, err)
	}
	if err := c.clearBreakglassClientKey(ctx, clusterName); err != nil {
		return nil, nil, err
	}
	return csr.Status.Certificate, keyPEM, nil
}

func (c *Controller) createBreakglassCSR(ctx context.Context, client ctrlclient.Client, cluster db.Cluster) (*certificatesv1.CertificateSigningRequest, error) {
	clusterName := cluster.Name
	key, keyPEM, err := generatePrivateKey()
	if err != nil {
		return nil, err
	}
	if err := c.saveBreakglassClientKey(ctx, cluster, keyPEM); err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("%s:%s", breakglassGroup, clusterName),
			Organization: []string{breakglassGroup},
		},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("creating client CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: cert.CertificateRequestBlockType, Bytes: csrDER})
	lifetime := certLifetime
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           csrPEM,
			SignerName:        certificatesv1.KubeAPIServerClientSignerName,
			Usages:            []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
			ExpirationSeconds: &lifetime,
		},
	}
	if err := client.Create(ctx, csr); err != nil {
		return nil, fmt.Errorf("submitting client CSR: %w", err)
	}
	return csr, nil
}

func generatePrivateKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating client key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling client key: %w", err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: keyutil.PrivateKeyBlockType, Bytes: keyDER}), nil
}

func approveBreakglassCSR(ctx context.Context, client ctrlclient.Client, csr *certificatesv1.CertificateSigningRequest) error {
	now := metav1.Now()
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:               certificatesv1.CertificateApproved,
		Status:             corev1.ConditionTrue,
		Reason:             "NVKETenantAccess",
		Message:            "approved by nvke tenant-access",
		LastUpdateTime:     now,
		LastTransitionTime: now,
	})
	if err := client.SubResource("approval").Update(ctx, csr); err != nil {
		return fmt.Errorf("approving client CSR %s: %w", csr.Name, err)
	}
	return nil
}

// trustedBreakglassCSR reports whether csr is the client request this controller
// stored a key for: the breakglass subject, the kube-apiserver client signer,
// client-auth usage, and the same public key as breakglass-key.
func (c *Controller) trustedBreakglassCSR(ctx context.Context, clusterName string, csr *certificatesv1.CertificateSigningRequest) error {
	if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName {
		return fmt.Errorf("client CSR %s signer is %q", csr.Name, csr.Spec.SignerName)
	}
	if len(csr.Spec.Usages) != 1 || csr.Spec.Usages[0] != certificatesv1.UsageClientAuth {
		return fmt.Errorf("client CSR %s usages are %v", csr.Name, csr.Spec.Usages)
	}
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return fmt.Errorf("client CSR %s request is not PEM", csr.Name)
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("parsing client CSR %s request: %w", csr.Name, err)
	}
	if err := req.CheckSignature(); err != nil {
		return fmt.Errorf("client CSR %s request signature: %w", csr.Name, err)
	}
	wantCN := fmt.Sprintf("%s:%s", breakglassGroup, clusterName)
	if req.Subject.CommonName != wantCN || !slices.Equal(req.Subject.Organization, []string{breakglassGroup}) {
		return fmt.Errorf("client CSR %s subject is %s", csr.Name, req.Subject)
	}
	keyPEM, err := c.breakglassClientKey(ctx, clusterName)
	if err != nil {
		return err
	}
	if !sameECDSAPublicKey(req.PublicKey, keyPEM) {
		return fmt.Errorf("client CSR %s public key does not match the stored breakglass key", csr.Name)
	}
	return nil
}

func csrCondition(csr *certificatesv1.CertificateSigningRequest, conditionType certificatesv1.RequestConditionType) *certificatesv1.CertificateSigningRequestCondition {
	for i := range csr.Status.Conditions {
		cond := &csr.Status.Conditions[i]
		if cond.Type == conditionType && cond.Status == corev1.ConditionTrue {
			return cond
		}
	}
	return nil
}

// isValidBreakglassKubeconfig reports whether kubeconfig is one this controller
// wrote and its client cert is still outside the renewal window. Empty means
// the first publish. A kubeconfig without the breakglass user is the node
// admin.conf the previous loop copied into the Secret. A certificate in another
// group, or a private key that does not match it, is reissued.
func isValidBreakglassKubeconfig(kubeconfig []byte, now time.Time, renewBefore time.Duration) bool {
	if len(kubeconfig) == 0 {
		return false
	}
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil || cfg == nil {
		return false
	}
	user := cfg.AuthInfos[breakglassUser]
	if user == nil {
		return false
	}
	block, _ := pem.Decode(user.ClientCertificateData)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return breakglassCertMatches(cert, user.ClientKeyData) && cert.NotAfter.After(now.Add(renewBefore))
}

func breakglassCertMatches(cert *x509.Certificate, keyPEM []byte) bool {
	if !slices.Contains(cert.Subject.Organization, breakglassGroup) {
		return false
	}
	return sameECDSAPublicKey(cert.PublicKey, keyPEM)
}

func sameECDSAPublicKey(pub any, keyPEM []byte) bool {
	key, err := keyutil.ParsePrivateKeyPEM(keyPEM)
	priv, ok := key.(*ecdsa.PrivateKey)
	if err != nil || !ok {
		return false
	}
	got, ok := pub.(*ecdsa.PublicKey)
	return ok && got.Equal(&priv.PublicKey)
}

func applyBreakglassBinding(ctx context.Context, client ctrlclient.Client) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassBinding},
		Subjects: []rbacv1.Subject{{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     breakglassGroup,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     breakglassRoleKind,
			Name:     breakglassRoleName,
		},
	}
	err := client.Create(ctx, binding)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("applying %s ClusterRoleBinding: %w", breakglassBinding, err)
	}
	return nil
}

func (c *Controller) saveBreakglassClientKey(ctx context.Context, cluster db.Cluster, keyPEM []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AdminKubeconfigSecretName(cluster.Name),
			Namespace: c.secretsNamespace(),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{breakglassKeySecretKey: keyPEM},
	}
	capi.SetSecretOwnership(secret, cluster, constants.LabelValueKubeconfigModeAdmin, c.cfg.ManagedByLabelValue)
	if err := c.upsertSecret(ctx, cluster, secret); err != nil {
		return err
	}
	// upsertSecret skips the write when the cluster has started deleting, and
	// reports that as success. Do not submit a CSR unless this key is stored.
	stored, err := c.breakglassClientKey(ctx, cluster.Name)
	if err != nil {
		return fmt.Errorf("storing breakglass key for cluster %s: %w", cluster.Name, err)
	}
	if !bytes.Equal(stored, keyPEM) {
		return fmt.Errorf("storing breakglass key for cluster %s: secret was not updated", cluster.Name)
	}
	return nil
}

func (c *Controller) breakglassClientKey(ctx context.Context, clusterName string) ([]byte, error) {
	secret, err := c.adminKubeconfigSecret(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	keyPEM := secret.Data[breakglassKeySecretKey]
	if len(keyPEM) == 0 {
		return nil, fmt.Errorf("breakglass key for cluster %s is missing", clusterName)
	}
	return keyPEM, nil
}

func (c *Controller) clearBreakglassClientKey(ctx context.Context, clusterName string) error {
	secret, err := c.adminKubeconfigSecret(ctx, clusterName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(secret.Data[breakglassKeySecretKey]) == 0 {
		return nil
	}
	delete(secret.Data, breakglassKeySecretKey)
	if err := c.k8sClient.Update(ctx, secret); err != nil {
		return fmt.Errorf("clearing breakglass key for cluster %s: %w", clusterName, err)
	}
	return nil
}

func (c *Controller) adminKubeconfigSecret(ctx context.Context, clusterName string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName(clusterName),
		Namespace: c.secretsNamespace(),
	}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("getting admin kubeconfig secret for cluster %s: %w", clusterName, err)
	}
	return secret, nil
}

// reuseBreakglassKubeconfig returns the kubeconfig to keep. The boolean is false
// when admin.conf's CA differs, so the caller reissues the client cert.
func reuseBreakglassKubeconfig(existing, adminConf []byte) ([]byte, bool, error) {
	cfg, err := clientcmd.Load(existing)
	if err != nil {
		return nil, false, fmt.Errorf("parsing breakglass kubeconfig: %w", err)
	}
	admin, err := clientcmd.Load(adminConf)
	if err != nil {
		return nil, false, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	server, ca, err := getClusterAccess(cfg)
	adminServer, adminCA, adminErr := getClusterAccess(admin)
	if adminErr != nil {
		return nil, false, adminErr
	}
	if err != nil || !bytes.Equal(ca, adminCA) {
		return nil, false, nil
	}
	if server == adminServer {
		return existing, true, nil
	}
	cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster].Server = adminServer
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func issuedBreakglassCertChains(certPEM, caPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("issued client certificate does not chain to the current cluster CA")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	roots := x509.NewCertPool()
	if err == nil && !roots.AppendCertsFromPEM(caPEM) {
		err = fmt.Errorf("cluster CA is not PEM")
	}
	if err == nil {
		_, err = leaf.Verify(x509.VerifyOptions{
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
	}
	if err != nil {
		return fmt.Errorf("issued client certificate does not chain to the current cluster CA: %w", err)
	}
	return nil
}

func assembleBreakglassKubeconfig(adminKubeconfig, certPEM, keyPEM []byte) ([]byte, error) {
	cfg, err := clientcmd.Load(adminKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	server, ca, err := getClusterAccess(cfg)
	if err != nil {
		return nil, err
	}

	// The CSR may already hold a certificate signed before this admin.conf CA.
	// Writing that pair makes the CA bytes match, so later reconciles keep it.
	if err := issuedBreakglassCertChains(certPEM, ca); err != nil {
		return nil, err
	}

	out := clientcmdapi.NewConfig()
	out.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: ca,
	}
	out.AuthInfos[breakglassUser] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	out.Contexts[breakglassUser] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: breakglassUser}
	out.CurrentContext = breakglassUser
	return clientcmd.Write(*out)
}

func getClusterAccess(cfg *clientcmdapi.Config) (string, []byte, error) {
	if ctx := cfg.Contexts[cfg.CurrentContext]; ctx != nil {
		if cluster := cfg.Clusters[ctx.Cluster]; cluster != nil && cluster.Server != "" {
			return cluster.Server, cluster.CertificateAuthorityData, nil
		}
	}
	return "", nil, fmt.Errorf("admin kubeconfig has no cluster endpoint")
}
