// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

type rewriteCompletionPersister struct {
	*JSONFilePersister
	completed chan error
}

func (p *rewriteCompletionPersister) Persist(snapshot *NodeStatus) error {
	err := p.JSONFilePersister.Persist(snapshot)
	if k8sAPIRewriteTerminal(snapshot.K8sAPIRewrite.State) {
		p.completed <- err
		close(p.completed)
	}
	return err
}

func TestRewriteRecovery_RestartsPersistedRunningOperationOnce(t *testing.T) {
	const operationID = "acknowledged-before-agent-restart"
	persister := &rewriteCompletionPersister{
		JSONFilePersister: &JSONFilePersister{Path: filepath.Join(t.TempDir(), "state.json")},
		completed:         make(chan error, 1),
	}
	// This is exactly the durable state left by an acknowledged Start when its
	// process is lost. Loading it creates a new server with no live goroutine.
	require.NoError(t, persister.Persist(&NodeStatus{Role: ControlPlaneRole, K8sAPIRewrite: newRewriteOperation(operationID)}))
	loaded, err := persister.LoadFromStateFile()
	require.NoError(t, err)
	srv := &NodeAgentServer{NodeStatus: loaded, Persister: persister}
	require.False(t, srv.rewriteActive)

	nodes := kubernetesfake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var traversals atomic.Int32
	nodes.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if traversals.Add(1) == 1 {
			close(entered)
		}
		<-release
		return false, nil, nil
	})
	k8s.SetClientsetForTest(nodes)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "preexisting", Namespace: "default"},
		Data:       map[string][]byte{"fixture": []byte("public-fixture")},
	}
	dyn := dynamicfake.NewSimpleDynamicClient(clientscheme.Scheme, secret)
	k8s.SetDynamicClientForTest(dyn)
	recoveryRequested := false
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if recoveryRequested {
			select {
			case <-persister.completed:
			case <-time.After(5 * time.Second):
				t.Error("recovery goroutine did not stop before client cleanup")
			}
		}
		k8s.SetDynamicClientForTest(nil)
		k8s.SetClientsetForTest(nil)
	}()

	// Get is observational, so the controller must send same-ID Start while
	// polling. Neither a new reset-run ID nor a terminal replacement is needed.
	for range 3 {
		resp, getErr := srv.GetEtcdEncryptionAtRestObjectsRewriteStatus(t.Context(), &pb.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest{OperationId: operationID})
		require.NoError(t, getErr)
		require.Equal(t, pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING, resp.Operation.State)
		_, active := rewriteStateSnapshot(srv)
		require.False(t, active)
	}

	type startResult struct {
		response *pb.StartEtcdEncryptionAtRestObjectsRewriteResponse
		err      error
	}
	const polls = 16
	results := make(chan startResult, polls)
	recoveryRequested = true
	for range polls {
		go func() {
			resp, startErr := srv.StartEtcdEncryptionAtRestObjectsRewrite(context.Background(), &pb.StartEtcdEncryptionAtRestObjectsRewriteRequest{OperationId: operationID})
			results <- startResult{response: resp, err: startErr}
		}()
	}
	for range polls {
		select {
		case result := <-results:
			require.NoError(t, result.err)
			require.Equal(t, operationID, result.response.GetOperation().GetOperationId())
			require.Equal(t, pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING, result.response.GetOperation().GetState())
		case <-time.After(5 * time.Second):
			t.Fatal("same-ID recovery Start did not return")
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered rewrite did not start")
	}
	require.EqualValues(t, 1, traversals.Load(), "concurrent polling must launch only one rewrite")
	releaseOnce.Do(func() { close(release) })
	select {
	case persistErr := <-persister.completed:
		require.NoError(t, persistErr)
	case <-time.After(5 * time.Second):
		t.Fatal("recovered rewrite did not persist completion")
	}

	completed, err := persister.LoadFromStateFile()
	require.NoError(t, err)
	require.Equal(t, operationID, completed.K8sAPIRewrite.ID)
	require.Equal(t, K8sAPIRewriteStateSucceeded, completed.K8sAPIRewrite.State)
	updates := 0
	for _, action := range dyn.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "secrets" {
			updates++
		}
	}
	require.Equal(t, 1, updates, "the preexisting object must actually be rewritten exactly once")
	gotNode, err := nodes.CoreV1().Nodes().Get(t.Context(), "cp-0", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "true", gotNode.Labels["nvke.nvidia.com/k8s-api-encryption-rewrite-complete"])

	// Late duplicate polling remains a terminal no-op.
	for range 3 {
		resp, startErr := srv.StartEtcdEncryptionAtRestObjectsRewrite(t.Context(), &pb.StartEtcdEncryptionAtRestObjectsRewriteRequest{OperationId: operationID})
		require.NoError(t, startErr)
		require.Equal(t, pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED, resp.GetOperation().GetState())
	}
	require.EqualValues(t, 1, traversals.Load())
}
