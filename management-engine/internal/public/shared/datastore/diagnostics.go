// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Datastore failures become public errors and diagnostic categories without exposing private values.
package datastore

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// PublicError translates datastore failures made with service credentials into safe public errors.
// resource is a service-owned label that must not contain caller input or upstream error text.
func PublicError(err error, resource string) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return status.Error(codes.DeadlineExceeded, "storage request timed out; a write may have completed")
	case apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, resource+" not found")
	case apierrors.IsAlreadyExists(err):
		return status.Error(codes.AlreadyExists, resource+" already exists")
	case apierrors.IsConflict(err):
		return status.Error(codes.Aborted, resource+" changed; read its current resource_version")
	case apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		return status.Error(codes.InvalidArgument, "page token expired; restart listing")
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return status.Error(codes.Internal, resource+" storage identity is not authorized; contact the service administrator")
	case apierrors.IsInternalError(err):
		return status.Error(codes.Internal, resource+" storage failed")
	case apierrors.IsTooManyRequests(err):
		return status.Error(codes.ResourceExhausted, resource+" storage is busy")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return status.Error(codes.InvalidArgument, resource+" configuration is invalid")
	default:
		return status.Error(codes.Unavailable, resource+" storage is unavailable")
	}
}

// ErrorAttributes reports safe failure categories and status codes for structured logs.
func ErrorAttributes(err error) []any {
	attrs := []any{"error_class", errorClass(err), "error_type", fmt.Sprintf("%T", err)}
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		attrs = append(attrs, "storage_status", apiStatus.Status().Code)
	}
	return attrs
}

// errorClass supplies actionable failure categories without logging sensitive error strings.
func errorClass(err error) string {
	_, unknownCA := errors.AsType[x509.UnknownAuthorityError](err)
	_, hostname := errors.AsType[x509.HostnameError](err)
	_, certificate := errors.AsType[x509.CertificateInvalidError](err)
	_, dns := errors.AsType[*net.DNSError](err)
	switch {
	case err == nil:
		return "invalid_storage_response"
	case apierrors.IsNotFound(err):
		return "storage_not_found"
	case apierrors.IsUnauthorized(err):
		return "storage_unauthorized"
	case apierrors.IsForbidden(err):
		return "storage_forbidden"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "storage_timeout"
	case apierrors.IsInternalError(err):
		return "storage_internal_error"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "storage_invalid_configuration"
	case unknownCA:
		return "tls_unknown_authority"
	case hostname:
		return "tls_hostname_mismatch"
	case certificate:
		return "tls_invalid_certificate"
	case dns:
		return "dns_lookup_failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	default:
		return "unexpected_failure"
	}
}
