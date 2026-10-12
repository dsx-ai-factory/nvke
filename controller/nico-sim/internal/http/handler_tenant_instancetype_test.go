// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/seed"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/sshexec"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// Seeded identifiers mirrored from internal/seed/default-seed.yaml so the tests
// exercise the exact IDs CAPNICo resolves against nico-vms.
const (
	testOrgID          = "novxwn552ssc"
	testTenantID       = "d282929f-49d5-4dec-8d27-21ceb30ade19"
	testTenantName     = "Forge-Tenant-NKE"
	testSiteID         = "a1b2c3d4-0001-4000-8000-000000000001"
	testVPCID          = "a1b2c3d4-0002-4000-8000-000000000001"
	testVPCName        = "mock-vpc"
	testInstanceTypeID = "a1b2c3d4-0005-4000-8000-000000000001"
)

// A second, unrelated organisation used by the org-isolation tests. Every
// resource below belongs to org B only; org A (above) must never be able to read
// them by ID, and vice versa.
const (
	testOrgBID          = "qw81mn03tzkd"
	testTenantBID       = "b7c1f0a2-3e44-4b91-9c6d-5f2a8e1d0b37"
	testTenantBName     = "Forge-Tenant-Other"
	testSiteBID         = "b1b2c3d4-0001-4000-8000-000000000002"
	testVPCBID          = "b1b2c3d4-0002-4000-8000-000000000002"
	testVPCBName        = "other-org-vpc"
	testInstanceTypeBID = "b1b2c3d4-0005-4000-8000-000000000002"
)

// newTestHandler opens a fresh store, applies a minimal seed with the default
// tenant/site/instance-type IDs, and builds the HTTP handler. Constructing the
// handler also proves the router registers without a ServeMux conflict panic
// (see the instance/type/{instanceTypeId} route-precedence note in router.go).
func newTestHandler(t *testing.T) nethttp.Handler {
	t.Helper()

	store, err := state.Open(filepath.Join(t.TempDir(), "nico-sim.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	doc := seed.Document{
		Tenants: []seed.Tenant{
			{ID: testTenantID, OrgID: testOrgID, DisplayName: testTenantName},
		},
		Sites: []seed.Site{
			{ID: testSiteID, Name: "NICo VMs Site", TenantID: testTenantID},
		},
		VPCs: []seed.VPC{
			{ID: testVPCID, Name: testVPCName, TenantID: testTenantID, SiteID: testSiteID},
		},
		InstanceTypes: []seed.InstanceType{
			{ID: testInstanceTypeID, Name: "cpu2-mem2gb-disk30gb", SiteID: testSiteID, CPUCount: 2, MemoryGiB: 2, DiskGiB: 30},
		},
	}
	if err := store.ApplySeed(context.Background(), doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(store, nil, logger, nil, 0, "", "", sshexec.Config{})
}

// newTwoOrgTestHandler builds a handler over a store seeded with TWO unrelated
// organisations, each owning its own tenant + site + vpc + instance type. It
// backs the org-isolation tests: every by-ID lookup must be constrained to the
// org in the request path, so org A cannot read org B's resources or vice versa.
func newTwoOrgTestHandler(t *testing.T) nethttp.Handler {
	t.Helper()

	store, err := state.Open(filepath.Join(t.TempDir(), "nico-sim.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	doc := seed.Document{
		Tenants: []seed.Tenant{
			{ID: testTenantID, OrgID: testOrgID, DisplayName: testTenantName},
			{ID: testTenantBID, OrgID: testOrgBID, DisplayName: testTenantBName},
		},
		Sites: []seed.Site{
			{ID: testSiteID, Name: "NICo VMs Site", TenantID: testTenantID},
			{ID: testSiteBID, Name: "Other Org Site", TenantID: testTenantBID},
		},
		VPCs: []seed.VPC{
			{ID: testVPCID, Name: testVPCName, TenantID: testTenantID, SiteID: testSiteID},
			{ID: testVPCBID, Name: testVPCBName, TenantID: testTenantBID, SiteID: testSiteBID},
		},
		InstanceTypes: []seed.InstanceType{
			{ID: testInstanceTypeID, Name: "cpu2-mem2gb-disk30gb", SiteID: testSiteID, CPUCount: 2, MemoryGiB: 2, DiskGiB: 30},
			{ID: testInstanceTypeBID, Name: "cpu4-mem8gb-disk60gb", SiteID: testSiteBID, CPUCount: 4, MemoryGiB: 8, DiskGiB: 60},
		},
	}
	if err := store.ApplySeed(context.Background(), doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(store, nil, logger, nil, 0, "", "", sshexec.Config{})
}

// authedGet issues an authenticated GET against the handler.
func authedGet(t *testing.T, h nethttp.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func TestHandleGetCurrentTenant(t *testing.T) {
	h := newTestHandler(t)

	rw := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/tenant/current")
	if rw.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}

	// Decode with the SDK type CAPNICo uses so the assertion matches its
	// GetCurrentTenantID path exactly (it reads tenant.GetId()).
	var tenant nicosdk.Tenant
	if err := json.Unmarshal(rw.Body.Bytes(), &tenant); err != nil {
		t.Fatalf("decode tenant: %v; body=%s", err, rw.Body.String())
	}
	if got := tenant.GetId(); got != testTenantID {
		t.Fatalf("tenant id = %q, want %q", got, testTenantID)
	}
	if got := tenant.GetOrg(); got != testOrgID {
		t.Fatalf("tenant org = %q, want %q", got, testOrgID)
	}
	if got := tenant.GetOrgDisplayName(); got != testTenantName {
		t.Fatalf("tenant orgDisplayName = %q, want %q", got, testTenantName)
	}
}

func TestHandleGetInstanceType(t *testing.T) {
	h := newTestHandler(t)

	rw := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/type/"+testInstanceTypeID)
	if rw.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}

	var it nicosdk.InstanceType
	if err := json.Unmarshal(rw.Body.Bytes(), &it); err != nil {
		t.Fatalf("decode instance type: %v; body=%s", err, rw.Body.String())
	}
	if got := it.GetId(); got != testInstanceTypeID {
		t.Fatalf("instance type id = %q, want %q", got, testInstanceTypeID)
	}
	// Mirror CAPNICo's availability check: allocationStats.unusedUsable must be
	// present and > 0 for the controller to proceed to instance creation.
	if it.AllocationStats == nil {
		t.Fatal("allocationStats is nil, want populated")
	}
	if it.AllocationStats.UnusedUsable == nil {
		t.Fatal("allocationStats.unusedUsable is nil, want set")
	}
	if *it.AllocationStats.UnusedUsable <= 0 {
		t.Fatalf("allocationStats.unusedUsable = %d, want > 0", *it.AllocationStats.UnusedUsable)
	}
}

func TestHandleGetInstanceTypeNotFound(t *testing.T) {
	h := newTestHandler(t)

	rw := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/type/does-not-exist")
	if rw.Code != nethttp.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rw.Code, rw.Body.String())
	}
	// 404 must use the Forge error shape so the SDK maps it to nico.ErrNotFound.
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, rw.Body.String())
	}
	if body["source"] != "forge" {
		t.Fatalf("error source = %v, want \"forge\"; body=%s", body["source"], rw.Body.String())
	}
}

