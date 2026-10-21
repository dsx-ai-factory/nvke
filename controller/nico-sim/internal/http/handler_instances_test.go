// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
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
