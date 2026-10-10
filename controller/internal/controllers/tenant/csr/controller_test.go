// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package csr

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

type staticDatabase struct {
	cluster    db.Cluster
	clusterErr error
	nodes      []db.Node
	nodesErr   error
}

func TestTenantCSRControllerRegistrationUsesClusterScopedNamesAndRestartOptions(t *testing.T) {
	clusterA := uuid.New()
	clusterB := uuid.New()
	nameA, optionsA := tenantCSRControllerRegistration(clusterA)
	nameB, optionsB := tenantCSRControllerRegistration(clusterB)

	require.Equal(t, "tenant-csr-"+clusterA.String(), nameA)
	require.Equal(t, "tenant-csr-"+clusterB.String(), nameB)
	require.NotEqual(t, nameA, nameB)
	require.NotNil(t, optionsA.SkipNameValidation)
	require.True(t, *optionsA.SkipNameValidation)
	require.NotNil(t, optionsB.SkipNameValidation)
	require.True(t, *optionsB.SkipNameValidation)
}

func TestNewRegistrarSupportsMultipleTenantsAndRestart(t *testing.T) {
	clusterA := uuid.New()
	clusterB := uuid.New()

	// Register through NewRegistrar, not the helper. Asserting on the helper
	// alone stays green when the call site drops the name or the options.
	register := newRegistrarForTest(t)

	require.NoError(t, register(clusterA), "first tenant should register")
	require.NoError(t, register(clusterB), "second tenant should register")
	require.NoError(t, register(clusterA), "restarted tenant manager should register")
}

// newRegistrarForTest runs the registrar against a fresh manager per call, as
// ManagerSet does per generation. Nothing dials: the RESTMapper is lazy and the
// manager is never started.
func newRegistrarForTest(t *testing.T) func(uuid.UUID) error {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))

	registrar := NewRegistrar(staticDatabase{}, nil)

	return func(clusterID uuid.UUID) error {
		t.Helper()
		mgr, err := manager.New(&rest.Config{Host: "https://127.0.0.1:1"}, manager.Options{
			Scheme:  scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		require.NoError(t, err)
		return registrar(clusterID, mgr)
	}
}

func (d staticDatabase) GetCluster(context.Context, uuid.UUID) (db.Cluster, error) {
	return d.cluster, d.clusterErr
}

func (d staticDatabase) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return d.nodes, d.nodesErr
}

func TestValidateServingCSR(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate     func(*certificatesv1.CertificateSigningRequest)
		commonName string
		org        []string
		dnsNames   []string
		ips        []net.IP
		uris       []*url.URL
		wantReason string
	}{
		"valid DNS and IP": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			ips:        []net.IP{net.ParseIP("192.0.2.10")},
		},
		"valid DNS only": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
		},
		"missing group": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			mutate: func(csr *certificatesv1.CertificateSigningRequest) {
				csr.Spec.Groups = nil
			},
			wantReason: "spec.groups does not include system:nodes",
		},
		"missing server auth": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			mutate: func(csr *certificatesv1.CertificateSigningRequest) {
				csr.Spec.Usages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature}
			},
			wantReason: "spec.usages does not include server auth",
		},
		"disallowed usage": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			mutate: func(csr *certificatesv1.CertificateSigningRequest) {
				csr.Spec.Usages = append(csr.Spec.Usages, certificatesv1.UsageClientAuth)
			},
			wantReason: `spec.usages contains disallowed usage "client auth"`,
		},
		"malformed request": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			mutate: func(csr *certificatesv1.CertificateSigningRequest) {
				csr.Spec.Request = []byte("not PEM")
			},
			wantReason: "spec.request is not a single PEM-encoded certificate request",
		},
		"wrong organization": {
			commonName: "system:node:worker-1",
			org:        []string{"unexpected"},
			dnsNames:   []string{"worker-1"},
			wantReason: "subject organization is not system:nodes",
		},
		"wrong common name": {
			commonName: "system:node:other",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"worker-1"},
			wantReason: "subject common name is not system:node:worker-1",
		},
		"missing SAN": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			wantReason: "certificate request has no subject alternative names",
		},
		"wrong DNS SAN": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			dnsNames:   []string{"other"},
			wantReason: `DNS SAN "other" does not match controller node name`,
		},
		"wrong IP SAN": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			ips:        []net.IP{net.ParseIP("192.0.2.11")},
			wantReason: `IP SAN "192.0.2.11" does not match controller node IP`,
		},
		"URI SAN": {
			commonName: "system:node:worker-1",
			org:        []string{"system:nodes"},
			uris:       []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/node/worker-1"}},
			wantReason: "subject alternative names contain an unsupported identity",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			csr := newServingCSR(t, "csr", time.Now(), test.commonName, test.org, test.dnsNames, test.ips, test.uris)
			if test.mutate != nil {
				test.mutate(csr)
			}
			require.Equal(t, test.wantReason, validateServingCSR(csr, "worker-1", "192.0.2.10"))
		})
	}
}

func TestPlanDecision(t *testing.T) {
	t.Parallel()

	nodes := []db.Node{{Name: "worker-1", IP: "192.0.2.10"}}
	valid := newServingCSR(t, "valid", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)

	t.Run("valid CSR is approved", func(t *testing.T) {
		t.Parallel()
		decision := planDecision(valid, nodes)
		require.Equal(t, decisionApprove, decision.Action)
		require.Equal(t, valid.Name, decision.CSR.Name)
	})

	t.Run("invalid CSR is skipped", func(t *testing.T) {
		t.Parallel()
		invalid := newServingCSR(t, "invalid", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"other"}, nil, nil)
		decision := planDecision(invalid, nodes)
		require.Equal(t, decisionSkip, decision.Action)
		require.Equal(t, `DNS SAN "other" does not match controller node name`, decision.Reason)
	})

	t.Run("unknown node is skipped", func(t *testing.T) {
		t.Parallel()
		decision := planDecision(valid, nil)
		require.Equal(t, decisionSkip, decision.Action)
		require.Equal(t, "node is not present in the controller's desired state", decision.Reason)
	})
}