func TestHandleGetVPC(t *testing.T) {
	h := newTestHandler(t)

	rw := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/vpc/"+testVPCID)
	if rw.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}

	// Decode with the SDK type CAPNICo uses; its topology reconcile reads
	// vpc.GetId() and vpc.GetName().
	var vpc nicosdk.VPC
	if err := json.Unmarshal(rw.Body.Bytes(), &vpc); err != nil {
		t.Fatalf("decode vpc: %v; body=%s", err, rw.Body.String())
	}
	if got := vpc.GetId(); got != testVPCID {
		t.Fatalf("vpc id = %q, want %q", got, testVPCID)
	}
	if got := vpc.GetName(); got != testVPCName {
		t.Fatalf("vpc name = %q, want %q", got, testVPCName)
	}
}

func TestHandleGetVPCNotFound(t *testing.T) {
	h := newTestHandler(t)

	rw := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/vpc/does-not-exist")
	if rw.Code != nethttp.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rw.Code, rw.Body.String())
	}
	// 404 must use the Forge error shape so the SDK maps it to nico.ErrNotFound.
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, rw.Body.String())
	}
	if body["source"] != "forge" {
		t.Fatalf("error source = %v, want \"forge\"; body=%s", body["source"], rw.Body.String())
	}
}

func TestVPCByIDRequiresAuth(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(nethttp.MethodGet, "/v2/org/"+testOrgID+"/forge/vpc/"+testVPCID, nil) // no Authorization header
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != nethttp.StatusUnauthorized {
		t.Fatalf("vpc/{id} without auth: status = %d, want 401; body=%s", rw.Code, rw.Body.String())
	}
}

func TestTenantAndInstanceTypeRequireAuth(t *testing.T) {
	h := newTestHandler(t)

	paths := []string{
		"/v2/org/" + testOrgID + "/forge/tenant/current",
		"/v2/org/" + testOrgID + "/forge/instance/type/" + testInstanceTypeID,
	}
	for _, p := range paths {
		req := httptest.NewRequest(nethttp.MethodGet, p, nil) // no Authorization header
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != nethttp.StatusUnauthorized {
			t.Fatalf("%s without auth: status = %d, want 401; body=%s", p, rw.Code, rw.Body.String())
		}
	}
}

