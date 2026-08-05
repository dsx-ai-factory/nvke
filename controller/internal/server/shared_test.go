// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server_test

import (
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// mockDB and mockTx are aliases so existing server tests can keep using the same type names.
type mockDB = testutil.MockDB

type mockTx = testutil.MockTx
