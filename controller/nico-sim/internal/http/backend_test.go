// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/apierr"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/seed"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/sshexec"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

const (
	dispatchTestOrgID         = "novxwn552ssc"
	dispatchTestTenantID      = "d282929f-49d5-4dec-8d27-21ceb30ade19"
	testContainerSiteID       = "00000000-0000-0000-0000-000000000001"
	testContainerVPCID        = "00000000-0000-0000-0000-000000000002"
	testContainerSubnetID     = "00000000-0000-0000-0000-000000000003"
	testContainerSSHGroupID   = "00000000-0000-0000-0000-000000000004"
	testContainerInstanceType = "00000000-0000-0000-0000-000000000005"
	testVMVPCID               = "a1b2c3d4-0002-4000-8000-000000000001"
	testVMSubnetID            = "a1b2c3d4-0003-4000-8000-000000000001"
	testVMVPCPrefixID         = "a1b2c3d4-0004-4000-8000-000000000001"
	testVMInstanceTypeID      = "a1b2c3d4-0005-4000-8000-000000000001"
	testVMSSHGroupID          = "a1b2c3d4-0006-4000-8000-000000000001"
	testVMIBPartitionID       = "a1b2c3d4-0008-4000-8000-000000000001"
)

func TestContainerCreateFailureRollsBackInstanceRow(t *testing.T) {
	store := newUnifiedTestStore(t)
	containers := &fakeContainerService{
		instances: map[string]nicosdk.Instance{},
		createErr: apierr.ErrUnavailable,
	}
	handler := newDispatchHandler(t, store, containers)

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusServiceUnavailable {
		t.Fatalf("container create status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if containers.lastID == "" {
		t.Fatal("container service was not called with a reserved instance ID")
	}
	if _, err := store.GetInstance(context.Background(), containers.lastID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("instance row outlived the failed container create: %v", err)
	}

	// The name is free again, so an otherwise identical retry succeeds.
	containers.createErr = nil
	response = performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("retry after rollback status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
}

func TestContainerRequestsDispatchAcrossAPIAliases(t *testing.T) {
	store := newUnifiedTestStore(t)
	containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
	containers.onCreate = func(instanceID string) {
		record, err := store.GetInstance(context.Background(), instanceID)
		if err != nil {
			t.Errorf("instance row was not created before the container: %v", err)
			return
		}
		if record.Backend != state.SiteBackendContainer || record.SiteID != testContainerSiteID {
			t.Errorf("unexpected instance row: %+v", record)
		}
		if record.VM != nil || record.Container == nil {
			t.Errorf("container instance has invalid backend state: %+v", record)
		}
	}
	handler := newDispatchHandler(t, store, containers)

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container create status = %d, body=%s", response.Code, response.Body.String())
	}
	if containers.creates != 1 {
		t.Fatalf("container create calls = %d, want 1", containers.creates)
	}
	if got := containers.lastCreate.GetInterfaces()[0].GetIpAddress(); got != "172.18.0.99" {
		t.Fatalf("NICo-only requested IP was lost: got %q", got)
	}

	response = performJSONRequest(t, handler, nethttp.MethodGet,
		"/v2/org/"+dispatchTestOrgID+"/carbide/instance/"+containers.lastID, nil)
	if response.Code != nethttp.StatusOK {
		t.Fatalf("carbide get status = %d, body=%s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, handler, nethttp.MethodPatch,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance/"+containers.lastID, map[string]any{"triggerReboot": true})
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container patch status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if containers.updates != 1 || !containers.lastUpdate.GetTriggerReboot() {
		t.Fatalf("container update was not dispatched: calls=%d request=%+v", containers.updates, containers.lastUpdate)
	}
}

// Docker labels are fixed when a container is created, so a label update has
// to survive in the instance row instead.
func TestContainerLabelUpdatePersistsInInstanceRow(t *testing.T) {
	store := newUnifiedTestStore(t)
	containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
	handler := newDispatchHandler(t, store, containers)

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container create status = %d, body=%s", response.Code, response.Body.String())
	}
	instanceID := containers.lastID

	response = performJSONRequest(t, handler, nethttp.MethodPatch,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance/"+instanceID,
		map[string]any{"labels": map[string]string{"machine": "m-1"}})
	if response.Code != nethttp.StatusOK {
		t.Fatalf("label patch status = %d, body=%s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, handler, nethttp.MethodGet,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance/"+instanceID, nil)
	if response.Code != nethttp.StatusOK {
		t.Fatalf("get after label patch status = %d, body=%s", response.Code, response.Body.String())
	}
	var instance nicosdk.Instance
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if got := instance.GetLabels()["machine"]; got != "m-1" {
		t.Fatalf("label update was lost: labels=%v", instance.GetLabels())
	}
}

func TestContainerDeleteRemovesInstanceRow(t *testing.T) {
	store := newUnifiedTestStore(t)
	containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
	handler := newDispatchHandler(t, store, containers)

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container create status = %d, body=%s", response.Code, response.Body.String())
	}
	instanceID := containers.lastID

	response = performJSONRequest(t, handler, nethttp.MethodDelete,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance/"+instanceID, nil)
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := store.GetInstance(context.Background(), instanceID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("instance row outlived the container: %v", err)
	}
	if len(containers.instances) != 0 {
		t.Fatalf("container was not removed: %v", containers.instances)
	}
}

func TestInvalidCreatePlacementIsRejectedBeforeDispatch(t *testing.T) {
	tests := []struct {
		name   string
		orgID  string
		mutate func(*nicosdk.InstanceCreateRequest)
	}{
		{name: "unknown organization", orgID: "unknown-org"},
		{name: "wrong tenant", mutate: func(req *nicosdk.InstanceCreateRequest) { req.SetTenantId("wrong-tenant") }},
		{name: "unknown vpc", mutate: func(req *nicosdk.InstanceCreateRequest) { req.SetVpcId("unknown-vpc") }},
		{name: "subnet from VM site", mutate: func(req *nicosdk.InstanceCreateRequest) {
			interfaces := req.GetInterfaces()
			interfaces[0].SetSubnetId(testVMSubnetID)
			req.SetInterfaces(interfaces)
		}},
		{name: "prefix from VM site", mutate: func(req *nicosdk.InstanceCreateRequest) {
			interfaces := req.GetInterfaces()
			interfaces[0].SetSubnetId("")
			interfaces[0].SetVpcPrefixId(testVMVPCPrefixID)
			req.SetInterfaces(interfaces)
		}},
		{name: "ssh group from VM site", mutate: func(req *nicosdk.InstanceCreateRequest) {
			req.SetSshKeyGroupIds([]string{testVMSSHGroupID})
		}},
		{name: "infiniband partition from VM site", mutate: func(req *nicosdk.InstanceCreateRequest) {
			iface := nicosdk.NewInfiniBandInterfaceCreateRequest()
			iface.SetPartitionId(testVMIBPartitionID)
			req.SetInfinibandInterfaces([]nicosdk.InfiniBandInterfaceCreateRequest{*iface})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newUnifiedTestStore(t)
			containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
			handler := newDispatchHandler(t, store, containers)
			request := containerCreateRequest(testContainerInstanceType)
			if test.mutate != nil {
				test.mutate(&request)
			}
			orgID := test.orgID
			if orgID == "" {
				orgID = dispatchTestOrgID
			}
			response := performJSONRequest(t, handler, nethttp.MethodPost,
				"/v2/org/"+orgID+"/forge/instance", request)
			if response.Code != nethttp.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			if containers.creates != 0 {
				t.Fatal("container service was called for invalid placement")
			}
		})
	}
}

func TestMixedInstanceListUsesStoreOrderingAndPagination(t *testing.T) {
	store := newUnifiedTestStore(t)
	containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
	handler := newDispatchHandler(t, store, containers)

	response := performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", containerCreateRequest(testContainerInstanceType))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("container create status = %d, body=%s", response.Code, response.Body.String())
	}
	containerID := containers.lastID
	time.Sleep(time.Millisecond)
	response = performJSONRequest(t, handler, nethttp.MethodPost,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance", vmCreateRequest())
	if response.Code != nethttp.StatusOK {
		t.Fatalf("VM create status = %d, body=%s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, handler, nethttp.MethodGet,
		"/v2/org/"+dispatchTestOrgID+"/forge/instance?pageSize=1", nil)
	if response.Code != nethttp.StatusOK {
		t.Fatalf("list status = %d, body=%s", response.Code, response.Body.String())
	}
	var instances []nicosdk.Instance
	if err := json.Unmarshal(response.Body.Bytes(), &instances); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(instances) != 1 || instances[0].GetId() != containerID {
		t.Fatalf("first page = %+v, want container %s", instances, containerID)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type fakeContainerService struct {
	instances  map[string]nicosdk.Instance
	creates    int
	updates    int
	lastID     string
	lastCreate nicosdk.InstanceCreateRequest
	lastUpdate nicosdk.InstanceUpdateRequest
	onCreate   func(string)
	createErr  error
}

func (f *fakeContainerService) Create(_ context.Context, instanceID string, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	f.creates++
	f.lastID = instanceID
	f.lastCreate = req
	if f.onCreate != nil {
		f.onCreate(instanceID)
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	instance := nicosdk.NewInstance()
	instance.SetId(instanceID)
	instance.SetName(req.GetName())
	instance.SetTenantId(req.GetTenantId())
	instance.SetSiteId(testContainerSiteID)
	instance.SetVpcId(req.GetVpcId())
	instance.SetInstanceTypeId(req.GetInstanceTypeId())
	instance.SetMachineId("machine-" + instanceID[:8])
	interfaces := make([]nicosdk.Interface, 0, len(req.GetInterfaces()))
	for index, requested := range req.GetInterfaces() {
		iface := nicosdk.NewInterface()
		iface.SetId(fmt.Sprintf("interface-%d", index))
		iface.SetInstanceId(instanceID)
		iface.SetIpAddresses([]string{requested.GetIpAddress()})
		interfaces = append(interfaces, *iface)
	}
	instance.SetInterfaces(interfaces)
	instance.SetStatus(nicosdk.InstanceStatus("Ready"))
	instance.SetCreated(time.Unix(100, 0).UTC())
	f.instances[instance.GetId()] = *instance
	return instance, nil
}

func (f *fakeContainerService) Get(_ context.Context, id string) (*nicosdk.Instance, error) {
	instance, ok := f.instances[id]
	if !ok {
		return nil, apierr.ErrNotFound
	}
	return &instance, nil
}

func (f *fakeContainerService) Update(_ context.Context, id string, req nicosdk.InstanceUpdateRequest) (*nicosdk.Instance, error) {
	f.updates++
	f.lastUpdate = req
	instance, ok := f.instances[id]
	if !ok {
		return nil, apierr.ErrNotFound
	}
	return &instance, nil
}

func (f *fakeContainerService) Delete(_ context.Context, id string, _ *nicosdk.InstanceDeleteRequest) error {
	if _, ok := f.instances[id]; !ok {
		return apierr.ErrNotFound
	}
	delete(f.instances, id)
	return nil
}

func newDispatchHandler(t *testing.T, store *state.Store, containers ContainerService) nethttp.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(store, containers, logger, nil, -1, "", t.TempDir(), sshexec.Config{})
}

func containerCreateRequest(instanceTypeID string) nicosdk.InstanceCreateRequest {
	iface := nicosdk.NewInterfaceCreateRequest()
	iface.SetSubnetId(testContainerSubnetID)
	iface.SetIpAddress("172.18.0.99")
	request := *nicosdk.NewInstanceCreateRequest("nico-node", dispatchTestTenantID, testContainerVPCID)
	request.SetInterfaces([]nicosdk.InterfaceCreateRequest{*iface})
	request.SetInstanceTypeId(instanceTypeID)
	request.SetSshKeyGroupIds([]string{testContainerSSHGroupID})
	return request
}

func vmCreateRequest() nicosdk.InstanceCreateRequest {
	iface := nicosdk.NewInterfaceCreateRequest()
	iface.SetSubnetId(testVMSubnetID)
	request := *nicosdk.NewInstanceCreateRequest("vm-node", dispatchTestTenantID, testVMVPCID)
	request.SetInterfaces([]nicosdk.InterfaceCreateRequest{*iface})
	request.SetInstanceTypeId(testVMInstanceTypeID)
	request.SetSshKeyGroupIds([]string{testVMSSHGroupID})
	return request
}

func newUnifiedTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "nico-sim.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	doc, err := seed.Load(filepath.Join("..", "seed", "default-seed.yaml"))
	if err != nil {
		t.Fatalf("seed.Load failed: %v", err)
	}
	doc, err = seed.HydrateNetworkingCIDRs(doc, "172.30.0.0/24")
	if err != nil {
		t.Fatalf("HydrateNetworkingCIDRs failed: %v", err)
	}
	if err := store.ApplySeed(context.Background(), doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}
	return store
}

func performJSONRequest(t *testing.T, handler nethttp.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer test")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
