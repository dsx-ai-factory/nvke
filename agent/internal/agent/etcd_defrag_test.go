// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

// fakeOperationProgress captures what the operation would have persisted.
type fakeOperationProgress struct {
	steps  []string
	result string
}

func (f *fakeOperationProgress) SetStep(step string)     { f.steps = append(f.steps, step) }
func (f *fakeOperationProgress) SetResult(result string) { f.result = result }

// a manifest whose bootstrap-defrag threshold is 100 MiB, small enough that
// tests can sit either side of it with round numbers.
const etcdManifestThreshold100MiB = `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --quota-backend-bytes=17179869184
        - --bootstrap-defrag-threshold-megabytes=100
`

func TestEtcdDefragPreflight(t *testing.T) {
	const mib = int64(1024 * 1024)

	tests := []struct {
		name        string
		manifest    string
		client      fakeEtcdMemberClient
		wantRefused bool
		wantReason  string
	}{
		{
			name:     "passes when reclaimable space clears the threshold",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
		},
		{
			name:     "refuses when reclaimable space is below the threshold",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      150 * mib,
				dbSizeInUse: 100 * mib,
			},
			wantRefused: true,
			wantReason:  "below the configured bootstrap-defrag threshold",
		},
		{
			name: "refuses when no bootstrap-defrag flag is configured",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			wantRefused: true,
			wantReason:  "would not defragment at any size",
		},
		{
			name:     "refuses below the voting member floor",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembers(), // two voters
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			wantRefused: true,
			wantReason:  "2 of 2 voting members are available, need 3",
		},
		{
			name:     "refuses when learners pad the member count",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members: []*etcdserverpb.Member{
					{ID: 1, Name: "cp-1"},
					{ID: 2, Name: "cp-2"},
					{ID: 3, Name: "learner", IsLearner: true},
				},
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			// The learner is excluded from the registered count, and the one
			// voting peer advertises no client URL so it cannot be probed and
			// cannot serve clients either.
			wantRefused: true,
			wantReason:  "1 of 2 voting members are available, need 3",
		},
		{
			name:     "refuses a local member carrying its own alarm",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
				alarms: []*etcdserverpb.AlarmMember{
					{MemberID: 2, Alarm: etcdserverpb.AlarmType_NOSPACE},
				},
			},
			wantRefused: true,
			wantReason:  "unhealthy",
		},
		{
			// Alarms are replicated by raft, so every member reports the whole
			// cluster's set and only the MemberID says whose it is. Charging a
			// peer's alarm to this member refused the very defragmentation
			// NOSPACE calls for.
			name:     "proceeds when the alarm belongs to another member",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
				alarms: []*etcdserverpb.AlarmMember{
					{MemberID: 3, Alarm: etcdserverpb.AlarmType_NOSPACE},
				},
			},
		},
		{
			name:     "refuses a cluster with no leader",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    0,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			wantRefused: true,
			wantReason:  "no leader",
		},
		{
			// etcd treats a threshold of zero as disabled. Reporting it as
			// configured made the gate compare "reclaimable < 0", which is never
			// true, so the member was restarted for a defragmentation etcd was
			// never going to perform - the downtime-only restart this RPC exists
			// to refuse.
			name: "refuses an explicitly disabled threshold of zero",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --bootstrap-defrag-threshold-megabytes=0
`,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				leaderID:    1,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			wantRefused: true,
			wantReason:  "to a positive value",
		},
		{
			// MemberList reports membership, not liveness. A registered count
			// sees three voters here and allows the restart, which would leave
			// one voter against a quorum of two.
			name:     "refuses when a registered voter is offline",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:          testEtcdMembersThree(),
				localID:          2,
				leaderID:         1,
				dbSize:           500 * mib,
				dbSizeInUse:      100 * mib,
				unavailablePeers: map[string]bool{"https://cp-3:2379": true},
			},
			wantRefused: true,
			wantReason:  "2 of 3 voting members are available, need 3",
		},
		{
			name:     "refuses when the member cannot be reached",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:   testEtcdMembersThree(),
				localID:   2,
				statusErr: errors.New("connection refused"),
			},
			wantRefused: true,
			wantReason:  "local etcd member is unhealthy: connection refused",
		},
		{
			// A status with no header is as unusable as no status at all: the
			// member ID is what the leadership check reads. The pre-patch gate
			// used to accept it and only the post-restart wait would notice.
			name:     "refuses a status with no header",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members: testEtcdMembersThree(),
				localID: 2,
				statusFn: func(context.Context, int) (*clientv3.StatusResponse, error) {
					return &clientv3.StatusResponse{}, nil
				},
			},
			wantRefused: true,
			wantReason:  "etcd status has no header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			client := tt.client

			_, _, err := s.etcdDefragPreflight(context.Background(), &client)

			if !tt.wantRefused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantReason)
		})
	}
}

func TestPickEtcdLeadershipTransferee(t *testing.T) {
	tests := []struct {
		name    string
		members []*etcdserverpb.Member
		localID uint64
		wantID  uint64
		wantOK  bool
	}{
		{
			name:    "picks another voting member",
			members: testEtcdMembersThree(),
			localID: 2,
			wantID:  1,
			wantOK:  true,
		},
		{
			name: "skips learners, which cannot lead",
			members: []*etcdserverpb.Member{
				{ID: 1, Name: "learner", IsLearner: true},
				{ID: 3, Name: "cp-3"},
			},
			localID: 2,
			wantID:  3,
			wantOK:  true,
		},
		{
			name: "skips members that have not started",
			members: []*etcdserverpb.Member{
				{ID: 1, Name: ""},
				{ID: 3, Name: "cp-3"},
			},
			localID: 2,
			wantID:  3,
			wantOK:  true,
		},
		{
			name:    "reports none available on a single-member cluster",
			members: []*etcdserverpb.Member{{ID: 2, Name: "cp-2"}},
			localID: 2,
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickEtcdLeadershipTransferee(tt.members, tt.localID)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantID, got)
			}
		})
	}
}

func TestTransferEtcdLeadershipAway(t *testing.T) {
	t.Run("is a no-op when the local member is a follower", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1}

		require.NoError(t, s.transferEtcdLeadershipAway(context.Background(), client, testEtcdMembersThree()))
		assert.Zero(t, client.movedTo, "a follower must not trigger a leadership transfer")
	})

	t.Run("moves leadership off the local member", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 2}

		require.NoError(t, s.transferEtcdLeadershipAway(context.Background(), client, testEtcdMembersThree()))
		assert.Equal(t, uint64(1), client.movedTo)
		assert.Equal(t, []string{"https://cp-2:2379"}, client.movedFrom,
			"MoveLeader must be issued against the current leader, which is the local member")
	})

	t.Run("fails when there is nobody to hand leadership to", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{
			members:  []*etcdserverpb.Member{{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}}},
			localID:  2,
			leaderID: 2,
		}

		err := s.transferEtcdLeadershipAway(context.Background(), client, client.members)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no other voting etcd member")
	})
}

func TestRecordEtcdDefragOutcomes(t *testing.T) {
	t.Run("a refusal completes the operation successfully", func(t *testing.T) {
		s := &NodeAgentServer{}
		progress := &fakeOperationProgress{}

		// A refusal means nothing was touched, so the operation itself has not
		// failed; the REFUSED outcome carries the reason.
		err := s.recordEtcdDefragRefusal(progress, "below threshold", 123)
		require.NoError(t, err)

		var result persistedEtcdDefragResult
		require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_REFUSED), result.Outcome)
		assert.Equal(t, "below threshold", result.ErrorMessage)
		assert.Equal(t, int64(123), result.SizeBefore)
	})

	t.Run("an unrecovered member fails the operation and records the outcome", func(t *testing.T) {
		s := &NodeAgentServer{}
		progress := &fakeOperationProgress{}

		// Both signals matter: a caller watching only the operation phase must
		// see a failure, and one reading the outcome must see that etcd is down.
		err := s.recordEtcdDefragFailure(progress, "etcd never came back", 456)
		require.Error(t, err)

		var result persistedEtcdDefragResult
		require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
		assert.Equal(t, int64(456), result.SizeBefore)
	})
}

// etcdCrictlRunner answers the `crictl ps` probes the static-pod waits make, by
// modelling what actually decides the answer: kubelet replaces the etcd
// container whenever the manifest is removed or its content changes, and the
// replacement carries a new container ID. Both etcd RPCs depend on that - the
// defrag flow moves the manifest aside and back, the patch flow rewrites it in
// place - and the ID changing is the only signal that distinguishes the new pod
// from the old one still serving during kubelet's scan window.
//
// Driving this off the file on disk rather than a fixed script is what lets the
// tests exercise the real stop and restore waits, including a manifest that is
// parked and never comes back.
type etcdCrictlRunner struct {
	t            *testing.T
	manifestPath string

	// stuck models a container that will not exit while its manifest is away,
	// which is the condition that times the stop phase out and would otherwise
	// leave etcd parked.
	stuck bool
	// failAfterMove makes every probe fail once the manifest has been moved
	// aside, modelling a single failed `crictl ps` during the exit wait. That
	// aborts the stop phase at the point where the manifest is already parked.
	failAfterMove bool
	// startDelay is how many probes report nothing running after a swap,
	// forcing the restart wait to actually sleep. Without it the replacement
	// appears on the first probe and no cancellation could ever be observed.
	startDelay int

	generation   int
	lastContent  string
	observed     bool
	absent       bool
	delayedSoFar int
	calls        int
}

func (r *etcdCrictlRunner) Run(name string, _ []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.t.Helper()
	require.Equal(r.t, "crictl", name, "the static-pod waits should only shell out to crictl")
	r.calls++

	content, err := os.ReadFile(r.manifestPath)
	if err != nil {
		r.absent = true
		if r.failAfterMove {
			return nil, errors.New("crictl ps: connection to the container runtime failed")
		}
		if r.stuck {
			return etcdCrictlPS(r.containerID()), nil
		}
		return etcdCrictlPS(), nil
	}
	if r.absent && r.failAfterMove {
		// The runtime is still unreachable while the manifest is being put back.
		return nil, errors.New("crictl ps: connection to the container runtime failed")
	}
	if !r.observed {
		r.observed = true
		r.lastContent = string(content)
		return etcdCrictlPS(r.containerID()), nil
	}
	if r.absent || string(content) != r.lastContent {
		r.absent = false
		r.lastContent = string(content)
		r.generation++
		r.delayedSoFar = 0
	}
	if r.delayedSoFar < r.startDelay {
		r.delayedSoFar++
		return etcdCrictlPS(), nil
	}
	return etcdCrictlPS(r.containerID()), nil
}

func (r *etcdCrictlRunner) containerID() string {
	return fmt.Sprintf("etcd-%d", r.generation)
}

func (r *etcdCrictlRunner) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	out, err := r.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: out}, err
}

func (*etcdCrictlRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func etcdCrictlPS(ids ...string) []byte {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q}`, id))
	}
	return []byte(`{"containers":[` + strings.Join(entries, ",") + `]}`)
}

