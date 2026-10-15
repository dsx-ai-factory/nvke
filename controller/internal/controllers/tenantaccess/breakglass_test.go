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
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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
	"k8s.io/client-go/util/cert"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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
	caKey, caCert, caPEM := testBreakglassCA(t)
	signedCert := clientCertSignedBy(t, caKey, caCert)
	adminConf := adminConfWithCA(t, caPEM)
	client := breakglassTenantClient(t, &submitted, signedCert)
	cluster := testBreakglassCluster()
	c := breakglassController(client)
	c.logger = slog.New(slog.DiscardHandler)
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c.db = dbwrapper.New(mockDB, c.logger)
	c.scheme = testScheme(t)
	c.cfg.ManagedByLabelValue = "cloud-workflow-engine"
	c.k8sClient = fake.NewClientBuilder().WithScheme(c.scheme).WithObjects(capiClusterFixture(cluster)).Build()

	// Create and approve, then stop. The signer fills the certificate later.
	_, err := c.issueAdminBreakglass(ctx, adminConf, cluster, nil)
	require.ErrorContains(t, err, "not signed yet")

	inflight := &corev1.Secret{}
	require.NoError(t, c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, inflight))
	assert.NotEmpty(t, inflight.Data[breakglassKeySecretKey])
	assert.Empty(t, inflight.Data[KubeconfigSecretKey])
	assert.Equal(t, cluster.Name, inflight.Labels[clusterv1.ClusterNameLabel])
	assert.Equal(t, "cloud-workflow-engine", inflight.Labels[constants.LabelKeyNvkeManagedBy])
	assert.Equal(t, cluster.ID.String(), inflight.Labels[constants.LabelKeyClusterID])
	assertOwnedByCAPICluster(t, inflight, cluster)

	got, err := c.issueAdminBreakglass(ctx, adminConf, cluster, nil)
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
	caKey, caCert, caPEM := testBreakglassCA(t)
	certPEM := clientCertSignedBy(t, caKey, caCert)
	key, keyPEM, err := generatePrivateKey()
	require.NoError(t, err)
	csr := submittedBreakglassCSR(t, key, certificatesv1.CertificateSigningRequestStatus{
		Conditions: []certificatesv1.CertificateSigningRequestCondition{{
			Type:   certificatesv1.CertificateApproved,
			Status: corev1.ConditionTrue,
		}},
		Certificate: certPEM,
	})
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
		Data:       map[string][]byte{breakglassKeySecretKey: keyPEM},
	})

	got, err := c.issueAdminBreakglass(ctx, adminConfWithCA(t, caPEM), testBreakglassCluster(), nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(got)
	require.NoError(t, err)
	assert.Equal(t, certPEM, cfg.AuthInfos[breakglassUser].ClientCertificateData)
	assert.Equal(t, keyPEM, cfg.AuthInfos[breakglassUser].ClientKeyData)

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
	assertBreakglassKeyCleared(t, ctx, c, "tenant-a")
}

func TestIssueAdminBreakglassRejectsCertSignedByOldCA(t *testing.T) {
	ctx := context.Background()
	oldKey, oldCert, _ := testBreakglassCA(t)
	_, _, newCA := testBreakglassCA(t)
	key, keyPEM, err := generatePrivateKey()
	require.NoError(t, err)
	csr := submittedBreakglassCSR(t, key, certificatesv1.CertificateSigningRequestStatus{
		Conditions: []certificatesv1.CertificateSigningRequestCondition{{
			Type:   certificatesv1.CertificateApproved,
			Status: corev1.ConditionTrue,
		}},
		Certificate: clientCertSignedBy(t, oldKey, oldCert),
	})
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, breakglassKeySecret("tenant-a", keyPEM))

	_, err = c.issueAdminBreakglass(ctx, adminConfWithCA(t, newCA), testBreakglassCluster(), nil)
	require.ErrorContains(t, err, "does not chain to the current cluster CA")

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
	assertBreakglassKeyCleared(t, ctx, c, "tenant-a")
}

func TestIssueAdminBreakglassDropsSignedCSRWithoutKey(t *testing.T) {
	ctx := context.Background()
	key, _, err := generatePrivateKey()
	require.NoError(t, err)
	csr := submittedBreakglassCSR(t, key, certificatesv1.CertificateSigningRequestStatus{
		Conditions: []certificatesv1.CertificateSigningRequestCondition{{
			Type:   certificatesv1.CertificateApproved,
			Status: corev1.ConditionTrue,
		}},
		Certificate: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
	})
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
	})

	_, err = c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
	require.ErrorContains(t, err, "breakglass key for cluster tenant-a is missing")

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassKeepsAnUnsignedCSR(t *testing.T) {
	ctx := context.Background()
	key, keyPEM, err := generatePrivateKey()
	require.NoError(t, err)
	csr := submittedBreakglassCSR(t, key, certificatesv1.CertificateSigningRequestStatus{
		Conditions: []certificatesv1.CertificateSigningRequestCondition{{
			Type:   certificatesv1.CertificateApproved,
			Status: corev1.ConditionTrue,
		}},
	})
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
		Data:       map[string][]byte{breakglassKeySecretKey: keyPEM},
	})

	_, err = c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
	require.ErrorContains(t, err, "not signed yet")

	got := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, got))
	assert.Nil(t, csrCondition(got, certificatesv1.CertificateDenied))
	secret := &corev1.Secret{}
	require.NoError(t, c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName("tenant-a"),
		Namespace: SecretsNamespace,
	}, secret))
	assert.Equal(t, keyPEM, secret.Data[breakglassKeySecretKey])
	assert.Empty(t, secret.Data["value"])
}

