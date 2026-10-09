// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/dsx-ai-factory/nke/controller/api"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClusterAPI replays a scripted sequence of cluster states, so a wait that
// takes several polls, one that fails, and one that never finishes are all
// reachable in a unit test.
type fakeClusterAPI struct {
	created   *api.CreateClusterJSONRequestBody
	createErr error

	states  []api.Cluster
	getErrs []error
	gets    int

	// nodeStates is replayed the same way states is: one control-plane node
	// list per poll, the last entry repeating once the script runs out.
	nodeStates [][]api.Node
	listErrs   []error
	lists      int
}

func (f *fakeClusterAPI) CreateCluster(
	_ context.Context,
	req api.CreateClusterJSONRequestBody,
) (*api.Cluster, error) {
	f.created = &req
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &api.Cluster{Name: req.Name, ID: uuid.New()}, nil
}

func (f *fakeClusterAPI) GetCluster(_ context.Context, _ openapi_types.UUID) (*api.Cluster, error) {
	idx := f.gets
	f.gets++
	if idx < len(f.getErrs) && f.getErrs[idx] != nil {
		return nil, f.getErrs[idx]
	}
	if idx >= len(f.states) {
		idx = len(f.states) - 1
	}
	cluster := f.states[idx]
	return &cluster, nil
}

func (f *fakeClusterAPI) ListControlPlaneNodes(
	_ context.Context,
	_ openapi_types.UUID,
) ([]api.Node, error) {
	idx := f.lists
	f.lists++
	if errIdx := min(idx, len(f.listErrs)-1); errIdx >= 0 && f.listErrs[errIdx] != nil {
		return nil, f.listErrs[errIdx]
	}
	if len(f.nodeStates) == 0 {
		return nil, nil
	}
	if idx >= len(f.nodeStates) {
		idx = len(f.nodeStates) - 1
	}
	return f.nodeStates[idx], nil
}

func condition(status api.ConditionStatus) api.Condition {
	return api.Condition{Status: status}
}

func clusterWith(provisioned, provisioning api.ConditionStatus) api.Cluster {
	return api.Cluster{
		Name: "nke-site",
		Conditions: api.ClusterConditions{
			Provisioned:  condition(provisioned),
			Provisioning: condition(provisioning),
			Deleting:     condition(api.ConditionStatusFalse),
		},
	}
}

func shortPollInterval(t *testing.T) {
	t.Helper()
	old := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = old })
}

func TestCreate_SendsTheSiteSpecAsACluster(t *testing.T) {
	client := &fakeClusterAPI{}

	cluster, err := Create(context.Background(), client, testSiteSpec(), quietLogger())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if cluster.Name != "nke-site" {
		t.Errorf("created cluster name = %q, want %q", cluster.Name, "nke-site")
	}
	if client.created == nil || client.created.Infra == nil || client.created.Infra.Cluster == nil {
		t.Fatal("Create did not send a CAPI infrastructure cluster")
	}
}

func TestCreate_PropagatesTheAPIError(t *testing.T) {
	client := &fakeClusterAPI{createErr: errors.New("400 Bad Request: orgID must be omitted")}

	_, err := Create(context.Background(), client, testSiteSpec(), quietLogger())
	if err == nil {
		t.Fatal("expected Create to propagate the API error")
	}
	if !strings.Contains(err.Error(), "orgID must be omitted") {
		t.Errorf("error %q does not carry what the controller said", err)
	}
}

// TestWaitProvisioned_ReturnsWhenProvisioned: the cluster is still provisioning
// on the first polls, which is the normal case — a bootstrap takes minutes.
func TestWaitProvisioned_ReturnsWhenProvisioned(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{states: []api.Cluster{
		clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue),
		clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue),
		clusterWith(api.ConditionStatusTrue, api.ConditionStatusFalse),
	}}

	cluster, err := WaitProvisioned(context.Background(), client, uuid.New(), time.Minute, quietLogger())
	if err != nil {
		t.Fatalf("WaitProvisioned returned error: %v", err)
	}
	if cluster.Name != "nke-site" {
		t.Errorf("returned cluster = %q, want the polled one", cluster.Name)
	}
	if client.gets != 3 {
		t.Errorf("polled %d times, want 3", client.gets)
	}
}

// TestWaitProvisioned_SurvivesAFailedRead: the controller restarting mid-bootstrap is a
// read failure, not a cluster failure. Giving up on it would fail a bootstrap
// that is going fine.
func TestWaitProvisioned_SurvivesAFailedRead(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{
		getErrs: []error{errors.New("connection refused"), nil},
		states: []api.Cluster{
			clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue),
			clusterWith(api.ConditionStatusTrue, api.ConditionStatusFalse),
		},
	}

	if _, err := WaitProvisioned(context.Background(), client, uuid.New(), time.Minute, quietLogger()); err != nil {
		t.Fatalf("WaitProvisioned gave up on a transient read failure: %v", err)
	}
}

