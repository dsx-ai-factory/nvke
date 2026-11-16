// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseKapsBenchRunSpecs(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected []string
	}{
		{name: "one run spec", value: "smoke/smoke.yaml", expected: []string{"smoke/smoke.yaml"}},
		{name: "two run specs in order", value: "smoke/smoke.yaml,netperf/hostnet-stream.yaml", expected: []string{"smoke/smoke.yaml", "netperf/hostnet-stream.yaml"}},
		{name: "an empty value", value: ""},
		{name: "an empty entry", value: "smoke/smoke.yaml,"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs, err := parseKapsBenchRunSpecs(tc.value)
			if tc.expected == nil {
				t.Log("Testing: an empty entry is refused, so kaps-bench is never run with an empty run spec")
				require.ErrorContains(t, err, "empty")
				require.Nil(t, specs)
				return
			}
			t.Log("Testing: the run specs come back in the order given")
			require.NoError(t, err)
			require.Equal(t, tc.expected, specs)
		})
	}
}

func TestRequireKapsBenchTools(t *testing.T) {
	t.Log("Doing: kaps-bench folders holding the smoke run spec, each with or without a kaps-bench file")
	folder := func(binary bool, mode os.FileMode) string {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "run-specs", "smoke"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "run-specs", "smoke", "smoke.yaml"), nil, 0o600))
		if binary {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "kaps-bench"), nil, mode))
		}
		return dir
	}
	dir := folder(true, 0o700)
	file := filepath.Join(dir, "kaps-bench.yaml")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	wd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(wd, dir)
	require.NoError(t, err)
	specs := []string{"smoke/smoke.yaml"}
	found := func(file string) (string, error) { return "/usr/local/bin/" + file, nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	tests := []struct {
		name     string
		specs    []string
		dir      string
		lookPath func(string) (string, error)
		expected []string
	}{
		{name: "no run specs needs nothing", dir: "", lookPath: missing},
		{name: "everything is here", specs: specs, dir: dir, lookPath: found},
		{name: "no folder named", specs: specs, dir: "", lookPath: found, expected: []string{"E2E_KAPS_BENCH_DIR"}},
		{name: "a folder that does not exist", specs: specs, dir: filepath.Join(dir, "absent"), lookPath: found, expected: []string{"-e2e.kaps-bench-dir"}},
		{name: "a file in place of the folder", specs: specs, dir: file, lookPath: found, expected: []string{"is not a folder"}},
		{name: "a relative folder path", specs: specs, dir: relative, lookPath: found, expected: []string{"is not an absolute path"}},
		{name: "no kaps-bench in the folder", specs: specs, dir: folder(false, 0), lookPath: found, expected: []string{"has no kaps-bench to run"}},
		{name: "a kaps-bench without the execute bit", specs: specs, dir: folder(true, 0o600), lookPath: found, expected: []string{"has no kaps-bench to run", "permission denied"}},
		{name: "a run spec the folder lacks", specs: []string{"smoke/smoke.yaml", "smoke/smok.yaml"}, dir: dir, lookPath: found, expected: []string{"kaps-bench has no run spec smoke/smok.yaml"}},
		{name: "helm not on PATH", specs: specs, dir: dir, lookPath: missing, expected: []string{"helm is not on PATH"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Testing: each missing piece is named in the one error")
			err := requireKapsBenchTools(tc.specs, tc.dir, tc.lookPath)
			if len(tc.expected) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tc.expected {
				require.ErrorContains(t, err, want)
			}
			if tc.name != "a run spec the folder lacks" {
				t.Log("Testing: one cause gives one error, so a missing folder names no run spec")
				require.NotContains(t, err.Error(), "has no run spec")
			}
		})
	}
}

func TestPrometheusInstallArgs(t *testing.T) {
	t.Log("Testing: helm installs the pinned kube-prometheus-stack into monitoring and waits for it")
	require.Equal(t, []string{
		"upgrade", "--install", "prometheus", "kube-prometheus-stack",
		"--repo", "https://prometheus-community.github.io/helm-charts", "--version", "76.5.0",
		"--namespace", "monitoring", "--create-namespace",
		"--values", "/w/kube-prometheus-stack-values.yaml",
		"--wait", "--timeout", "10m0s",
	}, prometheusInstallArgs("/w/kube-prometheus-stack-values.yaml"))
}

func TestKapsBenchRunArgs(t *testing.T) {
	t.Log("Testing: kaps-bench runs the spec against the tenant and publishes locally")
	require.Equal(t, []string{
		"run", "smoke/smoke.yaml",
		"--kubeconfig", "/w/kubeconfig",
		"--cluster-context", "/w/cluster-context.yaml",
		"--output-dir", "/w/output",
		"--local-publisher",
	}, kapsBenchRunArgs("smoke/smoke.yaml", "/w/kubeconfig", "/w/cluster-context.yaml", "/w/output"))
}

