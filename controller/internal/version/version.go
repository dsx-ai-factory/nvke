// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package version provides build version information.
package version

// Version is the build version, set via ldflags at build time.
// Example: -ldflags "-X github.com/NVIDIA/nke/controller/internal/version.Version=v0.0.335-4-g8fcba71"
var Version = "dev" //nolint:gochecknoglobals // Set via ldflags at build time
