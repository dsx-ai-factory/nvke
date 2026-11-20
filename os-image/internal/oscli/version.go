// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import "github.com/urfave/cli/v2"

// VersionResultSchema versions the version verb's JSON result. A breaking
// change to its shape moves it.
const VersionResultSchema = "os.nvke.dev/version-result/v1"

type versionResult struct {
	SchemaVersion string `json:"schemaVersion"`
	Name          string `json:"name"`
	Version       string `json:"version"`
}

// VersionCommand prints the binary's stamped version.
func VersionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print the version",
		Flags: []cli.Flag{formatFlag()},
		Action: func(c *cli.Context) error {
			result := versionResult{SchemaVersion: VersionResultSchema, Name: c.App.Name, Version: c.App.Version}
			return writeResult(c, result, result.Name+" version "+result.Version)
		},
	}
}
