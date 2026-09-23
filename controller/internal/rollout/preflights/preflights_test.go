// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/preflights"
)

// ---- fakes ---------------------------------------------------------------

type fakeNode struct {
	id       uuid.UUID
	name     string
	endpoint string
}

func (n fakeNode) ID() uuid.UUID          { return n.id }
func (n fakeNode) Name() string           { return n.name }
func (n fakeNode) Role() rollout.NodeRole { return rollout.NodeRoleControlPlane }
func (n fakeNode) AgentEndpoint() string  { return n.endpoint }

type fakeCluster struct {
	id   uuid.UUID
	name string
	cps  []rollout.Node
}

func (c *fakeCluster) ID() uuid.UUID                     { return c.id }
func (c *fakeCluster) Name() string                      { return c.name }
func (c *fakeCluster) KubernetesVersion() string         { return "v1.30.0" }
func (c *fakeCluster) ControlPlaneVIP() string           { return "10.0.0.1:6443" }
func (c *fakeCluster) ControlPlaneNodes() []rollout.Node { return c.cps }
func (c *fakeCluster) Nodes() []rollout.Node             { return c.cps }

// scriptedAgent is a raw nkeproto.NodeAgentClient that returns scripted
// responses per script name. It embeds the proto client interface so the
// many RPCs the preflights never call are satisfied (they panic if
// invoked); only ExecuteScript and GetKubeadmStatus are implemented.
//
// ExecuteScript looks up the script name in execResponses; a missing
// entry means "match any" and returns the defaultExec result.
// GetKubeadmStatus returns the scripted single status (or err).
type scriptedAgent struct {
	nkeproto.NodeAgentClient

	execResponses    map[string]execResp
	defaultExec      execResp
	kubeadmStatus    *nkeproto.KubeadmStatusResponse
	kubeadmStatusErr error

	// records every call for assertion
	execCalls []string
}

type execResp struct {
	code   int32
	stdout string
	stderr string
	err    error
}

func (a *scriptedAgent) ExecuteScript(
	_ context.Context, in *nkeproto.ExecuteScriptRequest, _ ...grpc.CallOption,
) (*nkeproto.ExecuteScriptResponse, error) {
	a.execCalls = append(a.execCalls, in.GetScriptName())
	r := a.defaultExec
	if rr, ok := a.execResponses[in.GetScriptName()]; ok {
		r = rr
	}
	if r.err != nil {
		return nil, r.err
	}
	return &nkeproto.ExecuteScriptResponse{
		ExitCode:     r.code,
		Output:       r.stdout,
		ErrorMessage: r.stderr,
	}, nil
}

func (a *scriptedAgent) GetKubeadmStatus(
	_ context.Context, _ *nkeproto.KubeadmStatusRequest, _ ...grpc.CallOption,
) (*nkeproto.KubeadmStatusResponse, error) {
	if a.kubeadmStatusErr != nil {
		return nil, a.kubeadmStatusErr
	}
	return a.kubeadmStatus, nil
}

// dialerForAgents binds scriptedAgents to a Dialer so tests can drive
// multiple node-targeted dialer calls in one test. Maps endpoint to agent; a
// missing endpoint returns a dial error.
func dialerForAgents(agents map[string]*scriptedAgent) nodeagent.Dialer {
	return func(_ context.Context, node rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		endpoint := node.AgentEndpoint()
		a, ok := agents[endpoint]
		if !ok {
			return nil, nil, errors.New("no agent stubbed for endpoint " + endpoint)
		}
		return a, func() {}, nil
	}
}

// onceAgentDialer returns a Dialer that returns the same agent for
// every endpoint. Convenience for single-node tests.
func onceAgentDialer(a *scriptedAgent) nodeagent.Dialer {
	return func(_ context.Context, _ rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		return a, func() {}, nil
	}
}

func newCluster(nNodes int) *fakeCluster {
	c := &fakeCluster{id: uuid.New(), name: "test-cluster"}
	for i := range nNodes {
		c.cps = append(c.cps, fakeNode{
			id:       uuid.New(),
			name:     "cp-" + strconv.Itoa(i),
			endpoint: "10.0.0." + strconv.Itoa(i+1) + ":50051",
		})
	}
	return c
}

// ---- NodeAgentReachable (cluster scope) ----------------------------------

func TestNodeAgentReachable_PassesWhenAllCPsDialOK(t *testing.T) {
	t.Parallel()

	healthy := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready", AgentVersion: "v0.1.17"},
	}
	c := newCluster(3)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		c.cps[1].AgentEndpoint(): healthy,
		c.cps[2].AgentEndpoint(): healthy,
	}

	fn := preflights.NewNodeAgentReachableFunc(dialerForAgents(agents))
	assert.NoError(t, fn(context.Background(), c, nil))
}

func TestNodeAgentReachable_FailsWhenOneCPDialFails(t *testing.T) {
	t.Parallel()

	healthy := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready"},
	}
	c := newCluster(3)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		// c.cps[1] intentionally absent — dialerForAgents returns an
		// error for endpoints not in the map, simulating an unreachable
		// agent.
		c.cps[2].AgentEndpoint(): healthy,
	}

	fn := preflights.NewNodeAgentReachableFunc(dialerForAgents(agents))
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2/3 CPs reachable")
	assert.Contains(t, err.Error(), c.cps[1].Name(),
		"error must name the specific unreachable CP for operator triage")
}

