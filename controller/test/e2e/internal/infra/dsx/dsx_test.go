// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/provision"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/teleport"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const (
	testOrg      = "ncx"
	testTenant   = "tenant-uuid"
	testInstance = "instance-uuid"
	testMachine  = "machine-uuid"
	testDatahall = "datahall-1"
)

// fakeNico stands in for a datahall's nico API, and for the SSA issuer in
// front of it. It asserts the path it is reached on, which is what proves the
// SDK was pointed at this substrate rather than Forge's.
type fakeNico struct {
	t *testing.T

	mu       sync.Mutex
	created  map[string]any
	statuses []string // served in order by GetInstance; the last repeats
	polls    int
	deleted  int

	// addressAfter is the poll at which an interface gains an address.
	addressAfter int
	createStatus int
	// tenantAsked records a GET of the current tenant, which this provider
	// must never make: the tenant is declared.
	tenantAsked bool
}

func (f *fakeNico) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_, _ = w.Write([]byte(`{"access_token":"minted"}`))
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer minted" {
		f.t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
	}
	// nico, not carbide and not forge. SetAPIName rewrites the segment the
	// SDK generates, so reaching this base at all is the assertion.
	base := "/v2/org/" + testOrg + "/nico"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"/tenant/current":
		f.tenantAsked = true
		_, _ = w.Write([]byte(`{"id":"` + testTenant + `"}`))
	case r.Method == http.MethodPost && r.URL.Path == base+"/instance":
		if err := json.NewDecoder(r.Body).Decode(&f.created); err != nil {
			f.t.Errorf("decoding the create request: %v", err)
		}
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"message":"the datahall said no"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testInstance + `","status":"Pending"}`))
	case r.Method == http.MethodGet && r.URL.Path == base+"/instance/"+testInstance:
		status := f.statuses[min(f.polls, len(f.statuses)-1)]
		f.polls++
		body := `{"id":"` + testInstance + `","status":"` + status + `"`
		if f.polls > f.addressAfter {
			body += `,"interfaces":[{"ipAddresses":["10.91.179.99/31"]}]`
		}
		_, _ = w.Write([]byte(body + "}"))
	case r.Method == http.MethodDelete && r.URL.Path == base+"/instance/"+testInstance:
		f.deleted++
		// Plain text, as Forge answers, which the SDK cannot decode as the
		// MessageResponse it declares.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("Deletion request was accepted"))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

const cleanCloudInit = `{"status":"done","extended_status":"done","errors":[]}`

type fakeConn struct {
	mu        sync.Mutex
	cloudInit string
	closed    bool
}

func (c *fakeConn) Run(_ context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(cmd, "--format=json") {
		if c.cloudInit != "" {
			return c.cloudInit, nil
		}
		return cleanCloudInit, nil
	}
	return "", nil
}
func (c *fakeConn) Put(context.Context, string, []byte, uint32) error { return nil }
func (c *fakeConn) Fetch(context.Context, string) ([]byte, error)     { return nil, nil }
func (c *fakeConn) Forward(context.Context, string) (string, error)   { return "127.0.0.1:0", nil }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

type fakeDialer struct {
	refusals int
	conn     *fakeConn
	targets  []teleport.Target
}

func (d *fakeDialer) Dial(_ context.Context, target teleport.Target) (provision.Conn, error) {
	d.targets = append(d.targets, target)
	if len(d.targets) <= d.refusals {
		return nil, errors.New("connection refused")
	}
	return d.conn, nil
}

func testEnv(api string) *Environment {
	return &Environment{
		Org: Org{ID: testOrg},
		Sites: map[string]Site{
			testDatahall: {
				API:           api,
				ID:            "site-uuid",
				TenantID:      testTenant,
				VpcID:         "vpc-uuid",
				VpcPrefixID:   "prefix-uuid",
				SSHKeyGroupID: "keygroup-uuid",
				NodeUser:      "dgxc-admin",
				JumpHost:      "datahall-1-node01",
				Machines: []Machine{
					{Name: "J701L43T", ID: testMachine, Provides: map[string]string{infra.CapabilityArch: "amd64"}},
				},
				InstanceTypes: []InstanceType{
					{Name: "datahall-1-nvke-tenant", ID: "tenant-type-uuid", Provides: map[string]string{infra.CapabilityMachineType: "cpu", infra.CapabilityArch: "amd64"}},
				},
			},
		},
	}
}

