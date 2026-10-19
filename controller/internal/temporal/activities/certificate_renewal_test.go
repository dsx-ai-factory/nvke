// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// TestBeginCertificateRenewalRecordsAcceptedStateAndLock requires acceptance and
// mutation ownership in the same write, so dispatch cannot precede ownership.
func TestBeginCertificateRenewalRecordsAcceptedStateAndLock(t *testing.T) {
	clusterID := uuid.New()
	due := time.Now().Add(-time.Hour).UTC()
	tx := &testutil.MockTx{}
	database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
		},
	}, nil).Once()
	tx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Once()
	tx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{{
		IP: "10.0.0.10", ControlPlane: true,
		Conditions: db.NodeConditions{Joined: db.NewCondition(true, ""), Provisioned: db.NewCondition(true, "")},
	}}, nil).Once()
	tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
		status := cluster.Conditions.CertificateRenewal
		update := cluster.Conditions.UpdateInProgress
		return status != nil && status.CycleID == "cycle-1" &&
			status.State == db.CertificateRenewalStateAccepted && !status.Forced &&
			status.ScheduledFor != nil && status.ScheduledFor.Equal(due) &&
			update != nil && update.Type == db.OperationTypeCertRenewal
	})).Return(db.Cluster{}, nil).Once()
	a := &Activities{db: database}

	_, err := a.BeginCertificateRenewal(context.Background(), BeginCertificateRenewalInput{
		ClusterID: clusterID, CycleID: "cycle-1", ScheduledFor: &due,
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

// TestCompleteCertificateRenewalAdvancesDueOnlyOnSuccess couples the new due time
// with recorded success and lock release rather than workflow dispatch.
func TestCompleteCertificateRenewalAdvancesDueOnlyOnSuccess(t *testing.T) {
	clusterID := uuid.New()
	oldDue := time.Now().Add(-time.Hour).UTC()
	started := time.Now().Add(-time.Minute).UTC()
	tx := &testutil.MockTx{}
	database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:                 clusterID,
		NextCertRenewalDue: &oldDue,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal},
			CertificateRenewal: &db.CertificateRenewalStatus{
				CycleID: "cycle-1", State: db.CertificateRenewalStateAccepted, StartedAt: started,
			},
		},
	}, nil).Once()
	tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
		status := cluster.Conditions.CertificateRenewal
		return cluster.Conditions.UpdateInProgress == nil && status != nil &&
			status.State == db.CertificateRenewalStateSucceeded &&
			status.Outcome == db.CertificateRenewalOutcomeRenewed && status.CompletedAt != nil &&
			cluster.NextCertRenewalDue != nil && cluster.NextCertRenewalDue.After(time.Now().Add(11*time.Hour))
	})).Return(db.Cluster{}, nil).Once()
	a := &Activities{db: database, cfg: config.Config{CertRenewalInterval: 12 * time.Hour}}

	_, err := a.CompleteCertificateRenewal(context.Background(), CompleteCertificateRenewalInput{
		ClusterID: clusterID, CycleID: "cycle-1", Outcome: db.CertificateRenewalOutcomeRenewed,
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

func TestCompleteCertificateRenewalHandlesClusterLookupErrors(t *testing.T) {
	tests := []struct {
		name             string
		lookupErr        error
		wantNonRetryable bool
	}{
		{name: "deleted cluster", lookupErr: db.ErrNotFound, wantNonRetryable: true},
		{name: "wrapped not found", lookupErr: fmt.Errorf("cluster lookup: %w", db.ErrNotFound), wantNonRetryable: true},
		{name: "transient database error", lookupErr: errors.New("database unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, test.lookupErr).Once()
			a := &Activities{db: database, cfg: config.Config{CertRenewalInterval: 12 * time.Hour}}

			_, err := a.CompleteCertificateRenewal(context.Background(), CompleteCertificateRenewalInput{
				ClusterID: clusterID, CycleID: "cycle-1", Outcome: db.CertificateRenewalOutcomeRenewed,
			})

			require.ErrorIs(t, err, test.lookupErr)
			var applicationErr *temporal.ApplicationError
			if test.wantNonRetryable {
				require.ErrorAs(t, err, &applicationErr)
				require.True(t, applicationErr.NonRetryable())
				require.Equal(t, "db.ErrNotFound", applicationErr.Type())
			} else {
				require.False(t, errors.As(err, &applicationErr) && applicationErr.NonRetryable())
			}
			database.AssertExpectations(t)
			tx.AssertExpectations(t)
			tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

func TestFailCertificateRenewalLeavesDueOverdueAndRecordsNode(t *testing.T) {
	clusterID := uuid.New()
	nodeID := uuid.New()
	oldDue := time.Now().Add(-time.Hour).UTC()
	tx := &testutil.MockTx{}
	database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
		ID:                 clusterID,
		NextCertRenewalDue: &oldDue,
		Conditions: db.ClusterConditions{
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal},
			CertificateRenewal: &db.CertificateRenewalStatus{
				CycleID: "cycle-1", State: db.CertificateRenewalStateAccepted,
			},
		},
	}, nil).Once()
	tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
		status := cluster.Conditions.CertificateRenewal
		return cluster.Conditions.UpdateInProgress == nil && cluster.NextCertRenewalDue.Equal(oldDue) &&
			status != nil && status.State == db.CertificateRenewalStateFailed &&
			status.FailedNodeID != nil && *status.FailedNodeID == nodeID && status.Phase == "postflight" &&
			status.Message == "etcd is unhealthy"
	})).Return(db.Cluster{}, nil).Once()
	a := &Activities{db: database}

	_, err := a.FailCertificateRenewal(context.Background(), FailCertificateRenewalInput{
		ClusterID: clusterID, CycleID: "cycle-1", FailedNodeID: &nodeID,
		Phase: "postflight", Message: "etcd is unhealthy",
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

func TestFailCertificateRenewalHandlesClusterLookupErrors(t *testing.T) {
	tests := []struct {
		name      string
		lookupErr error
		wantError bool
	}{
		{name: "deleted cluster", lookupErr: db.ErrNotFound},
		{name: "wrapped not found", lookupErr: fmt.Errorf("cluster lookup: %w", db.ErrNotFound)},
		{name: "transient database error", lookupErr: errors.New("database unavailable"), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, test.lookupErr).Once()
			a := &Activities{db: database}

			_, err := a.FailCertificateRenewal(context.Background(), FailCertificateRenewalInput{
				ClusterID: clusterID, CycleID: "cycle-1", Phase: "complete", Message: "cluster deleted",
			})

			if test.wantError {
				require.ErrorIs(t, err, test.lookupErr)
				var applicationErr *temporal.ApplicationError
				require.False(t, errors.As(err, &applicationErr) && applicationErr.NonRetryable())
			} else {
				require.NoError(t, err)
			}
			database.AssertExpectations(t)
			tx.AssertExpectations(t)
			tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

func TestTruncateCertificateRenewalMessagePreservesValidUTF8(t *testing.T) {
	message := "  " + strings.Repeat(string(rune(0x754c)), certificateRenewalFailureMessageLimit+1) + "  "

	got := truncateCertificateRenewalMessage(message)

	require.Equal(t, certificateRenewalFailureMessageLimit, utf8.RuneCountInString(got))
	require.True(t, utf8.ValidString(got))
	require.NotContains(t, got, " ")
}

func TestValidateCertificateRenewalHealthRequiresCanonicalSafetySignals(t *testing.T) {
	clusterID := uuid.New()
	nodes := []db.Node{
		healthyCertificateRenewalNode(clusterID, "cp-0"),
		healthyCertificateRenewalNode(clusterID, "cp-1"),
		healthyCertificateRenewalNode(clusterID, "cp-2"),
	}
	postflightNodeID := nodes[0].ID
	postflightNodeName := nodes[0].Name
	expected := []uuid.UUID{nodes[0].ID, nodes[1].ID, nodes[2].ID}
	objects := make([]client.Object, 0, 7)
	for _, node := range nodes {
		objects = append(objects, readyKubernetesNode(node.Name))
	}
	for _, component := range certificateRenewalMirrorComponents {
		objects = append(objects, readyMirrorPod(component+"-"+postflightNodeName))
	}
	a, database := certificateRenewalHealthActivities(t, clusterID, nodes, objects, func(db.Node) (*nvkeproto.ExecuteScriptResponse, error) {
		return &nvkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[+]etcd ok\nreadyz check passed", ExitCode: 0}, nil
	})

	err := a.ValidateCertificateRenewalHealth(context.Background(), ValidateCertificateRenewalHealthInput{
		ClusterID: clusterID, ExpectedNodeIDs: expected, PostflightNodeID: &postflightNodeID,
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
}

var certificateRenewalMirrorComponents = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}

// TestValidateCertificateRenewalHealthAllowsSingleControlPlane shows that a lone
// control-plane node is exempt from the lose-one-voter rule while every other
// gate, including postflight mirror pods, still applies.
func TestValidateCertificateRenewalHealthAllowsSingleControlPlane(t *testing.T) {
	clusterID := uuid.New()
	node := healthyCertificateRenewalNode(clusterID, "cp-0")
	objects := make([]client.Object, 0, 5)
	objects = append(objects, readyKubernetesNode(node.Name))
	for _, component := range certificateRenewalMirrorComponents {
		objects = append(objects, readyMirrorPod(component+"-"+node.Name))
	}
	tests := []struct {
		name             string
		postflightNodeID *uuid.UUID
	}{
		{name: "preflight"},
		{name: "postflight", postflightNodeID: &node.ID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, database := certificateRenewalHealthActivities(t, clusterID, []db.Node{node}, objects, func(db.Node) (*nvkeproto.ExecuteScriptResponse, error) {
				return &nvkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[+]etcd ok\nreadyz check passed", ExitCode: 0}, nil
			})

			err := a.ValidateCertificateRenewalHealth(context.Background(), ValidateCertificateRenewalHealthInput{
				ClusterID: clusterID, ExpectedNodeIDs: []uuid.UUID{node.ID}, PostflightNodeID: test.postflightNodeID,
			})

			require.NoError(t, err)
			database.AssertExpectations(t)
		})
	}
}

// TestValidateCertificateRenewalHealthFailsClosed covers unsafe voter capacity,
// membership drift and readiness disagreement; DB readiness alone cannot authorize
// taking another control-plane node out of service.
func TestValidateCertificateRenewalHealthFailsClosed(t *testing.T) {
	clusterID := uuid.New()
	healthyNodes := []db.Node{
		healthyCertificateRenewalNode(clusterID, "cp-0"),
		healthyCertificateRenewalNode(clusterID, "cp-1"),
		healthyCertificateRenewalNode(clusterID, "cp-2"),
	}
	tests := []struct {
		name      string
		nodes     []db.Node
		expected  []uuid.UUID
		objects   []client.Object
		readyz    func(db.Node) (*nvkeproto.ExecuteScriptResponse, error)
		wantError string
	}{
		{
			name:      "two voters cannot tolerate one unavailable member",
			nodes:     healthyNodes[:2],
			expected:  []uuid.UUID{healthyNodes[0].ID, healthyNodes[1].ID},
			wantError: "cannot tolerate one unavailable",
		},
		{
			name: "single control-plane node is not Ready in the database",
			nodes: func() []db.Node {
				node := healthyNodes[0]
				node.Conditions.KubernetesReady.Status = db.ConditionStatusFalse
				return []db.Node{node}
			}(),
			expected:  []uuid.UUID{healthyNodes[0].ID},
			wantError: "not healthy",
		},
		{
			name:      "node set changed",
			nodes:     healthyNodes,
			expected:  []uuid.UUID{healthyNodes[0].ID, healthyNodes[1].ID, uuid.New()},
			wantError: "node set changed",
		},
		{
			name: "database Kubernetes readiness is false",
			nodes: func() []db.Node {
				copyNodes := append([]db.Node(nil), healthyNodes...)
				copyNodes[1].Conditions.KubernetesReady.Status = db.ConditionStatusFalse
				return copyNodes
			}(),
			expected:  []uuid.UUID{healthyNodes[0].ID, healthyNodes[1].ID, healthyNodes[2].ID},
			wantError: "not healthy",
		},
		{
			name:     "active readyz etcd check fails",
			nodes:    healthyNodes,
			expected: []uuid.UUID{healthyNodes[0].ID, healthyNodes[1].ID, healthyNodes[2].ID},
			objects:  []client.Object{readyKubernetesNode("cp-0"), readyKubernetesNode("cp-1"), readyKubernetesNode("cp-2")},
			readyz: func(node db.Node) (*nvkeproto.ExecuteScriptResponse, error) {
				if node.Name == "cp-1" {
					return &nvkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[-]etcd failed", ExitCode: 1}, nil
				}
				return &nvkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[+]etcd ok", ExitCode: 0}, nil
			},
			wantError: "2/3 control-plane nodes healthy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, _ := certificateRenewalHealthActivities(t, clusterID, test.nodes, test.objects, test.readyz)
			err := a.ValidateCertificateRenewalHealth(context.Background(), ValidateCertificateRenewalHealthInput{
				ClusterID: clusterID, ExpectedNodeIDs: test.expected,
			})
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func certificateRenewalHealthActivities(
	t *testing.T,
	clusterID uuid.UUID,
	nodes []db.Node,
	objects []client.Object,
	readyz func(db.Node) (*nvkeproto.ExecuteScriptResponse, error),
) (*Activities, *testutil.MockDB) {
	t.Helper()
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil).Once()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	if readyz == nil {
		readyz = func(db.Node) (*nvkeproto.ExecuteScriptResponse, error) {
			return nil, errors.New("unexpected readyz call")
		}
	}
	fakeAgent := &agenttest.FakeClient{ExecuteScriptHook: func(node db.Node, _, _ string) (*nvkeproto.ExecuteScriptResponse, error) {
		return readyz(node)
	}}
	return &Activities{
		db:                   database,
		nodeAgentClient:      fakeAgent,
		tenantClientProvider: &certificateRenewalTenantClientProvider{tenantClient: tenantClient},
	}, database
}

type certificateRenewalTenantClientProvider struct {
	tenantClient client.Client
}

func (p *certificateRenewalTenantClientProvider) ClientForDBCluster(context.Context, db.Cluster) (client.Client, error) {
	return p.tenantClient, nil
}

func healthyCertificateRenewalNode(clusterID uuid.UUID, name string) db.Node {
	return db.Node{
		ID: uuid.New(), ClusterID: clusterID, Name: name, IP: "192.0.2.1", ControlPlane: true,
		Conditions: db.NodeConditions{
			Joined:          db.Condition{Status: db.ConditionStatusTrue},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
		},
	}
}

func readyKubernetesNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}
}

func readyMirrorPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// TestFailCertificateRenewalRetainsUnresolvedOwnershipAndIsIdempotent protects the
// first failure's ownership decision from activity redelivery with different
// inputs, including accidental release of an unresolved operation's lock.
func TestFailCertificateRenewalRetainsUnresolvedOwnershipAndIsIdempotent(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprintf("retain=%t", retain), func(t *testing.T) {
			clusterID, nodeID := uuid.New(), uuid.New()
			due := time.Now().Add(-time.Hour).UTC()
			cluster := db.Cluster{ID: clusterID, NextCertRenewalDue: &due, Conditions: db.ClusterConditions{
				UpdateInProgress:   &db.UpdateInProgress{Type: db.OperationTypeCertRenewal},
				CertificateRenewal: &db.CertificateRenewalStatus{CycleID: "cycle-1", State: db.CertificateRenewalStateAccepted},
			}}
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Twice()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
			tx.On("UpdateCluster", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
				cluster = *args.Get(1).(*db.Cluster)
			}).Return(db.Cluster{}, nil).Once()
			a := &Activities{db: database}
			input := FailCertificateRenewalInput{
				ClusterID: clusterID, CycleID: "cycle-1", FailedNodeID: &nodeID,
				Phase: "renew-node", Message: "termination unconfirmed", RetainMutationLock: retain,
			}

			_, err := a.FailCertificateRenewal(t.Context(), input)
			require.NoError(t, err)
			// Re-delivery cannot revise a failed cycle's ownership decision.
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
			input.RetainMutationLock = !retain
			_, err = a.FailCertificateRenewal(t.Context(), input)

			require.NoError(t, err)
			require.Equal(t, retain, cluster.Conditions.UpdateInProgress != nil)
			require.Equal(t, db.CertificateRenewalStateFailed, cluster.Conditions.CertificateRenewal.State)
			require.NotNil(t, cluster.Conditions.CertificateRenewal.CompletedAt)
			require.Equal(t, &nodeID, cluster.Conditions.CertificateRenewal.FailedNodeID)
			require.Equal(t, due, *cluster.NextCertRenewalDue)
			database.AssertExpectations(t)
			tx.AssertExpectations(t)
		})
	}
}

// TestFailCertificateRenewalCannotClearAnotherOwner prevents delayed cleanup from
// overwriting a newer cycle or releasing a different cluster mutation's lock.
func TestFailCertificateRenewalCannotClearAnotherOwner(t *testing.T) {
	for _, tt := range []struct {
		name          string
		cycleID       string
		state         db.CertificateRenewalState
		operationType db.OperationType
	}{
		{name: "new renewal cycle", cycleID: "cycle-2", state: db.CertificateRenewalStateAccepted, operationType: db.OperationTypeCertRenewal},
		{name: "another mutation", cycleID: "cycle-1", state: db.CertificateRenewalStateAccepted, operationType: db.OperationTypeEtcdEncryption},
		{name: "completed cycle", cycleID: "cycle-1", state: db.CertificateRenewalStateSucceeded, operationType: db.OperationTypeEtcdEncryption},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Conditions: db.ClusterConditions{
				UpdateInProgress:   &db.UpdateInProgress{Type: tt.operationType},
				CertificateRenewal: &db.CertificateRenewalStatus{CycleID: tt.cycleID, State: tt.state},
			}}, nil).Once()
			a := &Activities{db: database}

			_, err := a.FailCertificateRenewal(t.Context(), FailCertificateRenewalInput{ClusterID: clusterID, CycleID: "cycle-1"})

			require.Error(t, err)
			tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			tx.AssertExpectations(t)
		})
	}
}

