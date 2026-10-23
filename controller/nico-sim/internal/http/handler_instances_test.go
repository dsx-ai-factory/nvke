// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// The provider's client gives up after 30 s. A create that outlives it must
// still finish, or the container is removed under an instance the provider
// has already adopted by name.
func TestUnit_CreateInstanceFinishesAfterClientDisconnect(t *testing.T) {
	store := newUnifiedTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var createCtxErr error
	containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
	containers.onCreate = func(ctx context.Context, _ string) {
		close(started)
		<-release
		createCtxErr = ctx.Err()
	}
	handler := newDispatchHandler(t, store, containers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := newJSONRequest(t, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType)).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(response, request)
	}()

	select {
	case <-started:
	case <-done:
		t.Fatalf("handler returned before the backend create started: status = %d; body=%s", response.Code, response.Body.String())
	}
	cancel()
	close(release)
	<-done

	if createCtxErr != nil {
		t.Fatalf("backend create ran on the client's context: %v", createCtxErr)
	}
	if response.Code != nethttp.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if _, ok := containers.instances[containers.lastID]; !ok {
		t.Fatal("container did not survive the client disconnect")
	}
	if _, err := store.GetInstance(context.Background(), containers.lastID); err != nil {
		t.Fatalf("instance row did not survive the client disconnect: %v", err)
	}
}

func TestGetInstanceReturnsPersistedVMFailure(t *testing.T) {
	store := newUnifiedTestStore(t)
	handler := newDispatchHandler(t, store, &fakeContainerService{instances: map[string]nicosdk.Instance{}})

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", vmCreateRequest())
	if response.Code != nethttp.StatusOK {
		t.Fatalf("VM create status = %d, body=%s", response.Code, response.Body.String())
	}
	var instance nicosdk.Instance
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
		t.Fatalf("decode instance: %v", err)
	}

	reason := "vm create/start failed: test failure"
	if err := store.PatchVMInstanceState(context.Background(), instance.GetId(), state.VMInstanceStatePatch{
		Status:        stringPointer("error"),
		ObservedState: stringPointer(state.ObservedStateError),
		ErrorReason:   &reason,
	}); err != nil {
		t.Fatalf("persist failure reason: %v", err)
	}

	response = performJSONRequest(t, handler, nethttp.MethodGet,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance/"+instance.GetId(), nil)
	if response.Code != nethttp.StatusOK {
		t.Fatalf("get instance status = %d, body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
		t.Fatalf("decode failed instance: %v", err)
	}
	history := instance.GetStatusHistory()
	if len(history) != 1 || history[0].GetMessage() != reason {
		t.Fatalf("status history = %+v, want persisted failure reason %q", history, reason)
	}
}

func stringPointer(value string) *string {
	return &value
}
