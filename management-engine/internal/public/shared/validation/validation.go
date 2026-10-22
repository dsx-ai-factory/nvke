// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package validation checks protobuf input and reports field errors without resource-specific policy.
package validation

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protopath"
	"google.golang.org/protobuf/reflect/protorange"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ValidMessage rejects missing, oversized or unrecognized protobuf input before resource operations.
func ValidMessage(message proto.Message, maxBytes int) bool {
	if message == nil || !message.ProtoReflect().IsValid() || proto.Size(message) > maxBytes {
		return false
	}
	unknown := false
	// Inspect declared fields without interpreting opaque Any payloads.
	options := protorange.Options{Resolver: (*protoregistry.Types)(nil)}
	_ = options.Range(message.ProtoReflect(), func(path protopath.Values) error {
		if path.Index(-1).Step.Kind() == protopath.UnknownAccessStep {
			unknown = true
			return protorange.Terminate
		}
		return nil
	}, nil)
	return !unknown
}

// FieldViolation attaches a field path so callers can identify and correct invalid input.
// Callers supply reviewed descriptions without private values or upstream error text.
func FieldViolation(field, description string) error {
	base := status.New(codes.InvalidArgument, field+": "+description)
	detailed, err := base.WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
		Field: field, Description: description,
	}}})
	if err != nil {
		return base.Err()
	}
	return detailed.Err()
}
