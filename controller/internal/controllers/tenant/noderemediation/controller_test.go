// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderemediation

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUnit_NodeRequestsForDBNodeUsesNodeName(t *testing.T) {
	got, err := nodeRequestsForDBNode(context.Background(), db.Node{Name: "node-a"})
	require.NoError(t, err)
	require.Empty(t, got[0].Namespace)
	require.Equal(t, "node-a", got[0].Name)

	got, err = nodeRequestsForDBNode(context.Background(), db.Node{})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestUnit_RemediationNodePredicateOnlyMatchesRequestChanges(t *testing.T) {
	p := remediationNodePredicate()
	oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-1",
		Labels: map[string]string{
			constants.LabelKeyNodeLabelReboot: "codex-smoke",
		},
	}}
	statusOnlyNode := oldNode.DeepCopy()
	statusOnlyNode.Labels[constants.LabelKeyNodeLabelRebootStatus] = "InProgress"
	requestChangedNode := oldNode.DeepCopy()
	requestChangedNode.Labels[constants.LabelKeyNodeLabelReboot] = "operator-request"

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: requestChangedNode}))
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: statusOnlyNode}))
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}}))
}

func TestUnit_ReconcileStartsNodeRemediationWorkflows(t *testing.T) {
	tests := map[string]struct {
		labels      map[string]string
		annotations map[string]string
		wantPrefix  string
		wantInput   func(t *testing.T, arg interface{}, nodeID uuid.UUID)
	}{
		"reboot label starts reboot workflow": {
			labels:     map[string]string{constants.LabelKeyNodeLabelReboot: "operator-request"},
			wantPrefix: constants.RebootNodeWorkflowPrefix,
			wantInput: func(t *testing.T, arg interface{}, nodeID uuid.UUID) {
				t.Helper()
				input, ok := arg.(workflows.RebootNodeInput)
				require.True(t, ok)
				require.Equal(t, nodeID, input.NodeID)
				require.Equal(t, "operator-request", input.Reason)
				require.Equal(t, time.Minute, input.InitialTimeout)
				require.Equal(t, 2*time.Minute, input.TimeoutIncrement)
				require.Equal(t, 5*time.Minute, input.MaxTimeout)
				require.Equal(t, 10*time.Second, input.PollingInterval)
			},
		},
		"terminate label starts teardown workflow": {
			labels:     map[string]string{constants.LabelKeyNodeLabelTerminate: "replace-node"},
			wantPrefix: constants.TeardownNodeWorkflowPrefix,
			wantInput: func(t *testing.T, arg interface{}, nodeID uuid.UUID) {
				t.Helper()
				input, ok := arg.(workflows.TeardownNodeInput)
				require.True(t, ok)
				require.Equal(t, nodeID, input.NodeID)
				require.Equal(t, "replace-node", input.Reason)
				require.False(t, input.SkipRemoveFromCluster)
				require.Nil(t, input.MachineHealthIssue)
			},
		},
		"terminate label includes machine health issue": {
			labels: map[string]string{constants.LabelKeyNodeLabelTerminate: "replace-node"},
			annotations: map[string]string{
				constants.NodeAnnotationKeyMachineHealthIssue: `{"category":"Hardware","summary":"GPU failure","details":"xid errors"}`,
			},
			wantPrefix: constants.TeardownNodeWorkflowPrefix,
			wantInput: func(t *testing.T, arg interface{}, nodeID uuid.UUID) {
				t.Helper()
				input, ok := arg.(workflows.TeardownNodeInput)
				require.True(t, ok)
				require.Equal(t, nodeID, input.NodeID)
				require.Equal(t, "replace-node", input.Reason)
				require.NotNil(t, input.MachineHealthIssue)
				require.Equal(t, "Hardware", input.MachineHealthIssue.Category)
				require.Equal(t, "GPU failure", input.MachineHealthIssue.Summary)
				require.NotNil(t, input.MachineHealthIssue.Details)
				require.Equal(t, "xid errors", *input.MachineHealthIssue.Details)
			},
		},
		"terminate label wins over reboot label": {
			labels: map[string]string{
				constants.LabelKeyNodeLabelReboot:    "operator-request",
				constants.LabelKeyNodeLabelTerminate: "replace-node",
			},
			wantPrefix: constants.TeardownNodeWorkflowPrefix,
			wantInput: func(t *testing.T, arg interface{}, nodeID uuid.UUID) {
				t.Helper()
				input, ok := arg.(workflows.TeardownNodeInput)
				require.True(t, ok)
				require.Equal(t, nodeID, input.NodeID)
				require.Equal(t, "replace-node", input.Reason)
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			clusterID := uuid.New()
			nodeID := uuid.New()
			nodeName := "worker-1"
			dbNode := db.Node{ID: nodeID, ClusterID: clusterID, Name: nodeName}
			kubeNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Labels:      params.labels,
					Annotations: params.annotations,
				},
			}

			database := &testutil.MockDB{}
			database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
				return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
					filters.Name != nil && *filters.Name == nodeName
			})).Return([]db.Node{dbNode}, nil).Once()

			workflowClient := &recordingWorkflowStarter{}
			reconciler := newTestReconciler(t, clusterID, database, workflowClient, kubeNode)

			result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
			require.NoError(t, err)
			require.Empty(t, result)
			database.AssertExpectations(t)
			require.Len(t, workflowClient.calls, 1)
			call := workflowClient.calls[0]
			require.Equal(t, params.wantPrefix+nodeID.String(), call.options.ID)
			require.Equal(t, "test-task-queue", call.options.TaskQueue)
			require.Equal(t, enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING, call.options.WorkflowIDConflictPolicy)
			require.Equal(t, enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE, call.options.WorkflowIDReusePolicy)
			require.Len(t, call.args, 1)
			params.wantInput(t, call.args[0], nodeID)
		})
	}
}

