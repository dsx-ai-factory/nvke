// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	etcdLocalEndpoint         = "https://127.0.0.1:2379"
	etcdCACertPath            = "/etc/kubernetes/pki/etcd/ca.crt"
	etcdHealthcheckClientCert = "/etc/kubernetes/pki/etcd/healthcheck-client.crt"
	etcdHealthcheckClientKey  = "/etc/kubernetes/pki/etcd/healthcheck-client.key"
	etcdClientDialTimeout     = 5 * time.Second
)

type etcdMemberClient interface {
	MemberList(ctx context.Context, opts ...clientv3.OpOption) (*clientv3.MemberListResponse, error)
	Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error)
	TransferLeadership(ctx context.Context, leaderEndpoints []string, transfereeID uint64) error
	MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error)
	Close() error
}

type kubeadmEtcdClient struct {
	*clientv3.Client
	tlsConfig *tls.Config
}

func (c *kubeadmEtcdClient) TransferLeadership(ctx context.Context, leaderEndpoints []string, transfereeID uint64) error {
	leaderClient, err := clientv3.New(clientv3.Config{
		Endpoints:   leaderEndpoints,
		DialTimeout: etcdClientDialTimeout,
		TLS:         c.tlsConfig.Clone(),
	})
	if err != nil {
		return fmt.Errorf("connect to etcd leader: %w", err)
	}
	defer leaderClient.Close()

	if _, err := leaderClient.MoveLeader(ctx, transfereeID); err != nil {
		return fmt.Errorf("move etcd leader: %w", err)
	}
	return nil
}

// RemoveEtcdMember removes a named or hexadecimal-ID member through the local
// kubeadm etcd endpoint. An absent target is success so retries are idempotent.
func (s *NodeAgentServer) RemoveEtcdMember(
	ctx context.Context,
	req *pb.RemoveEtcdMemberRequest,
) (*pb.RemoveEtcdMemberResponse, error) {
	defer logRPCSpan(ctx, "RemoveEtcdMember")()

	target := strings.TrimSpace(req.GetTarget())
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "etcd member target is required")
	}

	client, err := s.newEtcdMemberClient()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "connect to local kubeadm etcd: %v", err)
	}
	defer client.Close()

	members, err := client.MemberList(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list etcd members: %v", err)
	}
	targetID, targetEndpoints, found := findEtcdMember(members, target)
	if !found {
		return &pb.RemoveEtcdMemberResponse{}, nil
	}

	localStatus, err := client.Status(ctx, etcdLocalEndpoint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get local etcd member status: %v", err)
	}
	if localStatus == nil || localStatus.Header == nil {
		return nil, status.Error(codes.Internal, "local etcd status has no member ID")
	}
	localID := localStatus.Header.MemberId
	if targetID == localID {
		return nil, status.Error(codes.FailedPrecondition, "refusing to remove the local etcd member")
	}

	if targetID == localStatus.Leader {
		if len(targetEndpoints) == 0 {
			slog.WarnContext(ctx, "skipping etcd leadership transfer because the leader has no client endpoint", "targetMemberID", fmt.Sprintf("%x", targetID))
		} else if err := client.TransferLeadership(ctx, targetEndpoints, localID); err != nil {
			// Removal remains safe: if the transfer cannot complete, the surviving
			// members use the normal election path after the configuration change.
			slog.WarnContext(ctx, "etcd leadership transfer failed; continuing with removal", "targetMemberID", fmt.Sprintf("%x", targetID), "err", err)
		}
	}

	// NKE controllers rely on etcd's default strict reconfiguration check to
	// reject a removal that would break quorum.
	if _, err := client.MemberRemove(ctx, targetID); err != nil {
		return nil, status.Errorf(codes.Internal, "remove etcd member %x: %v", targetID, err)
	}
	return &pb.RemoveEtcdMemberResponse{}, nil
}

func (s *NodeAgentServer) newEtcdMemberClient() (etcdMemberClient, error) {
	if s.etcdClient != nil {
		return s.etcdClient, nil
	}

	cert, err := tls.LoadX509KeyPair(
		filepath.Join(s.RootPath, etcdHealthcheckClientCert),
		filepath.Join(s.RootPath, etcdHealthcheckClientKey),
	)
	if err != nil {
		return nil, fmt.Errorf("load etcd healthcheck client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(s.RootPath, etcdCACertPath))
	if err != nil {
		return nil, fmt.Errorf("read etcd CA certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse etcd CA certificate")
	}

	tlsConfig := &tls.Config{ //nolint:gosec // kubeadm's etcd CA and client certificate authenticate etcd connections.
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdLocalEndpoint},
		DialTimeout: etcdClientDialTimeout,
		TLS:         tlsConfig,
	})
	if err != nil {
		return nil, err
	}
	return &kubeadmEtcdClient{Client: client, tlsConfig: tlsConfig}, nil
}

func findEtcdMember(response *clientv3.MemberListResponse, target string) (uint64, []string, bool) {
	normalizedID := strings.TrimPrefix(strings.ToLower(target), "0x")
	for _, member := range response.Members {
		if member.GetName() == target || strconv.FormatUint(member.GetID(), 16) == normalizedID {
			return member.GetID(), member.GetClientURLs(), true
		}
	}
	return 0, nil, false
}
