// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-sim/test/integration/testutil"
)

// TestIPXEBootFlow exercises the shared iPXE V1 flow on the mock backend.
// The same flow runs on qemu and libvirt with real VMs (including SSH verification).
func TestIPXEBootFlow(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)
	testutil.RunIPXEBootFlow(t, env, profile)
}

// TestIPXEInvalidScript verifies that an instance with an invalid iPXE script
// transitions to the error state with a descriptive error reason.
func TestIPXEInvalidScript(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)

	instanceName := testutil.UniqueInstanceName("ipxe-invalid")
	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		IpxeScript:     *forgesdk.NewNullableString(forgesdk.PtrString("not a valid ipxe script")),
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Cleanup(func() {
		testutil.CleanupInstanceByID(t, env, profile, created.GetId())
	})

	testutil.WaitForInstanceError(t, env, profile, created.GetId())

	errorReason := env.InstanceErrorReason(created.GetId())
	if !strings.Contains(errorReason, "ipxe") {
		t.Errorf("error reason should mention ipxe, got: %s", errorReason)
	}
	t.Logf("Instance correctly errored: %s", errorReason)
}

// TestIPXEImageDownloadFailure verifies that when the image URL is unreachable,
// the instance transitions to error with a descriptive message.
func TestIPXEImageDownloadFailure(t *testing.T) {
	profile := testutil.NewMockRuntimeProfile(t)
	env := testutil.NewTestEnv(t, testutil.DefaultSeedDoc(), profile)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	ipxeScript := fmt.Sprintf(`#!ipxe
set image-url %s/forbidden.qcow2
chain http://boot image_url=${image-url}
`, ts.URL)

	instanceName := testutil.UniqueInstanceName("ipxe-dl-fail")
	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		IpxeScript:     *forgesdk.NewNullableString(forgesdk.PtrString(ipxeScript)),
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Cleanup(func() {
		testutil.CleanupInstanceByID(t, env, profile, created.GetId())
	})

	testutil.WaitForInstanceError(t, env, profile, created.GetId())

	errorReason := env.InstanceErrorReason(created.GetId())
	if !strings.Contains(errorReason, "image download failed") {
		t.Errorf("error reason should mention image download failure, got: %s", errorReason)
	}
	t.Logf("Instance correctly errored on download failure: %s", errorReason)
}
