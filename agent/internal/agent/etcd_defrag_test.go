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
	"slices"
	"strconv"
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

const testKubeVipAddress = "192.168.100.1"

// the kube-vip manifest the defragmentation sequence parks and restores. The
// address is what resolveKubeVipAddress reads back out of it.
const testKubeVipManifest = `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: kube-vip
      env:
        - name: address
          value: ` + testKubeVipAddress + `
`

const testKubeAPIServerManifest = `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: kube-apiserver
      command:
        - kube-apiserver
        - --etcd-servers=https://127.0.0.1:2379
`

// controlPlaneNodeFake stands in for the parts of a control-plane node that the
// bundled defragmentation sequence drives: three static pods and the loopback
// address kube-vip advertises.
//
// The static pods are modelled rather than scripted, because what decides every
// `crictl ps` answer is a rule, not a sequence: kubelet runs a container while
// its manifest is present and reaps it when the manifest goes away, and the
// replacement always carries a new ID. That ID change is the only thing telling
// a restarted pod from the old one still serving inside kubelet's scan window,
// so a fake that did not model it would pass waits that production fails. It
// also means the same fake covers both ways this code stops a pod - parking the
// manifest for kube-vip and kube-apiserver, `crictl stop` for etcd - without
// either test knowing which.
type controlPlaneNodeFake struct {
	t    *testing.T
	root string
	pods map[string]*fakeStaticPod

	loopbackVIPs []string

	// crictlFailsOnceWithdrawn makes every crictl call fail from the moment the
	// node is fully out of service, and keeps failing. It models the container
	// runtime going away at the worst moment available: kube-vip and
	// kube-apiserver are both parked and the agent has lost the tool it uses to
	// confirm anything it does about that.
	crictlFailsOnceWithdrawn bool
	crictlFailed             bool
	// vipPeers is what `ss` reports for established sockets on the VIP. A
	// non-empty list fails the quiet window, which is how the withdrawal detects
	// traffic still arriving after the VIP was withdrawn.
	vipPeers string
}

type fakeStaticPod struct {
	manifestName string
	generation   int
	runningID    string
	// stuckExit keeps the container running after its manifest is parked, which
	// is the condition that times the exit wait out.
	stuckExit bool
	// startDelay is how many probes report nothing running before a replacement
	// appears, so the restart wait actually sleeps instead of being satisfied
	// on its first probe.
	startDelay int
	delayed    int
}

func newControlPlaneNodeFake(t *testing.T, s *NodeAgentServer) *controlPlaneNodeFake {
	t.Helper()
	f := &controlPlaneNodeFake{
		t:            t,
		root:         s.RootPath,
		loopbackVIPs: []string{testKubeVipAddress},
		pods: map[string]*fakeStaticPod{
			etcdContainerName:          {manifestName: etcdManifestName},
			kubeAPIServerContainerName: {manifestName: defaultKubeAPIServerManifestName},
			kubeVipContainerName:       {manifestName: defaultKubeVipManifestName},
		},
	}
	// Every pod starts running, which is the state a healthy node is in when
	// the RPC arrives.
	for name, pod := range f.pods {
		pod.generation = 1
		pod.runningID = fmt.Sprintf("%s-1", name)
	}
	return f
}

func (f *controlPlaneNodeFake) manifestParked(manifestName string) bool {
	_, err := os.Stat(filepath.Join(f.root, defaultStaticPodManifestDir, manifestName))
	return errors.Is(err, os.ErrNotExist)
}

// reconcile applies kubelet's rule to one pod before it is reported on.
func (f *controlPlaneNodeFake) reconcile(containerName string) (*fakeStaticPod, error) {
	pod := f.pods[containerName]
	if pod == nil {
		return nil, fmt.Errorf("crictl ps: unmodelled container %q", containerName)
	}
	if f.manifestParked(pod.manifestName) {
		if !pod.stuckExit {
			pod.runningID = ""
			pod.delayed = 0
		}
		return pod, nil
	}
	if pod.runningID == "" {
		if pod.delayed < pod.startDelay {
			pod.delayed++
			return pod, nil
		}
		pod.generation++
		pod.runningID = fmt.Sprintf("%s-%d", containerName, pod.generation)
		pod.delayed = 0
		if containerName == kubeVipContainerName && !slices.Contains(f.loopbackVIPs, testKubeVipAddress) {
			// kube-vip puts the address back on lo itself once it wins the
			// election; the agent only waits for it to appear.
			f.loopbackVIPs = append(f.loopbackVIPs, testKubeVipAddress)
		}
	}
	return pod, nil
}

