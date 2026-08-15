// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	// cobra requires package-level vars for PersistentFlags binding.
	configFile string //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	verbose    bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
	jsonLogs   bool   //nolint:gochecknoglobals // cobra PersistentFlags binding requires package-level var
)

func newRootCmd(ver, commit, treeState, date string) *cobra.Command {
	root := &cobra.Command{
		Use:          "nkectl",
		Short:        "NKE control-plane CLI",
		SilenceUsage: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return setupLogging(verbose, jsonLogs)
		},
	}

	root.PersistentFlags().StringVar(&configFile, "config", "",
		"Path to bootstrap spec file (default resolved by config loader in C2)")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.PersistentFlags().BoolVar(&jsonLogs, "json-logs", false, "Emit logs as JSON")

	root.AddCommand(newVersionCmd(ver, commit, treeState, date))
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBootstrapCmd())

	return root
}

// Execute is the entry point called from main.
func Execute(ver, commit, treeState, date string) error {
	return newRootCmd(ver, commit, treeState, date).Execute()
}

func setupLogging(verbose, jsonLogs bool) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if jsonLogs {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}
