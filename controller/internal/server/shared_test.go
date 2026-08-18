// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// mockDB and mockTx are aliases so existing server tests can keep using the same type names.
type mockDB = testutil.MockDB

type mockTx = testutil.MockTx
