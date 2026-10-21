// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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
	testOrg      = "org-uuid"
	testTenant   = "tenant-uuid"
	testInstance = "instance-uuid"
	testJumpHost = "jump.example"
)

// fakeForge serves the four calls a provision makes, under the forge path
// segment only, so a request the SDK still addresses to carbide fails.
type fakeForge struct {
	t *testing.T

	mu       sync.Mutex
	created  map[string]any
	statuses []string // served in order by GetInstance; the last repeats
	polls    int
	deleted  int

	// createDelay holds the create response back, standing in for a Forge
	// that has built the instance but is slow to say so.
	createDelay time.Duration
	// createStatus replaces 201 on create when non-zero.
	createStatus int
	// onPoll runs when readiness polling begins, after creation returned an ID.
	onPoll func()
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bearer nvapi-test" {
		f.t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
	}
	base := "/v2/org/" + testOrg + "/forge"
	// The SDK decodes a body only when it is declared JSON.
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"/tenant/current":
		_, _ = w.Write([]byte(`{"id":"` + testTenant + `"}`))
	case r.Method == http.MethodPost && r.URL.Path == base+"/instance":
		if err := json.NewDecoder(r.Body).Decode(&f.created); err != nil {
			f.t.Errorf("decoding the create request: %v", err)
		}
		time.Sleep(f.createDelay)
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"message":"forge said no"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testInstance + `","status":"Pending"}`))
	case r.Method == http.MethodGet && r.URL.Path == base+"/instance/"+testInstance:
		status := f.statuses[min(f.polls, len(f.statuses)-1)]
		f.polls++
		if f.onPoll != nil {
			f.onPoll()
		}
		body := `{"id":"` + testInstance + `","status":"` + status + `"`
		if status == "Ready" {
			body += `,"interfaces":[{"ipAddresses":["10.0.0.7/24"]}]`
		}
		_, _ = w.Write([]byte(body + "}"))
	case r.Method == http.MethodDelete && r.URL.Path == base+"/instance/"+testInstance:
		f.deleted++
		// What site-1 answers: plain text, which the SDK cannot decode as
		// the MessageResponse it declares.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("Deletion request was accepted"))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// cleanCloudInit is what a node reports when nothing went wrong.
const cleanCloudInit = `{"status":"done","extended_status":"done","errors":[]}`

type fakeConn struct {
	mu     sync.Mutex
	ran    []string
	runErr error
	// cloudInit is the JSON the status probe answers with. Empty is a node
	// that finished clean.
	cloudInit string
	closed    bool
}

