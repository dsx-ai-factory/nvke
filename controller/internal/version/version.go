// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package version provides build version information.
package version

// Version is the build version, set via ldflags at build time.
// Example: -ldflags "-X github.com/dsx-ai-factory/nke/controller/internal/version.Version=v0.0.335-4-g8fcba71"
var Version = "dev" //nolint:gochecknoglobals // Set via ldflags at build time