func TestNodeAgentReachable_FailsOnAnyRPCError(t *testing.T) {
	t.Parallel()

	healthy := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready"},
	}
	rpcDown := &scriptedAgent{
		kubeadmStatusErr: errors.New("rpc error: code = Unavailable"),
	}
	c := newCluster(2)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		c.cps[1].AgentEndpoint(): rpcDown,
	}

	fn := preflights.NewNodeAgentReachableFunc(dialerForAgents(agents))
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1/2 CPs reachable")
	assert.Contains(t, err.Error(), c.cps[1].Name())
	assert.Contains(t, err.Error(), "Unavailable", "first-err detail must propagate")
}

func TestNodeAgentReachable_FailsOnEmptyCluster(t *testing.T) {
	t.Parallel()

	fn := preflights.NewNodeAgentReachableFunc(onceAgentDialer(nil))
	c := &fakeCluster{id: uuid.New(), name: "empty"}
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0 CPs")
}

// ---- NodeAgentReady ------------------------------------------------------

func TestNodeAgentReady_PassesWhenStatusReady(t *testing.T) {
	t.Parallel()

	agent := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready", KubeadmPhase: "initialize"},
	}
	fn := preflights.NewNodeAgentReadyFunc(onceAgentDialer(agent))

	c := newCluster(1)
	assert.NoError(t, fn(context.Background(), c, c.cps[0], nil))
}

func TestNodeAgentReady_FailsWhenStatusNotReady(t *testing.T) {
	t.Parallel()

	cases := []struct{ status, phase, msg string }{
		{"uninitialized", "", ""},
		{"pending", "join", "Joining node"},
		{"error", "join", "exit status 1"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			agent := &scriptedAgent{
				kubeadmStatus: &nkeproto.KubeadmStatusResponse{
					KubeadmStatus: tc.status, KubeadmPhase: tc.phase, KubeadmCurrentMessage: tc.msg,
				},
			}
			fn := preflights.NewNodeAgentReadyFunc(onceAgentDialer(agent))
			c := newCluster(1)
			err := fn(context.Background(), c, c.cps[0], nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.status)
		})
	}
}

// ---- NodeAgentVersion ----------------------------------------------------

func TestNodeAgentVersion_PassesAtMinimum(t *testing.T) {
	t.Parallel()

	agent := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready", AgentVersion: "v0.1.21"},
	}
	fn := preflights.NewNodeAgentVersionFunc(onceAgentDialer(agent))

	c := newCluster(1)
	err := fn(context.Background(), c, c.cps[0],
		map[string]string{preflights.ParamMinVersion: "v0.1.21"})
	assert.NoError(t, err)
}

func TestNodeAgentVersion_FailsBelowMinimum(t *testing.T) {
	t.Parallel()

	agent := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready", AgentVersion: "v0.1.17-3-gf5bc014"},
	}
	fn := preflights.NewNodeAgentVersionFunc(onceAgentDialer(agent))

	c := newCluster(1)
	err := fn(context.Background(), c, c.cps[0],
		map[string]string{preflights.ParamMinVersion: "v0.1.21"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v0.1.17")
	assert.Contains(t, err.Error(), "v0.1.21")
}

func TestNodeAgentVersion_FailsClosedOnMissingParam(t *testing.T) {
	t.Parallel()

	agent := &scriptedAgent{
		kubeadmStatus: &nkeproto.KubeadmStatusResponse{KubeadmStatus: "ready", AgentVersion: "v9.9.9"},
	}
	fn := preflights.NewNodeAgentVersionFunc(onceAgentDialer(agent))

	c := newCluster(1)
	err := fn(context.Background(), c, c.cps[0], nil) // no min_version param
	require.Error(t, err)
	assert.Contains(t, err.Error(), preflights.ParamMinVersion)
}

// ---- KubeAPIHealthy ------------------------------------------------------
//
// Walks every CP and applies the healthyRequired threshold so the
// rollout keeps a safe majority of control planes available.

func TestKubeAPIHealthy_PassesWhenAllCPsHealthy(t *testing.T) {
	t.Parallel()

	healthy := &scriptedAgent{defaultExec: execResp{
		code:   0,
		stdout: "[+]ping ok\n[+]log ok\n[+]etcd ok\nreadyz check passed\n",
	}}
	c := newCluster(3)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		c.cps[1].AgentEndpoint(): healthy,
		c.cps[2].AgentEndpoint(): healthy,
	}

	fn := preflights.NewKubeAPIHealthyFunc(dialerForAgents(agents))
	assert.NoError(t, fn(context.Background(), c, nil))
}