func (c *fakeConn) Run(_ context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = append(c.ran, cmd)
	if strings.Contains(cmd, "--format=json") {
		if c.cloudInit != "" {
			return c.cloudInit, nil
		}
		return cleanCloudInit, nil
	}
	return "", c.runErr
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

// fakeDialer refuses the first refusals dials, as a booting node does.
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

func testEnv() *Environment {
	return &Environment{
		Org: Org{ID: testOrg},
		Sites: map[string]Site{
			"default": {
				Key:           "default",
				ID:            "site-uuid",
				VpcID:         "vpc-uuid",
				SubnetID:      "subnet-uuid",
				SSHKeyGroupID: "keygroup-uuid",
				InstanceTypes: []InstanceType{
					{Name: "cpu.himem.t1", ID: "cpu-type-uuid", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
				},
			},
		},
	}
}

func newTestProvider(t *testing.T, forge *fakeForge, dialer provision.Dialer) *Provider {
	t.Helper()
	srv := httptest.NewServer(forge)
	t.Cleanup(srv.Close)
	p, err := New(Options{
		Env:      testEnv(),
		APIKey:   "nvapi-test",
		Endpoint: srv.URL,
		Images: func(version, arch string) (string, string, error) {
			return "https://example.invalid/imager.efi", "https://example.invalid/nvke-node-non-fde-ubuntu-24.04.1-" + arch + "-" + version + ".qcow2", nil
		},
		Dialer:   dialer,
		JumpHost: testJumpHost,
		Node: nvke.NodeInputs{
			KubernetesVersion: "1.34.2",
			CNIVersion:        "1.4.0",
		},
		Tools:        provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		HTTPClient:   srv.Client(),
		ReadyTimeout: 2 * time.Second,
		ReachTimeout: 2 * time.Second,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func testRequest() infra.NodeRequest {
	return infra.NodeRequest{
		Name:         "e2e-site-1",
		Capabilities: map[string]string{"machineType": "cpu"},
		NodeVersion:  "1.2.0",
		Labels:       map[string]string{"nke-e2e-run": "run-1"},
	}
}

func TestProvisionNode(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Provisioning", "Ready"}}
	dialer := &fakeDialer{refusals: 2, conn: &fakeConn{}}
	p := newTestProvider(t, forge, dialer)

	node, session, err := p.ProvisionNode(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if node.IP != "10.0.0.7" {
		t.Errorf("IP = %q, want the address without its prefix length", node.IP)
	}
	// From the matched instance type, so a caller learns the architecture from
	// the node rather than by holding a Forge type to ask.
	if node.Arch != "amd64" {
		t.Errorf("Arch = %q, want the matched instance type's amd64", node.Arch)
	}

	created := forge.created
	for field, want := range map[string]any{
		"name":                     "e2e-site-1",
		"tenantId":                 testTenant,
		"vpcId":                    "vpc-uuid",
		"instanceTypeId":           "cpu-type-uuid",
		"phoneHomeEnabled":         false,
		"alwaysBootWithCustomIpxe": true,
	} {
		if created[field] != want {
			t.Errorf("create %s = %v, want %v", field, created[field], want)
		}
	}
	if ifaces, _ := created["interfaces"].([]any); len(ifaces) != 1 || ifaces[0].(map[string]any)["subnetId"] != "subnet-uuid" {
		t.Errorf("create interfaces = %v, want one on the site subnet", created["interfaces"])
	}
	if !strings.Contains(created["ipxeScript"].(string), "amd64-1.2.0.qcow2") {
		t.Error("iPXE script should boot the image for the requested node version")
	}
	userData := created["userData"].(string)
	if !strings.Contains(userData, "ssh-ed25519 ") {
		t.Error("user data should authorize the session key")
	}
	if !strings.Contains(userData, "--secure-server-port 15005") || strings.Contains(userData, "--disable-mtls") {
		t.Error("the agent should run with mTLS unless told otherwise")
	}

	if len(dialer.targets) != 3 {
		t.Errorf("dialed %d times, want retries until the node answered", len(dialer.targets))
	}
	target := dialer.targets[len(dialer.targets)-1]
	if target.JumpHost != testJumpHost || target.Address != "10.0.0.7" || target.User != provision.DefaultNodeUser || target.Signer == nil {
		t.Errorf("dial target = %+v", target)
	}
	wantRan := []string{
		"cloud-init status --wait >/dev/null 2>&1 || true",
		"cloud-init status --format=json 2>/dev/null || true",
	}
	if got := dialer.conn.ran; !slices.Equal(got, wantRan) {
		t.Errorf("ran %q, want the node held until cloud-init finished and then read for errors", got)
	}
	if forge.deleted != 0 {
		t.Fatal("a provisioned node must not be deleted")
	}

	if err := session.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if err := session.Teardown(context.Background()); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
	if forge.deleted != 1 || !dialer.conn.closed {
		t.Errorf("teardown deleted %d times, closed=%v; want the instance deleted once and the connection closed", forge.deleted, dialer.conn.closed)
	}
}

func TestProvisionNodeReleasesOnFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		statuses []string
		conn     *fakeConn
		cancel   bool
	}{
		"instance errors": {statuses: []string{"Provisioning", "Error"}, conn: &fakeConn{}},
		"cloud-init failed": {statuses: []string{"Ready"}, conn: &fakeConn{
			cloudInit: `{"status":"error","extended_status":"error","errors":["scripts-user failed"]}`,
		}},
		"never ready":          {statuses: []string{"Provisioning"}, conn: &fakeConn{}},
		"run canceled waiting": {statuses: []string{"Provisioning"}, conn: &fakeConn{}, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			forge := &fakeForge{t: t, statuses: tc.statuses}
			p := newTestProvider(t, forge, &fakeDialer{conn: tc.conn})
			if name == "never ready" {
				p.opts.ReadyTimeout = 20 * time.Millisecond
			}

			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				// Cancel after creation returns an instance ID so the test exercises
				// readiness cleanup rather than racing setup or instance creation.
				// See https://github.com/dsx-ai-factory/nvke/issues/1200.
				forge.onPoll = cancel
			}
			if _, _, err := p.ProvisionNode(ctx, testRequest()); err == nil {
				t.Fatal("expected an error")
			}
			if tc.cancel && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("run context error = %v, want cancellation during readiness polling", ctx.Err())
			}
			if forge.deleted != 1 {
				t.Errorf("deleted %d times, want the instance released", forge.deleted)
			}
		})
	}
}

// A Forge instance is created with phone-home disabled and cannot reach the
// metadata service, so cloud-init finds no datasource, falls back to
// DataSourceNone and finishes "degraded done" with a non-zero exit code. The
// user data still ran, so the node is fit to bootstrap on and the preflight
// must not reject it.
func TestProvisionNodeAcceptsDegradedCloudInit(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Ready"}}
	conn := &fakeConn{cloudInit: `{"status":"done","extended_status":"degraded done","errors":[],` +
		`"recoverable_errors":{"WARNING":["Used fallback datasource"]}}`}

	if _, _, err := newTestProvider(t, forge, &fakeDialer{conn: conn}).ProvisionNode(context.Background(), testRequest()); err != nil {
		t.Fatalf("a node whose cloud-init only warned must be accepted: %v", err)
	}
	if forge.deleted != 0 {
		t.Errorf("deleted %d times, want the instance kept", forge.deleted)
	}
}

func TestProvisionNodeDisableMTLS(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Ready"}}
	p := newTestProvider(t, forge, &fakeDialer{conn: &fakeConn{}})
	p.opts.DisableMTLS = true

	if _, _, err := p.ProvisionNode(context.Background(), testRequest()); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if !strings.Contains(forge.created["userData"].(string), "--disable-mtls") {
		t.Error("the agent should be told to run without mTLS")
	}
}

func TestProvisionNodeRejectsUnmatchedCapabilities(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Ready"}}
	p := newTestProvider(t, forge, &fakeDialer{conn: &fakeConn{}})
	req := testRequest()
	req.Capabilities = map[string]string{"gpu": "gb200"}

	if _, _, err := p.ProvisionNode(context.Background(), req); err == nil {
		t.Fatal("expected no instance type to match")
	}
	if forge.created != nil {
		t.Error("nothing should be created when no instance type matches")
	}
}

// newSlowCreateProvider is newTestProvider with the two timeouts that matter
// here set apart: everything runs under a cap shorter than the create delay,
// and create runs under one longer than it.
func newSlowCreateProvider(t *testing.T, forge *fakeForge, dialer provision.Dialer, perRequest, create time.Duration) *Provider {
	t.Helper()
	srv := httptest.NewServer(forge)
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Timeout = perRequest
	p, err := New(Options{
		Env:      testEnv(),
		APIKey:   "nvapi-test",
		Endpoint: srv.URL,
		Images: func(version, arch string) (string, string, error) {
			return "https://example.invalid/imager.efi", "https://example.invalid/nvke-node-non-fde-ubuntu-24.04.1-" + arch + "-" + version + ".qcow2", nil
		},
		Dialer:   dialer,
		JumpHost: testJumpHost,
		Node: nvke.NodeInputs{
			KubernetesVersion: "1.34.2",
			CNIVersion:        "1.4.0",
		},
		Tools:         provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		HTTPClient:    client,
		ReadyTimeout:  5 * time.Second,
		ReachTimeout:  5 * time.Second,
		PollInterval:  time.Millisecond,
		CreateTimeout: create,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

// A create slower than the cap every other call runs under must still be
// waited out. Giving up on it loses the id, and the id is the only handle on
// an instance Forge has already built.
func TestSlowCreateIsNotAbandoned(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Ready"}, createDelay: 300 * time.Millisecond}
	dialer := &fakeDialer{conn: &fakeConn{}}
	p := newSlowCreateProvider(t, forge, dialer, 50*time.Millisecond, 5*time.Second)

	node, session, err := p.ProvisionNode(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("a create slower than the per-request cap was abandoned: %v", err)
	}
	if session == nil {
		t.Fatal("no session")
	}
	if node.IP != "10.0.0.7" {
		t.Errorf("node.IP = %q, want 10.0.0.7", node.IP)
	}
	if forge.deleted != 0 {
		t.Errorf("the instance was deleted %d times; the create succeeded", forge.deleted)
	}
}

// A create that fails cannot be reported as though nothing was built, because
// nothing here can establish that either way.
func TestCreateFailureSaysTheInstanceMayExist(t *testing.T) {
	forge := &fakeForge{t: t, statuses: []string{"Ready"}, createStatus: http.StatusInternalServerError}
	dialer := &fakeDialer{conn: &fakeConn{}}
	p := newSlowCreateProvider(t, forge, dialer, time.Second, 5*time.Second)

	_, _, err := p.ProvisionNode(context.Background(), testRequest())
	if err == nil {
		t.Fatal("a 500 on create returned no error")
	}
	for _, want := range []string{"e2e-site-1", "may exist", "Forge console"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so nobody will go looking for the instance:\n%v", want, err)
		}
	}
}

func TestRequireCapacity(t *testing.T) {
	const (
		cpuID = "cpu-type-uuid"
		gpuID = "gpu-type-uuid"
	)
	env := testEnv()
	env.Sites["default"] = Site{
		Key:           "default",
		ID:            "site-uuid",
		VpcID:         "vpc-uuid",
		SubnetID:      "subnet-uuid",
		SSHKeyGroupID: "keygroup-uuid",
		InstanceTypes: []InstanceType{
			{Name: "cpu.himem.t1", ID: cpuID, Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "gpu.gb200", ID: gpuID, Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	stats := map[string]string{
		cpuID: `{"total":4,"used":0,"unused":4,"unusedUsable":4}`,
		gpuID: `{"total":14,"used":0,"unused":14,"unusedUsable":0}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer nvapi-test" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Query().Get("includeAllocationStats") != "true" {
			t.Errorf("%s requested allocation stats as %q", r.URL.Path, r.URL.Query().Get("includeAllocationStats"))
		}
		id := strings.TrimPrefix(r.URL.Path, "/v2/org/"+testOrg+"/forge/instance/type/")
		body, ok := stats[id]
		if !ok || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + id + `","allocationStats":` + body + `}`))
	}))
	t.Cleanup(srv.Close)
	p, err := New(Options{
		Env:      env,
		APIKey:   "nvapi-test",
		Endpoint: srv.URL,
		JumpHost: testJumpHost,
		Images: func(string, string) (string, string, error) {
			return "https://example.invalid/imager.efi", "https://example.invalid/node.qcow2", nil
		},
		Dialer:     &fakeDialer{},
		Tools:      provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	err = p.RequireCapacity(context.Background(), map[string]string{"arch": "amd64", "gpu": "gb200"}, 2, 1)
	if err == nil {
		t.Fatal("a GPU shape with unusedUsable=0 was accepted")
	}
	for _, want := range []string{"gpu.gb200", gpuID, "need 1", "unusedUsable=0", "total=14", "used=0", "unused=14"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "cpu.himem.t1") {
		t.Errorf("the CPU shape has room and was reported as a shortfall:\n%v", err)
	}

	stats[gpuID] = `{"total":14,"used":1,"unused":13,"unusedUsable":2}`
	if err := p.RequireCapacity(context.Background(), map[string]string{"arch": "amd64", "gpu": "gb200"}, 2, 1); err != nil {
		t.Fatalf("usable allocations for both shapes: %v", err)
	}
}

func TestWaitTerminating(t *testing.T) {
	var mu sync.Mutex
	status := map[string]string{
		"cp-uuid":     "Ready",
		"worker-uuid": "Terminating",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v2/org/"+testOrg+"/forge/instance/")
		mu.Lock()
		s, ok := status[id]
		mu.Unlock()
		if r.Method != http.MethodGet || !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if s == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + id + `","status":"` + s + `"}`))
	}))
	t.Cleanup(srv.Close)
	p, err := New(Options{
		Env:      testEnv(),
		APIKey:   "nvapi-test",
		Endpoint: srv.URL,
		JumpHost: testJumpHost,
		Images: func(string, string) (string, string, error) {
			return "https://example.invalid/imager.efi", "https://example.invalid/node.qcow2", nil
		},
		Dialer:       &fakeDialer{},
		Tools:        provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		HTTPClient:   srv.Client(),
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	ids := []string{"cp-uuid", "worker-uuid", "cp-uuid"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = p.WaitTerminating(ctx, ids)
	if err == nil {
		t.Fatal("an instance still Ready was accepted as terminating")
	}
	if !strings.Contains(err.Error(), "cp-uuid (last Ready)") || strings.Contains(err.Error(), "worker-uuid") {
		t.Errorf("the error should name only the Ready instance and its status:\n%v", err)
	}

	mu.Lock()
	status["cp-uuid"] = ""
	mu.Unlock()
	if err := p.WaitTerminating(context.Background(), ids); err != nil {
		t.Fatalf("one Terminating and one gone: %v", err)
	}
}

func TestNewRequiresJumpHost(t *testing.T) {
	_, err := New(Options{
		Env: testEnv(), APIKey: "nvapi-test", Tools: provision.HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"},
		Images: func(string, string) (string, string, error) { return "", "", nil },
		Dialer: &fakeDialer{},
	})
	if err == nil || !strings.Contains(err.Error(), "jump host") {
		t.Errorf("err = %v, want a missing jump host reported", err)
	}
}

func TestProvisionNodeKeepsTheMachineOnFailure(t *testing.T) {
	// The case the whole option exists for: a node that answers sshd and
	// refuses the key is indistinguishable from a dozen other causes, and
	// only the node itself can say which. Releasing it ends that.
	forge := &fakeForge{t: t, statuses: []string{"Ready"}}
	dialer := &fakeDialer{refusals: 100, conn: &fakeConn{}}
	provider := newTestProvider(t, forge, dialer)
	provider.opts.KeepOnFailure = true
	provider.opts.ReachTimeout = 10 * time.Millisecond

	_, _, err := provider.ProvisionNode(context.Background(), testRequest())
	if err == nil {
		t.Fatal("an unreachable node should fail the run")
	}
	if forge.deleted != 0 {
		t.Errorf("deleted %d times, want 0 -- the machine was supposed to be kept", forge.deleted)
	}
	// Useless if a reader cannot find it again, so the message carries both
	// the name and where it answered.
	for _, want := range []string{"KEPT", testInstance, "released by hand", "10.0.0.7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}
