// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

const (
	testDefragThresholdBytes = 200 << 20
	testReclaimableOver      = testDefragThresholdBytes + 1
)

// fakeDefragClient answers the three defragmentation RPCs. statusQueue lets a
// test walk the agent through RUNNING before a terminal phase; when it is
// exhausted the last entry repeats.
type fakeDefragClient struct {
	etcdStatus    *nvkeproto.GetEtcdStatusResponse
	etcdStatusErr error
	etcdStatusHit int

	// startErrs is consumed one entry per StartEtcdDefrag call. A nil entry or
	// an exhausted slice means the start succeeds, so a test can fail the first
	// attempt and let the retry through.
	startErrs []error
	startReqs []*nvkeproto.StartEtcdDefragRequest
	// recordBeforeStartErr records the operation before a failing start
	// returns, which is the lost-reply case: the agent accepted and published
	// it, and only the response did not arrive.
	recordBeforeStartErr bool
	// startsAccepted counts starts that created a record, as opposed to
	// returning one the agent already held. It is what separates a retry
	// resuming from a retry defragmenting a second time.
	startsAccepted int

	// recorded mirrors the agent holding a record under the operation ID.
	// Until a start publishes one, GetEtcdDefragStatus answers NotFound the way
	// the agent's getNodeAgentOperation does. Setting it up front is a node
	// whose defragmentation an earlier attempt already began.
	recorded bool
	// resolveErr replaces that NotFound, for an agent that cannot be asked.
	resolveErr error
	// blockResolve hangs the resolve probe until the context ends, which is
	// what an agent that accepts a connection and never answers looks like.
	blockResolve bool

	statusQueue   []*nvkeproto.GetEtcdDefragStatusResponse
	statusErrs    []error
	defragReqs    []*nvkeproto.GetEtcdDefragStatusRequest
	defragStatusI int
}

func (c *fakeDefragClient) GetEtcdStatus(
	_ context.Context,
	_ *nvkeproto.GetEtcdStatusRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.GetEtcdStatusResponse, error) {
	c.etcdStatusHit++
	if c.etcdStatusErr != nil {
		return nil, c.etcdStatusErr
	}
	return c.etcdStatus, nil
}

func (c *fakeDefragClient) StartEtcdDefrag(
	_ context.Context,
	req *nvkeproto.StartEtcdDefragRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.StartEtcdDefragResponse, error) {
	c.startReqs = append(c.startReqs, req)
	if i := len(c.startReqs) - 1; i < len(c.startErrs) && c.startErrs[i] != nil {
		if c.recordBeforeStartErr {
			c.record()
		}
		return nil, c.startErrs[i]
	}
	c.record()
	return &nvkeproto.StartEtcdDefragResponse{}, nil
}

// record mirrors startNodeAgentOperation: an ID the agent already holds is
// returned as it stands rather than run a second time.
func (c *fakeDefragClient) record() {
	if c.recorded {
		return
	}
	c.recorded = true
	c.startsAccepted++
}

func (c *fakeDefragClient) GetEtcdDefragStatus(
	ctx context.Context,
	req *nvkeproto.GetEtcdDefragStatusRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.GetEtcdDefragStatusResponse, error) {
	c.defragReqs = append(c.defragReqs, req)

	if !c.recorded {
		if c.blockResolve {
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if c.resolveErr != nil {
			return nil, c.resolveErr
		}
		return nil, status.Errorf(codes.NotFound, "operation %s of type etcd-defrag not found", req.GetOperationId())
	}

	i := c.defragStatusI
	c.defragStatusI++
	if i < len(c.statusErrs) && c.statusErrs[i] != nil {
		return nil, c.statusErrs[i]
	}
	if i >= len(c.statusQueue) {
		i = len(c.statusQueue) - 1
	}
	return c.statusQueue[i], nil
}

// fragmentedStatus is a member worth restarting: threshold configured, and
// enough reclaimable space to clear it.
func fragmentedStatus() *nvkeproto.GetEtcdStatusResponse {
	return &nvkeproto.GetEtcdStatusResponse{
		BootstrapDefragThresholdBytes: testDefragThresholdBytes,
		ReclaimableBytes:              testReclaimableOver,
		Healthy:                       true,
		VotingMemberCount:             3,
	}
}

func terminal(outcome nvkeproto.GetEtcdDefragStatusResponse_Outcome) *nvkeproto.GetEtcdDefragStatusResponse {
	return &nvkeproto.GetEtcdDefragStatusResponse{
		Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED},
		Outcome:   outcome,
	}
}