// etcdRestartTestServer builds a control-plane server whose manifest sits in a
// temp root, with every wait compressed so the timeout paths run in
// milliseconds instead of minutes.
func etcdRestartTestServer(t *testing.T, client *fakeEtcdMemberClient) (*NodeAgentServer, *etcdCrictlRunner) {
	t.Helper()
	s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
	runner := &etcdCrictlRunner{t: t, manifestPath: s.etcdManifestPath()}
	s.etcdClient = client
	s.Runner = runner
	s.etcdHealthTimeout = 100 * time.Millisecond
	s.etcdHealthPollInterval = time.Millisecond
	s.etcdExitTimeout = 100 * time.Millisecond
	s.drainKubeVipPollInterval = time.Millisecond
	return s, runner
}

// etcdSizingStatus reports a shrinking backend: the probes before the restart
// see sizeBefore and everything after sees sizeAfter, which is how the RPC
// measures what the restart reclaimed.
func etcdSizingStatus(sizeBefore, sizeInUse, sizeAfter int64, firstPostRestartCall int) func(context.Context, int) (*clientv3.StatusResponse, error) {
	return func(_ context.Context, call int) (*clientv3.StatusResponse, error) {
		dbSize := sizeBefore
		if call >= firstPostRestartCall {
			dbSize = sizeAfter
		}
		return &clientv3.StatusResponse{
			Header:      &etcdserverpb.ResponseHeader{MemberId: 2},
			Leader:      1,
			DbSize:      dbSize,
			DbSizeInUse: sizeInUse,
		}, nil
	}
}

