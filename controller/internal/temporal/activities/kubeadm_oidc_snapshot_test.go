// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const (
	snapshotSharedIssuer = "https://management.example.test"
	snapshotPinnedIssuer = "https://operator-a.example.test"
	snapshotTrustDomain  = "management.example.test"
)

func TestOIDCSnapshotSharedProviderControl(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())

	err := executeSnapshotInit(t, a, KubeadmInitInput{
		ClusterID:     cluster.ID,
		InitialNodeID: initialNode.ID,
	})

	require.NoError(t, err)
	auth := authenticationConfigFromFiles(t, requireSingleInitRequest(t, agent).GetFiles())
	require.Len(t, auth.JWT, 1)
	assert.Equal(t, snapshotSharedIssuer, auth.JWT[0].Issuer.URL)
	database.AssertExpectations(t)
}

func TestOIDCSnapshotJoinWithPinnedEmptyProvidersKeepsSharedProvider(t *testing.T) {
	cluster, joiningNode, joinedNode, nodeSet, nodePool := snapshotJoinRecords()
	cluster.OIDCProviders = []db.ClusterOIDCProvider{}
	database := snapshotJoinDatabase(cluster, joiningNode, joinedNode, nodeSet, nodePool)
	tenantClient := newKubeadmConfigTenantClient(t, snapshotKubeadmClusterConfiguration())
	require.NoError(t, tenantClient.Create(context.Background(), etcdMirrorPod(joinedNode.Name,
		"--quota-backend-bytes="+kubeadmconfig.EtcdQuotaBackendBytes)))
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, joinStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())
	a.tenantClientProvider = &fakeTenantClientProvider{tenantClient: tenantClient}

	err := executeSnapshotJoin(t, a, joiningNode)

	require.NoError(t, err)
	auth := authenticationConfigFromFiles(t, requireSingleJoinRequest(t, agent).GetFiles())
	require.Len(t, auth.JWT, 1)
	assert.Equal(t, snapshotSharedIssuer, auth.JWT[0].Issuer.URL)
	database.AssertExpectations(t)
}

func TestOIDCSnapshotInitUsesPinnedProviders(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	cluster.OIDCProviders = []db.ClusterOIDCProvider{snapshotOperatorProvider()}
	database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())

	err := executeSnapshotInit(t, a, KubeadmInitInput{
		ClusterID:     cluster.ID,
		InitialNodeID: initialNode.ID,
	})

	require.NoError(t, err)
	auth := authenticationConfigFromFiles(t, requireSingleInitRequest(t, agent).GetFiles())
	require.Len(t, auth.JWT, 2, "init combines the shared provider with the providers pinned on the cluster")
	assert.Equal(t, snapshotSharedIssuer, auth.JWT[0].Issuer.URL)
	assertSnapshotOperatorProvider(t, auth.JWT[1])
	assert.Contains(t, userValidationExpressions(auth.JWT[1]), `!user.username.startsWith("spiffe://`+snapshotTrustDomain+`/")`)
	database.AssertExpectations(t)
}

func TestOIDCSnapshotInitStopsWhenReservedIdentitiesCannotBeRead(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	cluster.OIDCProviders = []db.ClusterOIDCProvider{snapshotOperatorProvider()}
	database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())
	readErr := errors.New("apiserver unavailable")
	a.k8sClient = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return readErr
		},
	}).Build()

	err := executeSnapshotInit(t, a, KubeadmInitInput{
		ClusterID:     cluster.ID,
		InitialNodeID: initialNode.ID,
	})

	require.ErrorContains(t, err, readErr.Error())
	assert.Empty(t, agent.KubeadmFilesSupportCalls, "a failed nke-rbac read must stop file transport")
	assert.Empty(t, agent.initRequests, "a failed nke-rbac read must stop kubeadm init")
}

