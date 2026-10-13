// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

// These tests run commands through execute, which installs the process's default
// slog logger, so none is parallel.

var traceIDLineRE = regexp.MustCompile(`(?m)^Trace ID: ([0-9a-f]{32})$`)

// fakeController records the headers of every request and answers the calls
// "delete cluster" makes.
type fakeController struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	status  int // overrides every response when non-zero
}

func newFakeController(t *testing.T) *fakeController {
	t.Helper()
	f := &fakeController{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"boom"}`, status)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clusters":[{"id":"3f1c9a2e-0000-4000-8000-000000000000","name":"demo"}]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// runNvkectl executes nvkectl with args against a config pointing at url,
// returning stdout, stderr and the command's error as Execute reports it.
func runNvkectl(t *testing.T, url string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// No kubeconfig, so the client sends no bearer token and mints none.
	cfg := &config.Config{CurrentSite: "t", Sites: map[string]config.SiteConfig{"t": {URL: url}}}
	require.NoError(t, cfg.Save())

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs(args)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = execute(context.Background(), root, "v1.2.3", &errOut)
	return out.String(), errOut.String(), err
}

func traceparents(t *testing.T, f *fakeController) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, h := range f.headers {
		m := regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-01$`).FindStringSubmatch(h.Get("Traceparent"))
		require.NotNil(t, m, "traceparent %q", h.Get("Traceparent"))
		assert.Equal(t, "nvkectl/v1.2.3", h.Get("User-Agent"))
		ids = append(ids, m[1])
	}
	return ids
}

func TestTraceIDPrintedOnSuccessMatchesTraceparent(t *testing.T) {
	f := newFakeController(t)
	stdout, stderr, err := runNvkectl(t, f.URL, "delete", "cluster", "demo")
	require.NoError(t, err)

	m := traceIDLineRE.FindStringSubmatch(stderr)
	require.NotNil(t, m, "stderr:\n%s", stderr)
	ids := traceparents(t, f)
	require.Len(t, ids, 2, "lookup and delete")
	for _, id := range ids {
		assert.Equal(t, m[1], id, "every request carries the printed trace ID")
	}
	assert.Len(t, traceIDLineRE.FindAllString(stderr, -1), 1, "printed once")
	assert.NotContains(t, stdout, "Trace ID", "stdout keeps only the result")
}

func TestTraceIDOnFailureWhenControllerRejects(t *testing.T) {
	f := newFakeController(t)
	f.status = http.StatusInternalServerError
	_, _, err := runNvkectl(t, f.URL, "get", "clusters")
	require.Error(t, err)

	ids := traceparents(t, f)
	require.Len(t, ids, 1)
	assert.Contains(t, err.Error(), "\nTrace ID: "+ids[0])
}

func TestTraceIDOnFailureWhenRequestNeverReachesController(t *testing.T) {
	f := newFakeController(t)
	f.Close()
	_, _, err := runNvkectl(t, f.URL, "get", "clusters")
	require.Error(t, err)
	assert.Regexp(t, `\nTrace ID: [0-9a-f]{32}$`, err.Error())
}

// Cobra rejects these before it runs any hook.
func TestTraceIDOnFailureBeforeAnyHookRuns(t *testing.T) {
	f := newFakeController(t)
	tests := map[string][]string{
		"unknown flag":        {"get", "clusters", "--bogus"},
		"extra argument":      {"bootstrap", "site", "-f", "sitespec.yaml", "extra"},
		"missing required":    {"create", "cluster"},
		"extra to a mutation": {"delete", "cluster", "a", "b"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runNvkectl(t, f.URL, args...)
			require.Error(t, err)
			assert.Regexp(t, `\nTrace ID: [0-9a-f]{32}$`, err.Error())
			assert.Empty(t, stdout)
		})
	}
	assert.Empty(t, traceparents(t, f), "nothing reached the controller")
}

func TestNoTraceIDForHelpAndCompletionFailures(t *testing.T) {
	f := newFakeController(t)
	for _, args := range [][]string{{"completion", "--bogus"}, {"help", "--bogus"}, {"bogus"}} {
		_, _, err := runNvkectl(t, f.URL, args...)
		require.Error(t, err, args)
		assert.NotContains(t, err.Error(), "Trace ID", args)
	}
}

