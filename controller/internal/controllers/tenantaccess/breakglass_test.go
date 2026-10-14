// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

func TestIsValidBreakglassKubeconfig(t *testing.T) {
	now := time.Now()
	renew := 30 * 24 * time.Hour
	valid := kubeconfigWithClientCert(t, breakglassGroup, now.Add(200*24*time.Hour))
	assert.True(t, isValidBreakglassKubeconfig(valid, now, renew))
	assert.False(t, isValidBreakglassKubeconfig(nil, now, renew))

	near := kubeconfigWithClientCert(t, breakglassGroup, now.Add(10*24*time.Hour))
	assert.False(t, isValidBreakglassKubeconfig(near, now, renew))

	otherGroup := kubeconfigWithClientCert(t, "system:masters", now.Add(200*24*time.Hour))
	assert.False(t, isValidBreakglassKubeconfig(otherGroup, now, renew))

	cfg, err := clientcmd.Load(valid)
	require.NoError(t, err)
	cfg.AuthInfos[breakglassUser].ClientKeyData = nil
	missingKey, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	assert.False(t, isValidBreakglassKubeconfig(missingKey, now, renew))

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	otherDER, err := x509.MarshalPKCS8PrivateKey(other)
	require.NoError(t, err)
	cfg.AuthInfos[breakglassUser].ClientKeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER})
	mismatched, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	assert.False(t, isValidBreakglassKubeconfig(mismatched, now, renew))

	// The Secret still holds the node admin.conf copied by the previous loop.
	assert.False(t, isValidBreakglassKubeconfig([]byte(testAdminConf), now, renew))
}

func TestIssueAdminBreakglassMints(t *testing.T) {
	ctx := context.Background()
	var submitted *certificatesv1.CertificateSigningRequest
	signedCert := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	client := breakglassTenantClient(t, &submitted, signedCert)
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t)

	// Create and approve, then stop. The signer fills the certificate later.
	_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.ErrorContains(t, err, "not signed yet")

	got, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.NoError(t, err)

	cfg, err := clientcmd.Load(got)
	require.NoError(t, err)
	require.NotNil(t, cfg.AuthInfos[breakglassUser])
	assert.Equal(t, signedCert, cfg.AuthInfos[breakglassUser].ClientCertificateData)
	assert.NotEmpty(t, cfg.AuthInfos[breakglassUser].ClientKeyData)
	assert.Equal(t, testServer, cfg.Clusters["cluster"].Server)
	assert.Equal(t, breakglassUser, cfg.CurrentContext)
	require.NotNil(t, submitted)
	assert.Equal(t, certificatesv1.KubeAPIServerClientSignerName, submitted.Spec.SignerName)
	assert.Equal(t, []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth}, submitted.Spec.Usages)
	require.NotNil(t, submitted.Spec.ExpirationSeconds)
	assert.Equal(t, certLifetime, *submitted.Spec.ExpirationSeconds)
	block, _ := pem.Decode(submitted.Spec.Request)
	require.NotNil(t, block)
	req, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, breakglassGroup+":tenant-a", req.Subject.CommonName)
	assert.Equal(t, []string{breakglassGroup}, req.Subject.Organization)

	binding := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: breakglassBinding}, binding))
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, rbacv1.GroupKind, binding.Subjects[0].Kind)
	assert.Equal(t, breakglassGroup, binding.Subjects[0].Name)
	assert.Equal(t, breakglassRoleKind, binding.RoleRef.Kind)
	assert.Equal(t, breakglassRoleName, binding.RoleRef.Name)

	err = client.Get(ctx, types.NamespacedName{Name: submitted.Name}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
	assertBreakglassKeyCleared(t, ctx, c, "tenant-a")
}

func TestIssueAdminBreakglassContinuesAnApprovedCSR(t *testing.T) {
	ctx := context.Background()
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	keyPEM := []byte("-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n")
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
			Certificate: certPEM,
		},
	}
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
		Data:       map[string][]byte{breakglassKeySecretKey: keyPEM},
	})

	got, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(got)
	require.NoError(t, err)
	assert.Equal(t, certPEM, cfg.AuthInfos[breakglassUser].ClientCertificateData)
	assert.Equal(t, keyPEM, cfg.AuthInfos[breakglassUser].ClientKeyData)

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
	assertBreakglassKeyCleared(t, ctx, c, "tenant-a")
}

func TestIssueAdminBreakglassDropsSignedCSRWithoutKey(t *testing.T) {
	ctx := context.Background()
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
			Certificate: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
		},
	}
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
	})

	_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.ErrorContains(t, err, "breakglass key for cluster tenant-a is missing")

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassKeepsAnUnsignedCSR(t *testing.T) {
	ctx := context.Background()
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t)

	_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.ErrorContains(t, err, "not signed yet")

	got := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, got))
	secret := &corev1.Secret{}
	err = c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName("tenant-a"),
		Namespace: SecretsNamespace,
	}, secret)
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassDeletesADeniedCSR(t *testing.T) {
	ctx := context.Background()
	const denied = "This CSR was denied by kubectl certificate deny."
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Message: denied,
			}},
		},
	}
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t)

	_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", nil)
	require.ErrorContains(t, err, "client CSR "+breakglassCSRName+" failed: "+denied)

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassReusesValidCert(t *testing.T) {
	ctx := context.Background()
	c := &Controller{
		cfg: config.Config{CertRenewalThresholdDays: 30},
		tenantClientFor: func([]byte) (ctrlclient.Client, error) {
			return nil, errors.New("reused cert must not build a tenant client")
		},
	}
	existing := kubeconfigWithClientCert(t, breakglassGroup, time.Now().Add(200*24*time.Hour))

	got, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), "tenant-a", existing)
	require.NoError(t, err)
	assert.Equal(t, existing, got)
}

func breakglassTenantScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	return scheme
}

func breakglassManagementClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func breakglassController(client ctrlclient.Client) *Controller {
	return &Controller{
		cfg: config.Config{CertRenewalThresholdDays: 30},
		tenantClientFor: func([]byte) (ctrlclient.Client, error) {
			return client, nil
		},
	}
}

func assertBreakglassKeyCleared(t *testing.T, ctx context.Context, c *Controller, clusterName string) {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName(clusterName),
		Namespace: SecretsNamespace,
	}, secret))
	assert.Empty(t, secret.Data[breakglassKeySecretKey])
}

// breakglassTenantClient is the same controller-runtime client the OIDC path uses.
// A signed certificate is filled in once the CSR is approved, which is what
// kube-apiserver does after approval.
func breakglassTenantClient(t *testing.T, submitted **certificatesv1.CertificateSigningRequest, signedCert []byte) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if csr, ok := obj.(*certificatesv1.CertificateSigningRequest); ok {
				*submitted = csr.DeepCopy()
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
			if !ok {
				return nil
			}
			for _, cond := range csr.Status.Conditions {
				if cond.Type == certificatesv1.CertificateApproved && cond.Status == corev1.ConditionTrue {
					csr.Status.Certificate = signedCert
				}
			}
			return nil
		},
	}).Build()
}

func kubeconfigWithClientCert(t *testing.T, org string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: breakglassGroup + ":c", Organization: []string{org}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	cfg, err := clientcmd.Load([]byte(testAdminConf))
	require.NoError(t, err)
	cfg.AuthInfos[breakglassUser] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	raw, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	return raw
}