func newTestProvider(t *testing.T, nico *fakeNico, dialer provision.Dialer) *Provider {
	t.Helper()
	srv := httptest.NewServer(nico)
	t.Cleanup(srv.Close)

	// A supplied client, so no test reads a cluster.
	t.Setenv(testClientIDVar, "nvssa-prd-test")
	t.Setenv(testClientSecretVar, "ssap-test")

	provider, err := New(Options{
		Env:      testEnv(srv.URL),
		SiteName: testDatahall,
		Issuer:   srv.URL,
		Images: func(_, arch string) (string, string, error) {
			return "https://cdn.example/imager-" + arch + ".efi", "https://cdn.example/node-ubuntu-24.04.5-" + arch + ".qcow2", nil
		},
		Dialer:       dialer,
		Node:         nvke.NodeInputs{KubernetesVersion: "1.36.3", CNIVersion: "1.4.0"},
		Tools:        provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func request(t *testing.T) infra.NodeRequest {
	t.Helper()
	return infra.NodeRequest{
		Name:         "e2e-site-1",
		NodeVersion:  "v0.1.0",
		Capabilities: map[string]string{infra.CapabilityArch: "amd64"},
	}
}

func TestProvisionNodePinsAMachineWhenTheSiteDeclaresOne(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	dialer := &fakeDialer{conn: &fakeConn{}}
	provider := newTestProvider(t, nico, dialer)

	node, session, err := provider.ProvisionNode(context.Background(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Teardown(context.Background()) })

	// A site that declares machines pins one, which is the hand bring-up path.
	// The default is the other way round -- see the pooled case below.
	if got := nico.created["machineId"]; got != testMachine {
		t.Errorf("machineId = %v, want the declared machine", got)
	}
	if _, ok := nico.created["instanceTypeId"]; ok {
		t.Error("a pinned create should carry no instanceTypeId; the two are alternatives")
	}
	// True re-runs the custom iPXE on every boot instead of booting the disk
	// the imager just wrote, so the node images itself forever.
	if got := nico.created["alwaysBootWithCustomIpxe"]; got != false {
		t.Errorf("alwaysBootWithCustomIpxe = %v, want false", got)
	}
	if got := nico.created["phoneHomeEnabled"]; got != false {
		t.Errorf("phoneHomeEnabled = %v, want false", got)
	}
	if got := nico.created["tenantId"]; got != testTenant {
		t.Errorf("tenantId = %v, want the declared tenant", got)
	}
	// Declared, so the provider must not go asking.
	if nico.tenantAsked {
		t.Error("the provider read the current tenant; a datahall declares its own")
	}
	if node.IP != "10.91.179.99" {
		t.Errorf("node.IP = %q, want the address with its prefix length stripped", node.IP)
	}
	if node.Arch != "amd64" {
		t.Errorf("node.Arch = %q, want the machine's", node.Arch)
	}
}

func TestProvisionNodeAttachesByPrefix(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	provider := newTestProvider(t, nico, &fakeDialer{conn: &fakeConn{}})

	_, session, err := provider.ProvisionNode(context.Background(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Teardown(context.Background()) })

	interfaces, ok := nico.created["interfaces"].([]any)
	if !ok || len(interfaces) != 1 {
		t.Fatalf("interfaces = %v, want exactly one", nico.created["interfaces"])
	}
	iface, _ := interfaces[0].(map[string]any)
	// An FNN fabric exposes prefixes and nothing else, so there is no subnet
	// to fall back to the way Forge has one.
	if iface["vpcPrefixId"] != "prefix-uuid" {
		t.Errorf("interface = %v, want it attached by vpcPrefixId", iface)
	}
	if _, ok := iface["subnetId"]; ok {
		t.Error("the interface named a subnet, which an FNN fabric does not expose")
	}
}

func TestProvisionNodeDialsBeforeReady(t *testing.T) {
	// BootCompleted arrives about three and a half minutes after create, long
	// before the OS is up, so Ready is not what this provider waits for. An
	// address on a still-provisioning instance is enough to start dialing.
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	dialer := &fakeDialer{conn: &fakeConn{}}
	provider := newTestProvider(t, nico, dialer)

	node, session, err := provider.ProvisionNode(context.Background(), request(t))
	if err != nil {
		t.Fatalf("a provisioning instance with an address should be dialed: %v", err)
	}
	t.Cleanup(func() { _ = session.Teardown(context.Background()) })
	if node.IP == "" {
		t.Error("no address was taken from a non-Ready instance")
	}
	if len(dialer.targets) == 0 {
		t.Fatal("nothing was dialed")
	}
	target := dialer.targets[0]
	if target.JumpHost != "datahall-1-node01" {
		t.Errorf("dialed through %q, want the datahall's jump host", target.JumpHost)
	}
	if target.User != "dgxc-admin" {
		t.Errorf("dialed as %q, want the account this datahall's image bakes", target.User)
	}
}

func TestProvisionNodeStopsOnError(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Error"}, addressAfter: 99}
	provider := newTestProvider(t, nico, &fakeDialer{conn: &fakeConn{}})

	_, _, err := provider.ProvisionNode(context.Background(), request(t))
	if err == nil {
		t.Fatal("an instance that went to Error should fail the run")
	}
	if !strings.Contains(err.Error(), "Error") {
		t.Errorf("error should name the status, got %v", err)
	}
	// It existed, so it has to be released.
	if nico.deleted != 1 {
		t.Errorf("deleted %d times, want 1 -- a failed provision must not leak the machine", nico.deleted)
	}
}

func TestProvisionNodeReleasesWhenCloudInitFails(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	conn := &fakeConn{cloudInit: `{"status":"error","errors":["a module blew up"]}`}
	provider := newTestProvider(t, nico, &fakeDialer{conn: conn})

	_, _, err := provider.ProvisionNode(context.Background(), request(t))
	if err == nil {
		t.Fatal("a node whose cloud-init failed should fail the run")
	}
	if nico.deleted != 1 {
		t.Errorf("deleted %d times, want 1", nico.deleted)
	}
	if !conn.closed {
		t.Error("the connection was left open")
	}
}

func TestProvisionNodeAcceptsDegradedCloudInit(t *testing.T) {
	// Phone-home is off, so cloud-init finds no datasource and finishes
	// "degraded done" over a warning that says nothing about the user data.
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	conn := &fakeConn{cloudInit: `{"status":"degraded done","extended_status":"degraded done","errors":[],"recoverable_errors":{"WARNING":["no datasource"]}}`}
	provider := newTestProvider(t, nico, &fakeDialer{conn: conn})

	_, session, err := provider.ProvisionNode(context.Background(), request(t))
	if err != nil {
		t.Fatalf("a degraded cloud-init with no errors is a usable node: %v", err)
	}
	t.Cleanup(func() { _ = session.Teardown(context.Background()) })
}

func TestCreateFailureSaysTheInstanceMayExist(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Pending"}, createStatus: http.StatusInternalServerError}
	provider := newTestProvider(t, nico, &fakeDialer{conn: &fakeConn{}})

	_, _, err := provider.ProvisionNode(context.Background(), request(t))
	if err == nil {
		t.Fatal("a rejected create should fail the run")
	}
	// Nothing here can clean up after a create whose id never arrived.
	if !strings.Contains(err.Error(), "may exist even so") {
		t.Errorf("error should warn the instance may exist, got %v", err)
	}
	if nico.deleted != 0 {
		t.Error("a create that returned no id cannot be deleted, so nothing should have tried")
	}
}

func TestProvisionNodeRefusesAnUnofferedArchitecture(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Pending"}}
	provider := newTestProvider(t, nico, &fakeDialer{conn: &fakeConn{}})

	req := request(t)
	req.Capabilities = map[string]string{infra.CapabilityArch: "arm64"}
	_, _, err := provider.ProvisionNode(context.Background(), req)
	if err == nil {
		t.Fatal("a datahall with no arm64 machine should not provision one")
	}
	// The serial, because that is what a capacity conversation is held in.
	if !strings.Contains(err.Error(), "J701L43T") {
		t.Errorf("error should name the machines offered, got %v", err)
	}
	if nico.polls != 0 {
		t.Error("nothing should have been created for a request no machine satisfies")
	}
}

func TestNewRequiresWhatCannotBeDefaulted(t *testing.T) {
	srv := httptest.NewServer(&fakeNico{t: t})
	t.Cleanup(srv.Close)

	complete := func() Options {
		return Options{
			Env:      testEnv(srv.URL),
			SiteName: testDatahall,
			Issuer:   srv.URL,
			Images:   func(string, string) (string, string, error) { return "", "", nil },
			Dialer:   &fakeDialer{conn: &fakeConn{}},
			Tools:    provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		}
	}

	for name, mutate := range map[string]func(*Options){
		"no environment": func(o *Options) { o.Env = nil },
		"no images":      func(o *Options) { o.Images = nil },
		"no dialer":      func(o *Options) { o.Dialer = nil },
		"no tools":       func(o *Options) { o.Tools = provision.HostTools{} },
		"no jump host": func(o *Options) {
			env := testEnv(srv.URL)
			site := env.Sites[testDatahall]
			site.JumpHost = ""
			env.Sites[testDatahall] = site
			o.Env = env
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := complete()
			mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Errorf("New with %s should have failed before anything was created", name)
			}
		})
	}
}

// The default, and the opposite of the pinned case above: a site declaring no
// machines sends an instanceTypeId and lets the datahall pick a host that is
// free and healthy. Pinning one blocked every run on 2026-10-12 while six
// other machines in the datahall sat idle.
func TestProvisionNodePlacesOnAnInstanceTypeWhenNoMachineIsDeclared(t *testing.T) {
	nico := &fakeNico{t: t, statuses: []string{"Provisioning"}}
	dialer := &fakeDialer{conn: &fakeConn{}}
	provider := newTestProvider(t, nico, dialer)
	provider.site.Machines = nil

	node, session, err := provider.ProvisionNode(context.Background(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Teardown(context.Background()) })

	if _, ok := nico.created["machineId"]; ok {
		t.Error("an unpinned create should carry no machineId; it needs a capability an instance type does not")
	}
	if got := nico.created["instanceTypeId"]; got == nil || got == "" {
		t.Errorf("instanceTypeId = %v, want the declared type", got)
	}
	if node.Arch == "" {
		t.Error("the node should still carry the architecture, which chooses its images")
	}
}