// A subcommand's own PersistentPreRunE does not affect tracing.
func TestSubcommandPreRunDoesNotDisableTracing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := newRootCmd("test", "test", "test", "test")
	childHookRan := false
	root.AddCommand(&cobra.Command{
		Use:               "probe",
		PersistentPreRunE: func(*cobra.Command, []string) error { childHookRan = true; return nil },
		RunE:              func(*cobra.Command, []string) error { return nil },
	})
	root.SetArgs([]string{"probe"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)

	require.NoError(t, execute(context.Background(), root, "test", &stderr))

	assert.True(t, childHookRan)
	assert.Regexp(t, traceIDLineRE, stderr.String())
}

func TestQueryCommandsPrintNoTraceIDOnSuccess(t *testing.T) {
	f := newFakeController(t)
	for _, args := range [][]string{{"get", "clusters", "-o", "json"}, {"version"}} {
		stdout, stderr, err := runNvkectl(t, f.URL, args...)
		require.NoError(t, err, args)
		assert.NotContains(t, stdout+stderr, "Trace ID", "%v writes only its result", args)
	}
	assert.Len(t, traceparents(t, f), 1, "get still sends a traced request")
}

func TestStateChangingCommandsPrintTraceIDOnSuccess(t *testing.T) {
	root := newRootCmd("test", "test", "test", "test")
	for _, path := range []string{"create cluster", "patch nodepool", "delete cluster", "deprecate", "bootstrap site"} {
		cmd, _, err := root.Find(strings.Fields(path))
		require.NoError(t, err, path)
		var out bytes.Buffer
		require.NoError(t, announceTrace(cmd, &out, "4bf92f3577b34da6a3ce929d0e0e4736", nil))
		assert.Equal(t, "Trace ID: 4bf92f3577b34da6a3ce929d0e0e4736\n", out.String(), path)
	}
}

func TestIsTraced(t *testing.T) {
	tests := map[string]bool{
		"create cluster":         true,
		"create nodepool":        true,
		"get clusters":           true,
		"get nodepools":          true,
		"get bootstrap-manifest": true,
		"patch cluster":          true,
		"delete nodepool":        true,
		"bootstrap site":         true,
		"version":                true,
		"config view":            true,
		"generate sitespec":      true,
		"help":                   false,
		"completion bash":        false,
	}
	root := newRootCmd("test", "test", "test", "test")
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for path, want := range tests {
		cmd, _, err := root.Find(strings.Fields(path))
		require.NoError(t, err, path)
		assert.Equal(t, want, isTraced(cmd), path)
	}
	assert.False(t, isTraced(root), "the root command alone prints help")
}

func TestBootstrapSiteReportsTraceIDOnStderr(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := newRootCmd("test", "test", "test", "test")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"bootstrap", "site", "-f", writeSiteSpec(t, "not: a site spec")})

	err := execute(context.Background(), root, "test", &stderr)
	require.Error(t, err)

	m := regexp.MustCompile(`(?m)^Trace ID: ([0-9a-f]{32})$`).FindStringSubmatch(stderr.String())
	require.NotNil(t, m, "stderr:\n%s", stderr.String())
	assert.Contains(t, err.Error(), "\nTrace ID: "+m[1], "the error ends with the same ID")
	assert.Empty(t, stdout.String(), "a failure before any result writes nothing to stdout")
}

// Cobra answers -h and --help, and a bare group such as "create", without
// running anything, so a mutating command must not report a trace ID for it.
func TestHelpFlagPrintsNoTraceID(t *testing.T) {
	f := newFakeController(t)
	for _, args := range [][]string{
		{"delete", "cluster", "--help"},
		{"create", "cluster", "-h"},
		{"patch", "nodepool", "--help"},
		{"bootstrap", "site", "--help"},
		{"deprecate", "--help"},
		{"deprecate"},
		{"create"},
		{"bootstrap"},
	} {
		stdout, stderr, err := runNvkectl(t, f.URL, args...)
		require.NoError(t, err, args)
		assert.Contains(t, stdout, "Usage:", args)
		assert.Empty(t, stderr, "%v writes nothing to stderr", args)
	}
	assert.Empty(t, traceparents(t, f), "no request was made")
}
