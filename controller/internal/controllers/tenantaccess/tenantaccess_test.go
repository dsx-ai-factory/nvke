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
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

const (
	testKubeconfig = "kubeconfig-data"

	// testAdminConf is a minimal but parseable admin.conf: the OIDC path loads it
	// to extract the CA (base64 "test-ca") and server URL. testServer is that URL.
	testServer    = "https://198.51.100.10:6443"
	testAdminConf = `apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: https://198.51.100.10:6443
    certificate-authority-data: dGVzdC1jYQ==
contexts:
- name: ctx
  context:
    cluster: cluster
    user: admin
current-context: ctx
users:
- name: admin
  user: {}
`

	// testRBACManifest is the tenant RBAC the OIDC path applies via server-side apply.
	testRBACManifest = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nke-role
rules: []
`
)

// newPublishTestController builds a Controller for the publish-path tests. No agent
// client: these drive c.publish directly with an admin.conf, so nothing fetches
// one from a node.
func newPublishTestController(database db.DB, k8sClient ctrlclient.Client) *Controller {
	return newTestController(
		config.Config{SpiffeEndpointSocket: "unix:///run/spire/agent.sock"},
		database,
		k8sClient,
		nil,
	)
}

func nkeRBACConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-rbac", Namespace: SecretsNamespace},
		Data:       map[string]string{"manifest.yaml": testRBACManifest},
	}
}

// Kubeconfig fixtures mirroring the site-workflow-engine cluster controller's
// helpers_internal_test.go, so the ported extractors are exercised over the same
// valid / no-CA / no-server matrix.
const (
	testCAPEM     = "-----BEGIN CERTIFICATE-----\nMIIBcDCCARWgAwIBAgIQ\n-----END CERTIFICATE-----\n"
	encodedTestCA = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJjRENDQVJXZ0F3SUJBZ0lRCi0tLS0tRU5EIENFUlRJRklDQVRFLS0tLS0K"

	validKubeconfig    = "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://1.1.1.1:6443\n    certificate-authority-data: " + encodedTestCA + "\ncontexts:\n- name: ctx\n  context: {cluster: c, user: u}\ncurrent-context: ctx\nusers:\n- name: u\n  user: {}\n"
	kubeconfigNoCA     = "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://1.1.1.1:6443\ncontexts:\n- name: ctx\n  context: {cluster: c, user: u}\ncurrent-context: ctx\nusers:\n- name: u\n  user: {}\n"
	kubeconfigNoServer = "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    certificate-authority-data: " + encodedTestCA + "\ncontexts:\n- name: ctx\n  context: {cluster: c, user: u}\ncurrent-context: ctx\nusers:\n- name: u\n  user: {}\n"
)

func TestKubeconfigSecretName(t *testing.T) {
	assert.Equal(t, "prod-a-kubeconfig", KubeconfigSecretName("prod-a"))
	assert.Equal(t, "prod-a-admin-kubeconfig", AdminKubeconfigSecretName("prod-a"))
	assert.True(t, IsCWEKubeconfigSecretName("prod-a-kubeconfig"))
	assert.False(t, IsCWEKubeconfigSecretName("prod-a-admin-kubeconfig"))
}

func TestExtractCAFromKubeconfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantErr  string
		expectCA []byte
	}{
		{name: "valid kubeconfig returns decoded CA", input: validKubeconfig, expectCA: []byte(testCAPEM)},
		{name: "missing certificate-authority-data returns error", input: kubeconfigNoCA, wantErr: "kubeconfig contains no certificate-authority-data"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := clientcmd.Load([]byte(tc.input))
			require.NoError(t, err)

			got, err := extractCAFromKubeconfig(cfg)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expectCA, got)
		})
	}
}

func TestExtractServerFromKubeconfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		wantErr      string
		expectServer string
	}{
		{name: "valid kubeconfig returns server URL", input: validKubeconfig, expectServer: "https://1.1.1.1:6443"},
		{name: "missing server returns error", input: kubeconfigNoServer, wantErr: "kubeconfig contains no server URL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := clientcmd.Load([]byte(tc.input))
			require.NoError(t, err)

			got, err := extractServerFromKubeconfig(cfg)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expectServer, got)
		})
	}
}

func TestExtractServerAndCAFromKubeconfig_Missing(t *testing.T) {
	cfg, err := clientcmd.Load([]byte("apiVersion: v1\nkind: Config\n"))
	require.NoError(t, err)

	_, err = extractServerFromKubeconfig(cfg)
	require.Error(t, err)

	_, err = extractCAFromKubeconfig(cfg)
	require.Error(t, err)
}

func TestController_Reconcile(t *testing.T) {
	ctx := context.Background()
	activeCluster := db.Cluster{ID: uuid.New(), Name: "active"}
	deletingCluster := db.Cluster{ID: uuid.New(), Name: "deleting"}
	deletingCluster.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	notReadyCluster := db.Cluster{ID: uuid.New(), Name: "not-ready"}
	activeNodeID := uuid.New()

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(activeCluster.Name)).Return([]db.Cluster{activeCluster}, nil).Once()
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(deletingCluster.Name)).Return([]db.Cluster{deletingCluster}, nil).Once()
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(notReadyCluster.Name)).Return([]db.Cluster{notReadyCluster}, nil).Once()
	// Active is read by the mode refresh, admin Secret upsert, and CWE kubeconfig
	// Secret upsert.
	mockDB.On("GetCluster", mock.Anything, activeCluster.ID).Return(activeCluster, nil).Times(3)
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == activeCluster.ID
	})).Return([]db.Node{joinedCPNode(activeNodeID, activeCluster.ID)}, nil)
	// not-ready cluster has no joined control-plane node.
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == notReadyCluster.ID
	})).Return([]db.Node{}, nil)

	nodeAgentClient := &agenttest.FakeClient{
		Kubeconfigs: map[uuid.UUID]string{
			activeNodeID: testKubeconfig,
		},
	}

	kubeconfigSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: KubeconfigSecretName(deletingCluster.Name), Namespace: SecretsNamespace,
	}, Data: map[string][]byte{KubeconfigSecretKey: []byte("existing-kubeconfig")}}
	trustBundleSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: TrustBundleSecretName(deletingCluster.Name), Namespace: SecretsNamespace,
	}, Data: map[string][]byte{TrustBundleSecretKey: []byte("existing-trust-bundle")}}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		capiClusterFixture(activeCluster),
		capiClusterFixture(deletingCluster),
		capiClusterFixture(notReadyCluster),
		kubeconfigSecret,
		trustBundleSecret,
	).Build()

	cfg := config.Config{TenantAccessEnabled: true}
	tenantManagers := &recordingTenantManagerSet{}
	c := NewController(cfg, slog.New(slog.DiscardHandler), dbwrapper.New(mockDB, slog.New(slog.DiscardHandler)), k8sClient, nodeAgentClient, tenantManagers)
	c.scheme = testScheme(t)

	_, err := c.Reconcile(ctx, reconcileRequest(activeCluster.Name))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, reconcileRequest(deletingCluster.Name))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, reconcileRequest(notReadyCluster.Name))
	require.NoError(t, err)

	// The non-OIDC cluster publishes the admin kubeconfig verbatim.
	assertSecret(t, ctx, k8sClient, activeCluster, true)
	// Deleting clusters preserve existing access material for Kubernetes GC to
	// remove once the CAPI Cluster finishes deleting, while not-ready clusters
	// publish nothing.
	preservedKubeconfig := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: KubeconfigSecretName(deletingCluster.Name), Namespace: SecretsNamespace,
	}, preservedKubeconfig))
	assert.Equal(t, []byte("existing-kubeconfig"), preservedKubeconfig.Data[KubeconfigSecretKey])
	preservedTrustBundle := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: TrustBundleSecretName(deletingCluster.Name), Namespace: SecretsNamespace,
	}, preservedTrustBundle))
	assert.Equal(t, []byte("existing-trust-bundle"), preservedTrustBundle.Data[TrustBundleSecretKey])
	assertSecret(t, ctx, k8sClient, notReadyCluster, false)
	assert.Equal(t, []uuid.UUID{deletingCluster.ID}, tenantManagers.removed)

	mockDB.AssertExpectations(t)
	// Deleting cluster is skipped before any node lookup.
	mockDB.AssertNotCalled(t, "ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == deletingCluster.ID
	}))
}

// TestController_ReconcileNoCAPICluster covers the publish gate: a missing
// CAPI Cluster must never cause an unowned Secret to be created, and Kubernetes
// GC (not this controller) is what removes any Secrets left over from before
// the Cluster was deleted.
func TestController_ReconcileNoCAPICluster(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "gone"}

	mockDB := &testutil.MockDB{}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, k8sClient, &agenttest.FakeClient{})

	_, err := c.Reconcile(ctx, reconcileRequest(cluster.Name))
	require.NoError(t, err)

	assertSecret(t, ctx, k8sClient, cluster, false)
	mockDB.AssertNotCalled(t, "ListClusters", mock.Anything, mock.Anything)
}

// fetchAdminKubeconfig is the one piece of real new logic relative to SWE, which
// probes only GetFirstControlPlaneNode. These cover the candidate iteration and
// the readiness gate it implements.
func TestFetchAdminKubeconfig(t *testing.T) {
	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "c"}

	// Candidates are probed in node-ID order, so the fall-through cases pin IDs
	// that sort bad-before-good rather than relying on slice order.
	badID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	goodID := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	bad := joinedCPNode(badID, clusterID)
	good := joinedCPNode(goodID, clusterID)

	unhealthy := joinedCPNode(uuid.New(), clusterID)
	unhealthy.Conditions.KubernetesReady = db.Condition{Status: db.ConditionStatusFalse}

	unjoined := joinedCPNode(uuid.New(), clusterID)
	unjoined.Conditions.Joined = db.Condition{Status: db.ConditionStatusFalse}

	tests := []struct {
		name       string
		nodes      []db.Node
		kubeconfig map[uuid.UUID]string
		errors     map[uuid.UUID]error
		want       string
		wantReady  bool
		wantErr    string
		wantProbes int
	}{
		{
			name:  "no control-plane nodes is a not-ready skip, not an error",
			nodes: nil,
		},
		{
			name:  "unjoined control-plane node is not a candidate",
			nodes: []db.Node{unjoined},
		},
		{
			name:       "first candidate wins",
			nodes:      []db.Node{good},
			kubeconfig: map[uuid.UUID]string{goodID: testKubeconfig},
			want:       testKubeconfig,
			wantReady:  true,
			wantProbes: 1,
		},
		{
			name:       "erroring candidate falls through to the next",
			nodes:      []db.Node{bad, good},
			errors:     map[uuid.UUID]error{badID: errors.New("dial refused")},
			kubeconfig: map[uuid.UUID]string{goodID: testKubeconfig},
			want:       testKubeconfig,
			wantReady:  true,
			wantProbes: 2,
		},
		{
			name:       "empty kubeconfig falls through to the next",
			nodes:      []db.Node{bad, good},
			kubeconfig: map[uuid.UUID]string{badID: "", goodID: testKubeconfig},
			want:       testKubeconfig,
			wantReady:  true,
			wantProbes: 2,
		},
		{
			name:       "unhealthy joined node is still used as a fallback",
			nodes:      []db.Node{unhealthy},
			kubeconfig: map[uuid.UUID]string{unhealthy.ID: testKubeconfig},
			want:       testKubeconfig,
			wantReady:  true,
			wantProbes: 1,
		},
		{
			name:       "every candidate failing is an error, not a skip",
			nodes:      []db.Node{bad, good},
			errors:     map[uuid.UUID]error{badID: errors.New("dial refused"), goodID: errors.New("permission denied")},
			wantErr:    "failed to extract kubeconfig from 2 control plane candidates",
			wantProbes: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(tc.nodes, nil)

			nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: tc.kubeconfig, KubeconfigErrors: tc.errors}
			c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, nil, nodeAgentClient)

			got, ready, err := c.fetchAdminKubeconfig(context.Background(), cluster)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.False(t, ready)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantReady, ready)
				assert.Equal(t, tc.want, string(got))
			}
			assert.Len(t, nodeAgentClient.GetKubeconfigCalls, tc.wantProbes)
		})
	}
}

// Each probe must carry its own deadline: the controller-manager passes the
// long-lived loop context and runs sites sequentially, so an unbounded probe
// against an unreachable node-agent would stall reconciliation fleet-wide.
func TestFetchAdminKubeconfigBoundsEachProbe(t *testing.T) {
	clusterID := uuid.New()
	nodeID := uuid.New()

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)

	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}}
	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, nil, nodeAgentClient)

	// A parent with no deadline: any deadline seen by the RPC is one this code added.
	_, _, err := c.fetchAdminKubeconfig(context.Background(), db.Cluster{ID: clusterID, Name: "c"})
	require.NoError(t, err)

	require.Len(t, nodeAgentClient.GetKubeconfigCtxs, 1)
	deadline, ok := nodeAgentClient.GetKubeconfigCtxs[0].Deadline()
	require.True(t, ok, "GetKubeconfig must be called with a deadline")
	remaining := time.Until(deadline)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, probeTimeout)
}

func TestController_ReconcileOIDC(t *testing.T) {
	ctx := context.Background()
	oidcID := uuid.New()
	oidcNodeID := uuid.New()

	oidcCluster := db.Cluster{ID: oidcID, Name: "oidc", OIDCAuthEnabled: true}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(oidcCluster.Name)).Return([]db.Cluster{oidcCluster}, nil).Once()
	// The mode refresh, admin Secret upsert, SPIRE kubeconfig upsert, and
	// trust-bundle upsert each re-read the row.
	mockDB.On("GetCluster", mock.Anything, oidcID).Return(oidcCluster, nil).Times(4)
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == oidcID
	})).Return([]db.Node{joinedCPNode(oidcNodeID, oidcID)}, nil)

	nodeAgentClient := &agenttest.FakeClient{
		Kubeconfigs: map[uuid.UUID]string{oidcNodeID: testAdminConf},
	}

	// Management client seeded with the operator-managed nke-rbac ConfigMap and
	// the CAPI Cluster owner-ref target.
	rbacCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-rbac", Namespace: SecretsNamespace},
		Data:       map[string]string{"manifest.yaml": testRBACManifest},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rbacCM, capiClusterFixture(oidcCluster)).Build()

	// Tenant client the OIDC path server-side-applies the RBAC into.
	tenantClient := fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build()

	cfg := config.Config{
		TenantAccessEnabled:  true,
		SpiffeEndpointSocket: "unix:///run/spire/agent.sock",
	}
	c := newTestController(cfg, mockDB, k8sClient, nodeAgentClient)
	var gotAdminConf []byte
	c.tenantClientFor = func(adminConf []byte) (ctrlclient.Client, error) {
		gotAdminConf = adminConf
		return tenantClient, nil
	}

	_, err := c.Reconcile(ctx, reconcileRequest(oidcCluster.Name))
	require.NoError(t, err)

	// The tenant client is built from the node-agent admin.conf.
	assert.Equal(t, []byte(testAdminConf), gotAdminConf)

	// RBAC-first: the ClusterRole is server-side-applied into the tenant cluster.
	var role rbacv1.ClusterRole
	require.NoError(t, tenantClient.Get(ctx, types.NamespacedName{Name: "nke-role"}, &role))

	// kubeconfig Secret carries the SPIRE exec-auth kubeconfig (not verbatim admin.conf).
	kcSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, kcSecret))
	spireCfg, err := clientcmd.Load(kcSecret.Data[KubeconfigSecretKey])
	require.NoError(t, err)
	require.NotNil(t, spireCfg.Clusters["cluster"])
	assert.Equal(t, testServer, spireCfg.Clusters["cluster"].Server)
	require.NotNil(t, spireCfg.AuthInfos["spiffe-user"])
	require.NotNil(t, spireCfg.AuthInfos["spiffe-user"].Exec)
	assert.Equal(t, spireExecCommand, spireCfg.AuthInfos["spiffe-user"].Exec.Command)
	assert.Equal(t, spireExecAPIVersion, spireCfg.AuthInfos["spiffe-user"].Exec.APIVersion)
	var audience, socket string
	for _, e := range spireCfg.AuthInfos["spiffe-user"].Exec.Env {
		switch e.Name {
		case "SPIFFE_JWT_AUDIENCE":
			audience = e.Value
		case "SPIFFE_ENDPOINT_SOCKET":
			socket = e.Value
		}
	}
	assert.Equal(t, oidcCluster.Name, audience)
	assert.Equal(t, "unix:///run/spire/agent.sock", socket)
	assertOwnedByCAPICluster(t, kcSecret, oidcCluster)

	// trust-bundle Secret carries the tenant CA extracted from admin.conf.
	tbSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      TrustBundleSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, tbSecret))
	assert.Equal(t, []byte("test-ca"), tbSecret.Data[TrustBundleSecretKey])
	assert.Equal(t, constants.LabelValueNkeManagedBy, tbSecret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, oidcCluster.ID.String(), tbSecret.Labels[constants.LabelKeyClusterID])
	assert.Equal(t, constants.LabelValueKubeconfigModeSPIRE, kcSecret.Labels[constants.LabelKeyKubeconfigMode])
	assert.Equal(t, corev1.SecretTypeOpaque, kcSecret.Type)
	assertOwnedByCAPICluster(t, tbSecret, oidcCluster)

	// Always-admin Secret carries verbatim admin.conf even when OIDC is on.
	assertAdminSecret(t, ctx, k8sClient, oidcCluster, testAdminConf)

	mockDB.AssertExpectations(t)
}

// TestController_ReconcileMixed reconciles an OIDC and a non-OIDC cluster
// independently and verifies one cluster's state does not leak into the other.
func TestController_ReconcileMixed(t *testing.T) {
	ctx := context.Background()

	plainID, plainNodeID := uuid.New(), uuid.New()
	oidcID, oidcNodeID := uuid.New(), uuid.New()

	plainCluster := db.Cluster{ID: plainID, Name: "plain"}
	oidcCluster := db.Cluster{ID: oidcID, Name: "oidc", OIDCAuthEnabled: true}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(plainCluster.Name)).Return([]db.Cluster{plainCluster}, nil).Once()
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(oidcCluster.Name)).Return([]db.Cluster{oidcCluster}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, plainID).Return(plainCluster, nil).Times(3)
	mockDB.On("GetCluster", mock.Anything, oidcID).Return(oidcCluster, nil).Times(4)
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == plainID
	})).Return([]db.Node{joinedCPNode(plainNodeID, plainID)}, nil)
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == oidcID
	})).Return([]db.Node{joinedCPNode(oidcNodeID, oidcID)}, nil)

	nodeAgentClient := &agenttest.FakeClient{
		Kubeconfigs: map[uuid.UUID]string{
			plainNodeID: testKubeconfig,
			oidcNodeID:  testAdminConf,
		},
	}

	rbacCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-rbac", Namespace: SecretsNamespace},
		Data:       map[string]string{"manifest.yaml": testRBACManifest},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		rbacCM, capiClusterFixture(plainCluster), capiClusterFixture(oidcCluster),
	).Build()
	tenantClient := fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build()

	cfg := config.Config{
		TenantAccessEnabled:  true,
		SpiffeEndpointSocket: "unix:///run/spire/agent.sock",
	}
	c := newTestController(cfg, mockDB, k8sClient, nodeAgentClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return tenantClient, nil
	}

	_, err := c.Reconcile(ctx, reconcileRequest(plainCluster.Name))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, reconcileRequest(oidcCluster.Name))
	require.NoError(t, err)

	// Non-OIDC cluster: verbatim admin kubeconfig published.
	assertSecret(t, ctx, k8sClient, plainCluster, true)

	// OIDC cluster (same Reconcile pass): RBAC applied into the tenant, and the
	// SPIRE exec-auth kubeconfig + trust-bundle Secrets published.
	var role rbacv1.ClusterRole
	require.NoError(t, tenantClient.Get(ctx, types.NamespacedName{Name: "nke-role"}, &role))

	kcSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, kcSecret))
	spireCfg, err := clientcmd.Load(kcSecret.Data[KubeconfigSecretKey])
	require.NoError(t, err)
	require.NotNil(t, spireCfg.AuthInfos["spiffe-user"])
	require.NotNil(t, spireCfg.AuthInfos["spiffe-user"].Exec)

	tbSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      TrustBundleSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, tbSecret))
	assert.Equal(t, []byte("test-ca"), tbSecret.Data[TrustBundleSecretKey])

	assertAdminSecret(t, ctx, k8sClient, oidcCluster, testAdminConf)

	mockDB.AssertExpectations(t)
}

func joinedCPNode(nodeID, clusterID uuid.UUID) db.Node {
	return db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         "cp-node",
		Conditions: db.NodeConditions{
			Joined:          db.Condition{Status: db.ConditionStatusTrue},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:        db.Condition{Status: db.ConditionStatusFalse},
		},
	}
}

// capiClusterFixture is the CAPI Cluster object a fake k8s client must contain
// for the controller to publish anything for cluster: existence of this object
// is the reconcile gate, and published Secrets carry an owner reference to it.
func capiClusterFixture(cluster db.Cluster) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: SecretsNamespace,
			UID:       types.UID(cluster.ID.String()),
		},
	}
}

// reconcileRequest builds the ctrl.Request the controller reconciles on: the
// CAPI Cluster's namespace/name identity, not the DB cluster ID.
func reconcileRequest(clusterName string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: SecretsNamespace, Name: clusterName}}
}

// clusterNameFilter matches the db.ClusterFilters Reconcile issues to resolve
// a CAPI Cluster's name back to its DB row.
func clusterNameFilter(name string) any {
	return mock.MatchedBy(func(f db.ClusterFilters) bool {
		return f.Name != nil && *f.Name == name
	})
}

func assertSecret(t *testing.T, ctx context.Context, k8sClient ctrlclient.Client, cluster db.Cluster, want bool) {
	t.Helper()

	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, secret)
	if !want {
		assert.True(t, apierrors.IsNotFound(err), "expected no secret for cluster %s", cluster.Name)
		adminErr := k8sClient.Get(ctx, types.NamespacedName{
			Name:      AdminKubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
		}, &corev1.Secret{})
		assert.True(t, apierrors.IsNotFound(adminErr), "expected no admin secret for cluster %s", cluster.Name)
		return
	}

	require.NoError(t, err)
	assert.Equal(t, []byte(testKubeconfig), secret.Data[KubeconfigSecretKey])
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	assert.Equal(t, cluster.Name, secret.Labels[clusterv1.ClusterNameLabel])
	assert.Equal(t, constants.LabelValueNkeManagedBy, secret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.ID.String(), secret.Labels[constants.LabelKeyClusterID])
	assert.Equal(t, constants.LabelValueKubeconfigModeAdmin, secret.Labels[constants.LabelKeyKubeconfigMode])
	assertOwnedByCAPICluster(t, secret, cluster)

	assertAdminSecret(t, ctx, k8sClient, cluster, testKubeconfig)
}

func assertAdminSecret(t *testing.T, ctx context.Context, k8sClient ctrlclient.Client, cluster db.Cluster, wantKubeconfig string) {
	t.Helper()

	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      AdminKubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, secret))
	assert.Equal(t, []byte(wantKubeconfig), secret.Data[KubeconfigSecretKey])
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	assert.Equal(t, cluster.Name, secret.Labels[clusterv1.ClusterNameLabel])
	assert.Equal(t, constants.LabelValueNkeManagedBy, secret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.ID.String(), secret.Labels[constants.LabelKeyClusterID])
	assert.Equal(t, constants.LabelValueKubeconfigModeAdmin, secret.Labels[constants.LabelKeyKubeconfigMode])
	assertOwnedByCAPICluster(t, secret, cluster)
}

// assertOwnedByCAPICluster checks the Secret carries a controller owner
// reference to the CAPI Cluster this cluster's tenant-access fixtures publish
// against, so Kubernetes GC removes it when that Cluster is deleted.
func assertOwnedByCAPICluster(t *testing.T, secret *corev1.Secret, cluster db.Cluster) {
	t.Helper()

	require.Len(t, secret.OwnerReferences, 1)
	owner := secret.OwnerReferences[0]
	assert.Equal(t, "Cluster", owner.Kind)
	assert.Equal(t, clusterv1.GroupVersion.String(), owner.APIVersion)
	assert.Equal(t, cluster.Name, owner.Name)
	assert.Equal(t, types.UID(cluster.ID.String()), owner.UID)
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller)
}

type recordingTenantManagerSet struct {
	removed []uuid.UUID
}

func (s *recordingTenantManagerSet) RemoveCluster(clusterID uuid.UUID) {
	s.removed = append(s.removed, clusterID)
}

func newTestController(
	cfg config.Config,
	database db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
) *Controller {
	logger := slog.New(slog.DiscardHandler)
	c := NewController(cfg, logger, dbwrapper.New(database, logger), k8sClient, nodeAgentClient, &recordingTenantManagerSet{})
	c.scheme = testSchemeForController()
	return c
}

func testSchemeForController() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	return scheme
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return scheme
}

// tenantScheme registers the API types the OIDC path server-side-applies into a
// tenant cluster (the NKE RBAC manifest).
func tenantScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	return scheme
}

// If a cluster starts deleting while a reconcile is still running, the stale
// row loaded at the start must not publish new access material.
func TestController_SkipsPublishWhenClusterStartsDeletingMidPass(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()

	// The snapshot the pass carries: not deleting.
	snapshot := db.Cluster{ID: clusterID, Name: "vanishing"}

	// The row as it stands by the time we are about to write: deleting.
	live := snapshot
	live.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(snapshot.Name)).Return([]db.Cluster{snapshot}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(live, nil).Once()

	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(snapshot)).Build()

	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, k8sClient, nodeAgentClient)
	_, err := c.Reconcile(ctx, reconcileRequest(snapshot.Name))
	require.NoError(t, err)

	assertSecret(t, ctx, k8sClient, snapshot, false)
}

// Same window, one step further along: teardown has already deleted the row.
func TestController_SkipsPublishWhenClusterRowIsGone(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()

	snapshot := db.Cluster{ID: clusterID, Name: "deleted"}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(snapshot.Name)).Return([]db.Cluster{snapshot}, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound).Once()

	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}}
	// The CAPI Cluster still exists -- only the DB row has raced ahead.
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(snapshot)).Build()

	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, k8sClient, nodeAgentClient)

	// A vanished cluster is a normal outcome of the teardown race, not an error.
	_, err := c.Reconcile(ctx, reconcileRequest(snapshot.Name))
	require.NoError(t, err)
	assertSecret(t, ctx, k8sClient, snapshot, false)
}

// Each control-plane node has its own admin.conf, and ListNodes has no ORDER BY,
// so a probe order that follows DB row order republishes different bytes for the
// same cluster on most passes -- rewriting the Secret and logging every time.
func TestFetchAdminKubeconfigPicksTheSameNodeRegardlessOfRowOrder(t *testing.T) {
	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "c"}

	first := joinedCPNode(uuid.MustParse("00000000-0000-4000-8000-0000000000aa"), clusterID)
	second := joinedCPNode(uuid.MustParse("00000000-0000-4000-8000-0000000000bb"), clusterID)
	third := joinedCPNode(uuid.MustParse("00000000-0000-4000-8000-0000000000cc"), clusterID)

	// Every node serves its own admin.conf, as kubeadm issues it.
	kubeconfigs := map[uuid.UUID]string{
		first.ID:  "admin-conf-aa",
		second.ID: "admin-conf-bb",
		third.ID:  "admin-conf-cc",
	}

	// The same three rows in three different orders, as Postgres may return them.
	orders := [][]db.Node{
		{first, second, third},
		{third, first, second},
		{second, third, first},
	}

	for _, rows := range orders {
		mockDB := &testutil.MockDB{}
		mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(rows, nil)

		nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: kubeconfigs}
		c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, nil, nodeAgentClient)

		got, ready, err := c.fetchAdminKubeconfig(context.Background(), cluster)
		require.NoError(t, err)
		require.True(t, ready)

		// Lowest node ID wins every time, so the published bytes are stable.
		assert.Equal(t, "admin-conf-aa", string(got))
		assert.Equal(t, []uuid.UUID{first.ID}, nodeAgentClient.GetKubeconfigCalls)
	}
}

func TestController_PublishesOIDCWhenTenantAccessDisabled(t *testing.T) {
	ctx := context.Background()
	oidcID, oidcNodeID := uuid.New(), uuid.New()
	oidcCluster := db.Cluster{ID: oidcID, Name: "oidc", OIDCAuthEnabled: true}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(oidcCluster.Name)).Return([]db.Cluster{oidcCluster}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, oidcID).Return(oidcCluster, nil).Times(4)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(oidcNodeID, oidcID)}, nil)

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(nkeRBACConfigMap(), capiClusterFixture(oidcCluster)).Build()
	c := newTestController(
		config.Config{TenantAccessEnabled: false, SpiffeEndpointSocket: "unix:///run/spire/agent.sock"},
		mockDB,
		k8sClient,
		&agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{oidcNodeID: testAdminConf}},
	)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build(), nil
	}
	_, err := c.Reconcile(ctx, reconcileRequest(oidcCluster.Name))
	require.NoError(t, err)

	kcSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, kcSecret))
	assert.Equal(t, constants.LabelValueKubeconfigModeSPIRE, kcSecret.Labels[constants.LabelKeyKubeconfigMode])
	assertAdminSecret(t, ctx, k8sClient, oidcCluster, testAdminConf)
}

func TestController_PublishesAdminWhenTenantAccessDisabled(t *testing.T) {
	ctx := context.Background()
	clusterID, nodeID := uuid.New(), uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "plain"}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(cluster.Name)).Return([]db.Cluster{cluster}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Times(3)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(cluster)).Build()
	c := newTestController(
		config.Config{TenantAccessEnabled: false},
		mockDB,
		k8sClient,
		&agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}},
	)
	_, err := c.Reconcile(ctx, reconcileRequest(cluster.Name))
	require.NoError(t, err)
	assertSecret(t, ctx, k8sClient, cluster, true)
}

func TestPublish_RefusesWrongOwnerSecret(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "oidc", OIDCAuthEnabled: true}
	otherID := uuid.New()

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
			Labels: map[string]string{
				constants.LabelKeyClusterID:      otherID.String(),
				constants.LabelKeyKubeconfigMode: constants.LabelValueKubeconfigModeSPIRE,
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing, nkeRBACConfigMap(), capiClusterFixture(cluster)).Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c := newPublishTestController(mockDB, k8sClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build(), nil
	}
	err := c.publish(ctx, cluster, []byte(testAdminConf))
	require.Error(t, err)
	assert.ErrorIs(t, err, capi.ErrWrongSecretOwner)
}

func TestController_PublishesAdminForCAPIWhenFlagsOff(t *testing.T) {
	ctx := context.Background()
	clusterID, nodeID := uuid.New(), uuid.New()
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "capi",
		ProviderType: db.ProviderTypeCAPI,
	}
	cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: []byte(`{"kind":"DockerCluster"}`)})

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, clusterNameFilter(cluster.Name)).Return([]db.Cluster{cluster}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Times(3)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(cluster)).Build()
	c := newTestController(
		config.Config{TenantAccessEnabled: false},
		mockDB,
		k8sClient,
		&agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}},
	)
	_, err := c.Reconcile(ctx, reconcileRequest(cluster.Name))
	require.NoError(t, err)
	assertSecret(t, ctx, k8sClient, cluster, true)
}

func TestPublish_SkipsMissingCluster(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "gone"}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(db.Cluster{}, db.ErrNotFound)
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := newPublishTestController(mockDB, k8sClient)
	require.NoError(t, c.publish(ctx, cluster, []byte(testKubeconfig)))
	assertSecret(t, ctx, k8sClient, cluster, false)
}

func TestPublish_SkipsDeletingCluster(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "gone"}
	live := cluster
	live.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(live, nil)
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := newPublishTestController(mockDB, k8sClient)
	require.NoError(t, c.publish(ctx, cluster, []byte(testKubeconfig)))
	assertSecret(t, ctx, k8sClient, cluster, false)
}

func TestPublish_RewritesAdminToSPIRE(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "rewrite", OIDCAuthEnabled: true}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
			Labels: map[string]string{
				constants.LabelKeyClusterID:      cluster.ID.String(),
				constants.LabelKeyKubeconfigMode: constants.LabelValueKubeconfigModeAdmin,
				clusterv1.ClusterNameLabel:       cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretKey: []byte(testKubeconfig)},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing, nkeRBACConfigMap(), capiClusterFixture(cluster)).Build()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c := newPublishTestController(mockDB, k8sClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build(), nil
	}
	require.NoError(t, c.publish(ctx, cluster, []byte(testAdminConf)))

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, got))
	assert.Equal(t, constants.LabelValueKubeconfigModeSPIRE, got.Labels[constants.LabelKeyKubeconfigMode])
	assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
	assert.Contains(t, string(got.Data[KubeconfigSecretKey]), "k8s-spiffe-workload-jwt-exec-auth")
	assertOwnedByCAPICluster(t, got, cluster)
}

func TestPublish_CAPIClusterOIDCConsumedFromNKESystem(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{
		ID:              uuid.New(),
		Name:            "capi-oidc",
		OIDCAuthEnabled: true,
	}
	cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: []byte(`{"kind":"DockerCluster"}`)})

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(nkeRBACConfigMap(), capiClusterFixture(cluster)).Build()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c := newPublishTestController(mockDB, k8sClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build(), nil
	}
	require.NoError(t, c.publish(ctx, cluster, kubeconfigWithParseableCA(t)))

	dbProvider := kube.NewDBClusterTenantClientProvider(nil, k8sClient, nil, false)
	tenantClient, err := dbProvider.ClientForDBCluster(ctx, cluster)
	require.NoError(t, err)
	require.NotNil(t, tenantClient)
}

func TestPublish_UsesLiveOIDCAuthEnabled(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	snapshot := db.Cluster{ID: id, Name: "stale", OIDCAuthEnabled: false}
	live := db.Cluster{ID: id, Name: "stale", OIDCAuthEnabled: true}

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(nkeRBACConfigMap(), capiClusterFixture(live)).Build()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, id).Return(live, nil)
	c := newPublishTestController(mockDB, k8sClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build(), nil
	}
	require.NoError(t, c.publish(ctx, snapshot, []byte(testAdminConf)))

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(live.Name),
		Namespace: SecretsNamespace,
	}, got))
	assert.Equal(t, constants.LabelValueKubeconfigModeSPIRE, got.Labels[constants.LabelKeyKubeconfigMode])
	assert.Contains(t, string(got.Data[KubeconfigSecretKey]), "k8s-spiffe-workload-jwt-exec-auth")
}

func TestPublish_RefusesSPIREWithoutRBAC(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "oidc", OIDCAuthEnabled: true}

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(cluster)).Build()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c := newPublishTestController(mockDB, k8sClient)
	err := c.publish(ctx, cluster, []byte(testAdminConf))
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), err)

	// The always-admin Secret is written before the SPIRE path; missing nke-rbac
	// must not leave operators without break-glass admin.conf.
	assertAdminSecret(t, ctx, k8sClient, cluster, testAdminConf)

	cweSecret := &corev1.Secret{}
	cweErr := k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, cweSecret)
	assert.True(t, apierrors.IsNotFound(cweErr), "SPIRE kubeconfig must not publish without RBAC")
}

func kubeconfigWithParseableCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://198.51.100.10:6443
    certificate-authority-data: %s
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user: {}
`, base64.StdEncoding.EncodeToString(caPEM))
}

// A cluster can start deleting after publish() has picked a mode but before the
// Secret is written -- on the OIDC path applyRBAC sits in that window and is
// bounded only by tenantRequestTimeout, so upsertSecret re-reads the DB row
// immediately before each write.
func TestPublish_SkipsClusterThatStartsDeletingBeforeTheWrite(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "racing"}
	deleting := cluster
	deleting.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}

	mockDB := &testutil.MockDB{}
	// First read is publish()'s mode selection; the row is still live.
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	// By the time upsertSecret re-reads, teardown has marked it deleting.
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(deleting, nil)

	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiClusterFixture(cluster)).Build()
	c := newPublishTestController(mockDB, k8sClient)

	require.NoError(t, c.publish(ctx, cluster, []byte(testKubeconfig)))
	assertSecret(t, ctx, k8sClient, cluster, false)
}