// Run reports an unexpected command as a command error rather than failing the
// test from here. runEtcdDefrag is driven from a goroutine in at least one
// test, and the testing package does not allow a Fatal from a goroutine other
// than the one running the test.
func (f *controlPlaneNodeFake) Run(name string, args []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	switch name {
	case "crictl":
		return f.runCrictl(args)
	case "ip":
		return f.runIP(args)
	case "ss":
		return []byte(f.vipPeers), nil
	default:
		return nil, fmt.Errorf("unexpected command %q", name)
	}
}

func (f *controlPlaneNodeFake) runCrictl(args []string) ([]byte, error) {
	if f.crictlFailsOnceWithdrawn && (f.crictlFailed || f.manifestParked(defaultKubeAPIServerManifestName)) {
		f.crictlFailed = true
		return nil, errors.New("crictl: connection to the container runtime failed")
	}
	switch args[0] {
	case "ps":
		containerName, err := containerNameFromCrictlPSArgs(args)
		if err != nil {
			return nil, err
		}
		pod, err := f.reconcile(containerName)
		if err != nil {
			return nil, err
		}
		if pod.runningID == "" {
			return crictlPS(), nil
		}
		return crictlPS(pod.runningID), nil
	case "stop":
		// crictl takes the grace period in whole seconds, and zero means kill
		// now. Rejecting it here is what holds the production path to asking
		// for a graceful stop rather than silently rounding down to one.
		if err := assertPositiveCrictlStopTimeout(args); err != nil {
			return nil, err
		}
		id := args[len(args)-1]
		for _, pod := range f.pods {
			if pod.runningID == id {
				pod.runningID = ""
				pod.delayed = 0
				return nil, nil
			}
		}
		return nil, fmt.Errorf("crictl stop: no such container %q", id)
	default:
		return nil, fmt.Errorf("unexpected crictl subcommand %q", args[0])
	}
}

func assertPositiveCrictlStopTimeout(args []string) error {
	for i, arg := range args {
		if arg != "--timeout" || i+1 >= len(args) {
			continue
		}
		seconds, err := strconv.Atoi(args[i+1])
		if err != nil {
			return fmt.Errorf("crictl stop: unparseable --timeout %q", args[i+1])
		}
		if seconds < 1 {
			return fmt.Errorf("crictl stop: --timeout %d would kill the container immediately", seconds)
		}
		return nil
	}
	return errors.New("crictl stop: no --timeout given, so the grace period is crictl's default rather than ours")
}

func (f *controlPlaneNodeFake) runIP(args []string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}):
		lines := make([]string, 0, len(f.loopbackVIPs))
		for i, vip := range f.loopbackVIPs {
			lines = append(lines, fmt.Sprintf("%d: lo    inet %s/32 scope host lo", i+1, vip))
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil
	case len(args) == 5 && args[0] == "addr" && args[1] == "del":
		vip := strings.TrimSuffix(args[2], "/32")
		f.loopbackVIPs = slices.DeleteFunc(f.loopbackVIPs, func(got string) bool { return got == vip })
		return nil, nil
	case len(args) == 5 && args[0] == "addr" && args[1] == "add":
		vip := strings.TrimSuffix(args[2], "/32")
		if !slices.Contains(f.loopbackVIPs, vip) {
			f.loopbackVIPs = append(f.loopbackVIPs, vip)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected ip args %v", args)
	}
}

func (f *controlPlaneNodeFake) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	out, err := f.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: out}, err
}

func (*controlPlaneNodeFake) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func containerNameFromCrictlPSArgs(args []string) (string, error) {
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			return strings.TrimPrefix(args[i+1], "io.kubernetes.container.name="), nil
		}
	}
	return "", fmt.Errorf("crictl ps without a container-name label: %v", args)
}

