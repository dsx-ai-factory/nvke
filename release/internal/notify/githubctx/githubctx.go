// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package githubctx builds the template context for a GitHub Actions run.
//
// The runner's environment carries credentials under the same GITHUB_ prefix as
// the values worth printing (GITHUB_TOKEN among them), and a template that can
// reach the whole environment can print any of it. So nothing here ranges over
// the environment: each key is read by name from a fixed list, and the list is
// the whole context.
package githubctx

import (
	"regexp"
	"strings"
)

// fromEnv maps each context key to the one environment variable it is read
// from. Adding an entry publishes that variable to every template, so it is a
// review-worthy change; the test beside this file fails on an entry that looks
// like a credential.
var fromEnv = map[string]string{
	"repository":  "GITHUB_REPOSITORY",
	"workflow":    "GITHUB_WORKFLOW",
	"run_id":      "GITHUB_RUN_ID",
	"run_number":  "GITHUB_RUN_NUMBER",
	"run_attempt": "GITHUB_RUN_ATTEMPT",
	"ref_name":    "GITHUB_REF_NAME",
	"ref_type":    "GITHUB_REF_TYPE",
	"sha":         "GITHUB_SHA",
	"actor":       "GITHUB_ACTOR",
	"event_name":  "GITHUB_EVENT_NAME",
	"server_url":  "GITHUB_SERVER_URL",
	"job":         "GITHUB_JOB",
}

// derived are keys computed from the values above.
var derived = []string{"sha_short", "run_url", "commit_url"}

const defaultServerURL = "https://github.com"

// Keys returns every key a template may reference as .Context.<key>.
func Keys() []string {
	keys := make([]string, 0, len(fromEnv)+len(derived))
	for k := range fromEnv {
		keys = append(keys, k)
	}
	return append(keys, derived...)
}

// Collect reads the context through getenv. A variable that is unset yields an
// empty value, so a template renders outside Actions, where the same keys exist
// and are blank.
func Collect(getenv func(string) string) map[string]string {
	ctx := make(map[string]string, len(fromEnv)+len(derived))
	for key, name := range fromEnv {
		ctx[key] = getenv(name)
	}

	server := strings.TrimRight(ctx["server_url"], "/")
	if server == "" {
		server = defaultServerURL
	}
	ctx["server_url"] = server

	if len(ctx["sha"]) >= 7 {
		ctx["sha_short"] = ctx["sha"][:7]
	} else {
		ctx["sha_short"] = ctx["sha"]
	}
	// Present and empty when there is no run to link, so a template that reads
	// them still renders.
	ctx["run_url"], ctx["commit_url"] = "", ""
	if repo := ctx["repository"]; repo != "" {
		if id := ctx["run_id"]; id != "" {
			ctx["run_url"] = server + "/" + repo + "/actions/runs/" + id
		}
		if sha := ctx["sha"]; sha != "" {
			ctx["commit_url"] = server + "/" + repo + "/commit/" + sha
		}
	}
	return ctx
}

// StateKey names the conversation a run's messages belong to.
//
// NOTIFY_STATE_KEY wins. Otherwise a run on a tag is keyed by its tag, so the
// jobs of the qualification a beta tag starts, which run on that tag, share one
// thread. Any other run is keyed by its run id: a branch name would put every
// dispatch from that branch into one thread.
//
// A run on a branch that announces a tag, such as the nightly that cuts it, is
// therefore in a different conversation from the qualification the tag starts.
// To join them it has to set NOTIFY_STATE_KEY to the tag. Falling back to the
// commit would not do it: the nightly tags a commit it selected, which need not
// be the commit it ran on.
func StateKey(getenv func(string) string) string {
	if key := getenv("NOTIFY_STATE_KEY"); key != "" {
		return key
	}
	if getenv("GITHUB_REF_TYPE") == "tag" && getenv("GITHUB_REF_NAME") != "" {
		return getenv("GITHUB_REF_NAME")
	}
	return getenv("GITHUB_RUN_ID")
}

// credentialName matches the names of variables that hold a secret.
var credentialName = regexp.MustCompile(`(?i)token|secret|key|password|credential`)

// LooksLikeCredential reports whether an environment variable name suggests a
// secret. It exists for the test that guards the allowlist.
func LooksLikeCredential(name string) bool { return credentialName.MatchString(name) }