func TestIssueAdminBreakglassDeletesADeniedCSR(t *testing.T) {
	ctx := context.Background()
	const denied = "This CSR was denied by kubectl certificate deny."
	key, keyPEM, err := generatePrivateKey()
	require.NoError(t, err)
	csr := submittedBreakglassCSR(t, key, certificatesv1.CertificateSigningRequestStatus{
		Conditions: []certificatesv1.CertificateSigningRequestCondition{{
			Type:    certificatesv1.CertificateDenied,
			Status:  corev1.ConditionTrue,
			Message: denied,
		}},
	})
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
	c := breakglassController(client)
	c.k8sClient = breakglassManagementClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName("tenant-a"), Namespace: SecretsNamespace},
		Data:       map[string][]byte{breakglassKeySecretKey: keyPEM},
	})

	_, err = c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
	require.ErrorContains(t, err, "client CSR "+breakglassCSRName+" failed: "+denied)

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassDeletesForeignCSR(t *testing.T) {
	ctx := context.Background()
	_, storedPEM, err := generatePrivateKey()
	require.NoError(t, err)
	foreign, _, err := generatePrivateKey()
	require.NoError(t, err)

	t.Run("no stored key", func(t *testing.T) {
		csr := submittedBreakglassCSR(t, foreign, certificatesv1.CertificateSigningRequestStatus{})
		approved := false
		client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, ctrlclient.Client, string, ctrlclient.Object, ...ctrlclient.SubResourceUpdateOption) error {
				approved = true
				return nil
			},
		}).Build()
		c := breakglassController(client)
		c.k8sClient = breakglassManagementClient(t)

		_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
		require.ErrorContains(t, err, "tenant-a-admin-kubeconfig\" not found")
		assert.False(t, approved)

		err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
		assert.True(t, apierrors.IsNotFound(err), err)
	})

	t.Run("unapproved", func(t *testing.T) {
		csr := submittedBreakglassCSR(t, foreign, certificatesv1.CertificateSigningRequestStatus{})
		approved := false
		client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, ctrlclient.Client, string, ctrlclient.Object, ...ctrlclient.SubResourceUpdateOption) error {
				approved = true
				return nil
			},
		}).Build()
		c := breakglassController(client)
		c.k8sClient = breakglassManagementClient(t, breakglassKeySecret("tenant-a", storedPEM))

		_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
		require.ErrorContains(t, err, "public key does not match the stored breakglass key")
		assert.False(t, approved)

		err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
		assert.True(t, apierrors.IsNotFound(err), err)
		secret := &corev1.Secret{}
		require.NoError(t, c.k8sClient.Get(ctx, types.NamespacedName{
			Name:      AdminKubeconfigSecretName("tenant-a"),
			Namespace: SecretsNamespace,
		}, secret))
		assert.Equal(t, storedPEM, secret.Data[breakglassKeySecretKey])
		assert.Empty(t, secret.Data["value"])
	})

	t.Run("already signed", func(t *testing.T) {
		csr := submittedBreakglassCSR(t, foreign, certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
			Certificate: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
		})
		client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).WithObjects(csr).Build()
		c := breakglassController(client)
		c.k8sClient = breakglassManagementClient(t, breakglassKeySecret("tenant-a", storedPEM))

		_, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), nil)
		require.ErrorContains(t, err, "public key does not match the stored breakglass key")

		err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
		assert.True(t, apierrors.IsNotFound(err), err)
	})
}

func TestIssueAdminBreakglassReusesValidCert(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).Build()
	c := &Controller{
		cfg: config.Config{CertRenewalThresholdDays: 30},
		tenantClientFor: func([]byte) (ctrlclient.Client, error) {
			return client, nil
		},
	}
	existing := kubeconfigWithClientCert(t, breakglassGroup, time.Now().Add(200*24*time.Hour))

	got, err := c.issueAdminBreakglass(ctx, []byte(testAdminConf), testBreakglassCluster(), existing)
	require.NoError(t, err)
	assert.Equal(t, existing, got)

	binding := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: breakglassBinding}, binding))
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, breakglassGroup, binding.Subjects[0].Name)
	assert.Equal(t, breakglassRoleName, binding.RoleRef.Name)
}