func TestKapsBenchClusterContext(t *testing.T) {
	t.Log("Testing: the cluster context names the tenant and the UID of its kube-system namespace")
	document, err := kapsBenchClusterContext("az41-ganesh-test", "209b2b00-e17b-4d05-8073-dccbed7ef5f0")
	require.NoError(t, err)
	require.Equal(t, "cluster:\n  name: az41-ganesh-test\n  uuid: 209b2b00-e17b-4d05-8073-dccbed7ef5f0\n", string(document))
}

func TestKeepKapsBenchResults(t *testing.T) {
	tests := []struct {
		name       string
		work       string
		specFailed bool
		expected   string
	}{
		{name: "the run spec passed, so lost results fail it", work: t.TempDir(), expected: "archiving"},
		{name: "the run spec failed, so its failure stays the verdict", work: t.TempDir(), specFailed: true},
		{name: "no install ran, so there is nothing to archive", work: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "kaps-bench-results.tar.gz")
			t.Log("Testing: an output dir kaps-bench never wrote leaves no archive behind")
			err := keepKapsBenchResults(tc.work, archive, tc.specFailed)
			if tc.expected == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expected)
			}
			require.NoFileExists(t, archive)
		})
	}
}

func TestPrometheusInstallRunsOnce(t *testing.T) {
	tests := []struct {
		name     string
		step     func() (string, error)
		expected string
	}{
		{name: "an install that succeeds", step: func() (string, error) { return "/tmp/kaps-bench-1", nil }},
		{name: "an install whose helm fails", step: func() (string, error) {
			return "", errors.New("helm upgrade --install prometheus: exit status 1")
		}, expected: "helm upgrade --install prometheus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var install prometheusInstall
			calls := 0
			step := func() (string, error) { calls++; return tc.step() }
			t.Log("Testing: no earlier failure and no install yet means the run spec runs")
			require.Empty(t, install.skipReason(false))

			first, firstErr := install.ensure(step)
			second, secondErr := install.ensure(step)
			t.Log("Testing: two run specs install once and see the same dir and error")
			require.Equal(t, 1, calls)
			require.Equal(t, first, second)
			require.Equal(t, firstErr, secondErr)
			if tc.expected == "" {
				require.NoError(t, firstErr)
				require.Equal(t, "/tmp/kaps-bench-1", first)
				require.Empty(t, install.skipReason(true), "a failed run spec does not skip the next one")
				return
			}
			require.ErrorContains(t, firstErr, tc.expected)
			require.Empty(t, first)
			t.Log("Testing: later run specs skip, naming the install failure")
			require.Contains(t, install.skipReason(false), "Prometheus did not install on the tenant: ")
		})
	}

	t.Run("an earlier tenant failure skips before any install", func(t *testing.T) {
		var install prometheusInstall
		require.Equal(t, "an earlier check on this tenant failed", install.skipReason(true))
	})

	t.Run("an install that panics", func(t *testing.T) {
		var install prometheusInstall
		require.Panics(t, func() { _, _ = install.ensure(func() (string, error) { panic("relay closed") }) })
		t.Log("Testing: the stored error stays, so no run spec reads a half-written work dir")
		work, err := install.ensure(func() (string, error) { t.Fatal("installed twice"); return "", nil })
		require.Empty(t, work)
		require.ErrorContains(t, err, "the Prometheus install did not finish")
	})
}

func TestKapsBenchSpecTimeoutAndToken(t *testing.T) {
	tests := []struct {
		name     string
		index    int
		expected time.Duration
	}{
		{name: "the first run spec also installs Prometheus", index: 0, expected: 45 * time.Minute},
		{name: "a later run spec", index: 1, expected: 30 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Testing: the token outlasts the run spec's own limit by five minutes")
			require.Equal(t, tc.expected, kapsBenchSpecTimeout(tc.index))
			require.Equal(t, tc.expected+5*time.Minute, kapsBenchTokenLifetime(tc.index))
		})
	}
}

func TestArchiveKapsBenchResults(t *testing.T) {
	t.Log("Doing: an output dir laid out the way kaps-bench writes one")
	output := t.TempDir()
	run := filepath.Join(output, "01a1c2e3-1f76-726d-8550-5149212bdcb2")
	require.NoError(t, os.MkdirAll(run, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "preflight.json"), []byte(`{"passed":true}`), 0o600))
	archive := filepath.Join(t.TempDir(), "kaps-bench-results.tar.gz")

	t.Log("Testing: the archive holds every file under the output dir by its relative path")
	require.NoError(t, archiveKapsBenchResults(output, archive))
	f, err := os.Open(archive)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[hdr.Name] = string(content)
		}
	}
	require.Equal(t, map[string]string{"01a1c2e3-1f76-726d-8550-5149212bdcb2/preflight.json": `{"passed":true}`}, files)

	t.Log("Testing: a failed rewrite keeps the archive already there and leaves no temp file")
	before, readErr := os.ReadFile(archive)
	require.NoError(t, readErr)
	require.ErrorContains(t, archiveKapsBenchResults(filepath.Join(output, "absent"), archive), "archiving")
	after, readErr := os.ReadFile(archive)
	require.NoError(t, readErr)
	require.Equal(t, before, after)
	entries, readErr := os.ReadDir(filepath.Dir(archive))
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
}
