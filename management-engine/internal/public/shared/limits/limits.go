// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package limits bounds public requests and datastore operations.
package limits

import "time"

const (
	// RequestBytes bounds both wire requests and decoded protobuf requests.
	RequestBytes = 64 << 10
	// PageTokenKeyMinBytes is the minimum raw pagination-key length.
	PageTokenKeyMinBytes = 32
	// PageTokenKeyMaxBytes bounds pagination-key configuration files.
	PageTokenKeyMaxBytes = 64
	// StorageTimeout bounds a datastore operation even without an ingress deadline.
	StorageTimeout = 5 * time.Second
	// ProbeTimeout bounds datastore readiness checks.
	ProbeTimeout = time.Second
)