// TestWaitProvisioned_StopsOnADeletingCluster: a cluster that starts deleting
// is never going to report provisioned, so waiting out the full timeout would
// hide the real answer for half an hour.
func TestWaitProvisioned_StopsOnADeletingCluster(t *testing.T) {
	shortPollInterval(t)
	deleting := clusterWith(api.ConditionStatusFalse, api.ConditionStatusFalse)
	deleting.Conditions.Deleting = condition(api.ConditionStatusTrue)
	client := &fakeClusterAPI{states: []api.Cluster{deleting}}

	_, err := WaitProvisioned(context.Background(), client, uuid.New(), time.Minute, quietLogger())
	if !errors.Is(err, ErrClusterFailed) {
		t.Fatalf("error = %v, want it to identify as ErrClusterFailed", err)
	}
}

// TestWaitProvisioned_StopsOnProvisioningFailed: the controller reports a dead
// provisioning workflow as Provisioning=False with the cause in the message.
func TestWaitProvisioned_StopsOnProvisioningFailed(t *testing.T) {
	shortPollInterval(t)
	failed := clusterWith(api.ConditionStatusFalse, api.ConditionStatusFalse)
	failed.Conditions.Provisioning.Message = "Workflow failed: kubeadm init failed"
	client := &fakeClusterAPI{states: []api.Cluster{failed}}

	_, err := WaitProvisioned(context.Background(), client, uuid.New(), time.Minute, quietLogger())
	if !errors.Is(err, ErrClusterFailed) {
		t.Fatalf("error = %v, want it to identify as ErrClusterFailed", err)
	}
	if !strings.Contains(err.Error(), "kubeadm init failed") {
		t.Errorf("error %q does not carry the condition's message", err)
	}
}

func TestWaitProvisioned_StopsOnUpdateFailed(t *testing.T) {
	shortPollInterval(t)
	failed := clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue)
	failed.Conditions.UpdateFailed = &api.Condition{
		Status:  api.ConditionStatusTrue,
		Message: "node1 never joined",
	}
	client := &fakeClusterAPI{states: []api.Cluster{failed}}

	_, err := WaitProvisioned(context.Background(), client, uuid.New(), time.Minute, quietLogger())
	if !errors.Is(err, ErrClusterFailed) {
		t.Fatalf("error = %v, want it to identify as ErrClusterFailed", err)
	}
	if !strings.Contains(err.Error(), "node1 never joined") {
		t.Errorf("error %q does not carry the condition's message", err)
	}
}

// TestWaitProvisioned_TimesOutSayingWhereItGotTo: the last state is the whole
// value of the message. "Timed out" alone does not distinguish a cluster stuck
// provisioning from one that was never reachable.
func TestWaitProvisioned_TimesOutSayingWhereItGotTo(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{states: []api.Cluster{
		clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue),
	}}

	_, err := WaitProvisioned(context.Background(), client, uuid.New(), 5*time.Millisecond, quietLogger())
	if err == nil {
		t.Fatal("expected WaitProvisioned to time out")
	}
	if !strings.Contains(err.Error(), "Provisioning") {
		t.Errorf("error %q does not say what the cluster was last doing", err)
	}
}

// nodesProvisioned renders a control-plane node list in which the first
// provisioned nodes are done and the rest are still being worked on, which is
// exactly the shape of a serialized join in progress.
func nodesProvisioned(total, provisioned int) []api.Node {
	nodes := make([]api.Node, 0, total)
	for i := range total {
		status, message := api.ConditionStatusFalse, "Joining node"
		if i < provisioned {
			status, message = api.ConditionStatusTrue, "Provisioned node"
		}
		nodes = append(nodes, api.Node{
			Name:         fmt.Sprintf("node%d", i+1),
			ControlPlane: true,
			Conditions: api.NodeConditions{
				Provisioned: api.Condition{Status: status, Message: message},
			},
		})
	}
	return nodes
}

// TestWaitControlPlaneJoined_ReturnsWhenEveryNodeIsProvisioned: the nodes join
// one at a time, so a three-node control plane is reached a node per poll.
func TestWaitControlPlaneJoined_ReturnsWhenEveryNodeIsProvisioned(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{nodeStates: [][]api.Node{
		nodesProvisioned(3, 1),
		nodesProvisioned(3, 2),
		nodesProvisioned(3, 3),
	}}

	if err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 3, time.Minute, quietLogger(),
	); err != nil {
		t.Fatalf("WaitControlPlaneJoined returned error: %v", err)
	}
	if client.lists != 3 {
		t.Errorf("polled %d times, want 3", client.lists)
	}
}

// TestWaitControlPlaneJoined_DoesNotReturnOnTheFirstNodeAlone is the regression
// test for the bug this wait exists to close: the cluster reports Provisioned
// once its initial node is up, and returning there tore down the workflow
// engine the other two nodes were still joining through.
func TestWaitControlPlaneJoined_DoesNotReturnOnTheFirstNodeAlone(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{nodeStates: [][]api.Node{nodesProvisioned(3, 1)}}

	err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 3, 5*time.Millisecond, quietLogger(),
	)
	if err == nil {
		t.Fatal("returned with only 1 of 3 control plane nodes provisioned")
	}
	for _, want := range []string{"1 of 3", "node2", "node3", "Joining node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "node1") {
		t.Errorf("error %q lists the node that did finish as outstanding", err)
	}
}

