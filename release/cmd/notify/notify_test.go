// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/notify"
)

// run executes the command with args and returns what it printed and its error.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := root()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

const shipped = "../../notify.yml"

func TestValidateShippedConfig(t *testing.T) {
	out, err := run(t, "validate", "--config", shipped)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ok:", "stream-failed", "stage, stream"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestValidateFailsOnABadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.yml")
	if err := os.WriteFile(path, []byte("subscribers: [{id: x}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "validate", "--config", path); err == nil {
		t.Error("validate accepted a config with no channel")
	}
}

func TestTriggerDryRun(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITHUB_TOKEN", "ghs_must-not-print")
	out, err := run(t, "trigger", "qualification:stream-failed", "--dry-run", "--config", shipped,
		"--data", "stream=nvke-gb200", "--data", "stage=bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"C0CF37FNXV1", "Stream failed", "nvke-gb200", "bootstrap"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ghs_must-not-print") {
		t.Error("the runner's token reached the output")
	}
}

// A fork's run has no secrets. A notification is a courtesy, so that is a
// warning and not a failed run.
func TestTriggerWithoutATokenWarns(t *testing.T) {
	t.Setenv(tokenEnv, "")
	t.Setenv("GITHUB_ACTIONS", "true")
	out, err := run(t, "trigger", "qualification:completed", "--config", shipped,
		"--data", "verdict=passed", "--data", "summary=ok")
	if err != nil {
		t.Fatalf("a missing token failed the step: %v", err)
	}
	if !strings.Contains(out, "::warning") || !strings.Contains(out, tokenEnv) {
		t.Errorf("output = %q, want an annotation naming the token", out)
	}

	if _, err := run(t, "trigger", "qualification:completed", "--config", shipped, "--strict",
		"--data", "verdict=passed", "--data", "summary=ok"); err == nil {
		t.Error("--strict let a missing token pass")
	}
}

// A config that does not load is a bug, not an outage, so it is never quiet.
func TestTriggerWithABadConfigFails(t *testing.T) {
	if _, err := run(t, "trigger", "x", "--dry-run", "--config", filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("a missing config did not fail")
	}
}

func TestTriggerWarnsOnAnEventNothingSubscribesTo(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	out, err := run(t, "trigger", "qualification:typo", "--dry-run", "--config", shipped)
	if err != nil || !strings.Contains(out, "no subscription matches") {
		t.Errorf("= %q, %v", out, err)
	}
}

// A dry run sends nothing and saves no thread, so it must not say it did: a
// cache step keyed on state-created would save state that does not exist.
func TestDryRunWritesNoStepOutputs(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "output")
	t.Setenv("GITHUB_OUTPUT", outputs)
	t.Setenv("GITHUB_ACTIONS", "")
	if _, err := run(t, "trigger", "qualification:tag-cut", "--dry-run", "--config", shipped,
		"--data", "tag=v1.0.0-beta.1", "--data", "commit=abc1234def", "--data", "commit_short=abc1234"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outputs)
	if err == nil && len(got) != 0 {
		t.Errorf("a dry run wrote outputs: %q", got)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestWriteOutputs(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "output")
	t.Setenv("GITHUB_OUTPUT", outputs)
	if err := writeOutputs(notify.Result{Posted: 2, StateCreated: true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "state-created=true\nposted=2\n" {
		t.Errorf("outputs = %q", got)
	}

	// Without GITHUB_OUTPUT there is nowhere to write, and that is not an error.
	t.Setenv("GITHUB_OUTPUT", "")
	if err := writeOutputs(notify.Result{}); err != nil {
		t.Errorf("writeOutputs outside Actions: %v", err)
	}
}

func TestEventData(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(file, []byte(`{"stream":"a","count":3,"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := eventData([]string{"stream=b"}, file)
	if err != nil {
		t.Fatal(err)
	}
	if got["stream"] != "b" || got["count"] != "3" || got["ok"] != "true" {
		t.Errorf("data = %v; want the pair to win over the file", got)
	}

	for _, tc := range []struct {
		name  string
		pairs []string
		body  string
	}{
		{"no equals", []string{"stream"}, ""},
		{"upper case key", []string{"Stream=a"}, ""},
		{"key with a dot", []string{"a.b=c"}, ""},
		{"nested JSON", nil, `{"a":{"b":1}}`},
		{"not an object", nil, `[1]`},
		{"bad key in JSON", nil, `{"A-b":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.body != "" {
				path = filepath.Join(t.TempDir(), "d.json")
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := eventData(tc.pairs, path); err == nil {
				t.Error("accepted it")
			}
		})
	}
}
