// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/notify"
)

func validateCmd() *cobra.Command {
	var configAt string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a config, and list the keys each notification reads",
		Long: `Check a config without sending anything.

It fails on an unknown field, a subscription that names a subscriber or a
notification that does not exist, a template that does not parse or reads a key
outside the allowlist, and a channel or mention that is malformed. It then lists
the .Event keys each notification reads, which are the values a workflow has to
pass with --data.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := notify.Load(configAt)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ok: %d subscribers, %d notifications, %d subscriptions\n",
				len(cfg.Subscribers), len(cfg.Notifications), len(cfg.Subscriptions))

			for _, n := range cfg.Notifications {
				keys := map[string]bool{}
				for _, text := range []string{n.Title, n.MessageTemplate} {
					refs, err := notify.References(text)
					if err != nil {
						return fmt.Errorf("notification %q: %w", n.Name, err)
					}
					for _, k := range refs.Event {
						keys[k] = true
					}
				}
				list := make([]string, 0, len(keys))
				for k := range keys {
					list = append(list, k)
				}
				sort.Strings(list)
				if len(list) == 0 {
					list = []string{"(none)"}
				}
				fmt.Fprintf(out, "  %-28s data: %s\n", n.Name, strings.Join(list, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configAt, "config", defaultConfig, "the notify config")
	return cmd
}