// TestWaitControlPlaneJoined_WaitsForNodesCWEHasNotCreatedYet: a node the controller has
// no record of yet is outstanding too. Counting only the records that exist
// would let a one-node list satisfy a three-node control plane.
func TestWaitControlPlaneJoined_WaitsForNodesCWEHasNotCreatedYet(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{nodeStates: [][]api.Node{nodesProvisioned(1, 1)}}

	err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 3, 5*time.Millisecond, quietLogger(),
	)
	if err == nil {
		t.Fatal("returned with 2 of 3 control plane nodes not yet created")
	}
	if !strings.Contains(err.Error(), "2 node(s) not yet created") {
		t.Errorf("error %q does not say how many nodes are missing", err)
	}
}

// TestWaitControlPlaneJoined_SurvivesAFailedRead: as in WaitProvisioned, the controller
// being briefly unreachable is not a cluster that failed to come up.
func TestWaitControlPlaneJoined_SurvivesAFailedRead(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{
		listErrs:   []error{errors.New("connection refused"), nil},
		nodeStates: [][]api.Node{nodesProvisioned(1, 0), nodesProvisioned(1, 1)},
	}

	if err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 1, time.Minute, quietLogger(),
	); err != nil {
		t.Fatalf("WaitControlPlaneJoined gave up on a transient read failure: %v", err)
	}
}

// TestWaitControlPlaneJoined_TimeoutReportsTheReadFailure: a wait that ran out
// because the controller was never reachable must not look like a wait that ran out
// because the nodes were slow.
func TestWaitControlPlaneJoined_TimeoutReportsTheReadFailure(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{listErrs: []error{errors.New("connection refused")}}

	err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 3, 5*time.Millisecond, quietLogger(),
	)
	if err == nil {
		t.Fatal("expected WaitControlPlaneJoined to time out")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not carry the read failure", err)
	}
}

// TestWaitControlPlaneJoined_NothingToWaitFor: a site spec with no nodes is
// rejected long before this point, so the wait must not turn a zero into a
// wait that can never be satisfied.
func TestWaitControlPlaneJoined_NothingToWaitFor(t *testing.T) {
	client := &fakeClusterAPI{}

	if err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 0, time.Minute, quietLogger(),
	); err != nil {
		t.Fatalf("WaitControlPlaneJoined returned error: %v", err)
	}
	if client.lists != 0 {
		t.Errorf("polled %d times for zero nodes, want 0", client.lists)
	}
}

// TestWaitControlPlaneJoined_TimeoutForgetsARecoveredReadFailure: the last
// error is there to say why a wait ran out. A read that failed once and
// recovered is not that reason, and reporting it sends the operator after the controller
// while the node that never joined goes unlooked-at.
func TestWaitControlPlaneJoined_TimeoutForgetsARecoveredReadFailure(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{
		listErrs:   []error{errors.New("connection refused"), nil},
		nodeStates: [][]api.Node{nodesProvisioned(3, 2)},
	}

	err := WaitControlPlaneJoined(
		context.Background(), client, uuid.New(), 3, 5*time.Millisecond, quietLogger(),
	)
	if err == nil {
		t.Fatal("expected WaitControlPlaneJoined to time out")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q blames a read failure that recovered", err)
	}
	if !strings.Contains(err.Error(), "node3") {
		t.Errorf("error %q does not name the node that never joined", err)
	}
}

// TestWaitControlPlaneJoined_SpentBudgetDoesNotStartAFreshWait: this wait is
// given what is left of a deadline it shares with WaitProvisioned. Reading a
// non-positive remainder as "no budget supplied" would grant it a new default
// timeout and overrun --timeout by that much.
func TestWaitControlPlaneJoined_SpentBudgetDoesNotStartAFreshWait(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		client := &fakeClusterAPI{nodeStates: [][]api.Node{nodesProvisioned(3, 3)}}

		start := time.Now()
		err := WaitControlPlaneJoined(
			context.Background(), client, uuid.New(), 3, timeout, quietLogger(),
		)
		if err == nil {
			t.Fatalf("timeout %s: waited on a spent budget instead of failing", timeout)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("timeout %s: took %s, so a fresh wait was started", timeout, elapsed)
		}
		if client.lists != 0 {
			t.Errorf("timeout %s: polled %d times on a spent budget", timeout, client.lists)
		}
	}
}

// TestWaitProvisioned_TimeoutForgetsARecoveredReadFailure: the same rule for
// the wait next door, which carries the same lastErr into its own message.
func TestWaitProvisioned_TimeoutForgetsARecoveredReadFailure(t *testing.T) {
	shortPollInterval(t)
	client := &fakeClusterAPI{
		getErrs: []error{errors.New("connection refused")},
		states: []api.Cluster{
			clusterWith(api.ConditionStatusFalse, api.ConditionStatusTrue),
		},
	}

	_, err := WaitProvisioned(context.Background(), client, uuid.New(), 5*time.Millisecond, quietLogger())
	if err == nil {
		t.Fatal("expected WaitProvisioned to time out")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q blames a read failure that recovered", err)
	}
}
