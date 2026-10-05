// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"testing"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

// TestUnit_HandleListMachinesReturnsEmptyInventory verifies both API names return an empty machine inventory.
func TestUnit_HandleListMachinesReturnsEmptyInventory(t *testing.T) {
	store := newUnifiedTestStore(t)
	handler := newDispatchHandler(t, store, &fakeContainerService{instances: map[string]nicosdk.Instance{}})
	for _, apiName := range []string{"carbide", "forge"} {
		t.Run(apiName, func(t *testing.T) {
			path := "/v2/org/" + dispatchTestOrgID + "/" + apiName + "/machine?siteId=site-1&hasInstanceType=true&orderBy=ID_ASC&pageNumber=1&pageSize=100"
			response := performJSONRequest(t, handler, nethttp.MethodGet, path, nil)
			if response.Code != nethttp.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, nethttp.StatusOK, response.Body.String())
			}
			var machines []nicosdk.Machine
			if err := json.Unmarshal(response.Body.Bytes(), &machines); err != nil {
				t.Fatalf("decode machines: %v; body=%s", err, response.Body.String())
			}
			if len(machines) != 0 {
				t.Fatalf("machine count = %d, want 0", len(machines))
			}
		})
	}
}

func TestUnit_HandleGetMachine(t *testing.T) {
	type parameters struct {
		setup      func(*testing.T, nethttp.Handler, *fakeContainerService) string
		wantStatus int
		verify     func(*testing.T, *nicosdk.Machine, string, *fakeContainerService)
	}
	createContainer := func(t *testing.T, handler nethttp.Handler, containers *fakeContainerService, name string) string {
		t.Helper()
		request := containerCreateRequest(testContainerInstanceType)
		request.SetName(name)
		response := performJSONRequest(t, handler, nethttp.MethodPost,
			"/v2/org/"+dispatchTestOrgID+"/forge/instance", request)
		if response.Code != nethttp.StatusOK {
			t.Fatalf("container create status = %d, body=%s", response.Code, response.Body.String())
		}
		return "machine-" + containers.lastID[:8]
	}
	tests := map[string]parameters{
		"returns primary interface with attached DPU machine": {
			setup: func(t *testing.T, handler nethttp.Handler, containers *fakeContainerService) string {
				return createContainer(t, handler, containers, "nico-node")
			},
			wantStatus: nethttp.StatusOK,
			verify: func(t *testing.T, machine *nicosdk.Machine, machineID string, containers *fakeContainerService) {
				t.Helper()
				if got := machine.GetId(); got != machineID {
					t.Fatalf("machine ID = %q, want %q", got, machineID)
				}
				interfaces := machine.GetMachineInterfaces()
				if len(interfaces) != 1 {
					t.Fatalf("machine interface count = %d, want 1", len(interfaces))
				}
				if !interfaces[0].GetIsPrimary() {
					t.Fatal("machine interface is not primary")
				}
				primaryIP := instancePrimaryIPAddress(t, containers.instances[containers.lastID])
				digest := sha256.Sum256([]byte(primaryIP))
				wantDPU := fmt.Sprintf("host-%x", digest[:8])
				if got := interfaces[0].GetAttachedDpuMachineID(); got != wantDPU {
					t.Fatalf("attached DPU machine ID = %q, want %q", got, wantDPU)
				}
			},
		},
		"returns not found for unknown machine": {
			setup: func(t *testing.T, handler nethttp.Handler, containers *fakeContainerService) string {
				createContainer(t, handler, containers, "nico-node")
				return "unknown"
			},
			wantStatus: nethttp.StatusNotFound,
		},
		"finds machine beyond the first page": {
			setup: func(t *testing.T, handler nethttp.Handler, containers *fakeContainerService) string {
				for index := range 100 {
					createContainer(t, handler, containers, fmt.Sprintf("earlier-node-%d", index))
				}
				return createContainer(t, handler, containers, "target-node")
			},
			wantStatus: nethttp.StatusOK,
		},
		"ignores unavailable unrelated container": {
			setup: func(t *testing.T, handler nethttp.Handler, containers *fakeContainerService) string {
				createContainer(t, handler, containers, "unavailable-node")
				delete(containers.instances, containers.lastID)
				return createContainer(t, handler, containers, "target-node")
			},
			wantStatus: nethttp.StatusOK,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			store := newUnifiedTestStore(t)
			containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
			handler := newDispatchHandler(t, store, containers)
			machineID := params.setup(t, handler, containers)
			path := "/v2/org/" + dispatchTestOrgID + "/carbide/machine/" + machineID + "?includeMetadata=true"
			response := performJSONRequest(t, handler, nethttp.MethodGet, path, nil)
			if response.Code != params.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, params.wantStatus, response.Body.String())
			}
			if params.verify == nil {
				return
			}
			var machine nicosdk.Machine
			if err := json.Unmarshal(response.Body.Bytes(), &machine); err != nil {
				t.Fatalf("decode machine: %v; body=%s", err, response.Body.String())
			}
			params.verify(t, &machine, machineID, containers)
		})
	}
}

func instancePrimaryIPAddress(t *testing.T, instance nicosdk.Instance) string {
	t.Helper()
	interfaces := instance.GetInterfaces()
	if len(interfaces) == 0 || len(interfaces[0].GetIpAddresses()) == 0 {
		t.Fatal("created instance has no primary IP address")
	}
	return interfaces[0].GetIpAddresses()[0]
}
