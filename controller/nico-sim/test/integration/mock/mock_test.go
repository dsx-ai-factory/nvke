// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"net/http"
	"strings"
	"testing"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/test/integration/testutil"
)

func TestCreatePollReadyAndDeleteFlow(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)
	testutil.RunCreatePollReadyAndDeleteFlow(t, env, profile)
}

func TestPatchLabelsAndRebootFlow(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)

	instanceName := testutil.UniqueInstanceName("node-reboot")
	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Cleanup(func() {
		testutil.CleanupInstanceByID(t, env, profile, created.GetId())
	})

	testutil.WaitForInstanceState(t, env, profile, created.GetId(), "ready")

	env.PatchInstance(created.GetId(), &forgesdk.InstanceUpdateRequest{
		Labels:        map[string]string{"nke.nvidia.com/ip-address": "192.168.200.10"},
		TriggerReboot: *forgesdk.NewNullableBool(forgesdk.PtrBool(true)),
	})

	testutil.WaitForInstanceStateChange(t, env, profile, created.GetId(), "ready")
	updated := testutil.WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	if updated.GetLabels()["nke.nvidia.com/ip-address"] != "192.168.200.10" {
		t.Fatalf("instance %s reached ready without patched label", created.GetId())
	}

	env.DeleteInstance(created.GetId())

	testutil.WaitForInstanceDeletion(t, env, profile, created.GetId())
}

func TestDuplicateCreateReturnsConflictMessage(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           "node-dup-1",
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": "node-dup-1-owner-a"},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}
	env.CreateInstance(createPayload)

	conflictPayload := &forgesdk.InstanceCreateRequest{
		Name:           "node-dup-1",
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": "node-dup-1-owner-b"},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}
	status, body := env.Do(http.MethodPost, env.Endpoint("instance"), conflictPayload)
	if status != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409", status)
	}
	if !strings.Contains(string(body), "already exists") {
		t.Fatalf("expected conflict body to contain 'already exists', got: %s", string(body))
	}
}

func TestExhaustionErrorMessages(t *testing.T) {
	// IP allocation now happens in the engine during provisioning, not at
	// create time. When a subnet is exhausted, the instance enters the
	// error state with an exhaustion message.
	doc := testutil.DefaultSeedDoc()
	doc.Subnets[0].CIDR = "172.30.0.240/30"
	doc.Subnets[0].PrefixLength = 30
	doc.VPCPrefixes[0].Prefix = "172.30.0.244/30"
	doc.VPCPrefixes[0].PrefixLength = 30
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, doc, profile)

	// First instance: succeeds (gets the only available IP).
	createSubnetPayload := &forgesdk.InstanceCreateRequest{
		Name:           "node-subnet-a",
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": "node-subnet-a"},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}
	first := env.CreateInstance(createSubnetPayload)
	testutil.WaitForInstanceState(t, env, profile, first.GetId(), "ready")

	// Second instance: create succeeds but provisioning fails with exhaustion error.
	createSubnetPayloadB := &forgesdk.InstanceCreateRequest{
		Name:           "node-subnet-b",
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": "node-subnet-b"},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}
	second := env.CreateInstance(createSubnetPayloadB)
	testutil.WaitForInstanceState(t, env, profile, second.GetId(), "error")
}

func TestDeleteMissingReturnsNotFound(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)
	status, body := env.Do(http.MethodDelete, env.Endpoint("instance/not-real-id"), &forgesdk.InstanceDeleteRequest{})
	if status != http.StatusNotFound {
		t.Fatalf("delete missing status = %d, want 404", status)
	}
	if !strings.Contains(string(body), "Could not find Instance with specified ID") {
		t.Fatalf("unexpected delete missing body: %s", string(body))
	}
}