// newDefragOperationForTest wires a single client behind the dialer and polls
// fast so the tests do not sleep.
func newDefragOperationForTest(client *fakeDefragClient) *DefragOperation {
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(2*time.Second))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		return client, func() {}, nil
	}
	return op
}

func defragPayload(t *testing.T, base string) rollout.ApplyToNodePayload {
	t.Helper()
	raw, err := json.Marshal(defragArtifact{OperationIDBase: base})
	require.NoError(t, err)
	return raw
}

func controlPlaneNode(name string) fakeNode {
	return fakeNode{id: uuid.New(), name: name, role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
}

func TestDefragOperationApplyToNodeOutcomeMapping(t *testing.T) {
	tests := []struct {
		name        string
		agent       nvkeproto.GetEtcdDefragStatusResponse_Outcome
		want        rollout.Outcome
		wantErr     bool
		errContains string
	}{
		{
			name:  "reclaimed is the success path",
			agent: nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED,
			want:  rollout.OutcomeApplied,
		},
		{
			// The member restarted and came back healthy holding mostly live
			// data. Reporting failure here would halt the rollout and stop
			// counting a healthy member toward the replica floor.
			name:  "reclaiming nothing is unchanged, not a failure",
			agent: nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED,
			want:  rollout.OutcomeUnchanged,
		},
		{
			// The pre-check already filtered benign refusals, so one arriving
			// here means the cluster changed underneath the rollout.
			name:        "a refusal after the pre-check halts without cascading",
			agent:       nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_REFUSED,
			want:        rollout.OutcomeFailedRestored,
			wantErr:     true,
			errContains: "refused",
		},
		{
			name:        "failed but restored halts cleanly",
			agent:       nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED,
			want:        rollout.OutcomeFailedRestored,
			wantErr:     true,
			errContains: "restored",
		},
		{
			name:        "failed and not restored cascades",
			agent:       nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED,
			want:        rollout.OutcomeFailedNotRestored,
			wantErr:     true,
			errContains: "out of service",
		},
		{
			// etcd may be down with kube-apiserver parked behind it, so an
			// agent that reports no verdict must not read as success.
			name:        "an unspecified outcome is treated as unknown node state",
			agent:       nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_UNSPECIFIED,
			want:        rollout.OutcomeFailedNotRestored,
			wantErr:     true,
			errContains: "no etcd defragmentation outcome",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeDefragClient{
				etcdStatus:  fragmentedStatus(),
				statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(tt.agent)},
			}
			op := newDefragOperationForTest(client)

			outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

			assert.Equal(t, tt.want, outcome)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				require.NoError(t, err)
			}
			assert.Len(t, client.startReqs, 1, "a member worth defragmenting must be started exactly once")
		})
	}
}

// An outcome the controller has never heard of must not read as success: a new
// agent enum value is exactly the case where guessing is dangerous.
func TestDefragOperationUnknownOutcomeFailsClosed(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{{
			Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED},
			Outcome:   nvkeproto.GetEtcdDefragStatusResponse_Outcome(99),
		}},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown etcd defragmentation outcome")
}

