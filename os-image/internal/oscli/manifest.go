// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import (
	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

// ManifestCommand is a placeholder until #2202 lands; it exits with the
// unsupported code.
func ManifestCommand() *cli.Command {
	return &cli.Command{
		Name:  "manifest",
		Usage: "list the packages installed in an image or on this node (not implemented yet)",
		Action: func(_ *cli.Context) error {
			return oserr.New(oserr.CodeUnsupported, "manifest is not implemented yet")
		},
	}
}
