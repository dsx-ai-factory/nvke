// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Datastore failures become stable public errors and diagnostics that omit private values.
package cluster

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/datastore"
)

// storageError distinguishes caller outcomes from storage failures that operators must diagnose.
func (s *Service) storageError(ctx context.Context, operation, name string, err error) error {
	if apierrors.IsNotFound(err) && operation != "get" && operation != "replace" {
		s.logFailure(ctx, operation, name, "cluster datastore configuration is unavailable", err)
		return status.Error(codes.Internal, "cluster storage is not configured; contact the service administrator")
	}
	public := datastore.PublicError(err, "cluster")
	switch status.Code(public) {
	case codes.Internal, codes.Unavailable, codes.DeadlineExceeded:
		s.logFailure(ctx, operation, name, "cluster datastore request failed", err)
	case codes.InvalidArgument:
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			s.logFailure(ctx, operation, name, "cluster datastore rejected configuration", err)
		}
	}
	return public
}

// internalError records an internal failure while returning only its reviewed public message.
func (s *Service) internalError(ctx context.Context, operation, name, message string, err error) error {
	s.logFailure(ctx, operation, name, message, err)
	return status.Error(codes.Internal, message)
}

// logFailure records classifications, never error strings: Kubernetes errors and
// wrapped network errors can contain request bodies, credentials and private URLs.
func (s *Service) logFailure(ctx context.Context, operation, name, message string, err error) {
	if name != "" && len(validation.IsDNS1123Subdomain(name)) != 0 {
		name = "invalid"
	}
	attrs := append(datastore.ErrorAttributes(err), "operation", operation,
		"resource", clusterResource.Resource, "namespace", s.config.Namespace, "name", name)
	s.config.Logger.ErrorContext(ctx, message, attrs...)
}