func TestDefragOperationPreCheckSkipsMembersNotWorthRestarting(t *testing.T) {
	tests := []struct {
		name   string
		status *nvkeproto.GetEtcdStatusResponse
	}{
		{
			// A node provisioned before the threshold was configured. etcd
			// treats the flag as absent, so a restart is pure downtime however
			// fragmented the file is.
			name: "no bootstrap-defrag threshold configured",
			status: &nvkeproto.GetEtcdStatusResponse{
				BootstrapDefragThresholdBytes: 0,
				ReclaimableBytes:              8 << 30,
				Healthy:                       true,
			},
		},
		{
			// Sitting a little over live-data size is the normal steady state,
			// not a fault to remediate.
			name: "reclaimable space below the member's threshold",
			status: &nvkeproto.GetEtcdStatusResponse{
				BootstrapDefragThresholdBytes: testDefragThresholdBytes,
				ReclaimableBytes:              testDefragThresholdBytes - 1,
				Healthy:                       true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeDefragClient{etcdStatus: tt.status}
			op := newDefragOperationForTest(client)

			outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

			require.NoError(t, err)
			assert.Equal(t, rollout.OutcomeUnchanged, outcome)
			assert.Empty(t, client.startReqs, "a member not worth restarting must never be restarted")
		})
	}
}

// Exactly at the threshold etcd would defragment, so the member qualifies.
func TestDefragOperationPreCheckIsInclusiveAtTheThreshold(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: &nvkeproto.GetEtcdStatusResponse{
			BootstrapDefragThresholdBytes: testDefragThresholdBytes,
			ReclaimableBytes:              testDefragThresholdBytes,
			Healthy:                       true,
		},
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.Len(t, client.startReqs, 1)
}

// Two nodes in one rollout share a payload but must not share an operation ID:
// the agent treats it as an idempotency key, so a shared value would hand the
// second node the first node's finished operation.
func TestDefragOperationDerivesAPerNodeOperationID(t *testing.T) {
	seen := map[string]*fakeDefragClient{}
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(2*time.Second))
	op.nodeAgentClientDialer = func(_ context.Context, node rollout.Node) (nodeAgentDefragClient, func(), error) {
		client, ok := seen[node.Name()]
		if !ok {
			client = &fakeDefragClient{
				etcdStatus:  fragmentedStatus(),
				statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
			}
			seen[node.Name()] = client
		}
		return client, func() {}, nil
	}

	payload := defragPayload(t, "rollout-base")
	for _, name := range []string{"cp-0", "cp-1"} {
		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode(name), payload)
		require.NoError(t, err)
		require.Equal(t, rollout.OutcomeApplied, outcome)
	}

	require.Len(t, seen["cp-0"].startReqs, 1)
	require.Len(t, seen["cp-1"].startReqs, 1)
	first := seen["cp-0"].startReqs[0].GetOperationId()
	second := seen["cp-1"].startReqs[0].GetOperationId()
	assert.NotEqual(t, first, second, "two nodes must not share an idempotency key")
	assert.True(t, strings.HasPrefix(first, "rollout-base/"), "the key must derive from the payload base, got %q", first)
	assert.Equal(t, "rollout-base/cp-0", first)
	assert.Equal(t, "rollout-base/cp-1", second)
}

// The same node retried with the same payload resolves the key it already
// used, so an activity retry follows the in-flight defragmentation instead of
// restarting etcd a second time.
func TestDefragOperationRetryResumesRatherThanRestarting(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus:  fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := newDefragOperationForTest(client)
	payload := defragPayload(t, "rollout-base")
	node := controlPlaneNode("cp-0")

	for range 2 {
		outcome, err := op.ApplyToNode(context.Background(), node, payload)
		require.NoError(t, err)
		require.Equal(t, rollout.OutcomeApplied, outcome)
	}

	require.Len(t, client.startReqs, 1, "a retry must resume the recorded operation, not start a second one")
	assert.Equal(t, "rollout-base/cp-0", client.startReqs[0].GetOperationId())
	for _, req := range client.defragReqs {
		assert.Equal(t, "rollout-base/cp-0", req.GetOperationId(), "every call must address the key derived from the payload")
	}
}

