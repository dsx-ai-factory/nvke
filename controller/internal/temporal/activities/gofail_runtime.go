// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build gofail

package activities

// Gofail generates temporary *.fail.go files that import this runtime package.
// Keep the import in committed gofail-tagged code so go mod tidy does not prune it.
import _ "go.etcd.io/gofail/runtime"
