// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	api "github.com/NVIDIA/nke/controller/api"
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
	if client.created == nil || client.created.CapiInfraCluster == nil {
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
		t.Errorf("error %q does not carry what CWE said", err)
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

// TestWaitProvisioned_SurvivesAFailedRead: CWE restarting mid-bootstrap is a
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