// The bug this guards: a retry used to read etcd's status first, which fails
// precisely because the previous attempt has the member down, and reported the
// node restored while kube-apiserver was parked behind it. Resolving the
// operation first means the eligibility read is never reached.
func TestDefragOperationResumesWithoutConsultingEligibility(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatusErr: status.Error(codes.Unavailable, "etcd is restarting"),
		recorded:      true,
		statusQueue:   []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.Zero(t, client.etcdStatusHit, "an operation already in flight must not be re-qualified")
	assert.Empty(t, client.startReqs, "an operation already in flight must not be started again")
}

// The resolve call already carries the agent's verdict when the operation has
// finished. Polling for it again would discard an answer in hand, and polls
// failing from there to the deadline would report unknown state over it.
func TestDefragOperationTerminalResolveIsReturnedWithoutPolling(t *testing.T) {
	client := &fakeDefragClient{
		recorded:    true,
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
		// Every call after the resolve fails, so a second read cannot be what
		// produces the verdict.
		statusErrs: []error{nil, status.Error(codes.Unavailable, "connection refused")},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.Len(t, client.defragReqs, 1, "a terminal resolve must not be followed by a poll")
}

// A resolve that finds the operation still running hands off to the poll loop
// rather than treating a non-terminal phase as a verdict.
func TestDefragOperationNonTerminalResolveKeepsPolling(t *testing.T) {
	client := &fakeDefragClient{
		recorded: true,
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{
			{Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING}},
			terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED),
		},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.GreaterOrEqual(t, len(client.defragReqs), 2, "a RUNNING resolve must hand off to the poll loop")
	assert.Empty(t, client.startReqs)
}

// interruptedOperationErr is what the agent returns for a record left
// non-terminal with no worker behind it, which is an agent restart mid-attempt.
func interruptedOperationErr() error {
	return status.Error(codes.FailedPrecondition,
		"operation base/cp-0 (etcd-defrag) is non-terminal in persisted state, but no worker is active; agent may have restarted during the operation")
}

// StartEtcdDefrag is the only caller of resetInterruptedEtcdDefrag, so an
// interrupted record has to be answered by starting again. Reporting unreadable
// state instead would wedge the node: every retry resolves to the same error
// and nothing ever clears the record.
func TestDefragOperationRecoversFromAnInterruptedAttempt(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus:  fragmentedStatus(),
		resolveErr:  interruptedOperationErr(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.Len(t, client.startReqs, 1, "recovery is a fresh start, which is what clears the stale record")
}

// Recovery is not a blind restart. The interrupted attempt may have got as far
// as restarting etcd, in which case the member is already compact and the
// eligibility check leaves it alone.
func TestDefragOperationInterruptedAttemptThatAlreadyShrankTheMemberIsUnchanged(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: &nvkeproto.GetEtcdStatusResponse{
			BootstrapDefragThresholdBytes: testDefragThresholdBytes,
			ReclaimableBytes:              1,
			Healthy:                       true,
		},
		resolveErr: interruptedOperationErr(),
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeUnchanged, outcome)
	assert.Empty(t, client.startReqs, "a member that no longer needs restarting must not be restarted")
}

// The eligibility read only runs once the resolve has established that nothing
// is in flight under this key, so a failure there is a node this attempt has
// provably not touched -- unlike the same failure on a retry, which the resolve
// now intercepts before reaching here.
func TestDefragOperationEligibilityReadFailureHaltsCleanly(t *testing.T) {
	client := &fakeDefragClient{etcdStatusErr: status.Error(codes.Unavailable, "etcd is unreachable")}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get etcd status from node cp-0")
	assert.Empty(t, client.startReqs)
}