func TestOIDCSnapshotJoinUsesPinnedProviders(t *testing.T) {
	const exchanged = "spiffe://exchange.example.test/nke-service/capi-manager"
	cluster, joiningNode, joinedNode, nodeSet, nodePool := snapshotJoinRecords()
	cluster.OIDCAuthEnabled = false
	cluster.OIDCProviders = []db.ClusterOIDCProvider{snapshotOperatorProvider()}
	database := snapshotJoinDatabase(cluster, joiningNode, joinedNode, nodeSet, nodePool)
	tenantClient := newKubeadmConfigTenantClient(t, snapshotKubeadmClusterConfiguration())
	require.NoError(t, tenantClient.Create(context.Background(), etcdMirrorPod(joinedNode.Name,
		"--quota-backend-bytes="+kubeadmconfig.EtcdQuotaBackendBytes)))
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, joinStatus: true}
	a := snapshotActivities(database, agent, config.Config{}, nvkeRBACConfigMap(exchanged))
	a.tenantClientProvider = &fakeTenantClientProvider{tenantClient: tenantClient}

	err := executeSnapshotJoin(t, a, joiningNode)

	require.NoError(t, err)
	auth := authenticationConfigFromFiles(t, requireSingleJoinRequest(t, agent).GetFiles())
	require.Len(t, auth.JWT, 1)
	assertSnapshotOperatorProvider(t, auth.JWT[0])
	assert.Contains(t, userValidationExpressions(auth.JWT[0]), `user.username != "`+exchanged+`"`)
	database.AssertExpectations(t)
}

type snapshotWiringAgent struct {
	*agenttest.FakeClient
	initRequests []*nvkeproto.KubeadmInitRequest
	joinRequests []*nvkeproto.KubeadmJoinRequest
	initStatus   bool
	joinStatus   bool
	statusCalls  int
}

func (f *snapshotWiringAgent) GetKubeadmStatus(context.Context, db.Node) (*nvkeproto.KubeadmStatusResponse, error) {
	f.statusCalls++
	if f.initStatus {
		if f.statusCalls == 1 {
			return &nvkeproto.KubeadmStatusResponse{}, nil
		}
		return &nvkeproto.KubeadmStatusResponse{KubeadmPhase: kubeadmPhaseInit, KubeadmStatus: kubeadmStatusReady, CniInstallStatus: cniStatusReady}, nil
	}
	if f.joinStatus {
		return &nvkeproto.KubeadmStatusResponse{KubeadmPhase: kubeadmPhaseJoin, KubeadmStatus: kubeadmStatusReady}, nil
	}
	return &nvkeproto.KubeadmStatusResponse{}, nil
}

func (f *snapshotWiringAgent) ExecuteKubeadmInit(
	_ context.Context,
	_ db.Node,
	req *nvkeproto.KubeadmInitRequest,
) (*nvkeproto.KubeadmInitStatusResponse, error) {
	f.initRequests = append(f.initRequests, req)
	return &nvkeproto.KubeadmInitStatusResponse{}, nil
}

func (f *snapshotWiringAgent) ExecuteKubeadmJoin(
	_ context.Context,
	_ db.Node,
	req *nvkeproto.KubeadmJoinRequest,
) (*nvkeproto.KubeadmJoinStatusResponse, error) {
	f.joinRequests = append(f.joinRequests, req)
	return &nvkeproto.KubeadmJoinStatusResponse{}, nil
}

func (f *snapshotWiringAgent) UpdateAndFetchKubeAdmJoinStrings(
	context.Context,
	db.Node,
	*nvkeproto.KubeadmJoinStringRequest,
) (*nvkeproto.KubeadmJoinStringResponse, error) {
	return &nvkeproto.KubeadmJoinStringResponse{
		ControlplaneJoinString: "kubeadm join 192.0.2.10:6443 --control-plane --certificate-key REDACTED " +
			"--token example-token.REDACTED --discovery-token-ca-cert-hash sha256:REDACTED",
		WorkerJoinString: "kubeadm join 192.0.2.10:6443 --token example-token.REDACTED " +
			"--discovery-token-ca-cert-hash sha256:REDACTED",
	}, nil
}

