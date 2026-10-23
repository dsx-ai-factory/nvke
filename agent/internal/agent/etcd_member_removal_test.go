// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

type fakeEtcdMemberClient struct {
	members   []*etcdserverpb.Member
	localID   uint64
	leaderID  uint64
	listErr   error
	statusErr error
	moveErr   error
	removeErr error
	movedTo   uint64
	movedFrom []string
	removedID uint64

	// Backend sizing reported by Status, used by the etcd status and
	// defragmentation RPCs.
	dbSize      int64
	dbSizeInUse int64
	dbSizeQuota int64

	// statusFn, when set, replaces the canned Status response for the *local*
	// endpoint. It receives the context and the 1-based local call count, so a
	// test can model a member that is unreachable for the first few probes and
	// then comes back - what a restart looks like to the health wait - or one
	// that blocks until its deadline expires.
	statusFn    func(ctx context.Context, call int) (*clientv3.StatusResponse, error)
	statusCalls int

	// alarms is the cluster-wide alarm store AlarmList reports. It is returned
	// whole to every caller, as etcd does, so a test can check that an alarm
	// raised by one member is not charged to another.
	alarms   []*etcdserverpb.AlarmMember
	alarmErr error

	// unavailablePeers marks peer client URLs that fail the availability probe,
	// modelling a registered voter that is offline. Keyed by client URL because
	// that is what the probe dials. Peer probes are counted separately from
	// local ones so adding a peer does not shift statusFn's call numbering.
	unavailablePeers map[string]bool
	peerStatusCalls  int
}

func (f *fakeEtcdMemberClient) MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{Members: f.members}, f.listErr
}

func (f *fakeEtcdMemberClient) Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	if endpoint != etcdLocalEndpoint {
		f.peerStatusCalls++
		if f.unavailablePeers[endpoint] {
			return nil, errors.New("connection refused")
		}
		return &clientv3.StatusResponse{
			Header: &etcdserverpb.ResponseHeader{MemberId: f.memberIDForClientURL(endpoint)},
			Leader: f.leaderID,
			Errors: f.statusErrorStrings(),
		}, nil
	}
	f.statusCalls++
	if f.statusFn != nil {
		return f.statusFn(ctx, f.statusCalls)
	}
	return &clientv3.StatusResponse{
		Header:      &etcdserverpb.ResponseHeader{MemberId: f.localID},
		Leader:      f.leaderID,
		DbSize:      f.dbSize,
		DbSizeInUse: f.dbSizeInUse,
		DbSizeQuota: f.dbSizeQuota,
		Errors:      f.statusErrorStrings(),
	}, f.statusErr
}

// statusErrorStrings builds Errors the way etcd does: a no-leader note, then
// every alarm in the cluster flattened to a string, whoever raised it. Keeping
// the fake faithful on that point is what makes the member-scoping tests bite -
// a gate that goes back to reading Errors sees the peer's alarm here too.
func (f *fakeEtcdMemberClient) statusErrorStrings() []string {
	var errs []string
	if f.leaderID == 0 {
		errs = append(errs, "etcdserver: no leader")
	}
	for _, alarm := range f.alarms {
		errs = append(errs, alarm.String())
	}
	return errs
}

// memberIDForClientURL answers a peer probe as that peer, not as the local
// member, so a test can tell whose status it is looking at.
func (f *fakeEtcdMemberClient) memberIDForClientURL(endpoint string) uint64 {
	for _, m := range f.members {
		for _, url := range m.GetClientURLs() {
			if url == endpoint {
				return m.GetID()
			}
		}
	}
	return 0
}

func (f *fakeEtcdMemberClient) AlarmList(context.Context) (*clientv3.AlarmResponse, error) {
	if f.alarmErr != nil {
		return nil, f.alarmErr
	}
	return &clientv3.AlarmResponse{Alarms: f.alarms}, nil
}

func (f *fakeEtcdMemberClient) TransferLeadership(_ context.Context, endpoints []string, id uint64) error {
	f.movedFrom = endpoints
	f.movedTo = id
	return f.moveErr
}

func (f *fakeEtcdMemberClient) MemberRemove(_ context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	f.removedID = id
	return &clientv3.MemberRemoveResponse{}, f.removeErr
}

func (f *fakeEtcdMemberClient) Close() error { return nil }

func TestRemoveEtcdMember(t *testing.T) {
	tests := []struct {
		name          string
		target        string
		client        fakeEtcdMemberClient
		wantCode      codes.Code
		wantMovedTo   uint64
		wantMovedFrom []string
		wantRemovedID uint64
	}{
		{
			name:          "removes remote member by name",
			target:        "cp-1",
			client:        fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1},
			wantMovedTo:   2,
			wantMovedFrom: []string{"https://cp-1:2379"},
			wantRemovedID: 1,
		},
		{
			name:          "removes remote member by hexadecimal ID",
			target:        "0xA",
			client:        fakeEtcdMemberClient{members: []*etcdserverpb.Member{{ID: 10, Name: "cp-a"}}, localID: 2, leaderID: 3},
			wantRemovedID: 10,
		},
		{
			name:   "absent member is success",
			target: "missing",
			client: fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2},
		},
		{
			name:     "local member is rejected",
			target:   "cp-2",
			client:   fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2},
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "member list failure is returned",
			target:   "cp-1",
			client:   fakeEtcdMemberClient{listErr: errors.New("unavailable")},
			wantCode: codes.Internal,
		},
		{
			name:          "leader transfer failure does not block removal",
			target:        "cp-1",
			client:        fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1, moveErr: errors.New("unavailable")},
			wantMovedTo:   2,
			wantMovedFrom: []string{"https://cp-1:2379"},
			wantRemovedID: 1,
		},
		{
			name:          "leader without a client endpoint is removed",
			target:        "cp-1",
			client:        fakeEtcdMemberClient{members: []*etcdserverpb.Member{{ID: 1, Name: "cp-1"}}, localID: 2, leaderID: 1},
			wantRemovedID: 1,
		},
		{
			name:          "remove failure is returned",
			target:        "cp-1",
			client:        fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, removeErr: errors.New("unavailable")},
			wantCode:      codes.Internal,
			wantRemovedID: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.client
			server := &NodeAgentServer{etcdClient: &client}

			_, err := server.RemoveEtcdMember(context.Background(), &pb.RemoveEtcdMemberRequest{Target: tt.target})

			if tt.wantCode == codes.OK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.wantCode, status.Code(err))
			}
			assert.Equal(t, tt.wantMovedTo, client.movedTo)
			assert.Equal(t, tt.wantMovedFrom, client.movedFrom)
			assert.Equal(t, tt.wantRemovedID, client.removedID)
		})
	}
}

func testEtcdMembers() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
		{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
	}
}