// A dialer that reports success but hands back no client is as unusable as one
// that failed, and unlike an unconfigured dialer it may have reached the node.
func TestDefragOperationNilClientDoesNotClaimRestoration(t *testing.T) {
	cleanedUp := false
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(2*time.Second))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		return nil, func() { cleanedUp = true }, nil
	}

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned nil client")
	assert.True(t, cleanedUp, "the cleanup must run even when the client is unusable")
}

// A dial happens before the operation can be resolved, so it cannot tell a
// first attempt from a retry whose earlier attempt has the member down. The
// agent also listens independently of etcd and kube-apiserver, so failing to
// reach it is not what a node mid-defragmentation looks like.
func TestDefragOperationUnreachableAgentDoesNotClaimRestoration(t *testing.T) {
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(2*time.Second))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		return nil, nil, errors.New("connection refused")
	}

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial agent on node cp-0")
}

// An agent that cannot be asked about the operation leaves the node's state
// unknown, so the verdict must not be the clean halt that FailedRestored means.
func TestDefragOperationUnreadableStateDoesNotClaimRestoration(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		resolveErr: status.Error(codes.Unavailable, "connection refused"),
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read etcd defragmentation state")
	assert.Zero(t, client.etcdStatusHit)
	assert.Empty(t, client.startReqs)
}

func TestDefragOperationPollsUntilTerminal(t *testing.T) {
	running := &nvkeproto.GetEtcdDefragStatusResponse{
		Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING},
	}
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{
			running,
			running,
			terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED),
		},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.GreaterOrEqual(t, len(client.defragReqs), 3, "a RUNNING phase must keep the poll going")
}

// A status the agent has not populated is not a verdict. Reading an unset
// phase as terminal would fall through to the UNSPECIFIED outcome arm and
// cascade a rollback over a node that is busy succeeding.
func TestDefragOperationUnsetPhaseIsNotTerminal(t *testing.T) {
	tests := []struct {
		name    string
		pending *nvkeproto.GetEtcdDefragStatusResponse
	}{
		{
			name:    "status field absent entirely",
			pending: &nvkeproto.GetEtcdDefragStatusResponse{},
		},
		{
			name: "phase explicitly unspecified",
			pending: &nvkeproto.GetEtcdDefragStatusResponse{
				Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeDefragClient{
				etcdStatus: fragmentedStatus(),
				statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{
					tt.pending,
					terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED),
				},
			}
			op := newDefragOperationForTest(client)

			outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

			require.NoError(t, err)
			assert.Equal(t, rollout.OutcomeApplied, outcome)
			assert.GreaterOrEqual(t, len(client.defragReqs), 2, "an unset phase must keep the poll going")
		})
	}
}

// A FAILED phase is terminal even though the outcome carries the detail, so a
// failing node must not be polled until the timeout.
func TestDefragOperationFailedPhaseIsTerminal(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{{
			Operation:    &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED},
			Outcome:      nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED,
			ErrorMessage: "etcd did not come back",
		}},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "etcd did not come back")
	assert.Len(t, client.defragReqs, 2, "the resolve probe, then one poll: a FAILED phase ends it immediately")
}

// One node must cost one dial: the pre-check and the restart share a
// connection rather than opening two.
func TestDefragOperationDialsOncePerNode(t *testing.T) {
	dials := 0
	client := &fakeDefragClient{
		etcdStatus:  fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(2*time.Second))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		dials++
		return client, func() {}, nil
	}

	_, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, 1, dials)
}

// etcd restarts while the agent is unreachable, so a transient poll error is
// expected rather than fatal.
func TestDefragOperationToleratesTransientPollErrors(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus:  fragmentedStatus(),
		statusErrs:  []error{errors.New("connection refused"), errors.New("connection refused")},
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
	}
	op := newDefragOperationForTest(client)

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
}

// A node that never reaches a terminal phase may still have kube-apiserver
// parked behind a down etcd, which is unknown state rather than a clean halt.
func TestDefragOperationTimeoutCascades(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{{
			Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING},
		}},
	}
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(50*time.Millisecond))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		return client, func() {}, nil
	}

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not finish within")
}

