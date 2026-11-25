// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import (
	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

// VerifyCommand is a placeholder until #2205 lands; it exits with the
// unsupported code.
func VerifyCommand() *cli.Command {
	return &cli.Command{
		Name:  "verify",
		Usage: "compare an image or this node with the lock (not implemented yet)",
		Action: func(_ *cli.Context) error {
			return oserr.New(oserr.CodeUnsupported, "verify is not implemented yet")
		},
	}
}