func crictlPS(ids ...string) []byte {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q}`, id))
	}
	return []byte(`{"containers":[` + strings.Join(entries, ",") + `]}`)
}

// etcdRestartTestServer builds a control-plane server whose three static-pod
// manifests sit in a temp root, with every wait compressed so the timeout paths
// run in milliseconds instead of minutes.
func etcdRestartTestServer(t *testing.T, client *fakeEtcdMemberClient) (*NodeAgentServer, *controlPlaneNodeFake) {
	t.Helper()
	s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
	writeStaticPodManifest(t, s, defaultKubeAPIServerManifestName, testKubeAPIServerManifest)
	writeStaticPodManifest(t, s, defaultKubeVipManifestName, testKubeVipManifest)

	node := newControlPlaneNodeFake(t, s)
	s.etcdClient = client
	s.Runner = node
	s.etcdHealthTimeout = 100 * time.Millisecond
	s.etcdHealthPollInterval = time.Millisecond
	s.etcdExitTimeout = 100 * time.Millisecond
	s.staticPodStartTimeout = 100 * time.Millisecond
	s.staticPodExitTimeout = 100 * time.Millisecond
	s.controlPlanePollInterval = time.Millisecond
	s.kubeVipSocketQuietWindow = time.Millisecond
	return s, node
}

func writeStaticPodManifest(t *testing.T, s *NodeAgentServer, manifestName, content string) {
	t.Helper()
	path := filepath.Join(s.RootPath, defaultStaticPodManifestDir, manifestName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// staticPodManifestIsActive reports whether a manifest is in the directory
// kubelet watches, which is the whole question after a failed sequence: a
// manifest left in the backup directory is a pod that will not come back.
func staticPodManifestIsActive(t *testing.T, s *NodeAgentServer, manifestName string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(s.RootPath, defaultStaticPodManifestDir, manifestName))
	return err == nil
}

// assertNodeInService checks the three manifests that decide whether this node
// is carrying control-plane traffic again.
func assertNodeInService(t *testing.T, s *NodeAgentServer) {
	t.Helper()
	assert.True(t, staticPodManifestIsActive(t, s, etcdManifestName), "etcd must be active")
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName), "kube-apiserver must be restored")
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName), "kube-vip must be restored")
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
		assertNodeInService(t, s)

		// The order is the safety property, not an implementation detail. The
		// node leaves service before etcd is touched and only rejoins after it
		// is healthy, and kube-vip goes last so the VIP is never advertised
		// from a node whose apiserver is not back yet.
		assert.Equal(t, []string{
			stepEtcdDefragPreflight,
			stepEtcdDefragTransferLeader,
			stepResolvingKubeVip,
			stepStoppingKubeVip,
			stepWaitingKubeVipExit,
			stepClosingVipSockets,
			stepRemovingLoopbackVip,
			stepWaitingVipSocketQuietWindow,
			stepStoppingKubeAPIServer,
			stepWaitingKubeAPIServerExit,
			stepEtcdDefragStoppingEtcd,
			stepEtcdDefragWaitingRestart,
			stepEtcdDefragWaitingHealthy,
			stepEtcdDefragReturningToService,
			stepRestoringKubeAPIServer,
			stepWaitingKubeAPIServerStart,
			// No second resolve: the restore is handed the address the
			// withdrawal resolved, because by now the withdrawal has removed
			// what resolving reads.
			stepRestoringKubeVip,
			stepWaitingKubeVipStart,
			stepWaitingLoopbackVip,
			stepEtcdDefragMeasuring,
		}, progress.steps)
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

	// An etcd that does not come back inside the restart wait leaves the node
	// withdrawn, and the agent owes it a return to service. etcd itself needs
	// no undoing here - its manifest never moved, so kubelet keeps trying - but
	// the parked kube-apiserver and kube-vip manifests do.
	t.Run("an etcd restart that times out still returns the node to service", func(t *testing.T) {
		client := &fakeEtcdMemberClient{
			members:     testEtcdMembersThree(),
			localID:     2,
			leaderID:    1,
			dbSize:      500 * mib,
			dbSizeInUse: 100 * mib,
		}
		s, node := etcdRestartTestServer(t, client)
		// More probes than the compressed start timeout allows, so the wait for
		// the replacement container expires.
		node.pods[etcdContainerName].startDelay = 1_000_000
		progress := &fakeOperationProgress{}

		err := s.runEtcdDefrag(context.Background(), progress)

		// Nil, because the member is verifiably back: this is a clean stop for
		// a rollout, not a node that needs an operator.
		require.NoError(t, err)
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED), result.Outcome)
		assert.Contains(t, result.ErrorMessage, "restart the etcd container")
		assertNodeInService(t, s)
	})

	t.Run("a member that never returns healthy is FAILED_NOT_RESTORED and errors", func(t *testing.T) {
		// Nothing is restored here, and that is the decision rather than an
		// omission: an apiserver with no etcd behind it and a VIP advertised
		// from a node that cannot answer are both worse than staying out.
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
		assert.Contains(t, result.ErrorMessage, "left out of service",
			"the message has to say the node is still out, because nothing else will")
		assert.True(t, staticPodManifestIsActive(t, s, etcdManifestName),
			"etcd's manifest never moved, so kubelet keeps trying to start it")
		assert.False(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName),
			"kube-apiserver must stay parked while its only etcd is down")
		assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
			"kube-vip must stay parked rather than advertise a node that cannot answer")
	})
}

// The withdrawal can fail before etcd is ever touched, and that still has to
// put the node back: by then kube-vip is parked and its address is off lo,
// which no other component will undo. etcd is untouched, so the member is
// healthy and the honest report is FAILED_RESTORED with a nil error - a rollout
// can stop on this cleanly.
//
// The failure modelled is the socket quiet window: new sockets still arriving
// on the VIP after kube-vip is gone and the address is removed, which is how a
// stale sticky ARP entry on a DPU presents. Production stops there rather than
// going on to take the apiserver down, because worker kubelets still pinned to
// this node would lose their API path.
func TestRunEtcdDefragReturnsTheNodeAfterAFailedWithdrawal(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:     testEtcdMembersThree(),
		localID:     2,
		leaderID:    1,
		dbSize:      500 * mib,
		dbSizeInUse: 100 * mib,
	}
	s, node := etcdRestartTestServer(t, client)
	// ss omits the State column when it is given a state filter, so the fields
	// are Recv-Q, Send-Q, local, peer.
	node.vipPeers = "0 0 " + testKubeVipAddress + ":6443 10.0.0.9:51510\n"
	progress := &fakeOperationProgress{}

	require.NoError(t, s.runEtcdDefrag(context.Background(), progress),
		"the member is untouched and verifiably healthy, so this is a clean stop")

	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED), result.Outcome)
	assert.Contains(t, result.ErrorMessage, "vip socket quiet window failed")
	assertNodeInService(t, s)
	assert.Contains(t, node.loopbackVIPs, testKubeVipAddress,
		"the VIP has to be back on lo, not just the manifest back in place")
	assert.NotContains(t, progress.steps, stepEtcdDefragStoppingEtcd,
		"a withdrawal that failed must never reach the etcd restart")
	assert.NotContains(t, progress.steps, stepStoppingKubeAPIServer,
		"nor take the apiserver down while traffic is still arriving here")
}

// A container that ignores SIGTERM cannot be restarted in any way the agent can
// prove, and this pins down what it reports when that happens. Both waits fail:
// the exit wait because the container never goes away, and then the restore's
// start wait because the new-container gate has nothing new to see.
//
// So the outcome is FAILED_NOT_RESTORED with an error, which is the honest
// answer - nothing was verified - and the manifests are all back on disk
// regardless, so the node is not left one parked file short of recovering.
func TestRunEtcdDefragReportsAnUnkillableStaticPod(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:     testEtcdMembersThree(),
		localID:     2,
		leaderID:    1,
		dbSize:      500 * mib,
		dbSizeInUse: 100 * mib,
	}
	s, node := etcdRestartTestServer(t, client)
	node.pods[kubeVipContainerName].stuckExit = true
	progress := &fakeOperationProgress{}

	err := s.runEtcdDefrag(context.Background(), progress)

	require.Error(t, err, "an unverified node must fail the operation, not just the outcome")
	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
	assert.Contains(t, result.ErrorMessage, "wait for kube-vip exit")
	assertNodeInService(t, s)
	assert.NotContains(t, progress.steps, stepEtcdDefragStoppingEtcd,
		"a withdrawal that failed must never reach the etcd restart")
}

// The flaw this exists for: on the success path the two restores ran as
// separate guarded calls that each returned on failure, so a kube-apiserver
// that was slow to come back left kube-vip parked - over a problem that had
// nothing to do with kube-vip, on a node whose etcd had just been confirmed
// healthy. Both restores have to be attempted.
func TestRunEtcdDefragRestoresKubeVipEvenWhenKubeAPIServerDoesNotComeBack(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:  testEtcdMembersThree(),
		localID:  2,
		leaderID: 1,
		statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 3),
	}
	s, node := etcdRestartTestServer(t, client)
	// More probes than the compressed start timeout allows, so kube-apiserver's
	// restore reports failure while every other pod behaves.
	node.pods[kubeAPIServerContainerName].startDelay = 1_000_000
	progress := &fakeOperationProgress{}

	err := s.runEtcdDefrag(context.Background(), progress)

	require.Error(t, err)
	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
	assert.Contains(t, result.ErrorMessage, "restoring kube-apiserver failed")
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
		"kube-vip must be put back even though kube-apiserver was not confirmed")
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName),
		"the manifest goes back even when the pod is not seen running")
}

// The bug this exists for: the restore re-resolved the VIP address after the
// withdrawal had already destroyed two of the three places it can be resolved
// from - the manifest is parked and the address is gone from lo. On a node
// whose kube-vip is not a static pod, loopback was the only source, so
// re-resolution found nothing, concluded kube-vip was "not configured", skipped
// the restore and returned nil. The operation then reported RECLAIMED with the
// node silently and permanently out of the control-plane VIP, and the startup
// reconciler could not help either, since no manifest was ever parked to find.
func TestRunEtcdDefragRestoresAVipOnlyLoopbackKnewAbout(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:  testEtcdMembersThree(),
		localID:  2,
		leaderID: 1,
		statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 3),
	}
	s, node := etcdRestartTestServer(t, client)

	// kube-vip runs here but not as a static pod, so the address on lo is the
	// only record of it.
	require.NoError(t, os.Remove(filepath.Join(s.RootPath, defaultStaticPodManifestDir, defaultKubeVipManifestName)))
	node.pods[kubeVipContainerName].runningID = ""

	progress := &fakeOperationProgress{}

	require.NoError(t, s.runEtcdDefrag(context.Background(), progress))

	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED), result.Outcome)
	assert.Contains(t, node.loopbackVIPs, testKubeVipAddress,
		"the VIP must be back on lo even though no manifest recorded it")
}

// The regression this exists for: restoreStaticPod listed containers before
// renaming the manifest back and returned on a listing error, so a single
// failed `crictl ps` after the move left a manifest parked with its pod down.
// The listing is only the baseline for the new-container gate, so losing it has
// to weaken that gate rather than skip the rename.
//
// The same reasoning applies one level up, and that is the other half of this
// test. An unreachable runtime fails both start waits, so returning at the
// first would leave kube-vip parked over a kube-apiserver problem. Every
// manifest has to be put back before the operation reports.
func TestRunEtcdDefragPutsEveryManifestBackWhenCrictlFails(t *testing.T) {
	const mib = int64(1024 * 1024)
	client := &fakeEtcdMemberClient{
		members:     testEtcdMembersThree(),
		localID:     2,
		leaderID:    1,
		dbSize:      500 * mib,
		dbSizeInUse: 100 * mib,
	}
	s, node := etcdRestartTestServer(t, client)
	node.crictlFailsOnceWithdrawn = true
	progress := &fakeOperationProgress{}

	err := s.runEtcdDefrag(context.Background(), progress)

	// The pods cannot be verified while the runtime is unreachable, so this is
	// honestly FAILED_NOT_RESTORED - but the manifests are back, which is what
	// decides whether they can come up at all.
	require.Error(t, err)
	result := decodeDefragResult(t, progress)
	assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
	assertNodeInService(t, s)
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

// Per-call deadlines keep any one call bounded but say nothing about the wait
// as a whole: a probe that starts just inside the health budget used to run to
// its own completion, so the wait overran by a Status call and, when the status
// came back but AlarmList did not, by an AlarmList call as well. That is time
// spent holding staticPodMutationMu after the budget said to give up, and the
// error reported the nominal timeout rather than what was actually waited.
func TestWaitForLocalEtcdHealthyStopsAtItsOwnDeadline(t *testing.T) {
	const callTimeout = 5 * time.Second
	client := &fakeEtcdMemberClient{
		members:  testEtcdMembersThree(),
		localID:  2,
		leaderID: 1,
		statusFn: func(ctx context.Context, _ int) (*clientv3.StatusResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	s := &NodeAgentServer{
		etcdHealthTimeout:      50 * time.Millisecond,
		etcdHealthPollInterval: time.Millisecond,
		etcdCallTimeout:        callTimeout,
	}

	started := time.Now()
	_, err := s.waitForLocalEtcdHealthy(context.Background(), client)
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.Less(t, elapsed, callTimeout,
		"the wait ran past its own budget and into the call budget of a probe it should have cut short")
	// The deadline cut the probe short, so the reason etcd was unwell has to
	// survive into the error rather than being replaced by a bare cancellation.
	assert.Contains(t, err.Error(), "etcd did not become healthy within")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// The regression this exists for: an agent restart mid-defragmentation leaves a
// RUNNING record and no worker, and startNodeAgentOperation then rejects every
// retry - the same ID as inactive, a fresh one as already running, because the
// type check matches on the phase rather than on a live worker. The node would
// then refuse to defragment until someone edited its state file.
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
					Step:  stepEtcdDefragWaitingRestart,
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
	return crictlPS(listing...), nil
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
	s := &NodeAgentServer{controlPlanePollInterval: time.Millisecond}

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