func decodeDefragResult(t *testing.T, progress *fakeOperationProgress) persistedEtcdDefragResult {
	t.Helper()
	var result persistedEtcdDefragResult
	require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
	return result
}

func etcdManifestIsActive(t *testing.T, s *NodeAgentServer) bool {
	t.Helper()
	_, err := os.Stat(s.etcdManifestPath())
	return err == nil
}

func TestRunEtcdDefragRestartSequence(t *testing.T) {
	const mib = int64(1024 * 1024)

	t.Run("a member that comes back smaller reports what it reclaimed", func(t *testing.T) {
		// Status call 3 is the first probe after the restart: calls 1 and 2 are
		// the preflight and the leadership check.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 3),
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		require.NoError(t, s.runEtcdDefrag(context.Background(), progress))

		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED), result.Outcome)
		assert.Equal(t, 500*mib, result.SizeBefore)
		assert.Equal(t, 120*mib, result.SizeAfter)
		assert.Equal(t, 380*mib, result.Reclaimed)
		assert.Contains(t, progress.steps, stepEtcdDefragWaitingHealthy)
		assert.True(t, etcdManifestIsActive(t, s), "the manifest must be back in place")
	})

	t.Run("a member that comes back the same size is NOT_RECLAIMED, not a failure", func(t *testing.T) {
		// Live data cannot be defragmented away. The caller has to be able to
		// tell that apart from a broken restart, or it will retry forever.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: etcdSizingStatus(500*mib, 100*mib, 500*mib, 3),
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		require.NoError(t, s.runEtcdDefrag(context.Background(), progress))

		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED), result.Outcome)
		assert.Zero(t, result.Reclaimed)
	})

	// The regression this exists for: stopStaticPod moves the manifest aside
	// before it waits for the container to exit, so a timeout in that wait used
	// to return straight out and leave etcd.yaml parked with etcd down and
	// nothing scheduled to bring it back.
	t.Run("a stop that times out still puts the manifest back", func(t *testing.T) {
		client := &fakeEtcdMemberClient{
			members:     testEtcdMembersThree(),
			localID:     2,
			leaderID:    1,
			dbSize:      500 * mib,
			dbSizeInUse: 100 * mib,
		}
		s, runner := etcdRestartTestServer(t, client)
		runner.stuck = true
		progress := &fakeOperationProgress{}

		err := s.runEtcdDefrag(context.Background(), progress)

		// Nil, because the member is verifiably back: this is a clean stop for
		// a rollout, not a node that needs an operator.
		require.NoError(t, err)
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED), result.Outcome)
		assert.Contains(t, result.ErrorMessage, "stop etcd static pod")
		assert.True(t, etcdManifestIsActive(t, s), "a failed stop must not leave the manifest parked")
	})

	t.Run("a member that never returns healthy is FAILED_NOT_RESTORED and errors", func(t *testing.T) {
		// The manifest is already back by the time the health wait runs, so
		// there is nothing left for the agent to restore here - only an
		// operator can help an etcd that will not start from its own manifest.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: func(_ context.Context, call int) (*clientv3.StatusResponse, error) {
				// Calls 1 and 2 are the preflight and the leadership check;
				// everything from 3 on is the post-restart health wait.
				if call >= 3 {
					return nil, errors.New("connection refused")
				}
				return &clientv3.StatusResponse{
					Header:      &etcdserverpb.ResponseHeader{MemberId: 2},
					Leader:      1,
					DbSize:      500 * mib,
					DbSizeInUse: 100 * mib,
				}, nil
			},
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		err := s.runEtcdDefrag(context.Background(), progress)

		require.Error(t, err, "a node needing intervention must fail the operation, not just the outcome")
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
		assert.Contains(t, result.ErrorMessage, "did not return healthy")
		assert.True(t, etcdManifestIsActive(t, s))
	})
}

