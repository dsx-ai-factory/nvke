// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"errors"
	"log/slog"
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
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
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
	// Active is read once by Reconcile and once immediately before publication.
	mockDB.On("GetCluster", mock.Anything, activeCluster.ID).Return(activeCluster, nil).Twice()
	mockDB.On("GetCluster", mock.Anything, deletingCluster.ID).Return(deletingCluster, nil).Once()
	mockDB.On("GetCluster", mock.Anything, notReadyCluster.ID).Return(notReadyCluster, nil).Once()
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
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(kubeconfigSecret, trustBundleSecret).Build()

	cfg := config.Config{TenantAccessEnabled: true}
	tenantManagers := &recordingTenantManagerSet{}
	c := NewController(cfg, slog.New(slog.DiscardHandler), dbwrapper.New(mockDB, slog.New(slog.DiscardHandler)), k8sClient, nodeAgentClient, tenantManagers)

	_, err := c.Reconcile(ctx, dbreconcile.ClusterKey(activeCluster.ID))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, dbreconcile.ClusterKey(deletingCluster.ID))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, dbreconcile.ClusterKey(notReadyCluster.ID))
	require.NoError(t, err)

	// The non-OIDC cluster publishes the admin kubeconfig verbatim.
	assertSecret(t, ctx, k8sClient, activeCluster, true)
	// Deleting clusters preserve existing access material for the teardown
	// workflow, while not-ready clusters publish nothing.
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
// against an unreachable agent would stall reconciliation fleet-wide.
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
	// Reconcile reads once; each of the two published Secrets re-checks the row.
	mockDB.On("GetCluster", mock.Anything, oidcID).Return(oidcCluster, nil).Times(3)
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == oidcID
	})).Return([]db.Node{joinedCPNode(oidcNodeID, oidcID)}, nil)

	nodeAgentClient := &agenttest.FakeClient{
		Kubeconfigs: map[uuid.UUID]string{oidcNodeID: testAdminConf},
	}

	// Management client seeded with the operator-managed nke-rbac ConfigMap.
	rbacCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-rbac", Namespace: SecretsNamespace},
		Data:       map[string]string{"manifest.yaml": testRBACManifest},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rbacCM).Build()

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

	_, err := c.Reconcile(ctx, dbreconcile.ClusterKey(oidcID))
	require.NoError(t, err)

	// The tenant client is built from the agent admin.conf.
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

	// trust-bundle Secret carries the tenant CA extracted from admin.conf.
	tbSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      TrustBundleSecretName(oidcCluster.Name),
		Namespace: SecretsNamespace,
	}, tbSecret))
	assert.Equal(t, []byte("test-ca"), tbSecret.Data[TrustBundleSecretKey])
	assert.Equal(t, constants.LabelValueNkeManagedBy, tbSecret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, oidcCluster.ID.String(), tbSecret.Labels[constants.LabelKeyClusterID])

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
	mockDB.On("GetCluster", mock.Anything, plainID).Return(plainCluster, nil).Twice()
	mockDB.On("GetCluster", mock.Anything, oidcID).Return(oidcCluster, nil).Times(3)
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
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rbacCM).Build()
	tenantClient := fake.NewClientBuilder().WithScheme(tenantScheme(t)).Build()

	cfg := config.Config{
		TenantAccessEnabled:  true,
		SpiffeEndpointSocket: "unix:///run/spire/agent.sock",
	}
	c := newTestController(cfg, mockDB, k8sClient, nodeAgentClient)
	c.tenantClientFor = func([]byte) (ctrlclient.Client, error) {
		return tenantClient, nil
	}

	_, err := c.Reconcile(ctx, dbreconcile.ClusterKey(plainID))
	require.NoError(t, err)
	_, err = c.Reconcile(ctx, dbreconcile.ClusterKey(oidcID))
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

func assertSecret(t *testing.T, ctx context.Context, k8sClient ctrlclient.Client, cluster db.Cluster, want bool) {
	t.Helper()

	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      KubeconfigSecretName(cluster.Name),
		Namespace: SecretsNamespace,
	}, secret)
	if !want {
		assert.True(t, apierrors.IsNotFound(err), "expected no secret for cluster %s", cluster.Name)
		return
	}

	require.NoError(t, err)
	assert.Equal(t, []byte(testKubeconfig), secret.Data[KubeconfigSecretKey])
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	assert.Equal(t, constants.LabelValueNkeManagedBy, secret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.ID.String(), secret.Labels[constants.LabelKeyClusterID])
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
	return NewController(cfg, logger, dbwrapper.New(database, logger), k8sClient, nodeAgentClient, &recordingTenantManagerSet{})
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
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
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(snapshot, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(live, nil).Once()

	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, k8sClient, nodeAgentClient)
	_, err := c.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
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
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedCPNode(nodeID, clusterID)}, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(snapshot, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound).Once()

	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{nodeID: testKubeconfig}}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	c := newTestController(config.Config{TenantAccessEnabled: true}, mockDB, k8sClient, nodeAgentClient)

	// A vanished cluster is a normal outcome of the teardown race, not an error.
	_, err := c.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
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
