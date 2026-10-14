// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import "github.com/dsx-ai-factory/nvke/controller/internal/keys"

// testKeys is the default key set: every key under nke.nvidia.com and the
// nke-system management namespace.
var testKeys = keys.New("", "")
