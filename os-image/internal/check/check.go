// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package check dials an nke-agent gRPC endpoint over SPIFFE mTLS and verifies
// the NodeAgent service is reachable.
package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	// DefaultPort is the nke-agent gRPC listen port.
	DefaultPort = "15005"
	// DefaultSPIFFESocket is the workload API address nke-agent uses.
	DefaultSPIFFESocket = "unix:///tmp/spire-agent/public/api.sock"
	// NodeAgentService is the gRPC reflection name of proto.NodeAgent.
	NodeAgentService = "proto.NodeAgent"
	// ClientPathPrefix is the SPIFFE path prefix nke-agent authorizes.
	ClientPathPrefix = "/nke-service/"
)

// ErrWorkloadAPI means the local SPIFFE workload API was not usable.
var ErrWorkloadAPI = errors.New("spiffe workload api")

// ErrClientIdentity means the local SVID would be rejected by nke-agent.
var ErrClientIdentity = errors.New("client spiffe identity")

// ErrDial means the TCP/TLS handshake to the agent failed.
var ErrDial = errors.New("dial nke-agent")

// ErrAgentRPC means the agent was reachable but an RPC failed.
var ErrAgentRPC = errors.New("nke-agent rpc")

// Options configure a connectivity probe.
type Options struct {
	Endpoint          string
	SPIFFESocket      string
	ExpectTrustDomain string
}

// Result is a successful probe.
type Result struct {
	Endpoint     string
	ClientSVID   string
	AgentVersion string
	Kubeadm      string
	Services     []string
}

// NormalizeEndpoint accepts host or host:port and returns host:port.
func NormalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("endpoint is required")
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw, nil
	}
	if strings.Contains(raw, ":") && !strings.HasPrefix(raw, "[") {
		return "", fmt.Errorf("endpoint %q: IPv6 addresses must be [host]:port or wrapped in brackets", raw)
	}
	return net.JoinHostPort(raw, DefaultPort), nil
}

// Probe dials endpoint over SPIFFE mTLS, lists gRPC services, and calls
// GetKubeadmStatus. The local workload API must issue an SVID under
// /nke-service/ in the agent's trust domain.
func Probe(ctx context.Context, opts Options) (Result, error) {
	endpoint, err := NormalizeEndpoint(opts.Endpoint)
	if err != nil {
		return Result{}, err
	}
	socket := opts.SPIFFESocket
	if socket == "" {
		socket = DefaultSPIFFESocket
	}

	workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(socket))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrWorkloadAPI, err)
	}
	defer workloadClient.Close()

	x509Source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrWorkloadAPI, err)
	}
	defer x509Source.Close()

	bundleSource, err := workloadapi.NewBundleSource(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrWorkloadAPI, err)
	}
	defer bundleSource.Close()

	svid, err := x509Source.GetX509SVID()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrWorkloadAPI, err)
	}
	if !strings.HasPrefix(svid.ID.Path(), ClientPathPrefix) {
		return Result{}, fmt.Errorf("%w: %s does not start with %s (nke-agent will reject the handshake)",
			ErrClientIdentity, svid.ID, ClientPathPrefix)
	}
	clientTD := svid.ID.TrustDomain()
	if opts.ExpectTrustDomain != "" && clientTD.Name() != opts.ExpectTrustDomain {
		return Result{}, fmt.Errorf("%w: local SVID trust domain %s, expected %s",
			ErrClientIdentity, clientTD.Name(), opts.ExpectTrustDomain)
	}

	authorizer := tlsconfig.AdaptMatcher(func(id spiffeid.ID) error {
		if id.TrustDomain() != clientTD {
			return fmt.Errorf("agent SVID trust domain %s does not match client %s", id.TrustDomain(), clientTD)
		}
		return nil
	})

	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(grpccredentials.MTLSClientCredentials(x509Source, bundleSource, authorizer)),
	)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrDial, err)
	}
	defer conn.Close()

	services, err := ListServices(ctx, conn)
	if err != nil {
		return Result{}, fmt.Errorf("%w: reflection: %w", ErrAgentRPC, classifyDial(err))
	}
	if !HasNodeAgent(services) {
		return Result{}, fmt.Errorf("%w: %s not in reflection listing %v", ErrAgentRPC, NodeAgentService, services)
	}

	statusResp, err := nkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nkeproto.KubeadmStatusRequest{})
	if err != nil {
		return Result{}, fmt.Errorf("%w: GetKubeadmStatus: %w", ErrAgentRPC, err)
	}

	return Result{
		Endpoint:     endpoint,
		ClientSVID:   svid.ID.String(),
		AgentVersion: statusResp.GetAgentVersion(),
		Kubeadm:      statusResp.GetKubeadmStatus(),
		Services:     services,
	}, nil
}

func classifyDial(err error) error {
	if status.Code(err) == codes.Unavailable {
		return fmt.Errorf("%w: %w", ErrDial, err)
	}
	return err
}

// ListServices returns gRPC service names advertised over server reflection.
func ListServices(ctx context.Context, conn grpc.ClientConnInterface) ([]string, error) {
	stream, err := grpc_reflection_v1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&grpc_reflection_v1.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_ListServices{},
	}); err != nil {
		return nil, err
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	list := resp.GetListServicesResponse()
	if list == nil {
		if errResp := resp.GetErrorResponse(); errResp != nil {
			return nil, status.Error(codes.Code(errResp.GetErrorCode()), errResp.GetErrorMessage())
		}
		return nil, errors.New("reflection list services returned an empty response")
	}
	names := make([]string, 0, len(list.GetService()))
	for _, svc := range list.GetService() {
		names = append(names, svc.GetName())
	}
	return names, nil
}

// HasNodeAgent reports whether reflection listed proto.NodeAgent.
func HasNodeAgent(services []string) bool {
	for _, name := range services {
		if name == NodeAgentService {
			return true
		}
	}
	return false
}