func TestIssueAdminBreakglassRebuildsChangedClusterAccess(t *testing.T) {
	ctx := context.Background()
	existing := kubeconfigWithClientCert(t, breakglassGroup, time.Now().Add(200*24*time.Hour))
	existingCfg, err := clientcmd.Load(existing)
	require.NoError(t, err)
	certPEM := existingCfg.AuthInfos[breakglassUser].ClientCertificateData
	keyPEM := existingCfg.AuthInfos[breakglassUser].ClientKeyData

	client := fake.NewClientBuilder().WithScheme(breakglassTenantScheme(t)).Build()
	c := &Controller{
		cfg: config.Config{CertRenewalThresholdDays: 30},
		tenantClientFor: func([]byte) (ctrlclient.Client, error) {
			return client, nil
		},
	}
	adminCfg, err := clientcmd.Load([]byte(testAdminConf))
	require.NoError(t, err)
	adminCfg.Clusters[adminCfg.Contexts[adminCfg.CurrentContext].Cluster].Server = "https://192.0.2.20:6443"
	adminConf, err := clientcmd.Write(*adminCfg)
	require.NoError(t, err)

	got, err := c.issueAdminBreakglass(ctx, adminConf, testBreakglassCluster(), existing)
	require.NoError(t, err)
	gotCfg, err := clientcmd.Load(got)
	require.NoError(t, err)
	assert.Equal(t, "https://192.0.2.20:6443", gotCfg.Clusters["cluster"].Server)
	assert.Equal(t, certPEM, gotCfg.AuthInfos[breakglassUser].ClientCertificateData)
	assert.Equal(t, keyPEM, gotCfg.AuthInfos[breakglassUser].ClientKeyData)

	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	assert.True(t, apierrors.IsNotFound(err), err)
}

func TestIssueAdminBreakglassReissuesWhenCAChanges(t *testing.T) {
	ctx := context.Background()
	existing := kubeconfigWithClientCert(t, breakglassGroup, time.Now().Add(200*24*time.Hour))
	adminCfg, err := clientcmd.Load([]byte(testAdminConf))
	require.NoError(t, err)
	adminCfg.Clusters[adminCfg.Contexts[adminCfg.CurrentContext].Cluster].CertificateAuthorityData = []byte("updated-ca")
	adminConf, err := clientcmd.Write(*adminCfg)
	require.NoError(t, err)

	var submitted *certificatesv1.CertificateSigningRequest
	client := breakglassTenantClient(t, &submitted, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	cluster := testBreakglassCluster()
	c := breakglassController(client)
	c.logger = slog.New(slog.DiscardHandler)
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c.db = dbwrapper.New(mockDB, c.logger)
	c.scheme = testScheme(t)
	c.cfg.ManagedByLabelValue = "cloud-workflow-engine"
	c.k8sClient = fake.NewClientBuilder().WithScheme(c.scheme).WithObjects(capiClusterFixture(cluster)).Build()

	_, err = c.issueAdminBreakglass(ctx, adminConf, cluster, existing)
	require.ErrorContains(t, err, "not signed yet")
	require.NotNil(t, submitted)
	err = client.Get(ctx, types.NamespacedName{Name: breakglassCSRName}, &certificatesv1.CertificateSigningRequest{})
	require.NoError(t, err)
}

func testBreakglassCluster() db.Cluster {
	return db.Cluster{ID: uuid.MustParse("8c1c6b2e-3f4a-4d5b-9e6f-7a8b9c0d1e2f"), Name: "tenant-a"}
}

func breakglassTenantScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	return scheme
}

func submittedBreakglassCSR(t *testing.T, key *ecdsa.PrivateKey, status certificatesv1.CertificateSigningRequestStatus) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   breakglassGroup + ":tenant-a",
			Organization: []string{breakglassGroup},
		},
	}, key)
	require.NoError(t, err)
	lifetime := certLifetime
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: breakglassCSRName},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           pem.EncodeToMemory(&pem.Block{Type: cert.CertificateRequestBlockType, Bytes: der}),
			SignerName:        certificatesv1.KubeAPIServerClientSignerName,
			Usages:            []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
			ExpirationSeconds: &lifetime,
		},
		Status: status,
	}
}

func breakglassKeySecret(clusterName string, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminKubeconfigSecretName(clusterName), Namespace: SecretsNamespace},
		Data:       map[string][]byte{breakglassKeySecretKey: keyPEM},
	}
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
// kube-controller-manager does after approval.
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

func testBreakglassCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return key, parsed, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func clientCertSignedBy(t *testing.T, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: breakglassGroup + ":c", Organization: []string{breakglassGroup}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(200 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func adminConfWithCA(t *testing.T, caPEM []byte) []byte {
	t.Helper()
	cfg, err := clientcmd.Load([]byte(testAdminConf))
	require.NoError(t, err)
	cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster].CertificateAuthorityData = caPEM
	raw, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	return raw
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