// TestHandleGetVPCOrgIsolation proves vpc/{vpcId} is constrained to the org in
// the request path: each org reads only its own VPC, and a VPC that exists but
// belongs to another org 404s with a response byte-identical to the 404 for an ID
// that exists nowhere — no message or shape difference that leaks existence.
func TestHandleGetVPCOrgIsolation(t *testing.T) {
	h := newTwoOrgTestHandler(t)

	cases := []struct {
		name       string
		orgID      string
		ownVPCID   string
		ownVPCName string
		otherVPCID string
	}{
		{"org A cannot read org B", testOrgID, testVPCID, testVPCName, testVPCBID},
		{"org B cannot read org A", testOrgBID, testVPCBID, testVPCBName, testVPCID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The owning org still gets its VPC back unchanged.
			rwOwn := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/vpc/"+tc.ownVPCID)
			if rwOwn.Code != nethttp.StatusOK {
				t.Fatalf("own vpc: status = %d, want 200; body=%s", rwOwn.Code, rwOwn.Body.String())
			}
			var vpc nicosdk.VPC
			if err := json.Unmarshal(rwOwn.Body.Bytes(), &vpc); err != nil {
				t.Fatalf("decode vpc: %v; body=%s", err, rwOwn.Body.String())
			}
			if got := vpc.GetId(); got != tc.ownVPCID {
				t.Fatalf("vpc id = %q, want %q", got, tc.ownVPCID)
			}
			if got := vpc.GetName(); got != tc.ownVPCName {
				t.Fatalf("vpc name = %q, want %q", got, tc.ownVPCName)
			}

			// A VPC owned by the other org is invisible...
			rwCross := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/vpc/"+tc.otherVPCID)
			if rwCross.Code != nethttp.StatusNotFound {
				t.Fatalf("cross-org vpc: status = %d, want 404; body=%s", rwCross.Code, rwCross.Body.String())
			}

			// ...and indistinguishable from an ID that does not exist at all.
			rwMissing := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/vpc/does-not-exist")
			if rwMissing.Code != nethttp.StatusNotFound {
				t.Fatalf("unknown vpc: status = %d, want 404; body=%s", rwMissing.Code, rwMissing.Body.String())
			}
			if rwCross.Body.String() != rwMissing.Body.String() {
				t.Fatalf("cross-org 404 body differs from unknown-ID 404 body:\n cross-org = %s\n unknown   = %s",
					rwCross.Body.String(), rwMissing.Body.String())
			}
			if got, want := rwCross.Header().Get("Content-Type"), rwMissing.Header().Get("Content-Type"); got != want {
				t.Fatalf("cross-org 404 Content-Type = %q, want %q", got, want)
			}
		})
	}
}

// TestHandleGetInstanceTypeOrgIsolation proves instance/type/{instanceTypeId} is
// constrained to the org in the request path via site -> tenant -> org, that the
// allocation-stats calculation still runs for the owning org, and that a
// cross-org instance type 404s exactly like a nonexistent one.
func TestHandleGetInstanceTypeOrgIsolation(t *testing.T) {
	h := newTwoOrgTestHandler(t)

	cases := []struct {
		name        string
		orgID       string
		ownTypeID   string
		otherTypeID string
	}{
		{"org A cannot read org B", testOrgID, testInstanceTypeID, testInstanceTypeBID},
		{"org B cannot read org A", testOrgBID, testInstanceTypeBID, testInstanceTypeID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The owning org still gets the type back with allocationStats, which
			// CAPNICo's create preflight requires.
			rwOwn := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/instance/type/"+tc.ownTypeID)
			if rwOwn.Code != nethttp.StatusOK {
				t.Fatalf("own instance type: status = %d, want 200; body=%s", rwOwn.Code, rwOwn.Body.String())
			}
			var it nicosdk.InstanceType
			if err := json.Unmarshal(rwOwn.Body.Bytes(), &it); err != nil {
				t.Fatalf("decode instance type: %v; body=%s", err, rwOwn.Body.String())
			}
			if got := it.GetId(); got != tc.ownTypeID {
				t.Fatalf("instance type id = %q, want %q", got, tc.ownTypeID)
			}
			if it.AllocationStats == nil {
				t.Fatal("allocationStats is nil, want populated")
			}
			if it.AllocationStats.UnusedUsable == nil {
				t.Fatal("allocationStats.unusedUsable is nil, want set")
			}
			if *it.AllocationStats.UnusedUsable <= 0 {
				t.Fatalf("allocationStats.unusedUsable = %d, want > 0", *it.AllocationStats.UnusedUsable)
			}

			// An instance type owned by the other org is invisible...
			rwCross := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/instance/type/"+tc.otherTypeID)
			if rwCross.Code != nethttp.StatusNotFound {
				t.Fatalf("cross-org instance type: status = %d, want 404; body=%s", rwCross.Code, rwCross.Body.String())
			}

			// ...and indistinguishable from an ID that does not exist at all.
			rwMissing := authedGet(t, h, "/v2/org/"+tc.orgID+"/forge/instance/type/does-not-exist")
			if rwMissing.Code != nethttp.StatusNotFound {
				t.Fatalf("unknown instance type: status = %d, want 404; body=%s", rwMissing.Code, rwMissing.Body.String())
			}
			if rwCross.Body.String() != rwMissing.Body.String() {
				t.Fatalf("cross-org 404 body differs from unknown-ID 404 body:\n cross-org = %s\n unknown   = %s",
					rwCross.Body.String(), rwMissing.Body.String())
			}
			if got, want := rwCross.Header().Get("Content-Type"), rwMissing.Header().Get("Content-Type"); got != want {
				t.Fatalf("cross-org 404 Content-Type = %q, want %q", got, want)
			}
		})
	}
}

