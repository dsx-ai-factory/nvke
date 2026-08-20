// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/test/bufconn"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "host and port", in: "192.0.2.10:15005", want: "192.0.2.10:15005"},
		{name: "host only uses default port", in: "192.0.2.10", want: "192.0.2.10:15005"},
		{name: "empty", in: "  ", wantErr: "endpoint is required"},
		{name: "ipv6 with port", in: "[2001:db8::1]:15005", want: "[2001:db8::1]:15005"},
		{name: "bare ipv6 rejected", in: "2001:db8::1", wantErr: "IPv6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeEndpoint(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeEndpoint(%q) error = nil, want %q", tc.in, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeEndpoint(%q) error = %v, want substring %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEndpoint(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHasNodeAgent(t *testing.T) {
	t.Parallel()
	if HasNodeAgent(nil) {
		t.Fatal("HasNodeAgent(nil) = true")
	}
	if HasNodeAgent([]string{"grpc.reflection.v1.ServerReflection"}) {
		t.Fatal("HasNodeAgent(reflection-only) = true")
	}
	if !HasNodeAgent([]string{"grpc.reflection.v1.ServerReflection", NodeAgentService}) {
		t.Fatal("HasNodeAgent(with NodeAgent) = false")
	}
}

type stubAgent struct {
	nkeproto.UnimplementedNodeAgentServer
}

func (s *stubAgent) GetKubeadmStatus(
	_ context.Context,
	_ *nkeproto.KubeadmStatusRequest,
) (*nkeproto.KubeadmStatusResponse, error) {
	return &nkeproto.KubeadmStatusResponse{
		KubeadmStatus: "pending",
		AgentVersion:  "test",
	}, nil
}

func TestListServicesAndStatus(t *testing.T) {
	t.Parallel()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	nkeproto.RegisterNodeAgentServer(server, &stubAgent{})
	reflection.Register(server)
	go func() {
		_ = server.Serve(lis)
	}()
	t.Cleanup(server.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	services, err := ListServices(ctx, conn)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !HasNodeAgent(services) {
		t.Fatalf("HasNodeAgent(%v) = false", services)
	}

	statusResp, err := nkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nkeproto.KubeadmStatusRequest{})
	if err != nil {
		t.Fatalf("GetKubeadmStatus: %v", err)
	}
	if statusResp.GetKubeadmStatus() != "pending" {
		t.Fatalf("kubeadm status = %q, want pending", statusResp.GetKubeadmStatus())
	}
	if statusResp.GetAgentVersion() != "test" {
		t.Fatalf("agent version = %q, want test", statusResp.GetAgentVersion())
	}
}
