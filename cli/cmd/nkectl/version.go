// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// VersionInfo holds build-time metadata.
type VersionInfo struct {
	BuildVersion string `json:"buildVersion" yaml:"buildVersion"`
	GitCommit    string `json:"gitCommit"    yaml:"gitCommit"`
	GitTreeState string `json:"gitTreeState" yaml:"gitTreeState"`
	BuildDate    string `json:"buildDate"    yaml:"buildDate"`
	GoVersion    string `json:"goVersion"    yaml:"goVersion"`
	Compiler     string `json:"compiler"     yaml:"compiler"`
	Platform     string `json:"platform"     yaml:"platform"`
}

func newVersionInfo(ver, commit, treeState, date string) VersionInfo {
	return VersionInfo{
		BuildVersion: ver,
		GitCommit:    commit,
		GitTreeState: treeState,
		BuildDate:    date,
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

func newVersionCmd(ver, commit, treeState, date string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := newVersionInfo(ver, commit, treeState, date)
			of, err := cmd.Flags().GetString("output")
			if err != nil {
				return fmt.Errorf("reading output flag: %w", err)
			}
			switch of {
			case "":
				fmt.Fprintf(os.Stdout, "nkectl %s (commit: %s, built: %s)\n",
					v.BuildVersion, v.GitCommit, v.BuildDate)
			case "short":
				fmt.Fprintln(os.Stdout, v.BuildVersion)
			case "yaml":
				out, err := yaml.Marshal(&v)
				if err != nil {
					return fmt.Errorf("marshaling yaml: %w", err)
				}
				fmt.Fprint(os.Stdout, string(out))
			case "json":
				out, err := json.MarshalIndent(&v, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling json: %w", err)
				}
				fmt.Fprintln(os.Stdout, string(out))
			default:
				return fmt.Errorf("unknown output format %q; supported: short, yaml, json", of)
			}
			return nil
		},
	}
	cmd.Flags().StringP("output", "o", "", "Output format (short|yaml|json)")
	return cmd
}