// A failed StartEtcdDefrag is classified by starting again rather than by
// asking for status. The agent does not observe the RPC context, so a lost
// reply leaves a handler still on its way to recording the operation, and a
// status probe can be answered NotFound while that is true. A second start
// serializes behind the handler, so it cannot.
func TestDefragOperationStartFailureIsClassifiedByStartingAgain(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "connection refused")
	declined := status.Error(codes.FailedPrecondition, "control-plane node required, current role \"worker\"")

	// An error only the handler can produce proves it ran and returned before
	// touching the node, so there is nothing to establish and nothing to retry.
	t.Run("an agent-produced refusal halts cleanly on the first attempt", func(t *testing.T) {
		client := &fakeDefragClient{
			etcdStatus: fragmentedStatus(),
			startErrs:  []error{declined},
		}
		op := newDefragOperationForTest(client)

		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

		assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "start etcd defragmentation")
		assert.Len(t, client.startReqs, 1, "a refusal the agent authored needs no second attempt")
	})

	// The first start never reached the agent, so the retry is what records the
	// operation. Distinct from the lost-reply case below, where the first start
	// did record it and the retry is answered with that record.
	t.Run("a start that never arrived is recorded by the retry", func(t *testing.T) {
		client := &fakeDefragClient{
			etcdStatus:  fragmentedStatus(),
			startErrs:   []error{unavailable},
			statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
		}
		op := newDefragOperationForTest(client)

		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

		require.NoError(t, err)
		assert.Equal(t, rollout.OutcomeApplied, outcome)
		assert.Len(t, client.startReqs, 2, "an ambiguous failure must be retried exactly once")
		assert.Equal(t, 1, client.startsAccepted, "the retry is the start that lands")
	})

	// Neither attempt reached the agent, so the node's state is unknown.
	// Cascading immediately beats polling a dead agent for the whole timeout.
	t.Run("two ambiguous failures cascade without waiting", func(t *testing.T) {
		client := &fakeDefragClient{
			etcdStatus: fragmentedStatus(),
			startErrs:  []error{unavailable, unavailable},
		}
		op := newDefragOperationForTest(client)

		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

		assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "state could not be established")
		assert.Len(t, client.startReqs, 2)
		assert.Len(t, client.defragReqs, 1, "only the resolve probe runs; a dead agent is not polled")
	})

	// The case the retry exists for: the agent accepted and published the
	// operation and only the reply was lost. The second start must be answered
	// with the record the first one created rather than defragment again.
	t.Run("a lost reply retries into the operation the agent already holds", func(t *testing.T) {
		client := &fakeDefragClient{
			etcdStatus:           fragmentedStatus(),
			startErrs:            []error{unavailable},
			recordBeforeStartErr: true,
			statusQueue:          []*nvkeproto.GetEtcdDefragStatusResponse{terminal(nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED)},
		}
		op := newDefragOperationForTest(client)

		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

		require.NoError(t, err)
		assert.Equal(t, rollout.OutcomeApplied, outcome)
		require.Len(t, client.startReqs, 2)
		assert.Equal(t, client.startReqs[0].GetOperationId(), client.startReqs[1].GetOperationId(),
			"the retry must address the key the lost attempt used")
		assert.Equal(t, 1, client.startsAccepted,
			"the retry must be answered with the existing operation, not defragment a second time")
	})

	// A refusal to the retry proves some handler ran, but not that it was the
	// first start's: the refusal can come from an unrelated defragmentation
	// holding the type while the first start waits on the mutex, and that start
	// can still publish once the other ends. Once the first attempt is
	// ambiguous, nothing the retry says restores the claim of restoration.
	t.Run("a refusal to the retry does not restore the claim the first attempt gave up", func(t *testing.T) {
		client := &fakeDefragClient{
			etcdStatus: fragmentedStatus(),
			startErrs:  []error{unavailable, declined},
		}
		op := newDefragOperationForTest(client)

		outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

		assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "state could not be established")
		assert.Len(t, client.startReqs, 2)
	})
}

