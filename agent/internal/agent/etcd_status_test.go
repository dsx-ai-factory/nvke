// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// writeEtcdManifest lays down an etcd static-pod manifest under a temporary
// root and returns a server rooted there, so the manifest-reading paths run
// against a real file without touching the host.
func writeEtcdManifest(t *testing.T, manifest string) *NodeAgentServer {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, defaultStaticPodManifestDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, etcdManifestName), []byte(manifest), 0o600))
	return &NodeAgentServer{
		RootPath:   root,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		// The manifest-writing paths resolve root's uid and gid, which a test
		// environment's user database may not carry.
		kubeadmFileOps: fileOps{
			lookupUser: func(name string) (*user.User, error) {
				if name == "root" {
					return &user.User{Username: "root", Uid: "0"}, nil
				}
				return nil, user.UnknownUserError(name)
			},
			lookupGroup: func(name string) (*user.Group, error) {
				if name == "root" {
					return &user.Group{Name: "root", Gid: "0"}, nil
				}
				return nil, user.UnknownGroupError(name)
			},
			chown: func(string, int, int) error { return nil },
		},
	}
}

const etcdManifestWithFlags = `apiVersion: v1
kind: Pod
metadata:
  name: etcd
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --quota-backend-bytes=17179869184
        - --bootstrap-defrag-threshold-megabytes=2048
`

func TestReadFlagValue(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		flag      string
		wantValue string
		wantFound bool
	}{
		{
			name:      "token form in command",
			manifest:  etcdManifestWithFlags,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "17179869184",
			wantFound: true,
		},
		{
			name: "split form is read as a value",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--quota-backend-bytes", "123"]
`,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "123",
			wantFound: true,
		},
		{
			name: "flag living in args is found",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
      args: ["--quota-backend-bytes=42"]
`,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "42",
			wantFound: true,
		},
		{
			name: "bare boolean flag is found with an empty value",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--some-toggle", "--other=1"]
`,
			flag:      "some-toggle",
			wantValue: "",
			wantFound: true,
		},
		{
			name:      "absent flag is reported as not found",
			manifest:  etcdManifestWithFlags,
			flag:      "nonexistent-flag",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			_, manifestJSON, err := s.readEtcdManifest()
			require.NoError(t, err)

			value, found, err := readFlagValue(manifestJSON, etcdContainerName, tt.flag)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}

func TestEtcdBootstrapDefragThresholdBytes(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		wantBytes int64
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "the stable flag is read",
			manifest:  etcdManifestWithFlags,
			wantBytes: 2048 * 1024 * 1024,
			wantFound: true,
		},
		{
			name: "a value given as an arg rather than in the command is read",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=512"]
`,
			wantBytes: 512 * 1024 * 1024,
			wantFound: true,
		},
		{
			name: "no flag present",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`,
			wantFound: false,
		},
		{
			// etcd 3.5's spelling. Nothing provisions a node that old any more,
			// and reading it would reintroduce the question of which name wins
			// when a manifest carries both. Reporting it as unconfigured is the
			// safe direction: the defragmentation is refused rather than a
			// member being restarted for a defragmentation that will not run.
			name: "the pre-3.6 experimental spelling is not read",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--experimental-bootstrap-defrag-threshold-megabytes=2048"]
`,
			wantFound: false,
		},
		{
			name: "non-numeric value is an error rather than a silent zero",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=lots"]
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			_, manifestJSON, err := s.readEtcdManifest()
			require.NoError(t, err)

			got, found, err := etcdBootstrapDefragThresholdBytes(manifestJSON)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantBytes, got)
		})
	}
}

func TestEtcdQuotaBackendBytes(t *testing.T) {
	s := writeEtcdManifest(t, etcdManifestWithFlags)
	_, manifestJSON, err := s.readEtcdManifest()
	require.NoError(t, err)

	t.Run("server-reported quota wins when populated", func(t *testing.T) {
		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{DbSizeQuota: 999}, manifestJSON)
		require.NoError(t, err)
		assert.Equal(t, int64(999), got)
	})

	t.Run("falls back to the manifest flag on etcd 3.5", func(t *testing.T) {
		// etcd 3.5 does not populate DbSizeQuota, which is the fleet today, so
		// the manifest fallback is the live path rather than a safety net.
		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{}, manifestJSON)
		require.NoError(t, err)
		assert.Equal(t, int64(17179869184), got)
	})

	t.Run("absent from both sources reports zero", func(t *testing.T) {
		bare := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`)
		_, bareJSON, err := bare.readEtcdManifest()
		require.NoError(t, err)

		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{}, bareJSON)
		require.NoError(t, err)
		assert.Zero(t, got, "zero signals unknown so callers decline rather than dividing by it")
	})

	t.Run("a negative flag value reports zero", func(t *testing.T) {
		// etcd would not start on this, so it describes no real quota. Handing
		// a negative back would be worse than saying nothing: a caller sizing a
		// threshold as a fraction of quota would compute a negative target and
		// find every member over it.
		negative := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --quota-backend-bytes=-1
`)
		_, negativeJSON, err := negative.readEtcdManifest()
		require.NoError(t, err)

		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{}, negativeJSON)
		require.NoError(t, err)
		assert.Zero(t, got, "a negative quota must read as the documented unknown sentinel")
	})
}

