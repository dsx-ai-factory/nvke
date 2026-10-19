// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package check dials an nvke-agent gRPC endpoint over SPIFFE mTLS and verifies
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

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	// DefaultPort is the nvke-agent gRPC listen port.
	DefaultPort = "15005"
	// DefaultSPIFFESocket is the workload API address nvke-agent uses.
	DefaultSPIFFESocket = "unix:///tmp/spire-agent/public/api.sock"
	// NodeAgentService is the gRPC reflection name of proto.NodeAgent.
	NodeAgentService = "proto.NodeAgent"
	// ClientPathPrefix is the SPIFFE path prefix nvke-agent authorizes.
	ClientPathPrefix = "/tenant/nvke/service/"
)

// ErrWorkloadAPI means the local SPIFFE workload API was not usable.
var ErrWorkloadAPI = errors.New("spiffe workload api")

// ErrClientIdentity means the local SVID would be rejected by nvke-agent.
var ErrClientIdentity = errors.New("client spiffe identity")

// ErrDial means the TCP/TLS handshake to the agent failed.
var ErrDial = errors.New("dial nvke-agent")

// ErrAgentRPC means the agent was reachable but an RPC failed.
var ErrAgentRPC = errors.New("nvke-agent rpc")

// Options configure a connectivity probe.
type Options struct {
	Endpoint          string
	SPIFFESocket      string
	ExpectTrustDomain string
	// ExpectAuthRecord matches an agent running --grpc-auth-mode=tls-jwt. Such
	// an agent writes an NKEAuth record before the HTTP/2 preface, and a probe
	// that does not consume it reads the record as a corrupt preface.
	ExpectAuthRecord bool
}

// Result is a successful probe.
type Result struct {
	Endpoint     string
	ClientSVID   string
	AgentVersion string
	Kubeadm      string
	Services     []string
	// AuthRecord reports that the agent sent an NKEAuth record, and
	// AuthSubject its sub claim. The claim is read without verifying the
	// signature: only the controller validates this token.
	AuthRecord  bool
	AuthSubject string
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
// GetKubeadmStatus. The local workload API must issue an SVID under a path
// authorizedClientPath accepts, in the agent's trust domain.
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
	if !authorizedClientPath(svid.ID.Path()) {
		return Result{}, fmt.Errorf("%w: %s does not start with %s (nvke-agent will reject the handshake)",
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

	creds := grpccredentials.MTLSClientCredentials(x509Source, bundleSource, authorizer)
	var authCreds *authRecordCredentials
	if opts.ExpectAuthRecord {
		authCreds = newAuthRecordCredentials(creds)
		creds = authCreds
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
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

	statusResp, err := nvkeproto.NewNodeAgentClient(conn).GetKubeadmStatus(ctx, &nvkeproto.KubeadmStatusRequest{})
	if err != nil {
		return Result{}, fmt.Errorf("%w: GetKubeadmStatus: %w", ErrAgentRPC, err)
	}

	result := Result{
		Endpoint:     endpoint,
		ClientSVID:   svid.ID.String(),
		AgentVersion: statusResp.GetAgentVersion(),
		Kubeadm:      statusResp.GetKubeadmStatus(),
		Services:     services,
	}
	if authCreds != nil {
		result.AuthRecord, result.AuthSubject = authCreds.observed()
	}
	return result, nil
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

// authorizedClientPath mirrors the node agent's transitional caller paths.
func authorizedClientPath(path string) bool {
	for _, prefix := range []string{ClientPathPrefix, "/nvke-service/", "/nke-service/", "/tenant/nke/service/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
