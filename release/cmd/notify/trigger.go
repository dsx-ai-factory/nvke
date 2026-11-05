// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/notify"
	"github.com/dsx-ai-factory/nvke/release/internal/notify/githubctx"
	"github.com/dsx-ai-factory/nvke/release/internal/notify/slack"
	"github.com/dsx-ai-factory/nvke/release/internal/notify/state"
)

const (
	tokenEnv       = "SLACK_BOT_TOKEN"
	defaultConfig  = "release/notify.yml"
	defaultStateAt = ".notify/cache"
)

// dataKey is the shape of an event key. Lower case with underscores, so a
// template reads it as .Event.stream_name.
var dataKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func triggerCmd() *cobra.Command {
	var (
		configAt string
		stateAt  string
		pairs    []string
		dataAt   string
		strict   bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "trigger <event>",
		Short: "Send what the config calls for on an event",
		Long: `Send what the config calls for on an event.

The event is a name such as qualification:stream-failed. The config decides who
is told and with what words; this only names the event and supplies the values
its templates read, as --data key=value or a JSON object in --data-file.

The bot token is read from ` + tokenEnv + ` and from nowhere else.

The first message of a conversation starts a Slack thread and its timestamp is
written under --state-dir; later messages reply in it. The conversation is the
tag for a tag run, otherwise the run id, or NOTIFY_STATE_KEY when that is set.
When a message started a thread this writes state-created=true to
$GITHUB_OUTPUT, so a caller that carries the directory between jobs saves it
only then.

A notification is a courtesy and a failed one must not fail a run, so a Slack
error, a missing token or a template error is a warning and the exit status is
zero. --strict makes them fatal. A config that does not load is always fatal.
--dry-run prints what would be sent, writes nothing and needs no token.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := notify.Load(configAt)
			if err != nil {
				return err
			}
			event := args[0]

			values, err := eventData(pairs, dataAt)
			if err != nil {
				return err
			}
			data := notify.Data{Context: githubctx.Collect(os.Getenv), Event: values}
			key := githubctx.StateKey(os.Getenv)

			n := &notify.Notifier{Config: cfg}
			if dryRun {
				n.Poster = notify.WriterPoster{W: cmd.OutOrStdout()}
				n.State = &notify.MemoryState{}
			} else {
				token := os.Getenv(tokenEnv)
				if token == "" {
					return problem(cmd, strict, fmt.Errorf("%s is not set, so nothing was sent (a run without secrets, such as one from a fork, ends here)", tokenEnv))
				}
				n.Poster = &slack.Client{Token: token}
				n.State = state.File{Dir: stateAt}
				n.Secrets = []string{token}
			}

			if key == "" && !dryRun {
				return problem(cmd, strict, errors.New("no conversation to thread under: set NOTIFY_STATE_KEY outside GitHub Actions"))
			}

			res, err := n.Trigger(cmd.Context(), event, data, key)
			if res.Matched == 0 {
				warn(cmd, fmt.Sprintf("no subscription matches %q, so nothing was sent", event))
			}
			// A dry run sent nothing and saved no thread, so it reports neither:
			// a cache step keyed on state-created would save state that is not
			// there.
			if !dryRun {
				if werr := writeOutputs(res); werr != nil {
					warn(cmd, werr.Error())
				}
			}
			if err != nil {
				return problem(cmd, strict, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configAt, "config", defaultConfig, "the notify config")
	cmd.Flags().StringVar(&stateAt, "state-dir", defaultStateAt, "where thread state is kept")
	cmd.Flags().StringArrayVar(&pairs, "data", nil, "a value for the templates, as key=value (repeatable)")
	cmd.Flags().StringVar(&dataAt, "data-file", "", "a JSON object of values for the templates")
	cmd.Flags().BoolVar(&strict, "strict", false, "fail when a notification cannot be sent")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be sent, and send nothing")
	return cmd
}

// problem reports an error as a warning, or returns it under --strict.
func problem(cmd *cobra.Command, strict bool, err error) error {
	if strict {
		return err
	}
	// errors.Join separates its parts with a newline, which would end a
	// workflow annotation early.
	warn(cmd, strings.ReplaceAll(err.Error(), "\n", "; "))
	return nil
}

// warn prints a warning, as an Actions annotation when it runs under Actions.
func warn(cmd *cobra.Command, msg string) {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Fprintf(cmd.OutOrStdout(), "::warning title=notify::%s\n", msg)
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "notify: warning:", msg)
}

// writeOutputs reports the result to the step's outputs when there are any.
func writeOutputs(res notify.Result) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("writing the step outputs: %w", err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "state-created=%t\nposted=%d\n", res.StateCreated, res.Posted)
	if err != nil {
		return fmt.Errorf("writing the step outputs: %w", err)
	}
	return nil
}

// eventData merges --data pairs over a --data-file. A pair wins, so a workflow
// can override one value of a file.
func eventData(pairs []string, file string) (map[string]string, error) {
	values := map[string]string{}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading --data-file: %w", err)
		}
		fromFile, err := decodeData(raw)
		if err != nil {
			return nil, fmt.Errorf("--data-file %s: %w", file, err)
		}
		for k, v := range fromFile {
			values[k] = v
		}
	}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("--data %q is not key=value", pair)
		}
		if !dataKey.MatchString(k) {
			return nil, fmt.Errorf("--data key %q is not lower case letters, digits and underscores", k)
		}
		values[k] = v
	}
	return values, nil
}

// decodeData reads a JSON object whose values are strings, numbers or booleans.
func decodeData(raw []byte) (map[string]string, error) {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	values := make(map[string]string, len(generic))
	for k, v := range generic {
		if !dataKey.MatchString(k) {
			return nil, fmt.Errorf("key %q is not lower case letters, digits and underscores", k)
		}
		switch v := v.(type) {
		case string:
			values[k] = v
		case float64, bool:
			values[k] = fmt.Sprint(v)
		default:
			return nil, fmt.Errorf("key %q is not a string, number or boolean", k)
		}
	}
	return values, nil
}
