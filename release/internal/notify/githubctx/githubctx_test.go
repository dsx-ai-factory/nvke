// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package githubctx

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestCollectOmitsCredentials is the guard on the allowlist. The runner's
// environment holds credentials under the GITHUB_ prefix; none of them may reach
// a template, whatever they are called.
func TestCollectOmitsCredentials(t *testing.T) {
	secrets := map[string]string{
		"GITHUB_TOKEN":                   "ghs_token-value",
		"ACTIONS_RUNTIME_TOKEN":          "runtime-token-value",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "id-token-value",
		"SLACK_BOT_TOKEN":                "xoxb-bot-token-value",
		"GITHUB_ENTERPRISE_SECRET":       "enterprise-secret-value",
	}
	got := Collect(env(secrets))

	for key, value := range got {
		for name, secret := range secrets {
			if strings.Contains(value, secret) {
				t.Errorf("Context[%q] carries %s", key, name)
			}
		}
	}
	for key, name := range fromEnv {
		if LooksLikeCredential(name) {
			t.Errorf("Context[%q] reads %s, which looks like a credential", key, name)
		}
	}
}

func TestCollect(t *testing.T) {
	got := Collect(env(map[string]string{
		"GITHUB_REPOSITORY": "dsx-ai-factory/nvke",
		"GITHUB_RUN_ID":     "42",
		"GITHUB_SHA":        "0123456789abcdef",
		"GITHUB_SERVER_URL": "https://github.com/",
	}))
	for key, want := range map[string]string{
		"run_url":    "https://github.com/dsx-ai-factory/nvke/actions/runs/42",
		"commit_url": "https://github.com/dsx-ai-factory/nvke/commit/0123456789abcdef",
		"sha_short":  "0123456",
		"actor":      "",
	} {
		if got[key] != want {
			t.Errorf("Context[%q] = %q, want %q", key, got[key], want)
		}
	}
}

func TestCollectOutsideActions(t *testing.T) {
	got := Collect(env(nil))
	if got["server_url"] != "https://github.com" {
		t.Errorf("server_url = %q, want the default", got["server_url"])
	}
	if got["run_url"] != "" {
		t.Errorf("run_url = %q, want empty when there is no run", got["run_url"])
	}
	for _, key := range Keys() {
		if _, ok := got[key]; !ok {
			t.Errorf("key %q is listed by Keys and missing from Collect", key)
		}
	}
}

func TestStateKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"override wins", map[string]string{"NOTIFY_STATE_KEY": "x", "GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v1", "GITHUB_RUN_ID": "9"}, "x"},
		{"a tag run is keyed by its tag", map[string]string{"GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v0.24.0-beta.3", "GITHUB_RUN_ID": "9"}, "v0.24.0-beta.3"},
		{"a branch run is keyed by its run, not the branch", map[string]string{"GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "main", "GITHUB_RUN_ID": "9"}, "9"},
		{"nothing set", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateKey(env(tc.env)); got != tc.want {
				t.Errorf("StateKey = %q, want %q", got, tc.want)
			}
		})
	}
}
