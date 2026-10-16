// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package publish ships one stream's QualificationResult and JUnit report to
// a destination. Destination is the seam: today only TestGrid exists, but
// `qualify publish` is written against the interface so a second destination
// is additive, not a rewrite.
package publish

import (
	"context"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// Destination is where a stream's qualification evidence goes.
type Destination interface {
	// Name is the --destination value that selects this implementation.
	Name() string

	// Publish ships result and the JUnit report at junitPath. result's own
	// Status.Combination.Stream.Capabilities (gpu/kube and similar) carries
	// whatever routing data a destination needs -- stamped there by `qualify
	// result`, not looked up separately. Implementations own their own
	// evidence shape; Publish must not mutate result or the file at
	// junitPath.
	Publish(ctx context.Context, result *qualification.Result, junitPath string) error
}