// TestBeginCertificateRenewalRecordsClusterStateRefusals records each refusal
// as a failed cycle without the mutation lock, so the controller backs off
// instead of dispatching the same cycle every minute. Two voters get their own
// phase, which the controller does not retry while the desired count stays two.
func TestBeginCertificateRenewalRecordsClusterStateRefusals(t *testing.T) {
	for _, test := range []struct {
		reason    string
		wantPhase string
	}{
		{"agent upgrade", db.CertificateRenewalPhaseAdmission},
		{"node renewal reservation", db.CertificateRenewalPhaseAdmission},
		{"control-plane upgrade", db.CertificateRenewalPhaseAdmission},
		{"repair", db.CertificateRenewalPhaseAdmission},
		{"recycling", db.CertificateRenewalPhaseAdmission},
		{"delete", db.CertificateRenewalPhaseAdmission},
		{"unjoined", db.CertificateRenewalPhaseAdmission},
		{"two voters", db.CertificateRenewalPhaseTwoControlPlaneVoters},
	} {
		t.Run(test.reason, func(t *testing.T) {
			clusterID := uuid.New()
			due := time.Now().Add(-time.Hour).UTC()
			cluster := db.Cluster{ID: clusterID, Conditions: db.ClusterConditions{
				Provisioned: db.NewCondition(true, ""),
				CertificateRenewal: &db.CertificateRenewalStatus{
					CycleID: "previous-cycle", State: db.CertificateRenewalStateSucceeded, Outcome: db.CertificateRenewalOutcomeRenewed,
				},
			}}
			node := db.Node{IP: "10.0.0.1", ControlPlane: true, Conditions: db.NodeConditions{
				Joined: db.NewCondition(true, ""), Provisioned: db.NewCondition(true, ""),
			}}
			nodes := []db.Node{node}
			switch test.reason {
			case "agent upgrade":
				cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: uuid.New()}
			case "node renewal reservation":
				cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: uuid.New(), OperationID: "earlier-cycle", Fenced: true}
			case "control-plane upgrade":
				cluster.Conditions.ControlPlaneUpgrading = db.NewCondition(true, "")
			case "repair":
				nodes[0].Conditions.MarkedForRepair = db.NewCondition(true, "")
			case "recycling":
				nodes[0].Conditions.MarkedForRecycling = db.NewCondition(true, "")
			case "delete":
				nodes[0].Conditions.DeleteRequested = db.NewCondition(true, "")
			case "unjoined":
				nodes[0].Conditions.Joined = db.NewCondition(false, "")
			case "two voters":
				nodes = append(nodes, node)
			}
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
			if test.reason != "agent upgrade" && test.reason != "node renewal reservation" && test.reason != "control-plane upgrade" {
				tx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Once()
				tx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
			}
			tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(updated *db.Cluster) bool {
				status := updated.Conditions.CertificateRenewal
				return updated.Conditions.UpdateInProgress == nil && updated.NextCertRenewalDue == nil &&
					status != nil && status.CycleID == "new-cycle" && status.State == db.CertificateRenewalStateFailed &&
					status.CompletedAt != nil && status.Phase == test.wantPhase && status.Message != "" &&
					status.ScheduledFor != nil && status.ScheduledFor.Equal(due) && status.Outcome == ""
			})).Return(db.Cluster{}, nil).Once()
			a := &Activities{db: database}

			_, err := a.BeginCertificateRenewal(t.Context(), BeginCertificateRenewalInput{ClusterID: clusterID, CycleID: "new-cycle", ScheduledFor: &due})

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			require.Equal(t, "CertificateRenewalPrecondition", appErr.Type())
			require.True(t, appErr.NonRetryable())
			tx.AssertExpectations(t)
			database.AssertExpectations(t)
		})
	}
}