func TestReachableVotingMembers(t *testing.T) {
	members := func(ms ...*etcdserverpb.Member) *clientv3.MemberListResponse {
		return &clientv3.MemberListResponse{Members: ms}
	}

	t.Run("counts the local member from the membership, not by assumption", func(t *testing.T) {
		// The regression this exists for: the local member used to be seeded
		// into the count before the list was walked, so a member that had been
		// removed from the cluster or demoted to a learner still counted toward
		// the floor that decides whether restarting it is safe.
		s := &NodeAgentServer{}
		client := &fakeEtcdMemberClient{}

		demoted := s.reachableVotingMembers(context.Background(), client, members(
			&etcdserverpb.Member{ID: 2, Name: "cp-2", IsLearner: true},
		), 2)
		assert.Empty(t, demoted, "a local member demoted to a learner does not vote")

		removed := s.reachableVotingMembers(context.Background(), client, members(
			&etcdserverpb.Member{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
		), 2)
		assert.Len(t, removed, 1, "a local member absent from the membership must not be counted")
	})

	t.Run("leaves out peers that do not answer", func(t *testing.T) {
		s := &NodeAgentServer{}
		client := &fakeEtcdMemberClient{
			unavailablePeers: map[string]bool{"https://cp-3:2379": true},
		}

		got := s.reachableVotingMembers(context.Background(), client, members(
			&etcdserverpb.Member{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
			&etcdserverpb.Member{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
			&etcdserverpb.Member{ID: 3, Name: "cp-3", ClientURLs: []string{"https://cp-3:2379"}},
		), 2)

		require.Len(t, got, 2)
		assert.Equal(t, uint64(1), got[0].GetID())
		assert.Equal(t, uint64(2), got[1].GetID())
	})
}

func TestReclaimableBytes(t *testing.T) {
	assert.Equal(t, int64(400), reclaimableBytes(1000, 600))
	assert.Equal(t, int64(0), reclaimableBytes(600, 600))
	assert.Equal(t, int64(0), reclaimableBytes(500, 600), "must never report negative reclaimable space")
}

func TestVotingMemberCount(t *testing.T) {
	assert.Equal(t, int32(0), votingMemberCount(nil))
	assert.Equal(t, int32(2), votingMemberCount(&clientv3.MemberListResponse{
		Members: []*etcdserverpb.Member{
			{ID: 1, Name: "cp-1"},
			{ID: 2, Name: "cp-2"},
			{ID: 3, Name: "learner", IsLearner: true},
		},
	}), "learners do not vote so they do not count toward quorum")
}

func TestGetEtcdStatus(t *testing.T) {
	t.Run("reports sizing, leadership and voters", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{
			members:     testEtcdMembers(),
			localID:     2,
			leaderID:    2,
			dbSize:      1_000_000,
			dbSizeInUse: 600_000,
		}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.Equal(t, int64(1_000_000), resp.GetDbTotalSizeBytes())
		assert.Equal(t, int64(600_000), resp.GetDbTotalSizeInUseBytes())
		assert.Equal(t, int64(400_000), resp.GetReclaimableBytes())
		assert.Equal(t, int64(17179869184), resp.GetQuotaBackendBytes())
		assert.True(t, resp.GetIsLeader())
		assert.Equal(t, int32(2), resp.GetVotingMemberCount())
		assert.Equal(t, "2", resp.GetMemberId())
		assert.True(t, resp.GetHealthy())
		assert.Equal(t, int64(2048*1024*1024), resp.GetBootstrapDefragThresholdBytes())
	})

	t.Run("a member with no threshold reports zero, not an error", func(t *testing.T) {
		// How a node provisioned before the threshold was configured identifies
		// itself. A caller skips it on this alone, rather than calling
		// StartEtcdDefrag to be told no on every poll.
		s := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--quota-backend-bytes=17179869184"]
`)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.Zero(t, resp.GetBootstrapDefragThresholdBytes(),
			"an unconfigured threshold must read as zero so a caller leaves the member alone")
		assert.True(t, resp.GetHealthy(), "no threshold is a configuration state, not ill health")
	})

	t.Run("an unparseable threshold fails the call rather than reading as zero", func(t *testing.T) {
		s := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=lots"]
`)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}

		_, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.Error(t, err, "reporting a typo as zero would silently retire the member")
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("a follower is not reported as leader", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetIsLeader())
	})

	t.Run("a cluster with no leader is not reported as leader", func(t *testing.T) {
		// Leader 0 means no leader is elected. Comparing it against a member ID
		// of 0 must not read as "this member leads".
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 0, leaderID: 0}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetIsLeader())
	})

	t.Run("this member's own alarm marks it unhealthy", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{
			members:  testEtcdMembers(),
			localID:  2,
			leaderID: 1,
			alarms: []*etcdserverpb.AlarmMember{
				{MemberID: 2, Alarm: etcdserverpb.AlarmType_NOSPACE},
			},
		}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetHealthy())
	})

	t.Run("another member's alarm leaves this one healthy", func(t *testing.T) {
		// AlarmList returns the whole replicated set from any member, so an
		// alarm raised by cp-1 arrives here too. Reporting cp-2 unhealthy for
		// it would hide the one member a caller could still defragment.
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{
			members:  testEtcdMembers(),
			localID:  2,
			leaderID: 1,
			alarms: []*etcdserverpb.AlarmMember{
				{MemberID: 1, Alarm: etcdserverpb.AlarmType_NOSPACE},
			},
		}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.True(t, resp.GetHealthy())
	})

	t.Run("a member that cannot see a leader is unhealthy", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 0}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetHealthy())
	})

	t.Run("worker nodes are rejected", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.NodeStatus = &NodeStatus{Role: "worker"}
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2}

		_, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("a missing manifest is a precondition failure", func(t *testing.T) {
		s := &NodeAgentServer{
			RootPath:   t.TempDir(),
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			etcdClient: &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2},
		}

		_, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}
