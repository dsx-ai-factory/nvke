// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import "github.com/urfave/cli/v2"

// versionResult is the data of a Version result.
type versionResult struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// VersionCommand prints the binary's stamped version.
func VersionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print the version",
		Action: func(c *cli.Context) error {
			result := versionResult{Name: c.App.Name, Version: c.App.Version}
			return writeResult(c, "Version", result, result.Name+" version "+result.Version)
		},
	}
}