// The regression this exists for: restoreStaticPod listed containers before
// renaming the manifest back and returned on a listing error, so a single
// failed `crictl ps` after the move left etcd.yaml parked with etcd down. The
// listing is only the baseline for the new-container gate, so losing it has to
// weaken that gate rather than skip the rename.
func TestRunEtcdDefragPutsTheManifestBackWhenCrictlFailsAfterTheMove(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:     testEtcdMembersThree(),
		localID:     2,
		leaderID:    1,
		dbSize:      500 * mib,
		dbSizeInUse: 100 * mib,
	}
	s, runner := etcdRestartTestServer(t, client)
	runner.failAfterMove = true
	progress := &fakeOperationProgress{}

	err := s.runEtcdDefrag(context.Background(), progress)

	// The member cannot be verified while the runtime is unreachable, so this
	// is honestly FAILED_NOT_RESTORED - but the manifest is back, which is what
	// decides whether etcd can come up at all.
	require.Error(t, err)
	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
	assert.True(t, etcdManifestIsActive(t, s), "a crictl failure after the move must not leave etcd parked")
}

// The regression this exists for: clientv3 waits for the connection to become
// ready instead of failing fast, and the defragmentation path runs on the
// operation framework's deadline-free background context. One call against a
// member that never serves blocked forever while holding staticPodMutationMu,
// leaving the operation RUNNING and every other static-pod transaction -
// certificate renewal, kube-vip restore, apiserver patching - stuck behind it
// until the agent restarted. The health wait's own limit does not help, because
// it is only consulted after a call returns.
func TestRunEtcdDefragDoesNotBlockOnAnUnresponsiveMember(t *testing.T) {
	client := &fakeEtcdMemberClient{
		members:  testEtcdMembersThree(),
		localID:  2,
		leaderID: 1,
		statusFn: func(ctx context.Context, _ int) (*clientv3.StatusResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	s, _ := etcdRestartTestServer(t, client)
	s.etcdCallTimeout = 20 * time.Millisecond
	progress := &fakeOperationProgress{}

	done := make(chan error, 1)
	go func() { done <- s.runEtcdDefrag(context.Background(), progress) }()

	select {
	case err := <-done:
		// A member that will not answer is a refusal: nothing was touched.
		require.NoError(t, err)
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_REFUSED), result.Outcome)
		assert.True(t, etcdManifestIsActive(t, s))
	case <-time.After(10 * time.Second):
		t.Fatal("runEtcdDefrag blocked on an etcd call; every call must carry its own deadline")
	}
}