func TestUnit_ReconcileSkipsNodeWithoutRemediationLabels(t *testing.T) {
	clusterID := uuid.New()
	nodeID := uuid.New()
	nodeName := "worker-1"
	dbNode := db.Node{ID: nodeID, ClusterID: clusterID, Name: nodeName}
	kubeNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}

	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{dbNode}, nil).Once()

	workflowClient := &recordingWorkflowStarter{}
	reconciler := newTestReconciler(t, clusterID, database, workflowClient, kubeNode)

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, result)
	database.AssertExpectations(t)
	require.Empty(t, workflowClient.calls)
}

func TestUnit_ReconcileSkipsDeletedKubernetesNode(t *testing.T) {
	clusterID := uuid.New()
	nodeID := uuid.New()
	nodeName := "worker-1"
	dbNode := db.Node{ID: nodeID, ClusterID: clusterID, Name: nodeName}

	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{dbNode}, nil).Once()

	workflowClient := &recordingWorkflowStarter{}
	reconciler := newTestReconciler(t, clusterID, database, workflowClient)

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, result)
	database.AssertExpectations(t)
	require.Empty(t, workflowClient.calls)
}

func newTestReconciler(t *testing.T, clusterID uuid.UUID, database *testutil.MockDB, workflowClient *recordingWorkflowStarter, kubeObjects ...runtime.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return &Reconciler{
		clusterID: clusterID,
		db:        dbwrapper.New(database, slog.New(slog.DiscardHandler)),
		client:    fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(kubeObjects...).Build(),
		remediator: newNodeRemediator(config.Config{
			TemporalTaskQueue:          "test-task-queue",
			NodeRebootInitialTimeout:   time.Minute,
			NodeRebootTimeoutIncrement: 2 * time.Minute,
			NodeRebootMaxTimeout:       5 * time.Minute,
			NodeRebootPollingInterval:  10 * time.Second,
		}, slog.New(slog.DiscardHandler), workflowClient),
	}
}

type workflowCall struct {
	options client.StartWorkflowOptions
	args    []interface{}
}

type recordingWorkflowStarter struct {
	calls []workflowCall
}

func (r *recordingWorkflowStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	r.calls = append(r.calls, workflowCall{
		options: options,
		args:    append([]interface{}(nil), args...),
	})
	return mockWorkflowRun{id: options.ID, runID: "run"}, nil
}

type mockWorkflowRun struct {
	id    string
	runID string
}

func (m mockWorkflowRun) GetID() string {
	return m.id
}

func (m mockWorkflowRun) GetRunID() string {
	return m.runID
}

func (m mockWorkflowRun) GetFirstExecutionRunID() string {
	return m.runID
}

func (m mockWorkflowRun) Get(_ context.Context, _ interface{}) error {
	return nil
}

func (m mockWorkflowRun) GetWithOptions(_ context.Context, _ interface{}, _ client.WorkflowRunGetOptions) error {
	return nil
}
