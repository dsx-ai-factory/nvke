//go:build !linux && !darwin

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import "os/exec"

func configureProcess(_ *exec.Cmd) {}
func stopProcess(_ *exec.Cmd)      {}