// The regression this exists for: a retry after an attempt died with the
// manifest parked read an absent manifest in preflight and reported
// OUTCOME_REFUSED, which states the node is untouched - while etcd was in fact
// down and this RPC was the only thing that could put the manifest back.
func TestRunEtcdDefragRecoversAParkedManifest(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:  testEtcdMembersThree(),
		localID:  2,
		leaderID: 1,
		// Local Status calls in order: the health wait after recovery, the
		// preflight, the leadership check, then the post-restart health wait.
		statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 4),
	}
	s, _ := etcdRestartTestServer(t, client)

	// Leave the manifest where a failed attempt would have left it.
	parked := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, etcdManifestName))
	require.NoError(t, os.MkdirAll(filepath.Dir(parked), 0o700))
	require.NoError(t, os.Rename(s.etcdManifestPath(), parked))
	require.False(t, etcdManifestIsActive(t, s), "precondition: the manifest starts parked")

	progress := &fakeOperationProgress{}
	require.NoError(t, s.runEtcdDefrag(context.Background(), progress))

	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED), result.Outcome,
		"a retry must recover and then do its job, not refuse")
	assert.Contains(t, progress.steps, stepEtcdDefragRecovering)
	assert.True(t, etcdManifestIsActive(t, s))
}

// The regression this exists for: the in-band recovery above was unreachable in
// the one case it was written for. An agent restart with etcd.yaml parked
// leaves a RUNNING record and no worker, and startNodeAgentOperation then
// rejects every retry - the same ID as inactive, a fresh one as already
// running, because the type check matches on the phase rather than on a live
// worker. runEtcdDefrag never ran, so nothing ever put the manifest back.
func TestStartEtcdDefragClearsARecordLeftRunningByAnAgentRestart(t *testing.T) {
	const mib = int64(1024 * 1024)
	tests := []struct {
		name    string
		retryID string
	}{
		{name: "retried under the same operation ID", retryID: "defrag-1"},
		{name: "retried under a fresh operation ID", retryID: "defrag-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeEtcdMemberClient{
				members:  testEtcdMembersThree(),
				localID:  2,
				leaderID: 1,
				statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 3),
			}
			s, _ := etcdRestartTestServer(t, client)
			s.Persister = &MockPersister{}
			// What the agent reads back after dying mid-defragmentation.
			s.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
				"defrag-1": {
					ID:    "defrag-1",
					Type:  operationTypeEtcdDefrag,
					Phase: NodeAgentOperationPhaseRunning,
					Step:  stepEtcdDefragWaitingExit,
				},
			}

			resp, err := s.StartEtcdDefrag(context.Background(), &pb.StartEtcdDefragRequest{OperationId: tt.retryID})
			require.NoError(t, err, "a record left RUNNING by an agent restart must not block the retry that recovers from it")
			require.Equal(t, tt.retryID, resp.GetOperation().GetOperationId())

			require.Eventually(t, func() bool {
				op, getErr := s.getNodeAgentOperation(nodeAgentOperationSpec{ID: tt.retryID, Type: operationTypeEtcdDefrag})
				return getErr == nil && nodeAgentOperationTerminal(op.Phase)
			}, 10*time.Second, time.Millisecond, "the retry must actually run")
		})
	}
}