// TestBeginCertificateRenewalLeavesOwnedStateUntouched does not record refusals
// that concern another owner or this cycle's own earlier record.
func TestBeginCertificateRenewalLeavesOwnedStateUntouched(t *testing.T) {
	for _, test := range []struct {
		name       string
		conditions db.ClusterConditions
	}{
		{"another operation holds the lock", db.ClusterConditions{
			Provisioned:      db.NewCondition(true, ""),
			UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption},
		}},
		{"cycle already failed", db.ClusterConditions{
			Provisioned:        db.NewCondition(true, ""),
			CertificateRenewal: &db.CertificateRenewalStatus{CycleID: "new-cycle", State: db.CertificateRenewalStateFailed},
		}},
		{"cluster is deleting", db.ClusterConditions{
			Provisioned: db.NewCondition(true, ""),
			Deleting:    db.NewCondition(true, ""),
		}},
		{"cluster is not provisioned", db.ClusterConditions{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Conditions: test.conditions}, nil).Once()
			a := &Activities{db: database}

			_, err := a.BeginCertificateRenewal(t.Context(), BeginCertificateRenewalInput{ClusterID: clusterID, CycleID: "new-cycle"})

			require.Error(t, err)
			tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			tx.AssertExpectations(t)
		})
	}
}
