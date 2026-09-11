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

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
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
}

func (f *fakeEtcdMemberClient) MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{Members: f.members}, f.listErr
}

func (f *fakeEtcdMemberClient) Status(context.Context, string) (*clientv3.StatusResponse, error) {
	return &clientv3.StatusResponse{
		Header: &etcdserverpb.ResponseHeader{MemberId: f.localID},
		Leader: f.leaderID,
	}, f.statusErr
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
