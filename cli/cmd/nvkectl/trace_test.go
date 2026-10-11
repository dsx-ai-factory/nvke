// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

// These tests install a process-wide tracer provider, so none is parallel.

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

// runNkectl executes nvkectl with args against a config pointing at url, returning
// stdout and the command's error as Execute reports it.
func runNkectl(t *testing.T, url string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// No kubeconfig, so the client sends no bearer token and mints none.
	cfg := &config.Config{CurrentSite: "t", Sites: map[string]config.SiteConfig{"t": {URL: url}}}
	require.NoError(t, cfg.Save())

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs(args)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := execute(context.Background(), root, "v1.2.3", &out)
	return out.String(), err
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
	out, err := runNkectl(t, f.URL, "delete", "cluster", "demo")
	require.NoError(t, err)

	m := traceIDLineRE.FindStringSubmatch(out)
	require.NotNil(t, m, "stdout:\n%s", out)
	ids := traceparents(t, f)
	require.Len(t, ids, 2, "lookup and delete")
	for _, id := range ids {
		assert.Equal(t, m[1], id, "every request carries the printed trace ID")
	}
	assert.Len(t, traceIDLineRE.FindAllString(out, -1), 1, "printed once")
}

func TestTraceIDOnFailureWhenControllerRejects(t *testing.T) {
	f := newFakeController(t)
	f.status = http.StatusInternalServerError
	_, err := runNkectl(t, f.URL, "get", "clusters")
	require.Error(t, err)

	ids := traceparents(t, f)
	require.Len(t, ids, 1)
	assert.Contains(t, err.Error(), "\nTrace ID: "+ids[0])
}

func TestTraceIDOnFailureWhenRequestNeverReachesController(t *testing.T) {
	f := newFakeController(t)
	f.Close()
	_, err := runNkectl(t, f.URL, "get", "clusters")
	require.Error(t, err)
	assert.Regexp(t, `\nTrace ID: [0-9a-f]{32}$`, err.Error())
}

func TestGetPrintsNoTraceIDOnSuccess(t *testing.T) {
	f := newFakeController(t)
	out, err := runNkectl(t, f.URL, "get", "clusters", "-o", "json")
	require.NoError(t, err)
	assert.NotContains(t, out, "Trace ID", "a successful get keeps stdout to the result")
	assert.Len(t, traceparents(t, f), 1, "the request is still traced")
}

func TestQueryCommandsTraceWithoutPrinting(t *testing.T) {
	f := newFakeController(t)
	out, err := runNkectl(t, f.URL, "version")
	require.NoError(t, err)
	assert.NotContains(t, out, "Trace ID", "a successful query command writes only its result")
}

func TestSuccessfulQueryLogsTraceIDAtDebug(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	root := newRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"get", "clusters"})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, announceTrace(cmd, &out, "4bf92f3577b34da6a3ce929d0e0e4736", nil))

	assert.Empty(t, out.String())
	assert.Contains(t, logs.String(), `"msg":"command finished"`)
	assert.Contains(t, logs.String(), `"command":"nvkectl get clusters"`)
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

	err := execute(context.Background(), root, "test", &stdout)
	require.Error(t, err)

	m := regexp.MustCompile(`(?m)^Trace ID: ([0-9a-f]{32})$`).FindStringSubmatch(stderr.String())
	require.NotNil(t, m, "stderr:\n%s", stderr.String())
	assert.Contains(t, err.Error(), "\nTrace ID: "+m[1], "the error ends with the same ID")
	assert.Empty(t, stdout.String(), "a failure before any result writes nothing to stdout")
}