func TestKubeAPIHealthy_3CP_FailsWhenOneCPReadyzNotGreen(t *testing.T) {
	t.Parallel()

	// 3-CP cluster: threshold=3 (no headroom). One CP failing → halt
	// with that CP's failing-check names in the error.
	healthy := &scriptedAgent{defaultExec: execResp{
		code:   0,
		stdout: "[+]ping ok\n[+]etcd ok\nreadyz check passed\n",
	}}
	etcdDown := &scriptedAgent{defaultExec: execResp{
		code:   0,
		stdout: "[+]ping ok\n[-]etcd failed: connection refused\n",
	}}
	c := newCluster(3)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		c.cps[1].AgentEndpoint(): etcdDown,
		c.cps[2].AgentEndpoint(): healthy,
	}

	fn := preflights.NewKubeAPIHealthyFunc(dialerForAgents(agents))
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2/3 CPs healthy")
	assert.Contains(t, err.Error(), "need >= 3")
	assert.Contains(t, err.Error(), c.cps[1].Name(),
		"error must name the specific unhealthy CP")
	assert.Contains(t, err.Error(), "failing checks: [etcd]",
		"error must surface the failing readyz subcheck names")
}

func TestKubeAPIHealthy_5CP_PassesWhenFourHealthy(t *testing.T) {
	t.Parallel()

	// 5-CP cluster — threshold is 4 (quorum 3 + 1 headroom). One down
	// is acceptable.
	healthy := &scriptedAgent{defaultExec: execResp{
		code:   0,
		stdout: "[+]ping ok\nreadyz check passed\n",
	}}
	broken := &scriptedAgent{defaultExec: execResp{code: 1, stderr: "connection refused"}}
	c := newCluster(5)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): healthy,
		c.cps[1].AgentEndpoint(): healthy,
		c.cps[2].AgentEndpoint(): broken,
		c.cps[3].AgentEndpoint(): healthy,
		c.cps[4].AgentEndpoint(): healthy,
	}

	fn := preflights.NewKubeAPIHealthyFunc(dialerForAgents(agents))
	assert.NoError(t, fn(context.Background(), c, nil),
		"5-CP can tolerate one broken apiserver (4 healthy >= threshold 4)")
}

func TestKubeAPIHealthy_FailsOnScriptExitCodeNonZero(t *testing.T) {
	t.Parallel()

	// 3-CP, one CP returns non-zero exit with no failing-check
	// parseable from stdout → fallback to exit+stderr detail.
	healthy := &scriptedAgent{defaultExec: execResp{
		code: 0, stdout: "[+]ping ok\nreadyz check passed\n",
	}}
	exitErr := &scriptedAgent{defaultExec: execResp{
		code:   1,
		stderr: "kubectl: Unable to connect to the server",
	}}
	c := newCluster(3)
	agents := map[string]*scriptedAgent{
		c.cps[0].AgentEndpoint(): exitErr,
		c.cps[1].AgentEndpoint(): healthy,
		c.cps[2].AgentEndpoint(): healthy,
	}

	fn := preflights.NewKubeAPIHealthyFunc(dialerForAgents(agents))
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2/3 CPs healthy")
	assert.Contains(t, err.Error(), "exit=1",
		"when no failing-check parseable, error must include exit code")
	assert.Contains(t, err.Error(), "Unable to connect",
		"error must include stderr detail for triage")
}

func TestKubeAPIHealthy_FailsOnEmptyCluster(t *testing.T) {
	t.Parallel()

	fn := preflights.NewKubeAPIHealthyFunc(onceAgentDialer(nil))
	c := &fakeCluster{id: uuid.New(), name: "empty"}
	err := fn(context.Background(), c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0 CPs")
}

// ---- RegisterDefaults ----------------------------------------------------

func TestRegisterDefaults_InstallsAllNames(t *testing.T) {
	t.Parallel()

	reg := rollout.NewRegistry()
	preflights.RegisterDefaults(reg, onceAgentDialer(&scriptedAgent{}))

	for _, name := range []string{
		preflights.PreflightNameNodeAgentReachable,
		preflights.PreflightNameKubeAPIHealthy,
	} {
		_, ok := reg.LookupClusterPreflight(name)
		assert.True(t, ok, "cluster preflight %q must be registered", name)
	}
	for _, name := range []string{
		preflights.PreflightNameNodeAgentReady,
		preflights.PreflightNameNodeAgentVersion,
	} {
		_, ok := reg.LookupNodePreflight(name)
		assert.True(t, ok, "node preflight %q must be registered", name)
	}
}

func TestDefaultSets_HaveExpectedNames(t *testing.T) {
	t.Parallel()

	cluster := preflights.DefaultClusterPreflights()
	require.Len(t, cluster, 2)
	assert.Equal(t, preflights.PreflightNameNodeAgentReachable, cluster[0].Name,
		"agent-reachable must run first — it's the cheapest physical-layer check, so an unreachable agent surfaces as itself rather than as a misleading apiserver-health failure on the next check's CP walk")
	assert.Equal(t, preflights.PreflightNameKubeAPIHealthy, cluster[1].Name,
		"kube-api-healthy runs after the reachability check so a broken agent is reported before the apiserver probe tries to use it")

	node := preflights.DefaultNodePreflights()
	assert.Empty(t, node, "node-level defaults are intentionally empty; agent-ready and -version are opt-in only")
}
