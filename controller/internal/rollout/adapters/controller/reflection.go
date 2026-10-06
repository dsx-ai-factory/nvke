// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// errGRPCMethodUnsupported identifies servers that cannot prove support for a
// requested method through gRPC reflection. NewMethodSupportChecker translates
// it into nodeagent.ErrMethodUnsupported for rollout consumers.
var errGRPCMethodUnsupported = errors.New("grpc method unsupported")

// ensureGRPCMethodSupported verifies that the connected server exposes
// fullMethod via gRPC server reflection. It returns nil when supported, an
// errGRPCMethodUnsupported-wrapped error when support cannot be proven, or a
// plain error on a transport failure.
func ensureGRPCMethodSupported(ctx context.Context, conn *grpc.ClientConn, fullMethod string) error {
	if conn == nil {
		return errors.New("grpc connection is nil")
	}

	service, symbol, err := grpcReflectionServiceAndSymbol(fullMethod)
	if err != nil {
		return err
	}

	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return fmt.Errorf("open grpc reflection stream: %w", err)
	}
	defer func() { _ = stream.CloseSend() }()

	if sendErr := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
	}); sendErr != nil {
		return fmt.Errorf("query grpc reflection services: %w", sendErr)
	}

	resp, err := stream.Recv()
	if err != nil {
		return grpcReflectionRecvError(fullMethod, err)
	}

	// FileContainingSymbol can resolve descriptors linked into the process even
	// when the service is not registered, so first check what the server advertises.
	serviceFound := false
	for _, svc := range resp.GetListServicesResponse().GetService() {
		if svc.GetName() == service {
			serviceFound = true
			break
		}
	}
	if !serviceFound {
		return fmt.Errorf("%w: server does not expose %s via grpc reflection", errGRPCMethodUnsupported, service)
	}

	if sendErr := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	}); sendErr != nil {
		return fmt.Errorf("query grpc reflection for %s: %w", fullMethod, sendErr)
	}

	resp, err = stream.Recv()
	if err != nil {
		return grpcReflectionRecvError(fullMethod, err)
	}
	if resp.GetFileDescriptorResponse() != nil {
		return nil
	}
	if reflectionErr := resp.GetErrorResponse(); reflectionErr != nil {
		return fmt.Errorf("%w: server does not expose %s via grpc reflection: code=%d message=%s",
			errGRPCMethodUnsupported,
			fullMethod,
			reflectionErr.GetErrorCode(),
			reflectionErr.GetErrorMessage(),
		)
	}

	return fmt.Errorf("grpc reflection returned no descriptor for %s", fullMethod)
}

func grpcReflectionRecvError(fullMethod string, err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("grpc reflection stream closed without response for %s", fullMethod)
	}
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%w: grpc reflection is not enabled", errGRPCMethodUnsupported)
	}
	return fmt.Errorf("receive grpc reflection response for %s: %w", fullMethod, err)
}

func grpcReflectionServiceAndSymbol(fullMethod string) (string, string, error) {
	serviceAndMethod := strings.TrimPrefix(fullMethod, "/")
	service, method, ok := strings.Cut(serviceAndMethod, "/")
	if !ok || service == "" || method == "" {
		return "", "", fmt.Errorf("invalid grpc full method name %q", fullMethod)
	}
	return service, service + "." + method, nil
}