func snapshotActivities(database *testutil.MockDB, agent *snapshotWiringAgent, cfg config.Config, objects ...ctrlclient.Object) *Activities {
	cfg.NodeAgent.Port = "15005"
	return &Activities{
		cfg:             cfg,
		db:              database,
		k8sClient:       fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objects...).Build(),
		nodeAgentClient: agent,
		trustDomain:     snapshotTrustDomain,
	}
}

func nvkeRBACConfigMap(subject string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
		Data: map[string]string{nvkerbac.ConfigMapKey: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nke:capi-cluster-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: ` + subject + "\n"},
	}
}

func userValidationExpressions(authenticator apiserverv1.JWTAuthenticator) []string {
	expressions := make([]string, 0, len(authenticator.UserValidationRules))
	for _, rule := range authenticator.UserValidationRules {
		expressions = append(expressions, rule.Expression)
	}
	return expressions
}

func snapshotSharedConfig() config.Config {
	return config.Config{NvkeOIDCAuth: config.NvkeOIDCAuth{
		Enabled:   true,
		IssuerURL: snapshotSharedIssuer,
	}}
}

func snapshotInitRecords() (db.Cluster, db.Node, db.NodeSet, db.NodePool) {
	cluster := snapshotCluster()
	nodePoolID := uuid.New()
	nodeSet := db.NodeSet{ID: uuid.New(), NodePoolID: nodePoolID}
	nodePool := db.NodePool{ID: nodePoolID, ClusterID: cluster.ID, ControlPlane: true}
	node := db.Node{
		ID:           uuid.New(),
		ClusterID:    cluster.ID,
		NodePoolID:   nodePoolID,
		NodeSetID:    nodeSet.ID,
		ControlPlane: true,
		Name:         "cp-0",
		IP:           "192.0.2.10",
	}
	return cluster, node, nodeSet, nodePool
}

func snapshotJoinRecords() (db.Cluster, db.Node, db.Node, db.NodeSet, db.NodePool) {
	cluster := snapshotCluster()
	nodePoolID := uuid.New()
	nodeSet := db.NodeSet{ID: uuid.New(), NodePoolID: nodePoolID}
	nodePool := db.NodePool{ID: nodePoolID, ClusterID: cluster.ID, ControlPlane: true}
	joiningNode := db.Node{
		ID:           uuid.New(),
		ClusterID:    cluster.ID,
		NodePoolID:   nodePoolID,
		NodeSetID:    nodeSet.ID,
		ControlPlane: true,
		Name:         "cp-1",
		IP:           "192.0.2.11",
	}
	joinedNode := db.Node{
		ID:           uuid.New(),
		ClusterID:    cluster.ID,
		ControlPlane: true,
		Name:         "cp-0",
		IP:           "192.0.2.10",
		Conditions: db.NodeConditions{
			Joined: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	return cluster, joiningNode, joinedNode, nodeSet, nodePool
}

func snapshotCluster() db.Cluster {
	return db.Cluster{
		ID:                uuid.New(),
		Name:              "snapshot-cluster",
		CniPlugin:         db.CNICalico,
		KubernetesVersion: "v1.34.1",
		PodCIDR:           "192.168.0.0/16",
		ServiceCIDR:       "10.96.0.0/12",
		OIDCAuthEnabled:   true,
		ProviderType:      db.ProviderTypeNico,
	}
}

func snapshotOperatorProvider() db.ClusterOIDCProvider {
	return db.ClusterOIDCProvider{
		Name:           "operator-a",
		IssuerURL:      snapshotPinnedIssuer,
		Audiences:      []string{"tenant-cluster", "kubectl"},
		UsernameClaim:  "email",
		UsernamePrefix: "operator-a:",
		GroupsClaim:    "groups",
		GroupsPrefix:   "operator-a:",
		RequiredClaims: map[string]string{"tenant": "production"},
		CABundle:       "operator-a CA",
	}
}

// snapshotAdmissionTx serves the control-plane mutation admission that init and
// join take before any provisioning work: the cluster row lock and the node row.
func snapshotAdmissionTx(cluster db.Cluster, node db.Node) *testutil.MockTx {
	tx := &testutil.MockTx{}
	tx.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	tx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil).Once()
	tx.On("UpdateNode", mock.Anything, mock.Anything).Return(node, nil).Once()
	return tx
}

func snapshotInitDatabase(cluster db.Cluster, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool) *testutil.MockDB {
	database := &testutil.MockDB{MockTx: snapshotAdmissionTx(cluster, node)}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil).Once()
	database.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil).Once()
	return database
}

func snapshotJoinDatabase(
	cluster db.Cluster,
	joiningNode db.Node,
	joinedNode db.Node,
	nodeSet db.NodeSet,
	nodePool db.NodePool,
) *testutil.MockDB {
	database := &testutil.MockDB{MockTx: snapshotAdmissionTx(cluster, joiningNode)}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == cluster.ID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return([]db.Node{joinedNode}, nil).Twice()
	database.On("GetNodeSet", mock.Anything, joiningNode.NodeSetID).Return(nodeSet, nil).Once()
	database.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil).Once()
	return database
}

func executeSnapshotInit(t *testing.T, a *Activities, input KubeadmInitInput) error {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(a.kubeadmInit)
	_, err := env.ExecuteActivity(a.kubeadmInit, input)
	return err
}

func executeSnapshotJoin(t *testing.T, a *Activities, node db.Node) error {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(a.kubeadmJoin)
	_, err := env.ExecuteActivity(a.kubeadmJoin, node)
	return err
}

func authenticationConfigFromFiles(t *testing.T, files []*nvkeproto.File) apiserverv1.AuthenticationConfiguration {
	t.Helper()
	for _, file := range files {
		if file.GetPath() != kubeadmconfig.OIDCAuthConfigPath {
			continue
		}
		var auth apiserverv1.AuthenticationConfiguration
		require.NoError(t, yaml.Unmarshal(file.GetContent(), &auth))
		return auth
	}
	require.FailNow(t, "kubeadm request did not carry an AuthenticationConfiguration file")
	return apiserverv1.AuthenticationConfiguration{}
}

func requireSingleInitRequest(t *testing.T, agent *snapshotWiringAgent) *nvkeproto.KubeadmInitRequest {
	t.Helper()
	require.Len(t, agent.initRequests, 1)
	return agent.initRequests[0]
}

func requireSingleJoinRequest(t *testing.T, agent *snapshotWiringAgent) *nvkeproto.KubeadmJoinRequest {
	t.Helper()
	require.Len(t, agent.joinRequests, 1)
	return agent.joinRequests[0]
}

func assertSnapshotOperatorProvider(t *testing.T, got apiserverv1.JWTAuthenticator) {
	t.Helper()
	want := snapshotOperatorProvider()
	assert.Equal(t, want.IssuerURL, got.Issuer.URL)
	assert.Equal(t, want.Audiences, got.Issuer.Audiences)
	assert.Equal(t, want.CABundle, got.Issuer.CertificateAuthority)
	assert.Equal(t, want.UsernameClaim, got.ClaimMappings.Username.Claim)
	require.NotNil(t, got.ClaimMappings.Username.Prefix)
	assert.Equal(t, want.UsernamePrefix, *got.ClaimMappings.Username.Prefix)
	assert.Equal(t, want.GroupsClaim, got.ClaimMappings.Groups.Claim)
	require.NotNil(t, got.ClaimMappings.Groups.Prefix)
	assert.Equal(t, want.GroupsPrefix, *got.ClaimMappings.Groups.Prefix)
	assert.Equal(t, []apiserverv1.ClaimValidationRule{{
		Claim:         "tenant",
		RequiredValue: "production",
	}}, got.ClaimValidationRules)
}

func snapshotKubeadmClusterConfiguration() string {
	return `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
etcd:
  local:
    extraArgs:
      quota-backend-bytes: "17179869184"
`
}