// A canceled parent must not be reported as a timeout the node never reached.
func TestDefragOperationCancellationIsNotReportedAsATimeout(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{{
			Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING},
		}},
	}
	op := newDefragOperationForTest(client)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	outcome, err := op.ApplyToNode(ctx, controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was interrupted")
	assert.NotContains(t, err.Error(), "did not finish within")
}

// A caller's own deadline expiring is not the node overrunning its budget.
// Both surface as DeadlineExceeded on the derived context, so only the cause
// attached to this operation's timeout tells them apart.
func TestDefragOperationParentDeadlineIsNotReportedAsTheNodeTimeout(t *testing.T) {
	client := &fakeDefragClient{
		etcdStatus: fragmentedStatus(),
		statusQueue: []*nvkeproto.GetEtcdDefragStatusResponse{{
			Operation: &nvkeproto.NodeAgentOperationStatus{Phase: nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING},
		}},
	}
	op := newDefragOperationForTest(client)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	outcome, err := op.ApplyToNode(ctx, controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was interrupted")
	assert.NotContains(t, err.Error(), "did not finish within")
}

// Nothing in the dialer contract imposes a deadline, so an agent that accepts
// the connection and never answers would hang every call made before the poll
// loop. The node timeout has to cover those too.
func TestDefragOperationTimeoutCoversCallsBeforeThePollLoop(t *testing.T) {
	client := &fakeDefragClient{etcdStatus: fragmentedStatus(), blockResolve: true}
	op := NewDefragOperation(WithDefragPollInterval(time.Millisecond), WithDefragTimeout(50*time.Millisecond))
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
		return client, func() {}, nil
	}

	// Asserting on elapsed time would hang for the package timeout if the
	// bound were ever removed, so the deadline is enforced here instead.
	type result struct {
		outcome rollout.Outcome
		err     error
	}
	payload := defragPayload(t, "base")
	node := controlPlaneNode("cp-0")
	done := make(chan result, 1)
	go func() {
		outcome, err := op.ApplyToNode(context.Background(), node, payload)
		done <- result{outcome, err}
	}()

	select {
	case got := <-done:
		assert.Equal(t, rollout.OutcomeFailedNotRestored, got.outcome)
		require.Error(t, got.err)
		assert.Empty(t, client.startReqs)
	case <-time.After(5 * time.Second):
		t.Fatal("the node timeout must bound a call made before the poll loop")
	}
}

func TestDefragOperationRejectsNonControlPlaneNodes(t *testing.T) {
	client := &fakeDefragClient{etcdStatus: fragmentedStatus()}
	op := newDefragOperationForTest(client)
	worker := fakeNode{id: uuid.New(), name: "worker-0", role: rollout.NodeRoleWorker}

	outcome, err := op.ApplyToNode(context.Background(), worker, defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only supports control-plane nodes")
	assert.Zero(t, client.etcdStatusHit, "a worker must not even be probed")
}

func TestDefragOperationPayloadValidation(t *testing.T) {
	tests := []struct {
		name        string
		payload     rollout.ApplyToNodePayload
		errContains string
	}{
		{name: "empty", payload: nil, errContains: "payload is empty"},
		{name: "not json", payload: []byte("{"), errContains: "parse etcd defragmentation payload"},
		{name: "missing base", payload: []byte(`{}`), errContains: "missing operationIdBase"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeDefragClient{etcdStatus: fragmentedStatus()}
			op := newDefragOperationForTest(client)

			outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), tt.payload)

			assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
			assert.Empty(t, client.startReqs)
		})
	}
}