// TestInstanceTypeRoutePrecedence proves the isolated instance/type/{id} route
// does not swallow the sibling instance/{instanceId}/... routes and vice versa.
func TestInstanceTypeRoutePrecedence(t *testing.T) {
	h := newTestHandler(t)

	// instance/type/{id} reaches the instance-type handler (allocationStats present),
	// and is NOT captured by instance/{instanceId} or its sub-routes.
	rwType := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/type/"+testInstanceTypeID)
	if rwType.Code != nethttp.StatusOK {
		t.Fatalf("instance/type/{id}: status = %d, want 200; body=%s", rwType.Code, rwType.Body.String())
	}
	if !strings.Contains(rwType.Body.String(), "allocationStats") {
		t.Fatalf("instance/type/{id} did not reach the instance-type handler; body=%s", rwType.Body.String())
	}

	// instance/type (list) still resolves to the list handler (a JSON array).
	rwList := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/type")
	if rwList.Code != nethttp.StatusOK {
		t.Fatalf("instance/type list: status = %d, want 200; body=%s", rwList.Code, rwList.Body.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(rwList.Body.String()), "[") {
		t.Fatalf("instance/type list is not a JSON array; body=%s", rwList.Body.String())
	}

	// instance/{instanceId}/diagnostics is NOT swallowed by instance/type/{id}:
	// it reaches the diagnostics catalog handler. Using "type" as the instanceId
	// exercises the exact overlap path that would panic if both patterns shared
	// one mux.
	rwDiag := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/type/diagnostics")
	// "…/instance/type/diagnostics" is a valid instance/type/{id} lookup with
	// id="diagnostics" (unknown) → 404 from the instance-type handler, proving
	// the isolated mux owns "…/instance/type/*".
	if rwDiag.Code != nethttp.StatusNotFound {
		t.Fatalf("instance/type/diagnostics: status = %d, want 404; body=%s", rwDiag.Code, rwDiag.Body.String())
	}

	// A genuine instance sub-route (instanceId != "type") still reaches the
	// diagnostics handler. The unified backend dispatcher rejects the unknown
	// instance before returning the VM-only catalog.
	rwRealDiag := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/inst-abc/diagnostics")
	if rwRealDiag.Code != nethttp.StatusNotFound {
		t.Fatalf("instance/{id}/diagnostics: status = %d, want 404; body=%s", rwRealDiag.Code, rwRealDiag.Body.String())
	}
	if !strings.Contains(rwRealDiag.Body.String(), "Instance") {
		t.Fatalf("instance/{id}/diagnostics did not reach the diagnostics handler; body=%s", rwRealDiag.Body.String())
	}

	// A plain instance GET (instanceId != "type") reaches the instance handler,
	// which 404s in Forge shape for an unknown instance — never the type handler.
	rwInst := authedGet(t, h, "/v2/org/"+testOrgID+"/forge/instance/inst-abc")
	if rwInst.Code != nethttp.StatusNotFound {
		t.Fatalf("instance/{id}: status = %d, want 404; body=%s", rwInst.Code, rwInst.Body.String())
	}
	if !strings.Contains(rwInst.Body.String(), "Instance") {
		t.Fatalf("instance/{id} did not reach the instance handler; body=%s", rwInst.Body.String())
	}
}