// The regression this exists for: a five-voter cluster whose first eligible
// member is offline. The quorum floor allows the restart with four of five
// available, but the transferee was chosen from the raw membership, so
// MoveLeader was issued at a member that was already down - blocking to the
// call deadline and refusing a defragmentation the cluster could have taken.
func TestRunEtcdDefragPicksAReachableLeadershipTransferee(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members: []*etcdserverpb.Member{
			{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
			{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
			{ID: 3, Name: "cp-3", ClientURLs: []string{"https://cp-3:2379"}},
			{ID: 4, Name: "cp-4", ClientURLs: []string{"https://cp-4:2379"}},
			{ID: 5, Name: "cp-5", ClientURLs: []string{"https://cp-5:2379"}},
		},
		localID:          2,
		unavailablePeers: map[string]bool{"https://cp-1:2379": true},
		// The local member leads, so leadership has to move before the restart.
		statusFn: func(_ context.Context, call int) (*clientv3.StatusResponse, error) {
			dbSize := 500 * mib
			if call >= 3 {
				dbSize = 120 * mib
			}
			return &clientv3.StatusResponse{
				Header:      &etcdserverpb.ResponseHeader{MemberId: 2},
				Leader:      2,
				DbSize:      dbSize,
				DbSizeInUse: 100 * mib,
			}, nil
		},
	}
	s, _ := etcdRestartTestServer(t, client)
	progress := &fakeOperationProgress{}

	require.NoError(t, s.runEtcdDefrag(context.Background(), progress))

	assert.Equal(t, uint64(3), client.movedTo,
		"leadership must go to a member that answered a probe, not to the offline cp-1")
}

// staticPodListRunner replays a fixed sequence of crictl listings, so a test
// can describe exactly what the restart wait observes.
type staticPodListRunner struct {
	t         *testing.T
	listings  [][]string
	callIndex int
}

func (r *staticPodListRunner) Run(name string, _ []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.t.Helper()
	require.Equal(r.t, "crictl", name)
	listing := r.listings[min(r.callIndex, len(r.listings)-1)]
	r.callIndex++
	return etcdCrictlPS(listing...), nil
}

func (r *staticPodListRunner) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	out, err := r.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: out}, err
}

func (*staticPodListRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

// The regression this exists for: when the pre-restore listing failed, the
// baseline was set to nil, and newStaticPodContainerIDs treats an empty
// baseline as "anything counts". The wait then accepted the container that was
// still running from before the move, reporting a restart that had not
// happened. That nil also carries a second, deliberate meaning three lines
// later - no swap expected - so the two had to be told apart rather than
// merged.
func TestWaitForStaticPodRunningWithAnUnknownBaseline(t *testing.T) {
	s := &NodeAgentServer{drainKubeVipPollInterval: time.Millisecond}

	t.Run("rejects the container that was already running", func(t *testing.T) {
		runner := &staticPodListRunner{t: t, listings: [][]string{{"etcd-old"}}}

		err := s.waitForStaticPodRunning(context.Background(), runner, etcdContainerName,
			staticPodBaseline{unknown: true}, 50*time.Millisecond)

		require.Error(t, err, "an unknown baseline must not accept a container that never went away")
		assert.Contains(t, err.Error(), "container not running after")
	})

	t.Run("accepts a container that appears after the old one is gone", func(t *testing.T) {
		runner := &staticPodListRunner{t: t, listings: [][]string{{"etcd-old"}, {}, {"etcd-new"}}}

		require.NoError(t, s.waitForStaticPodRunning(context.Background(), runner, etcdContainerName,
			staticPodBaseline{unknown: true}, time.Second))
	})

	t.Run("accepts any container when no swap is expected", func(t *testing.T) {
		// The manifest was already in place, so nothing was renamed and there
		// is no restart to wait for.
		runner := &staticPodListRunner{t: t, listings: [][]string{{"etcd-old"}}}

		require.NoError(t, s.waitForStaticPodRunning(context.Background(), runner, etcdContainerName,
			staticPodBaseline{anyContainerSatisfies: true}, time.Second))
	})
}

func testEtcdMembersThree() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
		{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
		{ID: 3, Name: "cp-3", ClientURLs: []string{"https://cp-3:2379"}},
	}
}