func TestLogDecisionUsesDecisionLevel(t *testing.T) {
	tests := map[string]struct {
		action    string
		wantLevel string
	}{
		"approval is informational": {
			action:    decisionApprove,
			wantLevel: `"level":"INFO"`,
		},
		"skip is diagnostic": {
			action:    decisionSkip,
			wantLevel: `"level":"DEBUG"`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			reconciler := &Reconciler{
				clusterID: uuid.New(),
				logger: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				})),
			}
			reconciler.logDecision(csrDecision{
				CSR:    &certificatesv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Name: "test-csr"}},
				Action: test.action,
			})

			require.Contains(t, output.String(), test.wantLevel)
		})
	}
}

func TestReconcilerApprovesValidatedCSR(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	csr := newServingCSR(t, "worker-1-serving", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(csr).Build()

	reconciler := &Reconciler{
		clusterID: clusterID,
		database:  staticDatabase{nodes: []db.Node{{Name: "worker-1", IP: "192.0.2.10"}}},
		client:    tenantClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.NoError(t, err)

	approved := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: csr.Name}, approved))
	require.Len(t, approved.Status.Conditions, 1)
	require.Equal(t, certificatesv1.CertificateApproved, approved.Status.Conditions[0].Type)
	require.Equal(t, corev1.ConditionTrue, approved.Status.Conditions[0].Status)
	require.Equal(t, approvalReason, approved.Status.Conditions[0].Reason)
}

func TestReconcilerOnlyApprovesRequestedCSR(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	now := time.Now()
	newest := newServingCSR(t, "newest", now, "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	older := newServingCSR(t, "older", now.Add(-time.Minute), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newest, older).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		database:  staticDatabase{nodes: []db.Node{{Name: "worker-1", IP: "192.0.2.10"}}},
		client:    tenantClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: older.Name}})
	require.NoError(t, err)

	gotOlder := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: older.Name}, gotOlder))
	require.Equal(t, certificatesv1.CertificateApproved, gotOlder.Status.Conditions[0].Type)
	gotNewest := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: newest.Name}, gotNewest))
	require.Empty(t, gotNewest.Status.Conditions)
}

func TestReconcilerRetriesApprovalFailure(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	pending := newServingCSR(t, "pending", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pending).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context,
				client.Client,
				string,
				client.Object,
				...client.SubResourceUpdateOption,
			) error {
				return errors.New("approval write failed")
			},
		}).
		Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		database:  staticDatabase{nodes: []db.Node{{Name: "worker-1", IP: "192.0.2.10"}}},
		client:    tenantClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pending.Name}})
	require.ErrorContains(t, err, "approval write failed")
	unchanged := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: pending.Name}, unchanged))
	require.Empty(t, unchanged.Status.Conditions)
}

func TestReconcilerIgnoresResolvedCSR(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	approved := newServingCSR(t, "approved", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	approved.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type:   certificatesv1.CertificateApproved,
		Status: corev1.ConditionTrue,
	}}
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(approved).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		database:  staticDatabase{nodesErr: errors.New("resolved CSR must not query nodes")},
		client:    tenantClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: approved.Name}})
	require.NoError(t, err)
}

func TestReconcilerRetriesNodeLookupFailure(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	csr := newServingCSR(t, "worker-1-serving", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(csr).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		database:  staticDatabase{nodesErr: errors.New("database unavailable")},
		client:    tenantClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.ErrorContains(t, err, "listing controller nodes")
}

func TestReconcilerDoesNotApproveForDeletingCluster(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	pending := newServingCSR(t, "worker-1-serving", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pending).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		database: staticDatabase{cluster: db.Cluster{Conditions: db.ClusterConditions{
			Deleting: db.Condition{Status: db.ConditionStatusTrue},
		}}},
		client: tenantClient,
		logger: slog.New(slog.DiscardHandler),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pending.Name}})
	require.NoError(t, err)
	unchanged := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: pending.Name}, unchanged))
	require.Empty(t, unchanged.Status.Conditions)
}

func newServingCSR(
	t *testing.T,
	name string,
	createdAt time.Time,
	commonName string,
	organizations []string,
	dnsNames []string,
	ips []net.IP,
	uris []*url.URL,
) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(createdAt)},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    createCertificateRequest(t, commonName, organizations, dnsNames, ips, uris),
			SignerName: certificatesv1.KubeletServingSignerName,
			Usages: []certificatesv1.KeyUsage{
				certificatesv1.UsageDigitalSignature,
				certificatesv1.UsageKeyEncipherment,
				certificatesv1.UsageServerAuth,
			},
			Username: nodeUsernamePrefix + "worker-1",
			Groups:   []string{nodeGroup},
		},
	}
}

func createCertificateRequest(
	t *testing.T,
	commonName string,
	organizations []string,
	dnsNames []string,
	ips []net.IP,
	uris []*url.URL,
) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: commonName, Organization: organizations},
		DNSNames:    dnsNames,
		IPAddresses: ips,
		URIs:        uris,
	}, privateKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request})
}