func TestDefragOperationBuildApplyToNodePayload(t *testing.T) {
	op := NewDefragOperation()
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a"}

	first, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)
	second, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)

	artifact, err := parseDefragPayload(first)
	require.NoError(t, err)
	assert.NotEmpty(t, artifact.OperationIDBase)
	// Each rollout gets its own base, so a later rollout is never mistaken for
	// a retry of an earlier one.
	assert.NotEqual(t, string(first), string(second))

	_, err = op.BuildApplyToNodePayload(context.Background(), nil)
	assert.Error(t, err)
}

func TestDefragOperationIsSatisfied(t *testing.T) {
	belowThreshold := &nvkeproto.GetEtcdStatusResponse{
		BootstrapDefragThresholdBytes: testDefragThresholdBytes,
		ReclaimableBytes:              1,
		Healthy:                       true,
	}

	t.Run("no member is worth restarting", func(t *testing.T) {
		op := NewDefragOperation()
		op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
			return &fakeDefragClient{etcdStatus: belowThreshold}, func() {}, nil
		}
		cluster := &fakeCluster{nodes: []rollout.Node{controlPlaneNode("cp-0"), controlPlaneNode("cp-1")}}

		satisfied, err := op.IsSatisfied(context.Background(), cluster)

		require.NoError(t, err)
		assert.True(t, satisfied)
	})

	// One fragmented member is enough to run the rollout; the executor's own
	// per-node pre-check leaves the others Unchanged.
	t.Run("a single fragmented member runs the rollout", func(t *testing.T) {
		op := NewDefragOperation()
		op.nodeAgentClientDialer = func(_ context.Context, node rollout.Node) (nodeAgentDefragClient, func(), error) {
			if node.Name() == "cp-1" {
				return &fakeDefragClient{etcdStatus: fragmentedStatus()}, func() {}, nil
			}
			return &fakeDefragClient{etcdStatus: belowThreshold}, func() {}, nil
		}
		cluster := &fakeCluster{nodes: []rollout.Node{controlPlaneNode("cp-0"), controlPlaneNode("cp-1")}}

		satisfied, err := op.IsSatisfied(context.Background(), cluster)

		require.NoError(t, err)
		assert.False(t, satisfied)
	})

	t.Run("an unreachable agent is an error rather than a silent skip", func(t *testing.T) {
		op := NewDefragOperation()
		op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentDefragClient, func(), error) {
			return nil, nil, errors.New("unavailable")
		}
		cluster := &fakeCluster{nodes: []rollout.Node{controlPlaneNode("cp-0")}}

		satisfied, err := op.IsSatisfied(context.Background(), cluster)

		require.Error(t, err)
		assert.False(t, satisfied)
	})

	t.Run("a cluster with no control-plane nodes has nothing to do", func(t *testing.T) {
		op := NewDefragOperation()
		satisfied, err := op.IsSatisfied(context.Background(), &fakeCluster{})

		require.NoError(t, err)
		assert.True(t, satisfied)
	})
}

// Unlike an agent that cannot be reached, an operation wired without a dialer
// provably emitted nothing, so this is the clean halt the other is not.
func TestDefragOperationRequiresADialer(t *testing.T) {
	op := NewDefragOperation()

	outcome, err := op.ApplyToNode(context.Background(), controlPlaneNode("cp-0"), defragPayload(t, "base"))

	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dialer is not configured")
}

// Compile must detect the optional capabilities this operation implements, and
// must not invent a RollbackHandler: a defragmentation cannot be undone.
func TestDefragOperationCapabilities(t *testing.T) {
	caps := rollout.Compile(NewDefragOperation()).Capabilities()

	assert.True(t, caps.HasApplyToNodePayloadBuilder)
	assert.True(t, caps.HasSatisfactionChecker)
	assert.False(t, caps.HasRollbackHandler)
	assert.False(t, caps.HasValidator)
	assert.False(t, caps.HasPostConditionChecker)
	assert.Equal(t, OperationNameDefragmentEtcd, rollout.Compile(NewDefragOperation()).Name())
}
