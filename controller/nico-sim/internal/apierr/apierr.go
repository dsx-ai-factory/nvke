// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package apierr defines errors shared by the VM and container backends.
// HTTP handlers use them to choose the response status.
package apierr

import "errors"

var (
	ErrNotFound          = errors.New("not found")
	ErrConflict          = errors.New("conflict")
	ErrBadRequest        = errors.New("bad request")
	ErrOwnershipConflict = errors.New("ownership conflict")
	ErrUnavailable       = errors.New("backend unavailable")
)
