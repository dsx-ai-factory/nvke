// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

// PtrTo returns a pointer to the given value.
// Useful for creating pointers to literals in tests.
func PtrTo[T any](v T) *T {
	return &v
}
